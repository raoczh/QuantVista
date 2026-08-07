package service

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"quantvista/datasource"
	"quantvista/model"
)

const (
	alertEventContextVersion  = 1
	alertEventContextMaxBytes = 4096
)

var errAlertEventContextInvalid = errors.New("提醒命中上下文无效")

// AlertEventContext 是 API 与持久化共用的白名单快照。结构只保存判定所需事实，
// 不接收任意 map，也不复制上游响应或用户请求。
type AlertEventContext struct {
	Version   int                    `json:"version"`
	Rule      AlertContextRule       `json:"rule"`
	Trigger   AlertContextTrigger    `json:"trigger"`
	Quote     *AlertContextQuote     `json:"quote,omitempty"`
	Bar       *AlertContextBar       `json:"bar,omitempty"`
	Indicator *AlertContextMetric    `json:"indicator,omitempty"`
	Position  *AlertContextPosition  `json:"position,omitempty"`
	Financial *AlertContextFinancial `json:"financial,omitempty"`
	Source    string                 `json:"source,omitempty"`
	AsOf      string                 `json:"as_of,omitempty"`
	Unknown   []string               `json:"unknown,omitempty"`
}

type AlertContextRule struct {
	Kind      string   `json:"kind"`
	Operator  string   `json:"operator,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Period    int      `json:"period,omitempty"`
}

type AlertContextTrigger struct {
	Field     string   `json:"field"`
	Value     *float64 `json:"value,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Operator  string   `json:"operator,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Reason    string   `json:"reason"`
}

type AlertContextQuote struct {
	Price     *float64 `json:"price,omitempty"`
	Open      *float64 `json:"open,omitempty"`
	High      *float64 `json:"high,omitempty"`
	Low       *float64 `json:"low,omitempty"`
	PrevClose *float64 `json:"prev_close,omitempty"`
	ChangePct *float64 `json:"change_pct,omitempty"`
	Volume    *int64   `json:"volume,omitempty"`
	Source    string   `json:"source,omitempty"`
	AsOf      string   `json:"as_of,omitempty"`
}

type AlertContextBar struct {
	TradeDate  string   `json:"trade_date,omitempty"`
	Open       *float64 `json:"open,omitempty"`
	High       *float64 `json:"high,omitempty"`
	Low        *float64 `json:"low,omitempty"`
	Close      *float64 `json:"close,omitempty"`
	Volume     *int64   `json:"volume,omitempty"`
	Source     string   `json:"source,omitempty"`
	SampleSize int      `json:"sample_size,omitempty"`
}

type AlertContextMetric struct {
	Name      string   `json:"name"`
	Value     *float64 `json:"value,omitempty"`
	Reference *float64 `json:"reference,omitempty"`
	Period    int      `json:"period,omitempty"`
	Unit      string   `json:"unit,omitempty"`
	Source    string   `json:"source,omitempty"`
	AsOf      string   `json:"as_of,omitempty"`
}

type AlertContextPosition struct {
	PositionID int64    `json:"position_id"`
	AvgCost    *float64 `json:"avg_cost,omitempty"`
	PeakPrice  *float64 `json:"peak_price,omitempty"`
	PeakDate   string   `json:"peak_date,omitempty"`
	PeakFrom   string   `json:"peak_from,omitempty"`
}

type AlertContextFinancial struct {
	FactType       string   `json:"fact_type"`
	ReportDate     string   `json:"report_date,omitempty"`
	AppointDate    string   `json:"appoint_date,omitempty"`
	NoticeDate     string   `json:"notice_date,omitempty"`
	ReportType     string   `json:"report_type,omitempty"`
	PredictType    string   `json:"predict_type,omitempty"`
	PredictFinance string   `json:"predict_finance,omitempty"`
	AmpLower       *float64 `json:"amp_lower,omitempty"`
	AmpUpper       *float64 `json:"amp_upper,omitempty"`
	Source         string   `json:"source,omitempty"`
	AsOf           string   `json:"as_of,omitempty"`
}

// AlertEventView 永不返回 ContextJSON；旧行、损坏行和未知版本显式标记不可用。
type AlertEventView struct {
	model.AlertEvent
	Context          *AlertEventContext `json:"context,omitempty"`
	ContextAvailable bool               `json:"context_available"`
	DeepLink         string             `json:"deep_link"`
}

func alertEventDeepLink(id int64) string {
	return fmt.Sprintf("/alerts?event_id=%d", id)
}

func alertFloat(value float64) *float64 {
	v := round4(value)
	return &v
}

func alertInt64(value int64) *int64 {
	v := value
	return &v
}

