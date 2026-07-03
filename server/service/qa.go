package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// QaService 个股 AI 问答：首轮采集一次个股数据快照并落库，之后多轮追问复用该快照，
// 不再重复拉数据。复用 ai_client（SafeHTTPClient 防 SSRF）+ 配额熔断 + 用户隔离。
type QaService struct {
	market *MarketService
	llm    *LLMService
}

func NewQaService(market *MarketService, llm *LLMService) *QaService {
	return &QaService{market: market, llm: llm}
}

const (
	qaMaxQuestionRunes = 500
	qaHistoryLimit     = 12  // 每次调用最多带入的历史消息条数（控上下文）
	qaMaxMessages      = 100 // 单会话消息上限
)

// QaAskRequest 提问入参。ConversationID=0 表示新建会话（需 Symbol/Market，
// 或给 AnalysisRecordID 复用该分析记录的数据快照——从分析结果一键「继续问答」）。
type QaAskRequest struct {
	ConversationID   int64  `json:"conversation_id"`
	Symbol           string `json:"symbol"`
	Market           string `json:"market"`
	LLMConfigID      int64  `json:"llm_config_id"`
	Question         string `json:"question"`
	AnalysisRecordID int64  `json:"analysis_record_id"` // >0：新会话复用该分析记录快照（须本人的个股分析）
}

// QaConversationView 会话 + 消息列表。
type QaConversationView struct {
	model.AiConversation
	Messages []model.AiConversationMessage `json:"messages"`
}

// Ask 提问：新建会话（首轮采集快照）或在已有会话上追问。返回会话全量视图。
func (s *QaService) Ask(ctx context.Context, userID int64, allowPrivate bool, req QaAskRequest) (*QaConversationView, error) {
	question := strings.TrimSpace(req.Question)
	if question == "" {
		return nil, errors.New("问题不能为空")
	}
	if len([]rune(question)) > qaMaxQuestionRunes {
		return nil, errors.New("问题过长（最多 500 字）")
	}

	// LLM 配置 + 配额熔断。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}
	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	// 取得或新建会话。
	var conv model.AiConversation
	isNewConv := req.ConversationID <= 0
	if !isNewConv {
		if err := common.DB.Where("id = ? AND user_id = ?", req.ConversationID, userID).First(&conv).Error; err != nil {
			return nil, errors.New("会话不存在")
		}
		if conv.MessageCount >= qaMaxMessages {
			return nil, errors.New("该会话消息已达上限，请新建会话")
		}
	} else if req.AnalysisRecordID > 0 {
		c, cerr := qaConversationFromAnalysis(userID, req.AnalysisRecordID, cfg, question)
		if cerr != nil {
			return nil, cerr
		}
		conv = *c
	} else {
		name, snap, err := buildStockSnapshot(ctx, s.market, req.Symbol, req.Market)
		if err != nil {
			return nil, err
		}
		snapJSON, _ := json.Marshal(fitBudget(snap))
		symbol, market, _ := normalizeSymbolMarket(req.Symbol, req.Market)
		conv = model.AiConversation{
			UserID: userID, Symbol: symbol, Market: market, Name: name,
			Title:       truncateRunes(question, 128),
			LLMConfigID: cfg.ID, Provider: cfg.Provider, Model: cfg.Model,
			DataSnapshot: string(snapJSON),
		}
		if err := common.DB.Create(&conv).Error; err != nil {
			return nil, err
		}
	}

	// 组装消息：系统提示（角色 + 数据快照）+ 历史 + 本轮提问。
	history, err := s.loadMessages(userID, conv.ID)
	if err != nil {
		return nil, err
	}
	messages := s.buildMessages(conv, history, question)

	res, callErr := chatCompletion(ctx, chatParams{
		BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model,
		Temperature: cfg.Temperature, MaxTokens: cfg.MaxTokens,
		Messages: messages, JSONMode: false, AllowPrivate: allowPrivate,
	})
	if callErr != nil {
		// 新会话首问失败：删除刚建的空会话，避免列表堆积 0 消息的孤儿会话。
		if isNewConv {
			common.DB.Delete(&model.AiConversation{}, conv.ID)
		}
		return nil, callErr
	}
	answer := strings.TrimSpace(res.Content)
	if answer == "" {
		answer = "（模型未返回内容，请重试或调整问题）"
	}

	// 事务落库：user 提问 + assistant 回答，并更新会话计数/token。
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		um := model.AiConversationMessage{ConversationID: conv.ID, UserID: userID, Role: model.QaRoleUser, Content: question}
		if err := tx.Create(&um).Error; err != nil {
			return err
		}
		am := model.AiConversationMessage{
			ConversationID: conv.ID, UserID: userID, Role: model.QaRoleAssistant, Content: answer,
			PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens, TotalTokens: res.Usage.TotalTokens,
		}
		if err := tx.Create(&am).Error; err != nil {
			return err
		}
		return tx.Model(&model.AiConversation{}).Where("id = ?", conv.ID).Updates(map[string]any{
			"message_count": gorm.Expr("message_count + 2"),
			"total_tokens":  gorm.Expr("total_tokens + ?", res.Usage.TotalTokens),
		}).Error
	})
	if err != nil {
		return nil, err
	}
	if res.Usage.TotalTokens > 0 {
		consumeQuota(userID, res.Usage.TotalTokens, true)
	}

	return s.Get(userID, conv.ID)
}

