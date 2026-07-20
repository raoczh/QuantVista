package service

import (
	"context"
	"errors"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// ThesisService 投资逻辑卡片：结构化研究假设的增删改查、到期复盘提示与一键体检。
// 卡片按 user+symbol+market 唯一——自选、持仓、推荐指向同一张卡，避免多处假设漂移。
type ThesisService struct {
	market *MarketService
}

func NewThesisService(market *MarketService) *ThesisService {
	return &ThesisService{market: market}
}

var validThesisStatus = map[string]bool{
	model.ThesisStatusActive:      true,
	model.ThesisStatusInvalidated: true,
	model.ThesisStatusArchived:    true,
}

// ThesisUpsertRequest 新建/更新入参（按 symbol+market 定位，存在即更新）。
type ThesisUpsertRequest struct {
	Symbol         string `json:"symbol"`
	Market         string `json:"market"`
	Thesis         string `json:"thesis"`
	KeyEvidence    string `json:"key_evidence"`
	Risks          string `json:"risks"`
	KillSwitches   string `json:"kill_switches"`
	TrackMetrics   string `json:"track_metrics"`
	NextReviewDate string `json:"next_review_date"`
}

// Upsert 创建或更新逻辑卡。代码经行情校验（含取名），失效/归档卡更新后自动回到 active。
func (s *ThesisService) Upsert(ctx context.Context, userID int64, req ThesisUpsertRequest) (*model.ThesisCard, error) {
	symbol, market, err := normalizeSymbolMarket(req.Symbol, req.Market)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.Thesis) == "" {
		return nil, errors.New("核心逻辑(thesis)不能为空")
	}
	if req.NextReviewDate != "" {
		if _, err := time.ParseInLocation("2006-01-02", req.NextReviewDate, time.Local); err != nil {
			return nil, errors.New("下次复盘日期格式应为 YYYY-MM-DD")
		}
	}

	// 行情校验 + 取名（数据源临时不可用不阻断，name 回退空由前端显示 symbol）。
	name := ""
	if q, qerr := s.market.GetQuote(ctx, market, symbol); qerr == nil {
		name = q.Name
	}

	var card model.ThesisCard
	err = common.DB.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).First(&card).Error
	if err != nil {
		card = model.ThesisCard{UserID: userID, Symbol: symbol, Market: market}
	}
	card.Thesis = strings.TrimSpace(req.Thesis)
	card.KeyEvidence = strings.TrimSpace(req.KeyEvidence)
	card.Risks = strings.TrimSpace(req.Risks)
	card.KillSwitches = strings.TrimSpace(req.KillSwitches)
	card.TrackMetrics = strings.TrimSpace(req.TrackMetrics)
	card.NextReviewDate = req.NextReviewDate
	card.Status = model.ThesisStatusActive
	card.InvalidReason = ""
	if name != "" {
		card.Name = name
	}
	if err := common.DB.Save(&card).Error; err != nil {
		return nil, err
	}
	return &card, nil
}

// List 用户的逻辑卡列表（status 为空返回全部）。
func (s *ThesisService) List(userID int64, status string) ([]model.ThesisCard, error) {
	q := common.DB.Where("user_id = ?", userID)
	if status != "" {
		if !validThesisStatus[status] {
			return nil, errors.New("无效的状态筛选")
		}
		q = q.Where("status = ?", status)
	}
	var cards []model.ThesisCard
	if err := q.Order("updated_at DESC").Find(&cards).Error; err != nil {
		return nil, err
	}
	return cards, nil
}

// GetBySymbol 按标的取卡（自选/持仓行内入口用；没有卡返回 nil 而非错误）。
func (s *ThesisService) GetBySymbol(userID int64, symbol, market string) (*model.ThesisCard, error) {
	var card model.ThesisCard
	err := common.DB.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).First(&card).Error
	if err != nil {
		return nil, nil
	}
	return &card, nil
}

// SetStatus 更新卡状态：invalidated 需给原因；恢复 active 清空原因。
func (s *ThesisService) SetStatus(userID, id int64, status, reason string) (*model.ThesisCard, error) {
	if !validThesisStatus[status] {
		return nil, errors.New("无效的状态")
	}
	var card model.ThesisCard
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&card).Error; err != nil {
		return nil, errors.New("逻辑卡不存在")
	}
	// 作废（invalidated）必须带失效原因：逻辑卡的价值在于「为何不再成立」的留痕，空原因等于丢失复盘依据。
	if status == model.ThesisStatusInvalidated && strings.TrimSpace(reason) == "" {
		return nil, errors.New("作废逻辑卡需填写失效原因")
	}
	card.Status = status
	if status == model.ThesisStatusInvalidated {
		card.InvalidReason = strings.TrimSpace(reason)
	} else {
		card.InvalidReason = ""
	}
	if err := common.DB.Save(&card).Error; err != nil {
		return nil, err
	}
	return &card, nil
}

