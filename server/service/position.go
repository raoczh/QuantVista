package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// PositionService 已购入持仓。按 userID 隔离；盈亏用实时行情计算，不落库快照。
type PositionService struct {
	market *MarketService
}

func NewPositionService(market *MarketService) *PositionService {
	return &PositionService{market: market}
}

var validPositionType = map[string]bool{
	model.PositionTypeShortTerm: true,
	model.PositionTypeLongTerm:  true,
}

// PositionView 持仓 + 实时盈亏（成本含买入费税；已平仓用卖出价，持仓中用现价）。
type PositionView struct {
	model.Position
	CurrentPrice float64 `json:"current_price"` // 持仓中的现价（已平仓为卖出价）
	QuoteOK      bool    `json:"quote_ok"`      // 现价是否取到
	Cost         float64 `json:"cost"`          // 买入总成本 = 买入价*数量 + 买入费 + 买入税
	MarketValue  float64 `json:"market_value"`  // 现值
	ProfitAmount float64 `json:"profit_amount"` // 盈亏额（已扣相关费税）
	ProfitPct    float64 `json:"profit_pct"`    // 收益率 %
	Realized     bool    `json:"realized"`      // 是否已实现（已平仓）

	HeldTradeDays   int  `json:"held_trade_days"`   // 已持有交易日（按交易日历；持仓中且有买入日期时计算）
	ShortTermReview bool `json:"short_term_review"` // 短线持仓持有超阈值，建议复盘

	NearStopLoss   bool       `json:"near_stop_loss"`   // 现价距计划止损 ≤3%（未破）
	BelowStopLoss  bool       `json:"below_stop_loss"`  // 现价已跌破计划止损
	LastAnalyzedAt *time.Time `json:"last_analyzed_at"` // 该标的最近一次个股 AI 分析时间
	AnalysisStale  bool       `json:"analysis_stale"`   // 持仓中从未分析或距上次分析超过 7 天
}

// shortHoldReviewDays 短线持仓超过该交易日数则提示复盘（短线一般不宜久拖）。
const shortHoldReviewDays = 10

// nearStopLossPct 现价高于止损但差距在该比例内视为「接近止损」。
const nearStopLossPct = 3.0

// analysisStaleDays 持仓超过该天数未做个股分析则提示（AgentFloor 的「过期未分析」思路）。
const analysisStaleDays = 7

// computeView 计算单条持仓的成本/现值/盈亏。price 为持仓中的实时现价，hasQuote 表示是否取到。
func computeView(p model.Position, price float64, hasQuote bool) PositionView {
	v := PositionView{Position: p}
	v.Cost = p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax

	if p.Status == model.PositionStatusClosed {
		// 已平仓：盈亏已实现，扣买卖全部费税。
		proceeds := p.SellPrice * p.Quantity
		v.CurrentPrice = p.SellPrice
		v.MarketValue = proceeds
		v.ProfitAmount = proceeds - p.SellFee - p.SellTax - v.Cost
		v.Realized = true
		v.QuoteOK = true
	} else if hasQuote {
		// 持仓中：用现价估未实现盈亏（仅扣已发生的买入费税）。
		v.CurrentPrice = price
		v.QuoteOK = true
		v.MarketValue = price * p.Quantity
		v.ProfitAmount = v.MarketValue - v.Cost
	}
	if v.Cost > 0 && v.QuoteOK {
		v.ProfitPct = v.ProfitAmount / v.Cost * 100
	}
	return v
}

