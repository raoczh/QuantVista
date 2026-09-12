package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

func exitPlanProfileDB(db *gorm.DB, p model.Position) (string, []string) {
	if p.RecommendationID <= 0 {
		return "balanced", nil
	}
	var rec model.Recommendation
	if err := db.Where("id = ? AND user_id = ? AND symbol = ? AND market = ?", p.RecommendationID, p.UserID, p.Symbol, p.Market).First(&rec).Error; err != nil {
		return "balanced", []string{"来源推荐无法核验，使用均衡退出配置，未推测原策略"}
	}
	var batch model.RecommendationBatch
	if err := db.Select("score_profile").Where("id = ? AND user_id = ?", rec.BatchID, p.UserID).First(&batch).Error; err != nil || batch.ScoreProfile == "" {
		return "balanced", []string{"来源推荐未记录评分侧重，使用均衡退出配置"}
	}
	return batch.ScoreProfile, nil
}

func exitPlanLocalBars(db *gorm.DB, p model.Position, now time.Time) ([]model.DailyBar, []string) {
	local := now.In(time.Local)
	cutoff := local.Format("2006-01-02")
	if local.Hour() < 15 {
		cutoff = local.AddDate(0, 0, -1).Format("2006-01-02")
	}
	var rows []model.DailyBar
	if err := db.Where("market = ? AND symbol = ? AND trade_date <= ?", p.Market, p.Symbol, cutoff).
		Order("trade_date DESC").Limit(120).Find(&rows).Error; err != nil {
		return nil, []string{"退出规划的本地日线读取失败"}
	}
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	bars, gaps := completedPositionExitBars(rows, cutoff, model.PositionExitSessionClose)
	if err := validateLocalAdjustedBars(p.Market, bars); err != nil {
		return nil, append(gaps, err.Error())
	}
	var dates []string
	if err := db.Model(&model.TradingCalendar{}).Where("market = ? AND is_open = ? AND trade_date <= ?", p.Market, true, cutoff).
		Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error; err != nil || len(dates) == 0 {
		return nil, append(gaps, "缺少交易日历，不能核验退出规划日线时效")
	}
	if len(bars) == 0 || bars[len(bars)-1].TradeDate != dates[0] {
		return nil, append(gaps, "退出规划缺少最近完整交易日的有效日线")
	}
	return bars, gaps
}

// 缓存行情失败不阻止真实交易记账。未知规划明确保存，数据恢复后由评估建立完整版本。
func initializePositionExitSeedDB(db *gorm.DB, p *model.Position, now time.Time, source string) {
	if seed := decodeExitSeed(p.ExitPlanSeedJSON); seed != nil && seed.BasisHash == exitPlanBasis(*p) {
		return
	}
	bars, gaps := exitPlanLocalBars(db, *p, now)
	profile, profileGaps := exitPlanProfileDB(db, *p)
	gaps = append(gaps, profileGaps...)
	if p.BuyDate != "" && p.BuyDate < now.In(time.Local).Format("2006-01-02") && source == "entry" {
		source = "recorded_later"
	}
	seed := buildExitPlanSeed(*p, bars, profile, source, now.In(time.Local).Format("2006-01-02"), now, gaps)
	if source == "recorded_later" {
		seed.Evidence = append(seed.Evidence, "这是补录时形成的规划，不代表历史买入当时已知的计划")
		sealExitSeed(&seed)
	}
	p.ExitPlanSeedJSON = mustPositionExitJSON(seed)
}

func positionExitSeedFor(db *gorm.DB, p model.Position, previous *ExitPlan, bars []model.DailyBar, now time.Time, gaps []string) ExitPlanSeed {
	basis := exitPlanBasis(p)
	if previous != nil && previous.ProtectionSuspended && p.PeakDataQuality != model.FactorQualityUnverifiedAdjustment && len(gaps) == 0 && len(bars) >= 21 {
		seed := buildExitPlanSeed(p, bars, previous.Initial.Profile, "data_recovery", now.In(time.Local).Format("2006-01-02"), now, nil)
		seed.Evidence = append(seed.Evidence, "峰值数据口径核验后重新建立保护；失效的旧保护保留审计，不再沿用")
		sealExitSeed(&seed)
		return seed
	}
	seed := decodeExitSeed(p.ExitPlanSeedJSON)
	if previous != nil && previous.Initial.BasisHash == basis && (seed == nil || seed.BasisHash != basis || seed.Hash == previous.Initial.Hash || seed.DataStatus == "unavailable") {
		seed = &previous.Initial
	}
	if seed != nil && seed.BasisHash == basis && seed.DataStatus != "unavailable" {
		return *seed
	}
	profile, profileGaps := exitPlanProfileDB(db, p)
	gaps = append(append([]string{}, gaps...), profileGaps...)
	source := "first_assessment"
	if seed != nil && seed.BasisHash == basis {
		source = seed.Source
	}
	current := buildExitPlanSeed(p, bars, profile, source, now.In(time.Local).Format("2006-01-02"), now, gaps)
	if p.ExitPlanSeedJSON == "" && previous == nil {
		current.Evidence = append(current.Evidence, "旧持仓从本次评估建立规划，未回填或伪造买入时的历史计划")
		sealExitSeed(&current)
	}
	if seed != nil && seed.BasisHash == basis && current.DataStatus == "unavailable" && seed.DataStatus == "unavailable" && current.BarsAsOf == seed.BarsAsOf {
		return *seed // 数据没有改善时不因采集秒数不同而无限追加规划。
	}
	return current
}

