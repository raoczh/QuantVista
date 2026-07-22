package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

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
	qaJobTimeout       = 10 * time.Minute
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
	// AllowStale 行情过期时的显式降级选择（与个股分析 allow_stale 同语义 fail-closed）：
	// 默认 false——新会话采集快照时全源无 fresh 行情则拒绝生成首答（前端弹确认）；
	// 显式置 true 才按「截至行情时刻的历史数据解释」口径创建会话（快照打 stale_mode）。
	AllowStale bool `json:"allow_stale"`
}

// QaTaskResult 是通用后台任务中保存的轻量定位结果。完整会话及历史消息仍以 QA 业务表
// 为唯一事实来源，避免每轮任务都重复保存逐渐增长的 QaConversationView。
type QaTaskResult struct {
	ConversationID int64 `json:"conversation_id"`
}

func normalizeQaAskRequest(req QaAskRequest) (QaAskRequest, error) {
	req.Question = strings.TrimSpace(req.Question)
	if req.Question == "" {
		return req, errors.New("问题不能为空")
	}
	if len([]rune(req.Question)) > qaMaxQuestionRunes {
		return req, errors.New("问题过长（最多 500 字）")
	}
	return req, nil
}

// QaConversationView 会话 + 消息列表。RiskFlags 为快照 risk_gate 的程序化风险标志
// （S1：详情不回传大快照，风险标志单独解析回传供 UI 展示）。
type QaConversationView struct {
	model.AiConversation
	Messages     []model.AiConversationMessage `json:"messages"`
	RiskFlags    []riskFlag                    `json:"risk_flags,omitempty"`
	SnapshotMeta *qaSnapshotMeta               `json:"snapshot_meta,omitempty"`
}

// qaSnapshotMeta 会话快照的行情新鲜度元数据，从固定快照 JSON 解析回传前端头部展示。
// 旧会话快照无这些顶层键时按 quote.data_time/source + 会话创建时间兜底。
type qaSnapshotMeta struct {
	CapturedAt      string `json:"captured_at,omitempty"`
	QuoteAsOf       string `json:"quote_as_of,omitempty"`
	BarsAsOf        string `json:"bars_as_of,omitempty"`
	QuoteSource     string `json:"quote_source,omitempty"`
	FreshnessStatus string `json:"freshness_status,omitempty"` // 快照创建时的判定（历史事实，不随时间变化）
	MarketState     string `json:"market_state,omitempty"`
	// 按「读取时刻」重判的当前时效（P0 第二轮：旧会话不会随时间重新判定过期——
	// 昨天创建时 fresh 的会话今天仍声明 fresh）。前端展示以 current_status 优先。
	CurrentStatus string `json:"current_status,omitempty"` // fresh | stale | unknown
	CurrentNote   string `json:"current_note,omitempty"`
}

// qaAskContext 一次提问的准备产物：prepare 与 finalize 之间的共享状态
// （流式与非流式两条路径共用同一套准备/收尾，保证快照、核验、落库口径完全一致）。
type qaAskContext struct {
	conv     model.AiConversation
	isNew    bool
	question string
	history  []model.AiConversationMessage
	messages []chatMessage
	cfg      *model.LLMConfig
	apiKey   string
	run      *llmRun // P0-2：本轮提问的调用组（会话级 trace + 每问一个 run）
	// promptVersion 本轮实际使用的 prompt 版本（P0-6 修复批：prepare 阶段从同一模板快照
	// 产出，buildMessages 正文/run/会话落库三处共用——finalize 刷新会话字段不再重新查库）。
	promptVersion string
}

// qaConvLocks 会话级进程内互斥：同一会话的并发追问必须串行，否则两问会各自
// loadMessages 到相同历史（上下文重复）、消息落库交错倒序、并各自通过 qaMaxMessages
// 检查后 +2 突破上限。个人自用单实例，进程内锁足够，无需 DB 锁。key=conversationID。
// 新会话（ConversationID<=0）不加锁：其 ID 未返回前无第二个请求能引用它，天然无竞争。
var qaConvLocks sync.Map // int64 -> *sync.Mutex