// List 列出持仓（status: holding/closed/all），富化实时盈亏。
func (s *PositionService) List(ctx context.Context, userID int64, status string) ([]PositionView, error) {
	q := common.DB.Where("user_id = ?", userID)
	switch status {
	case model.PositionStatusHolding, model.PositionStatusClosed:
		q = q.Where("status = ?", status)
	case "", "all":
		// 全部
	default:
		return nil, errors.New("非法的状态筛选")
	}
	var positions []model.Position
	if err := q.Order("status, id DESC").Find(&positions).Error; err != nil {
		return nil, err
	}

	// 仅持仓中的需要现价。
	refs := make([]QuoteRef, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		if p.Status != model.PositionStatusHolding {
			continue
		}
		k := QuoteKey(p.Market, p.Symbol)
		if !seen[k] {
			seen[k] = true
			refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
		}
	}
	quotes := s.market.QuotesFor(ctx, refs)

	// 最近一次个股 AI 分析时间（一次分组查询，供「持仓过久未分析」提示）。
	lastAnalyzed := lastStockAnalysisFor(userID, positions)

	out := make([]PositionView, 0, len(positions))
	now := time.Now()
	for _, p := range positions {
		price, ok := 0.0, false
		if q := quotes[QuoteKey(p.Market, p.Symbol)]; q != nil {
			price, ok = q.Price, true
		}
		v := computeView(p, price, ok)
		// 短线状态提示：持仓中且有买入日期时，按交易日历算已持有交易日，
		// 短线持有超阈值给出复盘提示（阶段6：持仓页短线状态提示）。
		if p.Status == model.PositionStatusHolding && p.BuyDate != "" {
			if days, hasCal := countOpenTradeDaysAfter(p.Market, p.BuyDate); hasCal {
				v.HeldTradeDays = days
				if p.PositionType == model.PositionTypeShortTerm && days > shortHoldReviewDays {
					v.ShortTermReview = true
				}
			}
		}
		// 止损计划信号：现价已破 / 接近计划止损（仅持仓中且有现价）。
		if p.Status == model.PositionStatusHolding && ok && p.PlanStopLoss > 0 {
			switch {
			case price <= p.PlanStopLoss:
				v.BelowStopLoss = true
			case (price-p.PlanStopLoss)/p.PlanStopLoss*100 <= nearStopLossPct:
				v.NearStopLoss = true
			}
		}
		// 分析时效：持仓中从未做过个股分析、或距上次分析超过阈值。
		if p.Status == model.PositionStatusHolding {
			if t, exists := lastAnalyzed[p.Symbol]; exists {
				tt := t
				v.LastAnalyzedAt = &tt
				v.AnalysisStale = now.Sub(t) > analysisStaleDays*24*time.Hour
			} else {
				v.AnalysisStale = true
			}
		}
		out = append(out, v)
	}
	return out, nil
}

// lastStockAnalysisFor 一次分组查询取用户对这批持仓标的最近一次「个股分析」时间。
func lastStockAnalysisFor(userID int64, positions []model.Position) map[string]time.Time {
	out := map[string]time.Time{}
	if common.DB == nil || len(positions) == 0 {
		return out
	}
	symbols := make([]string, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		if p.Status == model.PositionStatusHolding && !seen[p.Symbol] {
			seen[p.Symbol] = true
			symbols = append(symbols, p.Symbol)
		}
	}
	if len(symbols) == 0 {
		return out
	}
	var rows []struct {
		Symbol string
		Last   time.Time
	}
	if err := common.DB.Model(&model.AnalysisRecord{}).
		Select("symbol, MAX(created_at) AS last").
		Where("user_id = ? AND module = ? AND symbol IN ?", userID, model.AnalysisModuleStock, symbols).
		Group("symbol").Scan(&rows).Error; err != nil {
		return out
	}
	for _, r := range rows {
		out[r.Symbol] = r.Last
	}
	return out
}

// PortfolioOverview 组合总览：持仓聚合 + 集中度/风控信号（Ghostfolio 的组合层视角）。
type PortfolioOverview struct {
	HoldingCount   int     `json:"holding_count"`
	TotalCost      float64 `json:"total_cost"`
	TotalValue     float64 `json:"total_value"`
	TotalProfit    float64 `json:"total_profit"`
	ProfitPct      float64 `json:"profit_pct"`
	RealizedProfit float64 `json:"realized_profit"` // 已平仓累计已实现盈亏
	WinCount       int     `json:"win_count"`       // 盈利仓数（持仓中）
	LoseCount      int     `json:"lose_count"`      // 亏损仓数（持仓中）
	ShortValue     float64 `json:"short_value"`     // 短线市值
	LongValue      float64 `json:"long_value"`      // 长线市值
	TopSymbol      string  `json:"top_symbol"`
	TopName        string  `json:"top_name"`
	TopWeightPct   float64 `json:"top_weight_pct"` // 最大单一持仓占比 %

	Signals []string `json:"signals"` // 风控信号（集中度/止损/未分析）
}

// topWeightWarnPct 单一持仓市值占比超过该值提示集中度风险。
const topWeightWarnPct = 40.0