// Delete 删除逻辑卡。
func (s *ThesisService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.ThesisCard{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("逻辑卡不存在")
	}
	return nil
}

// DueForUser 到期待复盘的 active 卡（NextReviewDate 非空且 <= 今天），供待办聚合。
func (s *ThesisService) DueForUser(userID int64) ([]model.ThesisCard, error) {
	today := time.Now().In(time.Local).Format("2006-01-02")
	var cards []model.ThesisCard
	err := common.DB.Where(
		"user_id = ? AND status = ? AND next_review_date <> '' AND next_review_date <= ?",
		userID, model.ThesisStatusActive, today,
	).Order("next_review_date ASC").Find(&cards).Error
	return cards, err
}

// ThesisCheckItem 体检结果单项：卡片 + 行情富化 + 提示信号。
// 失效条件是自由文本，系统不解析语义——体检给出现价/涨跌/回撤事实 + 到期标记，
// 由用户对照 kill_switches 自查（轻量体检，深入判断走 AI 分析）。
// 行情时效契约：QuoteOK 仅 fresh；非 fresh 的现价/当日涨跌不出（最近已知价放
// LastPrice 并带截至时刻），近 20 日回撤基于历史日线照算（收盘序列与实时价无关）。
type ThesisCheckItem struct {
	Card         model.ThesisCard `json:"card"`
	QuoteOK      bool             `json:"quote_ok"`
	Price        float64          `json:"price"`
	ChangePct    float64          `json:"change_pct"`
	ChangePct20d float64          `json:"change_pct_20d"` // 近 20 日涨跌幅（体检时计算）
	ReviewDue    bool             `json:"review_due"`     // 复盘日期已到
	Signals      []string         `json:"signals"`        // 需要注意的信号（如大幅回撤）

	QuoteAsOf       string  `json:"quote_as_of,omitempty"`      // 行情数据源时刻
	FreshnessStatus string  `json:"freshness_status,omitempty"` // fresh | stale | unknown
	LastPrice       float64 `json:"last_price,omitempty"`       // 最近已知价（非 fresh 展示用）
}

// thesisDrawdownWarn 近 20 日跌幅超过该值（%）时提示重检逻辑。
const thesisDrawdownWarn = -15.0

// CheckUp 一键体检：对全部 active 卡批量富化行情与近 20 日表现，标记到期与异常信号。
func (s *ThesisService) CheckUp(ctx context.Context, userID int64) ([]ThesisCheckItem, error) {
	cards, err := s.List(userID, model.ThesisStatusActive)
	if err != nil {
		return nil, err
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	refs := make([]QuoteRef, 0, len(cards))
	for _, c := range cards {
		refs = append(refs, QuoteRef{Market: c.Market, Symbol: c.Symbol})
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)

	items := make([]ThesisCheckItem, 0, len(cards))
	for _, c := range cards {
		item := ThesisCheckItem{Card: c, Signals: []string{}}
		if c.NextReviewDate != "" && c.NextReviewDate <= today {
			item.ReviewDue = true
			item.Signals = append(item.Signals, "复盘日期已到，请检查逻辑是否仍成立")
		}
		// 近 20 日表现：历史收盘序列与实时价无关，行情过期也照算（体检为手动低频操作）。
		checkBars := func() {
			if bars, berr := s.market.GetDailyBars(ctx, c.Market, c.Symbol, 25); berr == nil && len(bars) > 0 {
				closes := make([]float64, len(bars))
				for i, b := range bars {
					closes[i] = b.Close
				}
				item.ChangePct20d = changeOverN(closes, 20)
				if item.ChangePct20d <= thesisDrawdownWarn {
					item.Signals = append(item.Signals, "近 20 日回撤较大，请对照失效条件自查")
				}
			}
		}
		if fq, ok := quotes[QuoteKey(c.Market, c.Symbol)]; ok && fq.Quote != nil && fq.Quote.Price > 0 {
			item.FreshnessStatus = fq.Fresh.Status
			if !fq.Quote.DataTime.IsZero() {
				item.QuoteAsOf = fq.Quote.DataTime.In(time.Local).Format("2006-01-02 15:04")
			}
			if fq.Fresh.Status == freshStatusFresh {
				item.QuoteOK = true
				item.Price = round2(fq.Quote.Price)
				item.ChangePct = round2(fq.Quote.ChangePct)
			} else {
				// fail-closed：过期行情不冒充现价（旧价对照失效条件会得出假「未触发」）。
				item.LastPrice = round2(fq.Quote.Price)
				item.Signals = append(item.Signals, "行情已过期（截至 "+orStr(item.QuoteAsOf, "未知时间")+"），现价与当日涨跌未知，请勿按最近已知价对照失效条件")
			}
			checkBars()
		} else {
			item.FreshnessStatus = freshStatusStale
			item.Signals = append(item.Signals, "行情暂不可用（可能停牌），请留意")
			checkBars()
		}
		items = append(items, item)
	}
	return items, nil
}