func qaConvLock(convID int64) *sync.Mutex {
	m, _ := qaConvLocks.LoadOrStore(convID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// AskAsync 创建通用 LLM 后台任务并立即返回。实际快照采集、模型调用、证据核验和
// 会话落库均在独立于 HTTP 请求的 Context 中执行，反向代理或浏览器断开不会取消任务。
// Ask 保留同步语义，供后台 runner、服务内调用和既有测试复用。
func (s *QaService) AskAsync(userID int64, allowPrivate bool, req QaAskRequest) (*LLMTaskView, error) {
	var err error
	req, err = normalizeQaAskRequest(req)
	if err != nil {
		return nil, err
	}
	return StartAsyncLLMTask(userID, "qa", req, qaJobTimeout, func(ctx context.Context) (any, error) {
		view, err := s.ask(ctx, userID, allowPrivate, req)
		if err != nil {
			return nil, err
		}
		return QaTaskResult{ConversationID: view.ID}, nil
	})
}

// Ask 提问（非流式）：新建会话（首轮采集快照）或在已有会话上追问。返回会话全量视图。
func (s *QaService) Ask(ctx context.Context, userID int64, allowPrivate bool, req QaAskRequest) (*QaConversationView, error) {
	var err error
	req, err = normalizeQaAskRequest(req)
	if err != nil {
		return nil, err
	}
	return s.ask(ctx, userID, allowPrivate, req)
}

func (s *QaService) ask(ctx context.Context, userID int64, allowPrivate bool, req QaAskRequest) (*QaConversationView, error) {
	if req.ConversationID > 0 {
		mu := qaConvLock(req.ConversationID)
		mu.Lock()
		defer mu.Unlock()
	}
	ac, err := s.prepareAsk(ctx, userID, req)
	if err != nil {
		return nil, err
	}
	// 新会话的 ID 直到 prepareAsk 落库后才确定；从这里起同样持有会话锁，避免
	// 后台调用期间被删除而留下孤儿消息。已有会话已在函数入口加锁，不能重复锁。
	if req.ConversationID <= 0 {
		mu := qaConvLock(ac.conv.ID)
		mu.Lock()
		defer mu.Unlock()
	}
	res, callErr := chatCompletion(ctx, chatParams{
		BaseURL: ac.cfg.BaseURL, APIKey: ac.apiKey, Model: ac.cfg.Model, EndpointType: ac.cfg.EndpointType,
		Temperature: ac.cfg.Temperature, MaxTokens: moduleTokenCap("qa", ac.cfg.MaxTokens),
		Messages: ac.messages, JSONMode: false, AllowPrivate: llmAllowPrivate(allowPrivate, ac.cfg),
		Meta: ac.run.chatMeta(userID, ac.cfg, 1),
	})
	ac.run.record(res, callErr)
	if callErr != nil {
		s.abortNewConv(ac)
		return nil, callErr
	}
	return s.finalizeAsk(userID, ac, res)
}

// AskStream 流式提问（S1）：delta 增量经 onDelta 回调吐给调用方（controller 逐行推 NDJSON），
// 流结束后走与非流式完全相同的收尾（证据核验落 CheckJSON → 落库 → 配额），核验徽章由
// 最终返回的会话视图后置更新。LLM 配置关闭了 stream 时退回非流式调用、整段作为单个增量吐出。
func (s *QaService) AskStream(ctx context.Context, userID int64, allowPrivate bool, req QaAskRequest, onDelta func(string)) (*QaConversationView, error) {
	var err error
	req, err = normalizeQaAskRequest(req)
	if err != nil {
		return nil, err
	}
	if req.ConversationID > 0 {
		mu := qaConvLock(req.ConversationID)
		mu.Lock()
		defer mu.Unlock()
	}
	ac, err := s.prepareAsk(ctx, userID, req)
	if err != nil {
		return nil, err
	}
	if req.ConversationID <= 0 {
		mu := qaConvLock(ac.conv.ID)
		mu.Lock()
		defer mu.Unlock()
	}
	params := chatParams{
		BaseURL: ac.cfg.BaseURL, APIKey: ac.apiKey, Model: ac.cfg.Model, EndpointType: ac.cfg.EndpointType,
		Temperature: ac.cfg.Temperature, MaxTokens: moduleTokenCap("qa", ac.cfg.MaxTokens),
		Messages: ac.messages, JSONMode: false, AllowPrivate: llmAllowPrivate(allowPrivate, ac.cfg),
		Meta: ac.run.chatMeta(userID, ac.cfg, 1),
	}
	var res *chatResult
	var callErr error
	if ac.cfg.Stream {
		res, callErr = chatCompletionStream(ctx, params, onDelta)
	} else {
		res, callErr = chatCompletion(ctx, params)
		if callErr == nil && onDelta != nil {
			onDelta(res.Content)
		}
	}
	ac.run.record(res, callErr)
	if callErr != nil {
		s.abortNewConv(ac)
		return nil, callErr
	}
	return s.finalizeAsk(userID, ac, res)
}

// prepareAsk 校验入参、解析 LLM 配置与配额、取得/新建会话、组装消息序列。
func (s *QaService) prepareAsk(ctx context.Context, userID int64, req QaAskRequest) (*qaAskContext, error) {
	question := req.Question

	// LLM 配置 + 配额熔断。
	cfg, apiKey, err := s.llm.ResolveForUse(userID, req.LLMConfigID)
	if err != nil {
		return nil, err
	}
	if err := checkQuota(userID); err != nil {
		return nil, err
	}

	// P0-6 修复批：qa 模板一次固化快照——会话 PromptVersion、buildMessages 的角色段与
	// run 的 prompt 版本全部消费同一份（此前三处各查一次库，模板中途编辑会造成
	// 「正文与版本不一致」或同一次提问内版本漂移）。
	qaPrompt := loadPromptRuntime(userID, model.PromptModuleQa)
	promptVersion := qaPrompt.Version(qaPromptVersion)

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
		c, cerr := qaConversationFromAnalysis(userID, req.AnalysisRecordID, cfg, question, promptVersion)
		if cerr != nil {
			return nil, cerr
		}
		conv = *c
	} else {
		name, snap, err := buildStockSnapshot(ctx, s.market, req.Symbol, req.Market)
		if err != nil {
			return nil, err
		}
		// 首答新鲜度门（P0 第二轮，与个股分析 allow_stale 同语义 fail-closed）：
		// 全源无 fresh 行情（stale/unknown）时默认拒绝创建会话——「按最新行情采集」
		// 的承诺不能建立在旧行情上；用户显式 allow_stale 才按历史解释口径创建，
		// 快照打 stale_mode 标（buildMessages 会注入硬约束段）。
		if st, _ := snap["freshness_status"].(string); st != "" && st != freshStatusFresh {
			asOf, _ := snap["quote_as_of"].(string)
			if !req.AllowStale {
				if st == freshStatusUnknown {
					return nil, refusalErr(RefusalStaleQuote, "该市场无交易日历，无法核验行情时效；可选择「按截至行情时刻的历史数据解释」模式继续提问")
				}
				return nil, refusalErrf(RefusalStaleQuote, "行情已过期（仅更新至 %s，可能停牌、休市异常或数据源故障），无法按最新行情回答；可选择「按截至该时刻的历史数据解释」模式继续提问", orStr(asOf, "未知时间"))
			}
			snap["stale_mode"] = "historical_explanation"
			snap["stale_mode_note"] = "用户已确认行情过期，本会话回答为「截至 " + orStr(asOf, "未知时间") + " 的历史数据解释」，非当前盘面判断"
		}
		snapJSON, _ := json.Marshal(fitBudget(snap))
		symbol, market, _ := normalizeSymbolMarket(req.Symbol, req.Market)
		conv = model.AiConversation{
			UserID: userID, Symbol: symbol, Market: market, Name: name,
			Title:       truncateRunes(question, 128),
			LLMConfigID: cfg.ID, Provider: cfg.Provider, Model: cfg.Model,
			PromptVersion: promptVersion,
			TraceID:       newLLMTraceID(), // P0-2：会话级 trace，本会话全部调用共享
			DataSnapshot:  string(snapJSON),
		}
		if err := common.DB.Create(&conv).Error; err != nil {
			return nil, err
		}
	}

	// P0-2：旧会话（trace 列上线前创建）首次新提问时补写 trace_id——此后该会话的
	// 调用都能按 trace 关联；补写失败不阻断提问（只是缺关联）。
	if conv.TraceID == "" {
		conv.TraceID = newLLMTraceID()
		common.DB.Model(&model.AiConversation{}).Where("id = ?", conv.ID).Update("trace_id", conv.TraceID)
	}

	// 组装消息：系统提示（角色 + 数据快照）+ 历史 + 本轮提问。
	history, err := s.loadMessages(userID, conv.ID)
	if err != nil {
		return nil, err
	}
	messages := s.buildMessagesFrom(qaPrompt, conv, history, question)
	run := newLLMRun(conv.TraceID, "", "qa", "qa.free_text.v1", promptVersion)
	run.hashData(conv.DataSnapshot)
	run.hashPrompt(messages)
	return &qaAskContext{
		conv: conv, isNew: isNewConv, question: question, history: history,
		messages: messages,
		cfg:      cfg, apiKey: apiKey, run: run,
		promptVersion: promptVersion,
	}, nil
}

// abortNewConv 调用失败时清理刚建的空会话，避免列表堆积 0 消息的孤儿会话。
func (s *QaService) abortNewConv(ac *qaAskContext) {
	if ac.isNew && ac.conv.ID > 0 {
		common.DB.Delete(&model.AiConversation{}, ac.conv.ID)
	}
}

// finalizeAsk 调用成功后的统一收尾：证据核验 → 事务落库两条消息 → 配额 → 返回会话视图。
func (s *QaService) finalizeAsk(userID int64, ac *qaAskContext, res *chatResult) (*QaConversationView, error) {
	answer := strings.TrimSpace(res.Content)
	if answer == "" {
		answer = "（模型未返回内容，请重试或调整问题）"
	}

	// 证据核验：把本轮回答里引用的数字与会话固定的数据快照程序化比对（排除 recent_bars 明细）。
	// conv.DataSnapshot 是 JSON 字符串，解析失败则跳过核验（不阻断回答落库）。
	// 用户提问（本轮 + 历史 user 消息）里的数字是模型上下文的合法来源——复述用户给出的
	// 假设价位/成本不是幻觉，并入值域（同推荐域 verifyEvidence extra 变参口径）。
	checkJSON := ""
	var snap map[string]any
	if json.Unmarshal([]byte(ac.conv.DataSnapshot), &snap) == nil {
		vals := snapshotLabeledValues(snap, stockFieldHints(snap), "recent_bars")
		// 快照 news 舆情段的新闻标题是文本型合法来源，标题里的小数并入值域（N2）；
		// announcements 公告段标题（F1）与 risk_gate 提示文本（S1）同理。
		vals = append(vals, textLabeledValues("新闻标题", "context", newsTitleTexts(snap))...)
		vals = append(vals, textLabeledValues("公告标题", "context", announcementTitleTexts(snap))...)
		vals = append(vals, textLabeledValues("风险提示", "context", riskGateTexts(snap))...)
		userTexts := []string{ac.question}
		for _, m := range ac.history {
			if m.Role == model.QaRoleUser {
				userTexts = append(userTexts, m.Content)
			}
		}
		// Origin=user：用户提问里的数字命中只是「复述用户输入」，ev3 分类汇总与前端
		// 均不得把它冒充「数据快照佐证」。
		vals = append(vals, textLabeledValues("用户提问", "user", userTexts)...)
		check := verifyEvidenceLabeled([]evidenceSection{{Module: "回答", Text: answer}}, vals)
		// ev4（P0-3）：快照结构化数据缺口透传 + 关键结论段（回答）快照佐证计数。
		check.Unknowns = snapshotUnknownItems(snap)
		markKeySection(check, "回答")
		if b, err := json.Marshal(check); err == nil {
			checkJSON = string(b)
		}
	}

	// 事务落库：user 提问 + assistant 回答，并更新会话计数/token。
	// 不变式：conv.DataSnapshot 永不回写——旧会话快照原地覆盖会让历史回答与核验结果
	// 失去可复现性；「按最新数据」一律以新会话（新快照新 ID）承载。
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		um := model.AiConversationMessage{ConversationID: ac.conv.ID, UserID: userID, Role: model.QaRoleUser, Content: ac.question}
		if err := tx.Create(&um).Error; err != nil {
			return err
		}
		am := model.AiConversationMessage{
			ConversationID: ac.conv.ID, UserID: userID, Role: model.QaRoleAssistant, Content: answer, CheckJSON: checkJSON,
			RunID:        ac.run.RunID, // P0-2：本轮回答 ↔ llm_call_logs.run_id
			PromptTokens: res.Usage.PromptTokens, CompletionTokens: res.Usage.CompletionTokens, TotalTokens: res.Usage.TotalTokens,
		}
		if err := tx.Create(&am).Error; err != nil {
			return err
		}
		return tx.Model(&model.AiConversation{}).Where("id = ?", ac.conv.ID).Updates(map[string]any{
			"message_count": gorm.Expr("message_count + 2"),
			"total_tokens":  gorm.Expr("total_tokens + ?", res.Usage.TotalTokens),
			// P0-6 修复批：刷新用 prepare 阶段固化的版本（与本轮 buildMessages 正文同快照），
			// 不再重新查库——落库版本必然对应本轮实际使用的模板内容。
			"prompt_version": ac.promptVersion,
		}).Error
	})
	if err != nil {
		return nil, err
	}
	if res.Usage.TotalTokens > 0 {
		consumeQuota(userID, res.Usage.TotalTokens, true)
	}

	return s.Get(userID, ac.conv.ID)
}

