package service

import (
	"context"
	"errors"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// NoteService 投资笔记/决策日志：自由 CRUD，可选绑定标的形成个股时间线。
type NoteService struct {
	market *MarketService
}

func NewNoteService(market *MarketService) *NoteService {
	return &NoteService{market: market}
}

var validNoteKind = map[string]bool{
	"": true, // 不分类
	model.NoteKindDecision: true,
	model.NoteKindReview:   true,
	model.NoteKindIdea:     true,
	model.NoteKindEvent:    true,
}

// NoteInput 新建/编辑入参。
type NoteInput struct {
	Symbol  string `json:"symbol"` // 可空 = 通用笔记
	Market  string `json:"market"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Content string `json:"content"`
}

func (s *NoteService) normalize(ctx context.Context, in *NoteInput) (symbol, market, name string, err error) {
	if !validNoteKind[in.Kind] {
		return "", "", "", errors.New("无效的笔记类别")
	}
	if strings.TrimSpace(in.Content) == "" && strings.TrimSpace(in.Title) == "" {
		return "", "", "", errors.New("标题与内容不能同时为空")
	}
	if strings.TrimSpace(in.Symbol) == "" {
		return "", "", "", nil // 通用笔记不绑定标的
	}
	symbol, market, err = normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return "", "", "", err
	}
	// 取名 best-effort：数据源临时不可用不阻断记笔记。
	if q, qerr := s.market.GetQuote(ctx, market, symbol); qerr == nil {
		name = q.Name
	}
	return symbol, market, name, nil
}

// Create 新建笔记。
func (s *NoteService) Create(ctx context.Context, userID int64, in NoteInput) (*model.ResearchNote, error) {
	symbol, market, name, err := s.normalize(ctx, &in)
	if err != nil {
		return nil, err
	}
	note := model.ResearchNote{
		UserID: userID, Symbol: symbol, Market: market, Name: name,
		Kind: in.Kind, Title: strings.TrimSpace(in.Title), Content: strings.TrimSpace(in.Content),
	}
	if err := common.DB.Create(&note).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

// Update 编辑笔记（标的绑定也可改）。
func (s *NoteService) Update(ctx context.Context, userID, id int64, in NoteInput) (*model.ResearchNote, error) {
	var note model.ResearchNote
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&note).Error; err != nil {
		return nil, errors.New("笔记不存在")
	}
	symbol, market, name, err := s.normalize(ctx, &in)
	if err != nil {
		return nil, err
	}
	note.Symbol, note.Market = symbol, market
	if name != "" {
		note.Name = name
	} else if symbol == "" {
		note.Name = ""
	}
	note.Kind = in.Kind
	note.Title = strings.TrimSpace(in.Title)
	note.Content = strings.TrimSpace(in.Content)
	if err := common.DB.Save(&note).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

// List 笔记列表：可按标的过滤、按关键字（标题/内容）搜索，时间倒序。
func (s *NoteService) List(userID int64, symbol, market, keyword string, limit int) ([]model.ResearchNote, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := common.DB.Where("user_id = ?", userID)
	if symbol != "" {
		q = q.Where("symbol = ?", symbol)
		if market != "" {
			q = q.Where("market = ?", market)
		}
	}
	if kw := strings.TrimSpace(keyword); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("title LIKE ? OR content LIKE ?", like, like)
	}
	var notes []model.ResearchNote
	if err := q.Order("created_at DESC").Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// Delete 删除笔记。
func (s *NoteService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.ResearchNote{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("笔记不存在")
	}
	return nil
}