// Overview 组合总览与个人风控提示（基于 List 的富化结果聚合，读时计算不落库）。
func (s *PositionService) Overview(ctx context.Context, userID int64) (*PortfolioOverview, error) {
	views, err := s.List(ctx, userID, "all")
	if err != nil {
		return nil, err
	}
	ov := &PortfolioOverview{Signals: []string{}}
	// 按标的聚合市值算集中度（同一标的可能分批多仓）。
	valueBySymbol := map[string]float64{}
	nameBySymbol := map[string]string{}
	var nearStop, belowStop, stale int
	for _, v := range views {
		if v.Status == model.PositionStatusClosed {
			ov.RealizedProfit += v.ProfitAmount
			continue
		}
		ov.HoldingCount++
		ov.TotalCost += v.Cost
		if v.QuoteOK {
			ov.TotalValue += v.MarketValue
			ov.TotalProfit += v.ProfitAmount
			if v.ProfitAmount > 0 {
				ov.WinCount++
			} else if v.ProfitAmount < 0 {
				ov.LoseCount++
			}
			if v.PositionType == model.PositionTypeShortTerm {
				ov.ShortValue += v.MarketValue
			} else {
				ov.LongValue += v.MarketValue
			}
			valueBySymbol[v.Symbol] += v.MarketValue
			nameBySymbol[v.Symbol] = v.Name
		}
		if v.BelowStopLoss {
			belowStop++
		} else if v.NearStopLoss {
			nearStop++
		}
		if v.AnalysisStale {
			stale++
		}
	}
	if ov.TotalCost > 0 {
		ov.ProfitPct = round2(ov.TotalProfit / ov.TotalCost * 100)
	}
	ov.TotalCost = round2(ov.TotalCost)
	ov.TotalValue = round2(ov.TotalValue)
	ov.TotalProfit = round2(ov.TotalProfit)
	ov.RealizedProfit = round2(ov.RealizedProfit)
	ov.ShortValue = round2(ov.ShortValue)
	ov.LongValue = round2(ov.LongValue)

	for sym, val := range valueBySymbol {
		if val > 0 && ov.TotalValue > 0 {
			w := val / ov.TotalValue * 100
			if w > ov.TopWeightPct {
				ov.TopWeightPct = round2(w)
				ov.TopSymbol = sym
				ov.TopName = nameBySymbol[sym]
			}
		}
	}

	// 风控信号（个人风控提示：查询即提示，不推送）。
	if ov.TopWeightPct > topWeightWarnPct {
		ov.Signals = append(ov.Signals, fmt.Sprintf("%s 占持仓市值 %.1f%%，单一标的集中度偏高", orSymbol(ov.TopName, ov.TopSymbol), ov.TopWeightPct))
	}
	if belowStop > 0 {
		ov.Signals = append(ov.Signals, fmt.Sprintf("%d 笔持仓已跌破计划止损价，请立即复核", belowStop))
	}
	if nearStop > 0 {
		ov.Signals = append(ov.Signals, fmt.Sprintf("%d 笔持仓接近计划止损（3%%以内）", nearStop))
	}
	if stale > 0 {
		ov.Signals = append(ov.Signals, fmt.Sprintf("%d 笔持仓超过 %d 天未做个股分析", stale, analysisStaleDays))
	}
	return ov, nil
}

func orSymbol(name, symbol string) string {
	if name != "" {
		return name
	}
	return symbol
}

// PositionInput 新建/编辑持仓入参。
type PositionInput struct {
	Symbol       string  `json:"symbol"`
	Market       string  `json:"market"`
	Name         string  `json:"name"`
	PositionType string  `json:"position_type"`
	Currency     string  `json:"currency"`
	BuyPrice     float64 `json:"buy_price"`
	BuyDate      string  `json:"buy_date"`
	Quantity     float64 `json:"quantity"`
	BuyFee       float64 `json:"buy_fee"`
	BuyTax       float64 `json:"buy_tax"`
	BuyReason    string  `json:"buy_reason"`
	UserNote     string  `json:"user_note"`

	PlanStopLoss   float64 `json:"plan_stop_loss"`   // 计划止损价（0=未设）
	PlanTakeProfit float64 `json:"plan_take_profit"` // 计划止盈价（0=未设）
	ChecklistJSON  string  `json:"checklist_json"`   // 买入前检查清单快照

	RecommendationID int64 `json:"recommendation_id"` // 来源推荐（一键建仓带入；0=手动建仓）
}

// validCurrency 持仓币种枚举（DATABASE_DESIGN：CNY/USD/HKD）。
var validCurrency = map[string]bool{"CNY": true, "USD": true, "HKD": true}