// qaConversationFromAnalysis 从分析记录新建问答会话：复用其数据快照（已 fitBudget），
// 不重复拉数——问答的上下文与分析所见完全一致，追问「刚才的结论」才有据可依。
// 校验归属（仅本人）且必须是带快照的个股分析。落库后返回。
// promptVersion 由调用方（prepareAsk）从本轮模板快照产出，保证与实际消息正文同源。
func qaConversationFromAnalysis(userID, recordID int64, cfg *model.LLMConfig, question, promptVersion string) (*model.AiConversation, error) {
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
		PromptVersion: promptVersion,
		TraceID:       newLLMTraceID(), // P0-2：独立会话独立 trace（与源分析记录经快照复用间接关联）
		DataSnapshot:  rec.DataSnapshot,
	}
	if err := common.DB.Create(conv).Error; err != nil {
		return nil, err
	}
	return conv, nil
}

// qaPromptVersionFor 本次问答实际使用的 prompt 版本（启用 qa 自定义模板加 -custom 后缀归因）。
// 会话创建时固化一份；此后每次 ask 随消息落库前刷新会话字段——模板中途启停时记录跟随实际用量。
func qaPromptVersionFor(userID int64) string {
	return promptVersionFor(userID, model.PromptModuleQa, qaPromptVersion)
}