// PreviewPositionExitPlan 不写账本、不拉外部行情；实际保存仍由服务器按真实买入参数重算。
func (s *PositionService) PreviewPositionExitPlan(ctx context.Context, userID int64, in PositionInput, positionIDs ...int64) (*ExitPlanSeed, error) {
	if userID <= 0 {
		return nil, errors.New("用户身份无效")
	}
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	if err = validateBuy(&in); err != nil {
		return nil, err
	}
	currency, err := normalizeCurrency(in.Currency, market)
	if err != nil {
		return nil, err
	}
	now := time.Now().In(time.Local)
	p := model.Position{UserID: userID, Symbol: symbol, Market: market, Currency: currency, PositionType: in.PositionType,
		Status: model.PositionStatusHolding, BuyPrice: in.BuyPrice, BuyDate: in.BuyDate, Quantity: in.Quantity, BuyFee: in.BuyFee, BuyTax: in.BuyTax,
		PlanStopLoss: in.PlanStopLoss, PlanTakeProfit: in.PlanTakeProfit, RecommendationID: in.RecommendationID}
	p.TotalBuyCost = round4(in.BuyPrice*in.Quantity + in.BuyFee + in.BuyTax)
	p.TotalBuyQty = in.Quantity
	p.RemainingCost = p.TotalBuyCost
	p.PeakPrice, p.PeakFrom = peakInitFor(p.BuyPrice, p.BuyDate, now.Format("2006-01-02"))
	if len(positionIDs) > 0 && positionIDs[0] > 0 {
		var current model.Position
		if err := common.DB.WithContext(ctx).Scopes(withActivePositionAccount).Where("id = ? AND user_id = ? AND status = ?", positionIDs[0], userID, model.PositionStatusHolding).First(&current).Error; err != nil {
			return nil, errors.New("持仓不存在")
		}
		p.RecommendationID = current.RecommendationID
		if p.Symbol != current.Symbol || p.Market != current.Market {
			return nil, errors.New("持仓标的不匹配")
		}
		if p.BuyPrice == current.BuyPrice && p.Quantity == current.Quantity && p.BuyFee == current.BuyFee && p.BuyTax == current.BuyTax && p.BuyDate == current.BuyDate {
			p.TotalBuyCost, p.TotalBuyQty, p.RemainingCost = current.TotalBuyCost, current.TotalBuyQty, current.RemainingCost
			p.PeakFrom = current.PeakFrom
		}
	}
	if p.RecommendationID > 0 {
		linked, err := resolveRecommendationLink(common.DB.WithContext(ctx), userID, p.RecommendationID, symbol, market)
		if err != nil {
			return nil, err
		}
		if linked == 0 {
			return nil, errors.New("来源推荐不存在或不属于本人同一标的")
		}
	}
	initializePositionExitSeedDB(common.DB.WithContext(ctx), &p, now, "preview")
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	seed := decodeExitSeed(p.ExitPlanSeedJSON)
	if seed == nil {
		return nil, errors.New("退出规划无法形成有效结果")
	}
	return seed, nil
}

func attachRecommendationExitPlan(ctx context.Context, recType string, pick *recPick, c candidate, profile string) {
	if pick.ExecutionPlan == nil || common.DB == nil {
		return
	}
	price, quantity := pick.ExecutionPlan.PlannedPrice, float64(pick.ExecutionPlan.Quantity)
	if price <= 0 {
		price = c.Price
	}
	assumedQuantity := quantity <= 0
	if assumedQuantity {
		quantity = 100
	}
	fee, tax := tradeFee(c.Market, model.PaperSideBuy, c.Symbol, price*quantity)
	now := time.Now().In(time.Local)
	p := model.Position{Symbol: c.Symbol, Market: c.Market, Currency: defaultCurrencyFor(c.Market), PositionType: recType,
		Status: model.PositionStatusHolding, BuyPrice: price, Quantity: quantity, BuyFee: fee, BuyTax: tax,
		BuyDate: now.Format("2006-01-02"), PeakFrom: now.Format("2006-01-02"), RemainingCost: round4(price*quantity + fee + tax)}
	bars, gaps := exitPlanLocalBars(common.DB.WithContext(ctx), p, now)
	seed := buildExitPlanSeed(p, bars, profile, "recommendation", p.BuyDate, now, gaps)
	seed.Evidence = append(seed.Evidence, "按本次推荐参考买价规划；登记实际买入后按实际成本重新计算，模型原始价位保留在推荐依据中")
	if assumedQuantity {
		seed.Evidence = append(seed.Evidence, "尚无可用资金数量计划，费用按 100 股示例估算，实际买入时重算")
	}
	sealExitSeed(&seed)
	pick.ExecutionPlan.ExitPlan = &seed
	if !assumedQuantity && seed.DataStatus != "unavailable" {
		risk := seed.EstimatedRisk
		pick.ExecutionPlan.MaxPlannedLoss = &risk
	}
}