func safeAlertContextText(value string, max int) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	lower := strings.ToLower(value)
	compact := strings.NewReplacer("_", "", "-", "", " ", "").Replace(lower)
	for _, marker := range []string{"authorization", "bearer ", "cookie", "sk-", "apikey", "accesstoken", "refreshtoken", "secret"} {
		if strings.Contains(lower, marker) || strings.Contains(compact, strings.ReplaceAll(marker, " ", "")) {
			return "unknown"
		}
	}
	return truncateRunes(value, max)
}

func alertContextTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.In(time.Local).Format(time.RFC3339)
}

func newAlertEventContext(rule model.AlertRule, field string, value, threshold *float64, unit, reason string) *AlertEventContext {
	var configured *float64
	switch rule.Kind {
	case model.AlertKindPrice, model.AlertKindPctChange, model.AlertKindVolumeSurge, model.AlertKindAmplitude,
		model.AlertKindEarnDate, model.AlertKindCostGain, model.AlertKindCostDrawdown, model.AlertKindPeakDrawdown:
		configured = alertFloat(rule.Threshold)
	}
	return &AlertEventContext{
		Version: alertEventContextVersion,
		Rule: AlertContextRule{
			Kind: safeAlertContextText(rule.Kind, 16), Operator: safeAlertContextText(rule.Op, 8),
			Threshold: configured, Period: rule.Period,
		},
		Trigger: AlertContextTrigger{
			Field: safeAlertContextText(field, 48), Value: value, Threshold: threshold,
			Operator: safeAlertContextText(rule.Op, 8), Unit: safeAlertContextText(unit, 16),
			Reason: safeAlertContextText(reason, 256),
		},
	}
}

func quoteAlertContext(q *datasource.Quote) (*AlertContextQuote, []string) {
	if q == nil {
		return nil, []string{"quote"}
	}
	out := &AlertContextQuote{
		Price: alertFloat(q.Price), ChangePct: alertFloat(q.ChangePct),
		Source: safeAlertContextText(q.Source, 32), AsOf: alertContextTime(q.DataTime),
	}
	if q.Open > 0 {
		out.Open = alertFloat(q.Open)
	}
	if q.High > 0 {
		out.High = alertFloat(q.High)
	}
	if q.Low > 0 {
		out.Low = alertFloat(q.Low)
	}
	if q.PrevClose > 0 {
		out.PrevClose = alertFloat(q.PrevClose)
	}
	if q.Volume > 0 {
		out.Volume = alertInt64(q.Volume)
	}
	unknown := make([]string, 0, 2)
	if out.Source == "" {
		unknown = append(unknown, "quote.source")
	}
	if out.AsOf == "" {
		unknown = append(unknown, "quote.as_of")
	}
	return out, unknown
}

func latestAlertBar(bars []datasource.Bar) *AlertContextBar {
	if len(bars) == 0 {
		return nil
	}
	b := bars[len(bars)-1]
	out := &AlertContextBar{
		TradeDate: safeAlertContextText(b.TradeDate, 10), Source: safeAlertContextText(b.Source, 32),
		SampleSize: len(bars), Open: alertFloat(b.Open), High: alertFloat(b.High),
		Low: alertFloat(b.Low), Close: alertFloat(b.Close),
	}
	if b.Volume > 0 {
		out.Volume = alertInt64(b.Volume)
	}
	return out
}