// qaCurrentFreshness 按「当前时刻」重判会话固定快照的行情时效（P0 第二轮：会话快照
// 固化不回写，但时效声明必须随时间演进——昨天 fresh 的快照今天不能继续声明 fresh）。
// 返回 状态（fresh/stale/unknown；空=快照无行情时间或无法判定）与人话说明。
func (s *QaService) qaCurrentFreshness(market string, meta *qaSnapshotMeta) (string, string) {
	if s.market == nil || meta == nil || meta.QuoteAsOf == "" {
		return "", ""
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", meta.QuoteAsOf, time.Local)
	if err != nil {
		return "", ""
	}
	fi := s.market.QuoteFreshnessOf(orStr(market, "cn"), t)
	switch fi.Status {
	case freshStatusFresh:
		return freshStatusFresh, ""
	case freshStatusUnknown:
		return freshStatusUnknown, "该市场无交易日历，无法核验快照行情时效"
	default:
		note := fmt.Sprintf("快照行情截至 %s，按当前时刻核验已过期", meta.QuoteAsOf)
		if fi.ExpectedDate != "" {
			note += fmt.Sprintf("（期望交易日 %s）", fi.ExpectedDate)
		}
		return freshStatusStale, note
	}
}

// buildMessages 组装发送给 LLM 的消息序列（独立查询一次模板；业务链路 prepareAsk 用
// buildMessagesFrom 消费已固化的快照）。保留供单测直接调用。
func (s *QaService) buildMessages(conv model.AiConversation, history []model.AiConversationMessage, question string) []chatMessage {
	return s.buildMessagesFrom(loadPromptRuntime(conv.UserID, model.PromptModuleQa), conv, history, question)
}

// buildMessagesFrom 由模板快照组装消息序列。系统提示含个股数据快照，历史仅取最近若干条。
// P0-6：module=qa 的自定义模板是 L3 任务段（替换默认角色行，占位符宽容渲染），要求
// 契约段恒由系统追加不可覆盖；快照注入与时效重判段不变。
func (s *QaService) buildMessagesFrom(qaPrompt promptRuntime, conv model.AiConversation, history []model.AiConversationMessage, question string) []chatMessage {
	var sys strings.Builder
	intro := qaRoleIntro
	if custom, ok := qaPrompt.Render(map[string]string{
		"symbol": conv.Symbol, "name": conv.Name, "market": conv.Market,
	}); ok {
		intro = composeCustomTaskPrompt(custom, qaPromptContract)
	}
	sys.WriteString(intro)
	sys.WriteString("\n\n【个股数据快照】（本次会话固定，供多轮问答复用；JSON，价格为货币单位、金额单位为元）：\n")
	sys.WriteString(conv.DataSnapshot)
	// 快照时效按「本轮提问时刻」重判注入（q10）：快照内的 freshness_status 是创建时的
	// 历史事实，续问跨午休/收盘/隔天后必须以当前重判为准声明——否则昨天 fresh 的会话
	// 今天仍向模型声明 fresh。
	if st, note := s.qaCurrentFreshness(conv.Market, parseSnapshotMeta(conv.DataSnapshot, conv.CreatedAt)); st != "" && st != freshStatusFresh {
		sys.WriteString("\n\n【行情时效（按本轮提问时刻重新核验，优先级高于快照内 freshness_status）】" + note +
			"。本轮回答涉及价格/涨跌/盘面必须先声明「行情截至 " + orStr(quoteAsOfOf(conv), "快照采集时刻") +
			"」，一律按历史数据解释口径表述，严禁以「当前/现在/实时」口径描述该快照行情，严禁给出基于当前盘面的买入/卖出/加减仓行动参考。")
	}
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

// quoteAsOfOf 会话快照的行情数据时刻（buildMessages 时效段展示用）。
func quoteAsOfOf(conv model.AiConversation) string {
	if m := parseSnapshotMeta(conv.DataSnapshot, conv.CreatedAt); m != nil {
		return m.QuoteAsOf
	}
	return ""
}

// qaPromptVersion 问答系统提示版本（会话不落库版本列，仅供代码内追溯）。
// q10: 首答新鲜度门（全源无 fresh 默认拒绝、allow_stale 才生成且快照打 stale_mode）+ 快照时效按每轮提问时刻重判注入（旧会话跨天不再向模型声明 fresh）；q9: 快照新鲜度元数据（captured_at/quote_as_of/bars_as_of/quote_source/freshness_status/market_state），stale 时必须声明行情截至时间、非交易时段按收盘口径表述；q8: P3a org_view 机构观点段进快照说明（卖方乐观偏差纪律）；q7: F2 finance 财务段（F10 最新期+趋势）进快照说明；q6: risk_gate 风险闸门段、允许轻量 Markdown（流式渲染配套）；q5: announcements 公告段；q4: news 舆情段；q3: 回答引用的数字会被程序化核验，威慑幻觉；q2: 快照含五维量化评分锚点、要求引用数值、禁用先验记忆。
const qaPromptVersion = "q10"

// qaRoleTaskSeg 问答角色任务段（L3）：module=qa 自定义模板替换的部分——角色定位与
// 回答风格。P0-6 起自定义不再整段替换，要求契约段恒由系统追加。
const qaRoleTaskSeg = `你是一名严谨的证券研究助理，正在就某只个股与用户进行多轮问答。你的回答仅供研究参考，不构成投资建议。`

// qaPromptContract 问答模块契约段（L1，不可被自定义模板覆盖）：数据边界/时效/风险闸门/
// 数值核验/输出格式纪律。
const qaPromptContract = `要求：
1. 只依据【个股数据快照】中的事实回答，严禁编造未提供的财务、消息、评级或价格；数据不足时明确说明局限，不臆测。禁止使用你记忆中关于该公司的信息（名气/行业地位/历史印象都不算数据）。
2. 该快照仅含采集时点的行情（quote_as_of 为行情数据时间、quote_source 为来源、freshness_status 为新鲜度状态、market_state 为市场时段、captured_at 为采集时刻）、技术指标、五维量化评分 quant_score（纯技术面 0-100 参考锚点）、可能存在的 news 舆情段（最近相关新闻标题+情绪标签，覆盖面有限）、可能存在的 finance 财务段（F10 最新期主要指标与近几期趋势，季报口径有滞后；值为 0 可能表示上游缺失，不得据此下「归零」结论）与可能存在的 org_view 机构观点段（卖方研报评级分布/评级变动/目标价统计/调研密度；解读纪律：卖方评级普遍乐观，不得以「多少家买入」论证看多，有信息量的是评级下调、目标价与现价的偏离、调研密度变化），无财务全表明细与资金流，涉及缺失维度时如实告知。引用新闻只能复述给出的标题，不得臆测正文细节；news 标注「暂无直接相关新闻」时按 market_signals 判断并明示消息面缺失。
3. 行情新鲜度（必须遵守）：freshness_status=stale 或快照带 freshness_note 时，回答涉及价格/涨跌必须先声明「行情仅更新至 quote_as_of」，严禁以「实时/当前盘面」口径表述；market_state 非 trading（休市/午间/盘前）时按最近交易日收盘（或阶段）口径措辞，不得暗示实时成交。
4. 风险闸门（快照 risk_gate 块，必须遵守）：level=block（ST/退市警示）为硬约束——不得给出任何买入倾向的回答，先讲退市/警示风险；level=warn（一字板/流动性不足）须在回答中主动提示并约束结论（一字板不得按可正常成交分析）。risk_gate.note 声明的未接入维度（质押/解禁等），涉及时照实说「未接入数据，请自行核查」，严禁装作已核查。
5. 关键判断引用快照中的具体字段与数值（如「现价 12.34 高于 MA20=11.98」），让用户可以核对。系统会程序化核对你回答中引用的数字，与快照不符的会被标记展示给用户，因此不要编造或凭印象填写数字。
6. 回答简明、聚焦用户问题，使用简体中文；必要时给出研究性看法，但不下达买卖指令。可用轻量 Markdown 排版（短列表、**加粗**关键结论），不要输出表格、图片与链接。`

// qaRoleIntro 默认角色段 = 任务段 + 契约段（编译期拼接，与 P0-6 拆分前逐字节一致，
// 默认路径零行为变化）。
const qaRoleIntro = qaRoleTaskSeg + "\n\n" + qaPromptContract

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
			"llm_config_id", "provider", "model", "prompt_version", "trace_id", "message_count", "total_tokens", "created_at", "updated_at").
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
	flags := parseRiskFlagsFromSnapshot(conv.DataSnapshot)
	meta := parseSnapshotMeta(conv.DataSnapshot, conv.CreatedAt)
	// 详情读取时按当前时刻重判快照时效（P0 第二轮）：快照内 freshness_status 是创建时
	// 的历史事实，昨天 fresh 的会话今天必须能显示「行情非最新」。
	if meta != nil {
		if st, note := s.qaCurrentFreshness(conv.Market, meta); st != "" {
			meta.CurrentStatus = st
			meta.CurrentNote = note
		}
	}
	conv.DataSnapshot = "" // 详情不必回传大快照
	return &QaConversationView{AiConversation: conv, Messages: msgs, RiskFlags: flags, SnapshotMeta: meta}, nil
}

