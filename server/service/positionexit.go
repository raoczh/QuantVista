package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// 统一持仓卖出评估参数。所有技术指标参数均在此版本化，并随每条事实固化快照。
// 修改参数必须升级 Version，避免历史事实无法复现。
type positionExitParams struct {
	Version       string  `json:"version"`
	MA20Period    int     `json:"ma20_period"`
	MA60Period    int     `json:"ma60_period"`
	ATRPeriod     int     `json:"atr_period"`
	ATRMultiplier float64 `json:"atr_multiplier"`
	BarLimit      int     `json:"bar_limit"`
	MinBars       int     `json:"min_bars"`
}

var defaultPositionExitParams = positionExitParams{
	Version:    model.PositionExitAssessmentVersion,
	MA20Period: 20, MA60Period: 60,
	ATRPeriod: 14, ATRMultiplier: 3, BarLimit: 120, MinBars: 61,
}

type PositionExitSignal struct {
	Key       string  `json:"key"`
	Label     string  `json:"label"`
	Detail    string  `json:"detail"`
	Severity  string  `json:"severity"`
	Value     float64 `json:"value,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	Crossing  bool    `json:"crossing,omitempty"`
}

type PositionExitAssessmentView struct {
	model.PositionExitAssessment
	Signals       []PositionExitSignal `json:"signals"`
	Evidence      []string             `json:"evidence"`
	DataGaps      []string             `json:"data_gaps"`
	AlertEventIDs []int64              `json:"alert_event_ids"`
	SellReviewIDs []int64              `json:"sell_review_ids"`
}

type positionExitMarketProvider interface {
	FreshQuotesFor(context.Context, []QuoteRef) map[string]FreshQuoteResult
}

type PositionExitAssessmentService struct {
	market positionExitMarketProvider
	notify alertNotifier
	now    func() time.Time
	params positionExitParams
}

func NewPositionExitAssessmentService(market positionExitMarketProvider) *PositionExitAssessmentService {
	return &PositionExitAssessmentService{market: market, notify: NewNotifyService(), now: time.Now, params: defaultPositionExitParams}
}

type positionExitInput struct {
	position model.Position
	quote    FreshQuoteResult
	barRows  []model.DailyBar
	now      time.Time
	session  string
	rules    []model.AlertRule
	events   []model.AlertEvent
	reviews  []model.SellReview
	loadErrs []string
	// pendingCorpAdjust：存在除权日已到、未确认的送转折算。成本/峰值/计划价仍是
	// 除权前口径，价格与技术信号全部按数据缺口处理，防止 -50% 假止损。
	pendingCorpAdjust bool
	// hasPrevAssessment/prevBelowATR：上一条评估事实是否存在、其时点是否已处于
	// ATR14 保护线下方。保护线随峰值上移，纯 crossing（前收盘在线上）会在峰值刷新
	// 日恒 false 且此后永不触发；改用「上次不在线下 → 本次在线下」的状态迁移判定，
	// 无历史时退回前收盘判定。
	hasPrevAssessment bool
	prevBelowATR      bool
}

func (s *PositionExitAssessmentService) EvaluateUser(ctx context.Context, userID int64, session string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if session != model.PositionExitSessionClose {
		session = model.PositionExitSessionIntraday
	}
	var positions []model.Position
	// 只评 cn：日历/日线/ATR/事件语义都是 A 股口径，与 guard/sellreview 一致；
	// hk/us 持仓用 A 股交易日评估会产生错误的 as-of 与假信号。
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND status = ? AND market = ?", userID, model.PositionStatusHolding, "cn").
		Order("id ASC").Find(&positions).Error; err != nil {
		return 0, fmt.Errorf("读取持仓失败: %w", err)
	}
	if len(positions) == 0 {
		return 0, nil
	}
	refs := make([]QuoteRef, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		key := QuoteKey(p.Market, p.Symbol)
		if !seen[key] {
			seen[key] = true
			refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
		}
	}
	quotes := map[string]FreshQuoteResult{}
	if s.market != nil {
		quotes = s.market.FreshQuotesFor(ctx, refs)
	}
	return s.EvaluateUserWithSnapshot(ctx, userID, positions, quotes, session, s.now().In(time.Local))
}

// EvaluateUserWithSnapshot 供现有提醒轮/盘后链路复用已经批量取得的持仓与行情。
func (s *PositionExitAssessmentService) EvaluateUserWithSnapshot(
	ctx context.Context,
	userID int64,
	positions []model.Position,
	quotes map[string]FreshQuoteResult,
	session string,
	evaluatedAt time.Time,
) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if session != model.PositionExitSessionClose {
		session = model.PositionExitSessionIntraday
	}
	params := s.params
	if params.Version == "" {
		params = defaultPositionExitParams
	}
	peakErr := syncPositionExitPeaks(ctx, userID, positions, quotes, evaluatedAt)
	positionIDs := make([]int64, 0, len(positions))
	for _, p := range positions {
		positionIDs = append(positionIDs, p.ID)
	}
	pendingAdjust, pendingAdjustErr := positionsWithUnconfirmedShareAction(
		ctx, userID, positionIDs, evaluatedAt.In(time.Local).Format("2006-01-02"))
	created := 0
	var errs []error
	for _, p := range positions {
		if p.UserID != userID || p.Status != model.PositionStatusHolding || p.Market != "cn" {
			continue
		}
		in := positionExitInput{position: p, quote: quotes[QuoteKey(p.Market, p.Symbol)], now: evaluatedAt, session: session,
			pendingCorpAdjust: pendingAdjust[p.ID]}
		if peakErr != nil {
			in.loadErrs = append(in.loadErrs, "持仓峰值查询失败")
		}
		if pendingAdjustErr != nil {
			in.loadErrs = append(in.loadErrs, "除权折算状态查询失败")
		}
		if err := common.DB.WithContext(ctx).Where("symbol = ? AND market = ?", p.Symbol, p.Market).
			Order("trade_date DESC").Limit(params.BarLimit).Find(&in.barRows).Error; err != nil {
			in.loadErrs = append(in.loadErrs, "日线查询失败")
		}
		for i, j := 0, len(in.barRows)-1; i < j; i, j = i+1, j-1 {
			in.barRows[i], in.barRows[j] = in.barRows[j], in.barRows[i]
		}
		if err := common.DB.WithContext(ctx).Where("user_id = ? AND status = ? AND kind IN ? AND (symbol = '' OR symbol = ?)",
			userID, model.AlertStatusActive, positionAlertKinds, p.Symbol).Order("id ASC").Find(&in.rules).Error; err != nil {
			in.loadErrs = append(in.loadErrs, "持仓规则查询失败")
		}
		tradeDate := effectivePositionExitTradeDate(in.quote, evaluatedAt)
		if err := common.DB.WithContext(ctx).Where("user_id = ? AND position_id = ? AND trade_date = ?",
			userID, p.ID, tradeDate).Order("id ASC").Find(&in.events).Error; err != nil {
			in.loadErrs = append(in.loadErrs, "提醒事件查询失败")
		}
		if err := common.DB.WithContext(ctx).Where("user_id = ? AND position_id = ? AND status = ?",
			userID, p.ID, model.SellReviewStatusOpen).Order("trade_date DESC, id DESC").Find(&in.reviews).Error; err != nil {
			in.loadErrs = append(in.loadErrs, "卖出复核查询失败")
		}
		var prevRow model.PositionExitAssessment
		if err := common.DB.WithContext(ctx).Select("signals_json").
			Where("user_id = ? AND position_id = ?", userID, p.ID).
			Order("evaluated_at DESC, id DESC").First(&prevRow).Error; err == nil {
			in.hasPrevAssessment = true
			in.prevBelowATR = strings.Contains(prevRow.SignalsJSON, `"atr14_break"`)
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			in.loadErrs = append(in.loadErrs, "上一条评估查询失败")
		}
		row := evaluatePositionExit(in, params)
		inserted, notifyNeeded, err := persistPositionExitAssessment(ctx, row)
		if err != nil {
			errs = append(errs, fmt.Errorf("持仓 %d 落库失败: %w", p.ID, err))
			continue
		}
		if inserted {
			created++
			// 事实每次有意义变化都追加，但通知只在首次、等级升级或主因变化时发：
			// 逐日重算（open SellReview 逐日换 trade_date、ATR 线下逐日快照）不能
			// 变成每天一条重复推送。
			if notifyNeeded {
				s.notifyAssessment(ctx, row)
			}
		}
	}
	return created, errors.Join(errs...)
}

func (s *PositionExitAssessmentService) notifyAssessment(ctx context.Context, row model.PositionExitAssessment) {
	if s.notify == nil || (row.Level != model.PositionExitLevelReview && row.Level != model.PositionExitLevelUrgent) {
		return
	}
	enabled, err := notificationsEnabledFor(ctx, row.UserID, model.BrowserNotifyCategoryExitRisk)
	if err != nil || !enabled {
		return
	}
	kind := model.GuardKindPosExitReview
	// ntfy 语义（notify.go）：1=最低、5=最高、0=通道默认(3)。urgent 对齐旧
	// stop_loss 守护推送的高优先级 4；review 走默认。写 1 会把最紧急的止损
	// 触达降成静默级。
	priority := 0
	if row.Level == model.PositionExitLevelUrgent {
		kind = model.GuardKindPosExitUrgent
		priority = 4
	}
	levelName := "需要复核"
	if row.Level == model.PositionExitLevelUrgent {
		levelName = "紧急处理"
	}
	dataTime := strings.TrimSpace(row.QuoteAsOf)
	if row.BarsAsOf != "" && row.BarsAsOf != dataTime {
		dataTime = strings.TrimSpace(dataTime + " / 日线 " + row.BarsAsOf)
	}
	message := truncateRunes(fmt.Sprintf("%s(%s) · %s：%s。数据时间：%s。下一步：%s",
		orSymbol(row.Name, row.Symbol), row.Symbol, levelName, row.PrimaryReason, orSymbol(dataTime, "待补全"), row.NextAction), 512)
	hit := guardHit{
		PositionID: row.PositionID, Symbol: row.Symbol, Market: row.Market, Name: orSymbol(row.Name, row.Symbol),
		Kind: kind, Price: row.QuotePrice, Message: message,
		Route: fmt.Sprintf("/positions?position_id=%d&assessment_id=%d", row.PositionID, row.ID), Priority: priority,
	}
	if !recordGuardEvent(row.UserID, row.TradeDate, hit) {
		return
	}
	msg := NotifyMessage{
		Title: "QuantVista 持仓卖出风险复核", Content: message,
		Route: hit.Route, Kind: NotifyMsgKindGuard, Priority: priority,
		BrowserEvents: []BrowserNotificationInput{{
			SourceType: "position_exit_assessment", SourceID: row.ID,
			FactKey:  BrowserFactKey("position_exit_assessment", fmt.Sprint(row.ID), row.Level, row.FactHash),
			Category: model.BrowserNotifyCategoryExitRisk, Level: row.Level,
			Title: fmt.Sprintf("%s（%s）· %s", orSymbol(row.Name, row.Symbol), row.Symbol, levelName),
			Body:  message, Route: hit.Route,
		}},
	}
	if detached, ok := s.notify.(interface {
		SendMsgDetached(context.Context, int64, NotifyMessage)
	}); ok {
		detached.SendMsgDetached(ctx, row.UserID, msg)
		return
	}
	s.notify.SendMsgContext(ctx, row.UserID, msg)
}

func effectivePositionExitTradeDate(fq FreshQuoteResult, now time.Time) string {
	wall := now.In(time.Local).Format("2006-01-02")
	if fq.Quote == nil {
		return wall
	}
	return effectiveQuoteTradeDate(fq, wall)
}

// syncPositionExitPeaks 按每笔 fresh 行情自己的观测交易日补峰值。周末/休市日不能
// 用服务器墙上日期把上一交易日的 High 提前并入“历史”，否则后续 MA/ATR 与现价
// 不再处于同一个 as-of。positions 是可变快照，补齐结果会写回供本轮评估使用。
func syncPositionExitPeaks(
	ctx context.Context,
	userID int64,
	positions []model.Position,
	quotes map[string]FreshQuoteResult,
	evaluatedAt time.Time,
) error {
	type peakGroup struct {
		indices []int
		rows    []model.Position
	}
	wallDate := evaluatedAt.In(time.Local).Format("2006-01-02")
	groups := map[string]*peakGroup{}
	for i, p := range positions {
		fq, ok := quotes[QuoteKey(p.Market, p.Symbol)]
		if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
			continue
		}
		date := effectivePositionExitTradeDate(fq, evaluatedAt)
		if date == "" {
			date = wallDate
		}
		group := groups[date]
		if group == nil {
			group = &peakGroup{}
			groups[date] = group
		}
		group.indices = append(group.indices, i)
		group.rows = append(group.rows, p)
	}
	dates := make([]string, 0, len(groups))
	for date := range groups {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	var errs []error
	for _, date := range dates {
		group := groups[date]
		if _, err := syncPositionPeaksBefore(ctx, userID, group.rows, date); err != nil {
			errs = append(errs, fmt.Errorf("%s 峰值查询失败: %w", date, err))
		}
		for i, positionIndex := range group.indices {
			positions[positionIndex] = group.rows[i]
		}
	}
	return errors.Join(errs...)
}

func completedPositionExitBars(rows []model.DailyBar, cutoffDate, session string) ([]model.DailyBar, []string) {
	out := make([]model.DailyBar, 0, len(rows))
	var gaps []string
	lastDate := ""
	for _, b := range rows {
		if b.TradeDate == "" || b.TradeDate < lastDate || b.Open <= 0 || b.High <= 0 || b.Low <= 0 || b.Close <= 0 ||
			b.High < math.Max(b.Open, b.Close) || b.Low > math.Min(b.Open, b.Close) || b.Low > b.High {
			return nil, []string{"本地日线存在无效 OHLC 或日期乱序"}
		}
		lastDate = b.TradeDate
		if session == model.PositionExitSessionIntraday && b.TradeDate >= cutoffDate {
			continue
		}
		if session == model.PositionExitSessionClose && b.TradeDate > cutoffDate {
			continue
		}
		out = append(out, b)
	}
	if len(out) == 0 {
		gaps = append(gaps, "缺少已完成的本地日线")
	}
	return out, gaps
}

func positionExitATR14(bars []model.DailyBar, period int) (float64, bool) {
	if period <= 0 || len(bars) < period+1 {
		return 0, false
	}
	trs := make([]float64, 0, len(bars)-1)
	for i := 1; i < len(bars); i++ {
		prev := bars[i-1].Close
		tr := math.Max(bars[i].High-bars[i].Low, math.Max(math.Abs(bars[i].High-prev), math.Abs(bars[i].Low-prev)))
		trs = append(trs, tr)
	}
	sum := 0.0
	for _, v := range trs[len(trs)-period:] {
		sum += v
	}
	return round4(sum / float64(period)), true
}

func evaluatePositionExit(in positionExitInput, params positionExitParams) model.PositionExitAssessment {
	p := in.position
	now := in.now.In(time.Local)
	tradeDate := effectivePositionExitTradeDate(in.quote, now)
	row := model.PositionExitAssessment{
		UserID: p.UserID, PositionID: p.ID, Symbol: p.Symbol, Market: p.Market, Name: orSymbol(p.Name, p.Symbol),
		TradeDate: tradeDate, Session: in.session, EvaluatedAt: now,
		Level: model.PositionExitLevelUnknown, DataStatus: model.PositionExitDataUnknown,
		BuyPrice: p.BuyPrice, PeakPrice: p.PeakPrice, Version: params.Version,
	}
	paramsJSON, _ := json.Marshal(params)
	row.ParamsJSON = string(paramsJSON)
	row.ParamsHash = stablePositionExitHash(params)

	var gaps []string
	gaps = append(gaps, in.loadErrs...)
	q := in.quote.Quote
	quoteOK := q != nil && q.Price > 0 && !q.DataTime.IsZero() && in.quote.Fresh.Status == freshStatusFresh
	if q != nil {
		row.QuotePrice = q.Price
		if !q.DataTime.IsZero() {
			row.QuoteAsOf = q.DataTime.In(time.Local).Format(time.RFC3339)
		}
	}
	if !quoteOK {
		gaps = append(gaps, "行情不是 fresh，不能判断卖出风险")
	} else {
		if p.BuyPrice > 0 {
			row.ProfitPct = round2((q.Price - p.BuyPrice) / p.BuyPrice * 100)
		} else {
			gaps = append(gaps, "持仓成本无效")
		}
		if p.PeakPrice > 0 {
			row.PeakDrawdownPct = peakDrawdownPct(p.PeakPrice, q.Price)
		} else {
			gaps = append(gaps, "持仓峰值尚未建立")
		}
	}

	bars, barGaps := completedPositionExitBars(in.barRows, tradeDate, in.session)
	gaps = append(gaps, barGaps...)
	if len(bars) > 0 {
		row.BarsAsOf = bars[len(bars)-1].TradeDate
	}
	if len(bars) < params.MinBars {
		gaps = append(gaps, fmt.Sprintf("已完成日线不足 %d 根", params.MinBars))
	}
	var signals []PositionExitSignal
	var evidence []string
	alertIDs := make([]int64, 0, len(in.events))
	for _, e := range in.events {
		alertIDs = append(alertIDs, e.ID)
	}
	reviewIDs := make([]int64, 0, len(in.reviews))
	for _, r := range in.reviews {
		reviewIDs = append(reviewIDs, r.ID)
	}

	// 评估按证据能力分层：fresh 行情可独立确认计划价和成本类硬事实；MA/ATR
	// 只在已完成日线充分且有效时参与。旁路查询或技术数据不完整不能反向吞掉
	// 已经明确触发的计划止损，但缺口仍会落库并把 DataStatus 标为 partial。
	//
	// 建仓时点防护（与 positionalert.go 规则路径同口径，否则两条链路对同一仓
	// 结论矛盾）：行情有效日早于建仓/加仓起算日时，价格与技术判断全部发生在
	// 持仓之前，不能作为本仓风险证据；建仓当日整日 High/Low 可能发生在成交
	// 之前，一律只用现价——宁可漏掉盘中触达，也不制造持仓前假信号。
	priorQuote := quoteOK && p.PeakFrom != "" && p.PeakFrom > tradeDate
	sameDayEntry := p.PeakFrom != "" && p.PeakFrom == tradeDate
	if priorQuote {
		gaps = append(gaps, "行情早于建仓起算日，价格与技术信号不参与本次评估")
	}
	// 未确认的除权折算：成本/峰值/计划价是旧口径、行情是新口径，价格与技术信号
	// 全部暂缓（10 转 10 会凭空 -50% 假止损）；独立事件事实（SellReview）保留。
	if in.pendingCorpAdjust {
		gaps = append(gaps, "存在未确认的除权折算，成本/价格类信号暂缓评估，请先在持仓页确认")
	}
	priceBlocked := priorQuote || in.pendingCorpAdjust
	technicalReady := quoteOK && !priceBlocked && len(barGaps) == 0 && len(bars) >= params.MinBars
	if quoteOK && !priceBlocked {
		dayHigh, dayLow := q.Price, q.Price
		if !sameDayEntry {
			if q.High > dayHigh {
				dayHigh = q.High
			}
			if q.Low > 0 && q.Low < dayLow {
				dayLow = q.Low
			}
		}
		if p.PlanStopLoss > 0 && dayLow <= p.PlanStopLoss {
			signals = append(signals, PositionExitSignal{Key: "plan_stop", Label: "触达计划止损", Detail: fmt.Sprintf("当日最低 %.2f，计划止损 %.2f", dayLow, p.PlanStopLoss), Severity: model.PositionExitLevelUrgent, Value: dayLow, Threshold: p.PlanStopLoss, Crossing: true})
		}
		if p.PlanTakeProfit > 0 && dayHigh >= p.PlanTakeProfit {
			signals = append(signals, PositionExitSignal{Key: "plan_take", Label: "触达计划止盈", Detail: fmt.Sprintf("当日最高 %.2f，计划止盈 %.2f", dayHigh, p.PlanTakeProfit), Severity: model.PositionExitLevelWatch, Value: dayHigh, Threshold: p.PlanTakeProfit, Crossing: true})
		}
		for _, rule := range in.rules {
			input := positionAlertEval{AvgCost: p.BuyPrice, Price: q.Price, DayHigh: q.High, DayLow: q.Low, Peak: p.PeakPrice, PeakDate: p.PeakDate}
			if sameDayEntry {
				input.DayHigh, input.DayLow = 0, 0
			}
			hit, value, detail := evaluatePositionAlert(rule, p.Name, input)
			if !hit {
				continue
			}
			severity := model.PositionExitLevelWatch
			if rule.Kind == model.AlertKindCostDrawdown || rule.Kind == model.AlertKindPeakDrawdown {
				severity = model.PositionExitLevelReview
			}
			signals = append(signals, PositionExitSignal{Key: rule.Kind, Label: positionExitSignalLabel(rule.Kind), Detail: detail, Severity: severity, Value: value, Threshold: rule.Threshold, Crossing: true})
		}
		// AlertEvent 是已经发生且不可被规则后续暂停/修改抹掉的审计事实。当前规则
		// 重算未命中时，同交易日事件仍参与风险；同 kind/阈值的重算结果不重复展示。
		for _, event := range in.events {
			severity := model.PositionExitLevelWatch
			if event.Kind == model.AlertKindCostDrawdown || event.Kind == model.AlertKindPeakDrawdown {
				severity = model.PositionExitLevelReview
			} else if event.Kind != model.AlertKindCostGain {
				continue
			}
			threshold := 0.0
			if eventContext, ok := parseAlertEventContext(event.ContextVersion, event.ContextJSON); ok {
				if eventContext.Trigger.Threshold != nil {
					threshold = *eventContext.Trigger.Threshold
				} else if eventContext.Rule.Threshold != nil {
					threshold = *eventContext.Rule.Threshold
				}
			}
			if hasPositionExitSignal(signals, event.Kind, threshold) {
				continue
			}
			signals = append(signals, PositionExitSignal{
				Key: event.Kind, Label: positionExitSignalLabel(event.Kind), Detail: event.Message,
				Severity: severity, Threshold: threshold, Crossing: true,
			})
		}
		for _, review := range in.reviews {
			reviewIDs = appendUniqueInt64(reviewIDs, review.ID)
			if review.Trigger == model.SellReviewExDiv {
				evidence = append(evidence, "除权除息只改变账面口径，不单独构成卖出风险："+review.Title)
				continue
			}
			severity := model.PositionExitLevelWatch
			if review.Severity == model.SellReviewSeverityHigh {
				severity = model.PositionExitLevelReview
			}
			signals = append(signals, PositionExitSignal{Key: "sell_review:" + review.Trigger, Label: review.Title, Detail: review.Detail, Severity: severity})
		}
	}

	row.Trend = "unknown"
	if technicalReady {
		closes := make([]float64, 0, len(bars))
		for _, b := range bars {
			closes = append(closes, b.Close)
		}
		currentPrice := q.Price
		var prevCloses []float64
		var prevClose float64
		if in.session == model.PositionExitSessionClose && bars[len(bars)-1].TradeDate == tradeDate {
			currentPrice = bars[len(bars)-1].Close
			prevCloses = closes[:len(closes)-1]
			prevClose = bars[len(bars)-2].Close
		} else {
			prevCloses = closes
			prevClose = bars[len(bars)-1].Close
		}
		row.MA20, _ = movingAverage(closes, params.MA20Period)
		row.MA60, _ = movingAverage(closes, params.MA60Period)
		prevMA20, ok20 := movingAverage(prevCloses, params.MA20Period)
		prevMA60, ok60 := movingAverage(prevCloses, params.MA60Period)
		ma20Cross := ok20 && prevClose >= prevMA20 && currentPrice < row.MA20
		ma60Cross := ok60 && prevClose >= prevMA60 && currentPrice < row.MA60
		if ma20Cross {
			signals = append(signals, PositionExitSignal{Key: "ma20_break", Label: "刚跌破 MA20", Detail: fmt.Sprintf("现价 %.2f，MA20 %.2f", currentPrice, row.MA20), Severity: model.PositionExitLevelWatch, Value: currentPrice, Threshold: row.MA20, Crossing: true})
		}
		if ma60Cross {
			signals = append(signals, PositionExitSignal{Key: "ma60_break", Label: "刚跌破 MA60", Detail: fmt.Sprintf("现价 %.2f，MA60 %.2f", currentPrice, row.MA60), Severity: model.PositionExitLevelReview, Value: currentPrice, Threshold: row.MA60, Crossing: true})
		}
		row.ATR14, _ = positionExitATR14(bars, params.ATRPeriod)
		if p.PeakPrice > 0 && row.ATR14 > 0 {
			row.ATRLine = round4(p.PeakPrice - params.ATRMultiplier*row.ATR14)
			// 保护线随峰值上移：peak 刷新后线可能直接越过前收盘，纯「前收盘在线上→
			// 现价在线下」判定会在最需要保护的 V 型反转日恒 false 且此后永不触发。
			// 有上一条评估事实时用状态迁移（上次不在线下→本次在线下）判「刚跌破」；
			// 无历史时退回前收盘判定。持续在线下不重复成信号（长期在线下不天天报）。
			belowNow := currentPrice < row.ATRLine
			atrBreak := belowNow && !in.prevBelowATR
			if !in.hasPrevAssessment {
				atrBreak = belowNow && prevClose >= row.ATRLine
			}
			if atrBreak {
				signals = append(signals, PositionExitSignal{Key: "atr14_break", Label: "跌破 ATR14 保护线", Detail: fmt.Sprintf("现价 %.2f，保护线 %.2f（峰值 - %.1f×ATR14）", currentPrice, row.ATRLine, params.ATRMultiplier), Severity: model.PositionExitLevelReview, Value: currentPrice, Threshold: row.ATRLine, Crossing: true})
			}
		}
		row.Trend = "intact"
		if currentPrice < row.MA20 {
			row.Trend = "weak"
		}
		if currentPrice < row.MA60 || row.MA20 < row.MA60 {
			row.Trend = "broken"
		}
	}

	if quoteOK && (len(signals) > 0 || len(gaps) == 0) {
		row.Level, row.PrimarySignal = positionExitLevel(signals, row.Trend)
		row.DataStatus = model.PositionExitDataReady
		if len(gaps) > 0 {
			row.DataStatus = model.PositionExitDataPartial
		}
		row.ShouldTodo = row.Level == model.PositionExitLevelReview || row.Level == model.PositionExitLevelUrgent
		for _, signal := range signals {
			evidence = append(evidence, signal.Label+"："+signal.Detail)
			if signal.Key == row.PrimarySignal {
				row.PrimaryReason = signal.Label + "：" + signal.Detail
			}
		}
		if row.PrimaryReason == "" {
			row.PrimaryReason = "当前没有新触发的卖出风险事实"
		}
		row.NextAction = positionExitNextAction(row.Level)
	} else {
		row.PrimarySignal = "data_gap"
		row.PrimaryReason = strings.Join(uniqueStrings(gaps), "；")
		if quoteOK {
			row.DataStatus = model.PositionExitDataPartial
			row.NextAction = "当前可用事实未触发中高风险；补齐缺失数据后再完成判断。"
		} else {
			row.NextAction = "补齐 fresh 行情后再判断；当前不要据此执行卖出。"
		}
	}

	row.SignalsJSON = mustPositionExitJSON(signals)
	row.EvidenceJSON = mustPositionExitJSON(uniqueStrings(evidence))
	row.DataGapsJSON = mustPositionExitJSON(uniqueStrings(gaps))
	row.AlertEventIDsJSON = mustPositionExitJSON(alertIDs)
	row.SellReviewIDsJSON = mustPositionExitJSON(reviewIDs)
	factTradeDate := row.TradeDate
	if row.Level == model.PositionExitLevelNormal && len(signals) == 0 || row.Level == model.PositionExitLevelUnknown {
		factTradeDate = ""
	}
	fact := struct {
		Level, Primary, TradeDate, DataStatus, ParamsHash string
		Signals                                           []positionExitFactSignal
		AlertIDs, ReviewIDs                               []int64
		Gaps                                              []string
	}{row.Level, row.PrimarySignal, factTradeDate, row.DataStatus, row.ParamsHash, positionExitFactSignals(signals), alertIDs, reviewIDs, uniqueStrings(gaps)}
	row.FactHash = stablePositionExitHash(fact)
	return row
}

type positionExitFactSignal struct {
	Key       string  `json:"key"`
	Severity  string  `json:"severity"`
	Threshold float64 `json:"threshold,omitempty"`
	Crossing  bool    `json:"crossing,omitempty"`
}

func positionExitFactSignals(signals []PositionExitSignal) []positionExitFactSignal {
	out := make([]positionExitFactSignal, 0, len(signals))
	for _, signal := range signals {
		out = append(out, positionExitFactSignal{Key: signal.Key, Severity: signal.Severity, Threshold: signal.Threshold, Crossing: signal.Crossing})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func hasPositionExitSignal(signals []PositionExitSignal, key string, threshold float64) bool {
	for _, signal := range signals {
		if signal.Key == key && (threshold <= 0 || signal.Threshold == threshold) {
			return true
		}
	}
	return false
}

func positionExitLevel(signals []PositionExitSignal, trend string) (string, string) {
	byKey := map[string]PositionExitSignal{}
	for _, s := range signals {
		byKey[s.Key] = s
	}
	primaryOrder := []string{"plan_stop", "ma60_break", "atr14_break", model.AlertKindCostDrawdown, model.AlertKindPeakDrawdown, "ma20_break", "plan_take", model.AlertKindCostGain}
	primary := ""
	for _, key := range primaryOrder {
		if _, ok := byKey[key]; ok {
			primary = key
			break
		}
	}
	for _, s := range signals {
		if primary == "" || strings.HasPrefix(s.Key, "sell_review:") && s.Severity == model.PositionExitLevelReview {
			primary = s.Key
		}
	}
	if _, ok := byKey["plan_stop"]; ok {
		return model.PositionExitLevelUrgent, "plan_stop"
	}
	_, ma60 := byKey["ma60_break"]
	_, atr := byKey["atr14_break"]
	structural := ma60 || atr
	strongStructural := ma60 && atr
	drawdown := false
	if _, ok := byKey[model.AlertKindCostDrawdown]; ok {
		drawdown = true
	}
	if _, ok := byKey[model.AlertKindPeakDrawdown]; ok {
		drawdown = true
	}
	highReview, medReview := false, false
	for _, s := range signals {
		if strings.HasPrefix(s.Key, "sell_review:") {
			if s.Severity == model.PositionExitLevelReview {
				highReview = true
			} else {
				medReview = true
			}
		}
	}
	_, ma20 := byKey["ma20_break"]
	if strongStructural || drawdown && structural || highReview && (structural || drawdown || ma20) {
		return model.PositionExitLevelUrgent, primary
	}
	if ma60 || atr || drawdown || highReview || medReview && ma20 {
		return model.PositionExitLevelReview, primary
	}
	_, planTake := byKey["plan_take"]
	_, costGain := byKey[model.AlertKindCostGain]
	if (planTake || costGain) && trend == "broken" {
		return model.PositionExitLevelReview, primary
	}
	if _, ok := byKey["ma20_break"]; ok || planTake || costGain || medReview {
		return model.PositionExitLevelWatch, primary
	}
	return model.PositionExitLevelNormal, "normal"
}

func positionExitSignalLabel(kind string) string {
	switch kind {
	case model.AlertKindCostGain:
		return "成本浮盈达到用户阈值"
	case model.AlertKindCostDrawdown:
		return "成本回撤达到用户阈值"
	case model.AlertKindPeakDrawdown:
		return "峰值回撤达到用户阈值"
	}
	return kind
}

func positionExitNextAction(level string) string {
	switch level {
	case model.PositionExitLevelUrgent:
		return "立即核对计划与证据，优先执行既定风控；可对这一笔持仓发起 AI 并列复核。"
	case model.PositionExitLevelReview:
		return "今天完成卖出复核，确认继续持有、减仓或退出；可对这一笔持仓发起 AI 并列复核。"
	case model.PositionExitLevelWatch:
		return "保持观察并核对趋势，不主动推送，也不因单一信号直接卖出。"
	default:
		return "按原持仓计划继续跟踪。"
	}
}

func persistPositionExitAssessment(ctx context.Context, row model.PositionExitAssessment) (bool, bool, error) {
	inserted := false
	notifyNeeded := false
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var holding model.Position
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?",
			row.PositionID, row.UserID, model.PositionStatusHolding).First(&holding).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if holding.Symbol != row.Symbol || holding.Market != row.Market {
			return nil
		}
		var previous model.PositionExitAssessment
		err := tx.Where("user_id = ? AND position_id = ?", row.UserID, row.PositionID).
			Order("evaluated_at DESC, id DESC").First(&previous).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if errors.Is(err, gorm.ErrRecordNotFound) {
			notifyNeeded = true // 首次评估
		}
		if err == nil {
			if previous.Level == row.Level && previous.PrimarySignal == row.PrimarySignal && previous.FactHash == row.FactHash {
				if row.Session != model.PositionExitSessionClose || previous.Session == model.PositionExitSessionClose && previous.TradeDate == row.TradeDate {
					return nil
				}
			}
			row.PreviousID = previous.ID
			row.IsUpgrade = positionExitRank(row.Level) > positionExitRank(previous.Level)
			// 等级升级或主因变化才重新提醒；同等级同主因的逐日事实只落库不再推送。
			notifyNeeded = row.IsUpgrade || previous.PrimarySignal != row.PrimarySignal
		}
		row.EventKey = stablePositionExitHash(struct {
			PositionID                   int64
			TradeDate, Session, FactHash string
			PreviousID                   int64
		}{row.PositionID, row.TradeDate, row.Session, row.FactHash, row.PreviousID})
		result := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "event_key"}}, DoNothing: true}).Create(&row)
		if result.Error != nil {
			return result.Error
		}
		inserted = result.RowsAffected == 1
		return nil
	})
	return inserted, inserted && notifyNeeded, err
}

func positionExitRank(level string) int {
	switch level {
	case model.PositionExitLevelUrgent:
		return 4
	case model.PositionExitLevelReview:
		return 3
	case model.PositionExitLevelWatch:
		return 2
	case model.PositionExitLevelNormal:
		return 1
	default:
		return 0
	}
}

func LatestPositionExitAssessment(ctx context.Context, userID, positionID int64) (*PositionExitAssessmentView, error) {
	var row model.PositionExitAssessment
	err := common.DB.WithContext(ctx).Where("user_id = ? AND position_id = ?", userID, positionID).
		Order("evaluated_at DESC, id DESC").First(&row).Error
	if err != nil {
		return nil, err
	}
	view := decodePositionExitAssessment(row)
	return &view, nil
}

// PositionExitAssessmentByID 返回精确的历史评估事实。三个 ID 同时约束，避免仅凭
// assessment_id 读取到其他用户或同用户其他持仓的记录。
func PositionExitAssessmentByID(ctx context.Context, userID, positionID, assessmentID int64) (*PositionExitAssessmentView, error) {
	var row model.PositionExitAssessment
	if err := common.DB.WithContext(ctx).
		Where("id = ? AND user_id = ? AND position_id = ?", assessmentID, userID, positionID).
		First(&row).Error; err != nil {
		return nil, errors.New("卖出风险评估不存在")
	}
	view := decodePositionExitAssessment(row)
	return &view, nil
}

func LatestPositionExitAssessments(ctx context.Context, userID int64, positionIDs []int64) (map[int64]PositionExitAssessmentView, error) {
	out := map[int64]PositionExitAssessmentView{}
	if len(positionIDs) == 0 {
		return out, nil
	}
	var rows []model.PositionExitAssessment
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND position_id IN ?", userID, positionIDs).
		Order("position_id ASC, evaluated_at DESC, id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, row := range rows {
		if _, exists := out[row.PositionID]; exists {
			continue
		}
		out[row.PositionID] = decodePositionExitAssessment(row)
	}
	return out, nil
}

func decodePositionExitAssessment(row model.PositionExitAssessment) PositionExitAssessmentView {
	view := PositionExitAssessmentView{PositionExitAssessment: row}
	_ = json.Unmarshal([]byte(row.SignalsJSON), &view.Signals)
	_ = json.Unmarshal([]byte(row.EvidenceJSON), &view.Evidence)
	_ = json.Unmarshal([]byte(row.DataGapsJSON), &view.DataGaps)
	_ = json.Unmarshal([]byte(row.AlertEventIDsJSON), &view.AlertEventIDs)
	_ = json.Unmarshal([]byte(row.SellReviewIDsJSON), &view.SellReviewIDs)
	return view
}

func stablePositionExitHash(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustPositionExitJSON(value any) string {
	b, _ := json.Marshal(value)
	return string(b)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func appendUniqueInt64(in []int64, value int64) []int64 {
	for _, item := range in {
		if item == value {
			return in
		}
	}
	return append(in, value)
}