func alertIndicatorBars(kind string, bars []datasource.Bar) []datasource.Bar {
	if kind != model.AlertKindBreakout && kind != model.AlertKindVolumeSurge {
		return bars
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	out := make([]datasource.Bar, 0, len(bars))
	for _, bar := range bars {
		if bar.TradeDate != today {
			out = append(out, bar)
		}
	}
	return out
}

func buildMarketAlertContext(rule model.AlertRule, in alertEval, q *datasource.Quote, bars []datasource.Bar,
	metricSource, metricAsOf string, reason string) *AlertEventContext {
	field, unit := "quote.price", "元"
	value := in.Price
	threshold := rule.Threshold
	var metric *AlertContextMetric

	switch rule.Kind {
	case model.AlertKindPrice:
		if rule.Op == model.AlertOpGTE {
			field, value = "quote.high", in.DayHigh
			if value <= 0 {
				field, value = "quote.price", in.Price
			}
		} else {
			field, value = "quote.low", in.DayLow
			if value <= 0 {
				field, value = "quote.price", in.Price
			}
		}
	case model.AlertKindPctChange:
		field, unit, value = "quote.change_pct", "%", in.ChangePct
	case model.AlertKindMA:
		ma, ok := movingAverage(in.Closes, rule.Period)
		if ok {
			threshold = ma
			metric = &AlertContextMetric{Name: "moving_average", Value: alertFloat(ma), Period: rule.Period, Unit: "元"}
		}
	case model.AlertKindBreakout:
		if rule.Op == model.AlertOpGTE {
			field, value = "quote.high", in.DayHigh
			if value <= 0 {
				field, value = "quote.price", in.Price
			}
			if ref, ok := windowMax(in.Highs, rule.Period); ok {
				threshold = ref
				metric = &AlertContextMetric{Name: "previous_high", Value: alertFloat(ref), Period: rule.Period, Unit: "元"}
			}
		} else {
			field, value = "quote.low", in.DayLow
			if value <= 0 {
				field, value = "quote.price", in.Price
			}
			if ref, ok := windowMin(in.Lows, rule.Period); ok {
				threshold = ref
				metric = &AlertContextMetric{Name: "previous_low", Value: alertFloat(ref), Period: rule.Period, Unit: "元"}
			}
		}
	case model.AlertKindVolumeSurge:
		field, unit = "indicator.volume_ratio", "倍"
		if avg, ok := volumeAverage(in.Volumes, volumeAvgWindow); ok {
			value = round2(float64(in.DayVolume) / avg)
			metric = &AlertContextMetric{
				Name: "volume_ratio", Value: alertFloat(value), Reference: alertFloat(avg),
				Period: volumeAvgWindow, Unit: "倍",
			}
		}
	case model.AlertKindAmplitude:
		field, unit, value = "indicator.amplitude", "%", in.Amplitude
		metric = &AlertContextMetric{Name: "amplitude", Value: alertFloat(value), Unit: "%"}
	}

	ctx := newAlertEventContext(rule, field, alertFloat(value), alertFloat(threshold), unit, reason)
	ctx.Quote, ctx.Unknown = quoteAlertContext(q)
	ctx.Bar = latestAlertBar(alertIndicatorBars(rule.Kind, bars))
	if ctx.Bar != nil {
		switch rule.Kind {
		case model.AlertKindMA, model.AlertKindBreakout:
			ctx.Bar.SampleSize = rule.Period
		case model.AlertKindVolumeSurge:
			ctx.Bar.SampleSize = volumeAvgWindow
		}
	}
	ctx.Indicator = metric
	ctx.Source = safeAlertContextText(metricSource, 32)
	ctx.AsOf = safeAlertContextText(metricAsOf, 40)
	if ctx.Source == "" && ctx.Quote != nil {
		ctx.Source = ctx.Quote.Source
	}
	if ctx.AsOf == "" && ctx.Quote != nil {
		ctx.AsOf = ctx.Quote.AsOf
	}
	if metric != nil {
		metric.Source, metric.AsOf = ctx.Source, ctx.AsOf
		switch rule.Kind {
		case model.AlertKindMA, model.AlertKindBreakout, model.AlertKindVolumeSurge:
			if ctx.Bar != nil {
				metric.Source = ctx.Bar.Source
				metric.AsOf = ctx.Bar.TradeDate
				if metric.Source == "" {
					ctx.Unknown = append(ctx.Unknown, "indicator.source")
				}
				if metric.AsOf == "" {
					ctx.Unknown = append(ctx.Unknown, "indicator.as_of")
				}
			}
		}
	}
	return ctx
}

func buildPositionAlertContext(rule model.AlertRule, in positionAlertEval, q *datasource.Quote,
	position model.Position, value float64, reason, tradeDate string) *AlertEventContext {
	field := "position.cost_gain_pct"
	switch rule.Kind {
	case model.AlertKindCostDrawdown:
		field = "position.cost_drawdown_pct"
	case model.AlertKindPeakDrawdown:
		field = "position.peak_drawdown_pct"
	}
	ctx := newAlertEventContext(rule, field, alertFloat(value), alertFloat(rule.Threshold), "%", reason)
	ctx.Quote, ctx.Unknown = quoteAlertContext(q)
	ctx.Position = &AlertContextPosition{
		PositionID: position.ID, AvgCost: alertFloat(in.AvgCost),
		PeakDate: safeAlertContextText(in.PeakDate, 10), PeakFrom: safeAlertContextText(position.PeakFrom, 10),
	}
	peak := in.Peak
	if rule.Kind == model.AlertKindPeakDrawdown && in.DayHigh > peak {
		peak = in.DayHigh
	}
	if peak > 0 {
		ctx.Position.PeakPrice = alertFloat(peak)
	}
	if ctx.Quote != nil {
		ctx.Source, ctx.AsOf = ctx.Quote.Source, ctx.Quote.AsOf
	}
	if ctx.AsOf == "" {
		ctx.AsOf = safeAlertContextText(tradeDate, 10)
	}
	return ctx
}

func buildEarnDateAlertContext(rule model.AlertRule, sched model.DisclosureSchedule, value float64,
	reason string) *AlertEventContext {
	ctx := newAlertEventContext(rule, "financial.days_to_disclosure", alertFloat(value), alertFloat(rule.Threshold), "天", reason)
	asOf := alertContextTime(sched.UpdatedAt)
	ctx.Financial = &AlertContextFinancial{
		FactType: "disclosure_schedule", ReportDate: safeAlertContextText(sched.ReportDate, 10),
		AppointDate: safeAlertContextText(sched.AppointDate, 10), ReportType: safeAlertContextText(sched.ReportTypeName, 32),
		Source: "finance_cache", AsOf: asOf,
	}
	ctx.Source, ctx.AsOf = ctx.Financial.Source, asOf
	if asOf == "" {
		ctx.Unknown = []string{"financial.as_of"}
	}
	return ctx
}

func buildEarnForecastAlertContext(rule model.AlertRule, forecast model.EarningsForecast, value float64,
	reason string) *AlertEventContext {
	field, unit := "financial.notice_date", ""
	var triggerValue *float64
	if forecast.AmpLower != 0 || forecast.AmpUpper != 0 {
		field, unit, triggerValue = "financial.forecast_amp_lower", "%", alertFloat(value)
	}
	ctx := newAlertEventContext(rule, field, triggerValue, nil, unit, reason)
	ctx.Trigger.Operator = "new_fact"
	asOf := alertContextTime(forecast.UpdatedAt)
	ctx.Financial = &AlertContextFinancial{
		FactType: "earnings_forecast", ReportDate: safeAlertContextText(forecast.ReportDate, 10),
		NoticeDate: safeAlertContextText(forecast.NoticeDate, 10), PredictType: safeAlertContextText(forecast.PredictType, 16),
		PredictFinance: safeAlertContextText(forecast.PredictFinance, 32), Source: "finance_cache", AsOf: asOf,
	}
	if forecast.AmpLower != 0 || forecast.AmpUpper != 0 {
		ctx.Financial.AmpLower = alertFloat(forecast.AmpLower)
		ctx.Financial.AmpUpper = alertFloat(forecast.AmpUpper)
	}
	ctx.Source, ctx.AsOf = ctx.Financial.Source, asOf
	if asOf == "" {
		ctx.Unknown = []string{"financial.as_of"}
	}
	return ctx
}

func marshalAlertEventContext(ctx *AlertEventContext) (int, string, error) {
	if ctx == nil {
		return 0, "", nil
	}
	if ctx.Version != alertEventContextVersion || ctx.Trigger.Field == "" || ctx.Trigger.Reason == "" {
		return 0, "", errAlertEventContextInvalid
	}
	if len(ctx.Unknown) > 8 {
		ctx.Unknown = ctx.Unknown[:8]
	}
	raw, err := json.Marshal(ctx)
	if err != nil {
		return 0, "", err
	}
	lower := strings.ToLower(string(raw))
	compact := strings.NewReplacer("_", "", "-", "", " ", "").Replace(lower)
	for _, marker := range []string{"authorization", "bearer ", "cookie", "sk-", "apikey", "accesstoken", "refreshtoken", "secret"} {
		if strings.Contains(lower, marker) || strings.Contains(compact, strings.NewReplacer("_", "", "-", "", " ", "").Replace(marker)) {
			return 0, "", fmt.Errorf("%w: 包含敏感字段", errAlertEventContextInvalid)
		}
	}
	if len(raw) > alertEventContextMaxBytes {
		return 0, "", fmt.Errorf("%w: 超过 %d 字节", errAlertEventContextInvalid, alertEventContextMaxBytes)
	}
	return alertEventContextVersion, string(raw), nil
}

func parseAlertEventContext(version int, raw string) (*AlertEventContext, bool) {
	if version != alertEventContextVersion || raw == "" || len(raw) > alertEventContextMaxBytes {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	var ctx AlertEventContext
	if err := decoder.Decode(&ctx); err != nil || ctx.Version != version || ctx.Trigger.Field == "" || ctx.Trigger.Reason == "" {
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return &ctx, true
}

func toAlertEventView(event model.AlertEvent) AlertEventView {
	ctx, ok := parseAlertEventContext(event.ContextVersion, event.ContextJSON)
	event.ContextJSON = ""
	return AlertEventView{
		AlertEvent: event, Context: ctx, ContextAvailable: ok,
		DeepLink: alertEventDeepLink(event.ID),
	}
}