// parseSnapshotMeta 从会话固定快照 JSON 解析行情新鲜度元数据。新快照直接读顶层键；
// 旧会话快照（无这些键）按 quote.data_time/source 与会话创建时间兜底。全空返回 nil。
func parseSnapshotMeta(snapshotJSON string, createdAt time.Time) *qaSnapshotMeta {
	if snapshotJSON == "" {
		return nil
	}
	var snap map[string]any
	if json.Unmarshal([]byte(snapshotJSON), &snap) != nil {
		return nil
	}
	str := func(v any) string { s, _ := v.(string); return s }
	m := &qaSnapshotMeta{
		CapturedAt:      str(snap["captured_at"]),
		QuoteAsOf:       str(snap["quote_as_of"]),
		BarsAsOf:        str(snap["bars_as_of"]),
		QuoteSource:     str(snap["quote_source"]),
		FreshnessStatus: str(snap["freshness_status"]),
		MarketState:     str(snap["market_state"]),
	}
	// 兜底：captured_at ← data_as_of（分析派生快照有）← 会话创建时间。
	if m.CapturedAt == "" {
		if m.CapturedAt = str(snap["data_as_of"]); m.CapturedAt == "" && !createdAt.IsZero() {
			m.CapturedAt = createdAt.In(time.Local).Format("2006-01-02 15:04:05")
		}
	}
	if quote, ok := snap["quote"].(map[string]any); ok {
		if m.QuoteAsOf == "" {
			// quote.data_time 序列化为 RFC3339，重排为统一格式。
			if raw := str(quote["data_time"]); raw != "" {
				if t, err := time.Parse(time.RFC3339, raw); err == nil && !t.IsZero() {
					m.QuoteAsOf = t.In(time.Local).Format("2006-01-02 15:04:05")
				}
			}
		}
		if m.QuoteSource == "" {
			m.QuoteSource = str(quote["source"])
		}
	}
	if m.BarsAsOf == "" {
		if bars, ok := snap["recent_bars"].([]any); ok && len(bars) > 0 {
			if last, ok := bars[len(bars)-1].(map[string]any); ok {
				m.BarsAsOf = str(last["d"])
			}
		}
	}
	if m.CapturedAt == "" && m.QuoteAsOf == "" && m.QuoteSource == "" &&
		m.BarsAsOf == "" && m.FreshnessStatus == "" && m.MarketState == "" {
		return nil
	}
	return m
}

// Snapshot 返回会话固定的数据快照原文（供前端「数据快照」透明面板展示）。仅本人。
// 详情接口刻意清空快照以省流，需要时按需单取。
func (s *QaService) Snapshot(userID, id int64) (string, error) {
	var conv model.AiConversation
	if err := common.DB.Select("id", "user_id", "data_snapshot").
		Where("id = ? AND user_id = ?", id, userID).First(&conv).Error; err != nil {
		return "", errors.New("会话不存在")
	}
	return conv.DataSnapshot, nil
}

// Delete 删除会话及其消息（仅本人，事务）。
func (s *QaService) Delete(userID, id int64) error {
	if err := expireStaleLLMTasks(userID); err != nil {
		return err
	}
	var processing int64
	if err := common.DB.Model(&model.LLMTask{}).
		Where("user_id = ? AND kind = ? AND status = ?", userID, "qa", model.LLMTaskStatusProcessing).
		Count(&processing).Error; err != nil {
		return err
	}
	if processing > 0 {
		return errors.New("问答正在后台执行，请等任务结束后再删除会话")
	}

	mu := qaConvLock(id)
	if !mu.TryLock() {
		return errors.New("问答正在后台执行，请等任务结束后再删除会话")
	}
	defer mu.Unlock()

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