func exitSellableQuantity(db *gorm.DB, p model.Position, tradeDate string) (*float64, []string) {
	var trades []model.PositionTrade
	if err := db.Where("user_id = ? AND position_id = ? AND side = ? AND trade_date >= ?", p.UserID, p.ID, model.PositionTradeBuy, tradeDate).
		Find(&trades).Error; err != nil {
		return nil, []string{"当日买入流水读取失败，可卖数量未知"}
	}
	locked := 0.0
	for _, trade := range trades {
		locked += trade.Quantity
	}
	if len(trades) == 0 && effectiveTradeDate(p.BuyDate, p.CreatedAt) >= tradeDate {
		locked = p.Quantity
	}
	available := round4(math.Max(0, p.Quantity-locked))
	notes := []string{"可卖数量按已录入账本及 T+1 保守估算，最终以券商为准；不会自动记作卖出"}
	if locked > 0 {
		notes = append(notes, fmt.Sprintf("当日新增仓位受 T+1 限制，当前估算可卖 %g 股", available))
	}
	return &available, notes
}

func exitQuoteExecutionNotes(p model.Position, q FreshQuoteResult, now time.Time) []string {
	if q.Quote == nil {
		return []string{"缺少有效报价，不能判断当前成交条件"}
	}
	quote := q.Quote
	notes := []string{}
	local := now.In(time.Local)
	minutes := local.Hour()*60 + local.Minute()
	if local.Weekday() == time.Saturday || local.Weekday() == time.Sunday || minutes < 570 || minutes >= 900 || minutes >= 690 && minutes < 780 {
		notes = append(notes, "当前不在连续交易时段，执行前请核对开市时间与券商交易状态")
	}
	if quote.Volume <= 0 {
		notes = append(notes, "行情没有有效成交量，需核对停牌及实际可成交性")
	}
	if quote.PrevClose > 0 {
		limit := limitUpPctFor(p.Symbol, p.Name)
		if boardLimit := limitUpPctFor(p.Symbol, ""); boardLimit >= 20 {
			limit = boardLimit
		}
		if quote.Price <= exitFloor(quote.PrevClose*(1-limit/100), exitPriceTick(p.Symbol))+exitPriceTick(p.Symbol)/2 {
			notes = append(notes, "现价处于跌停附近，触发保护也可能无法及时成交，不能按保护价估计实际成交")
		}
	}
	return notes
}

func exitPlanHeldDays(db *gorm.DB, p model.Position, through string) *int {
	from := p.PeakFrom
	if from == "" {
		from = effectiveTradeDate(p.BuyDate, p.CreatedAt)
	}
	if from == "" {
		return nil
	}
	var endKnown int64
	if err := db.Model(&model.TradingCalendar{}).Where("market = ? AND trade_date = ? AND is_open = ?", p.Market, through, true).Count(&endKnown).Error; err != nil || endKnown == 0 {
		return nil
	}
	var count int64
	if err := db.Model(&model.TradingCalendar{}).Where("market = ? AND is_open = ? AND trade_date > ? AND trade_date <= ?", p.Market, true, from, through).Count(&count).Error; err != nil {
		return nil
	}
	days := int(count)
	return &days
}

func (s *PositionService) RefreshExitByAccount(ctx context.Context, userID, accountID int64) (int, error) {
	if _, err := ActivePortfolioAccountByID(userID, accountID, model.PortfolioKindReal); err != nil {
		return 0, err
	}
	lock := alertEvalLock(userID)
	if err := lock.Lock(ctx); err != nil {
		return 0, err
	}
	defer lock.Unlock()
	var positions []model.Position
	if err := common.DB.WithContext(ctx).Scopes(withActivePositionAccount).
		Where("user_id = ? AND account_id = ? AND status = ? AND market = ?", userID, accountID, model.PositionStatusHolding, "cn").Order("id ASC").Find(&positions).Error; err != nil {
		return 0, err
	}
	refs := make([]QuoteRef, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		key := QuoteKey(p.Market, p.Symbol)
		if !seen[key] {
			refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
			seen[key] = true
		}
	}
	quotes := map[string]FreshQuoteResult{}
	if s.market != nil && len(refs) > 0 {
		quotes = s.market.FreshQuotesFor(ctx, refs)
	}
	return NewPositionExitAssessmentService(s.market).EvaluateUserWithSnapshot(ctx, userID, positions, quotes, model.PositionExitSessionIntraday, time.Now())
}