// defaultCurrencyFor 按市场推导默认币种。
func defaultCurrencyFor(market string) string {
	switch market {
	case "us":
		return "USD"
	case "hk":
		return "HKD"
	}
	return "CNY"
}

// normalizeCurrency 归一并校验币种；空则按市场推导默认。
func normalizeCurrency(currency, market string) (string, error) {
	c := strings.ToUpper(strings.TrimSpace(currency))
	if c == "" {
		return defaultCurrencyFor(market), nil
	}
	if !validCurrency[c] {
		return "", errors.New("币种须为 CNY / USD / HKD")
	}
	return c, nil
}

// validateBuy 校验买入核心字段。
func validateBuy(in *PositionInput) error {
	if in.BuyPrice <= 0 {
		return errors.New("买入价格必须大于 0")
	}
	if in.Quantity <= 0 {
		return errors.New("买入数量必须大于 0")
	}
	if in.BuyFee < 0 || in.BuyTax < 0 {
		return errors.New("费用/税费不能为负")
	}
	if in.BuyDate != "" {
		if _, err := time.Parse("2006-01-02", in.BuyDate); err != nil {
			return errors.New("买入日期格式应为 YYYY-MM-DD")
		}
	}
	if !validPositionType[in.PositionType] {
		return errors.New("持仓类型须为 short_term 或 long_term")
	}
	// 风险计划：给了就必须与买入价自洽（止损 < 买价 < 止盈），否则计划无意义。
	if in.PlanStopLoss < 0 || in.PlanTakeProfit < 0 {
		return errors.New("止损/止盈价不能为负")
	}
	if in.PlanStopLoss > 0 && in.PlanStopLoss >= in.BuyPrice {
		return errors.New("计划止损价应低于买入价")
	}
	if in.PlanTakeProfit > 0 && in.PlanTakeProfit <= in.BuyPrice {
		return errors.New("计划止盈价应高于买入价")
	}
	if len(in.ChecklistJSON) > 4000 {
		return errors.New("检查清单数据过大")
	}
	return nil
}

// resolveRecommendationLink 校验推荐血缘归属：仅本人存在的推荐返回原 ID，
// 否则返回 0（不存在/他人的推荐不落血缘，静默忽略、不阻断建仓）。
func resolveRecommendationLink(userID, recID int64) int64 {
	if recID <= 0 {
		return 0
	}
	var n int64
	common.DB.Model(&model.Recommendation{}).
		Where("id = ? AND user_id = ?", recID, userID).Count(&n)
	if n == 0 {
		return 0
	}
	return recID
}

// Create 新建持仓。
func (s *PositionService) Create(ctx context.Context, userID int64, in PositionInput) (*model.Position, error) {
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	if err := validateBuy(&in); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(in.Name)
	if q, e := s.market.GetQuote(ctx, market, symbol); e == nil && q.Name != "" {
		name = q.Name
	} else if errors.Is(e, datasource.ErrSymbolInvalid) {
		return nil, errors.New("无法识别的股票代码")
	}
	currency, err := normalizeCurrency(in.Currency, market)
	if err != nil {
		return nil, err
	}
	// 推荐血缘：校验归属（不存在/他人的推荐不落血缘，静默忽略、不阻断建仓）。
	in.RecommendationID = resolveRecommendationLink(userID, in.RecommendationID)
	p := &model.Position{
		UserID:       userID,
		Symbol:       symbol,
		Market:       market,
		Name:         name,
		PositionType: in.PositionType,
		Status:       model.PositionStatusHolding,
		Currency:     currency,
		BuyPrice:     in.BuyPrice,
		BuyDate:      in.BuyDate,
		Quantity:     in.Quantity,
		BuyFee:       in.BuyFee,
		BuyTax:       in.BuyTax,
		BuyReason:    truncateRunes(strings.TrimSpace(in.BuyReason), 500),
		UserNote:     truncateRunes(strings.TrimSpace(in.UserNote), 500),

		PlanStopLoss:   in.PlanStopLoss,
		PlanTakeProfit: in.PlanTakeProfit,
		ChecklistJSON:  in.ChecklistJSON,

		RecommendationID: in.RecommendationID,
	}
	if err := common.DB.Create(p).Error; err != nil {
		return nil, err
	}
	return p, nil
}