// qaConversationFromAnalysis 从分析记录新建问答会话：复用其数据快照（已 fitBudget），
// 不重复拉数——问答的上下文与分析所见完全一致，追问「刚才的结论」才有据可依。
// 校验归属（仅本人）且必须是带快照的个股分析。落库后返回。
func qaConversationFromAnalysis(userID, recordID int64, cfg *model.LLMConfig, question string) (*model.AiConversation, error) {
	var rec model.AnalysisRecord
	if err := common.DB.Select("id", "user_id", "module", "market", "symbol", "target", "data_snapshot").
		Where("id = ? AND user_id = ?", recordID, userID).First(&rec).Error; err != nil {
		return nil, errors.New("分析记录不存在")
	}
	if rec.Module != model.AnalysisModuleStock || rec.Symbol == "" || rec.DataSnapshot == "" {
		return nil, errors.New("仅带数据快照的个股分析可发起问答")
	}
	name := rec.Target
	if name == "" {
		name = rec.Symbol
	}
	conv := &model.AiConversation{
		UserID: userID, Symbol: rec.Symbol, Market: rec.Market, Name: name,
		Title:       truncateRunes(question, 128),
		LLMConfigID: cfg.ID, Provider: cfg.Provider, Model: cfg.Model,
		DataSnapshot: rec.DataSnapshot,
	}
	if err := common.DB.Create(conv).Error; err != nil {
		return nil, err
	}
	return conv, nil
}

// buildMessages 组装发送给 LLM 的消息序列。系统提示含个股数据快照，历史仅取最近若干条。
func (s *QaService) buildMessages(conv model.AiConversation, history []model.AiConversationMessage, question string) []chatMessage {
	var sys strings.Builder
	sys.WriteString(qaRoleIntro)
	sys.WriteString("\n\n【个股数据快照】（本次会话固定，供多轮问答复用；JSON，价格为货币单位、金额单位为元）：\n")
	sys.WriteString(conv.DataSnapshot)
	sys.WriteString("\n\n对象：" + conv.Name + "（" + conv.Symbol + "）。请只依据以上数据回答，缺失的数据如实说明。")

	msgs := []chatMessage{{Role: "system", Content: sys.String()}}
	// 只带最近 qaHistoryLimit 条历史，避免上下文膨胀。
	start := 0
	if len(history) > qaHistoryLimit {
		start = len(history) - qaHistoryLimit
	}
	for _, m := range history[start:] {
		msgs = append(msgs, chatMessage{Role: m.Role, Content: m.Content})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: question})
	return msgs
}

const qaRoleIntro = `你是一名严谨的证券研究助理，正在就某只个股与用户进行多轮问答。你的回答仅供研究参考，不构成投资建议。

要求：
1. 只依据【个股数据快照】中的事实回答，严禁编造未提供的财务、消息、评级或价格；数据不足时明确说明局限，不臆测。
2. 该快照仅含实时行情与技术指标（无财务报表/新闻/资金流），涉及这些维度时如实告知数据缺失。
3. 回答简明、聚焦用户问题，使用简体中文；必要时给出研究性看法，但不下达买卖指令。`

// loadMessages 取会话的全部消息（升序）。仅本人。
func (s *QaService) loadMessages(userID, convID int64) ([]model.AiConversationMessage, error) {
	var msgs []model.AiConversationMessage
	err := common.DB.Where("conversation_id = ? AND user_id = ?", convID, userID).Order("id ASC").Find(&msgs).Error
	return msgs, err
}

// List 列出用户的问答会话（不含快照/消息）。
func (s *QaService) List(userID int64, limit int) ([]model.AiConversation, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var rows []model.AiConversation
	err := common.DB.Where("user_id = ?", userID).
		Select("id", "user_id", "symbol", "market", "name", "title",
			"llm_config_id", "provider", "model", "message_count", "total_tokens", "created_at", "updated_at").
		Order("updated_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Get 取会话详情（含消息）。仅本人。
func (s *QaService) Get(userID, id int64) (*QaConversationView, error) {
	var conv model.AiConversation
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&conv).Error; err != nil {
		return nil, errors.New("会话不存在")
	}
	msgs, err := s.loadMessages(userID, id)
	if err != nil {
		return nil, err
	}
	conv.DataSnapshot = "" // 详情不必回传大快照
	return &QaConversationView{AiConversation: conv, Messages: msgs}, nil
}

// Delete 删除会话及其消息（仅本人，事务）。
func (s *QaService) Delete(userID, id int64) error {
	var conv model.AiConversation
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&conv).Error; err != nil {
		return errors.New("会话不存在")
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("conversation_id = ? AND user_id = ?", id, userID).Delete(&model.AiConversationMessage{}).Error; err != nil {
			return err
		}
		return tx.Delete(&conv).Error
	})
}