// Update 编辑持仓的买入信息与备注（仅本人；不在此改状态/卖出）。
// 已平仓持仓的买入字段冻结（改动会直接改变已实现盈亏），仅允许改备注类字段。
func (s *PositionService) Update(userID, id int64, in PositionInput) (*model.Position, error) {
	var p model.Position
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
		return nil, errors.New("持仓不存在")
	}
	if p.Status == model.PositionStatusClosed {
		p.BuyReason = truncateRunes(strings.TrimSpace(in.BuyReason), 500)
		p.UserNote = truncateRunes(strings.TrimSpace(in.UserNote), 500)
		if err := common.DB.Save(&p).Error; err != nil {
			return nil, err
		}
		return &p, nil
	}
	if err := validateBuy(&in); err != nil {
		return nil, err
	}
	p.PositionType = in.PositionType
	p.BuyPrice = in.BuyPrice
	p.BuyDate = in.BuyDate
	p.Quantity = in.Quantity
	p.BuyFee = in.BuyFee
	p.BuyTax = in.BuyTax
	p.BuyReason = truncateRunes(strings.TrimSpace(in.BuyReason), 500)
	p.UserNote = truncateRunes(strings.TrimSpace(in.UserNote), 500)
	p.PlanStopLoss = in.PlanStopLoss
	p.PlanTakeProfit = in.PlanTakeProfit
	if in.ChecklistJSON != "" {
		p.ChecklistJSON = in.ChecklistJSON
	}
	if c := strings.TrimSpace(in.Currency); c != "" {
		currency, err := normalizeCurrency(c, p.Market)
		if err != nil {
			return nil, err
		}
		p.Currency = currency
	}
	if err := common.DB.Save(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// CloseInput 平仓（标记已卖出）入参。
type CloseInput struct {
	SellPrice  float64 `json:"sell_price"`
	SellDate   string  `json:"sell_date"`
	SellFee    float64 `json:"sell_fee"`
	SellTax    float64 `json:"sell_tax"`
	SellReason string  `json:"sell_reason"`
	ReviewNote string  `json:"review_note"`

	SellPlanned   string `json:"sell_planned"`   // yes/no/partial 是否按计划卖出
	AiVerdict     string `json:"ai_verdict"`     // right/wrong/mixed/unused 当时 AI 判断对错
	LessonLearned string `json:"lesson_learned"` // 下次策略调整点
}

var validSellPlanned = map[string]bool{"": true, "yes": true, "no": true, "partial": true}
var validAiVerdict = map[string]bool{"": true, "right": true, "wrong": true, "mixed": true, "unused": true}

// Close 标记持仓已卖出并填写复盘（仅本人；重复平仓报错）。
func (s *PositionService) Close(userID, id int64, in CloseInput) (*model.Position, error) {
	var p model.Position
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&p).Error; err != nil {
		return nil, errors.New("持仓不存在")
	}
	if p.Status == model.PositionStatusClosed {
		return nil, errors.New("该持仓已卖出")
	}
	if in.SellPrice <= 0 {
		return nil, errors.New("卖出价格必须大于 0")
	}
	if in.SellFee < 0 || in.SellTax < 0 {
		return nil, errors.New("费用/税费不能为负")
	}
	if in.SellDate != "" {
		if _, err := time.Parse("2006-01-02", in.SellDate); err != nil {
			return nil, errors.New("卖出日期格式应为 YYYY-MM-DD")
		}
		if p.BuyDate != "" && in.SellDate < p.BuyDate {
			return nil, errors.New("卖出日期不能早于买入日期")
		}
	}
	if !validSellPlanned[in.SellPlanned] {
		return nil, errors.New("是否按计划卖出取值须为 yes/no/partial")
	}
	if !validAiVerdict[in.AiVerdict] {
		return nil, errors.New("AI 判断对错取值须为 right/wrong/mixed/unused")
	}
	p.Status = model.PositionStatusClosed
	p.SellPrice = in.SellPrice
	p.SellDate = in.SellDate
	p.SellFee = in.SellFee
	p.SellTax = in.SellTax
	p.SellReason = truncateRunes(strings.TrimSpace(in.SellReason), 500)
	p.ReviewNote = truncateRunes(strings.TrimSpace(in.ReviewNote), 500)
	p.SellPlanned = in.SellPlanned
	p.AiVerdict = in.AiVerdict
	p.LessonLearned = truncateRunes(strings.TrimSpace(in.LessonLearned), 500)
	if err := common.DB.Save(&p).Error; err != nil {
		return nil, err
	}
	return &p, nil
}

// Delete 删除持仓（仅本人）。
func (s *PositionService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.Position{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("持仓不存在")
	}
	return nil
}
