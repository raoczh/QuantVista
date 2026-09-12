package service

import "quantvista/model"

const recommendationProfileVersion = "sp1"

type recommendationProfile struct {
	intent string
	short  string
	long   string
}

// 每个内置形态明确声明排序侧重，适用周期只用于持有期与结果评估。
// 增加策略时必须登记；完整性测试检查与策略目录一一对应。
var builtinRecommendationProfiles = map[string]recommendationProfile{
	"vol-break-20d":        {"breakout", "momentum", "growth"},
	"shrink-pullback-ma20": {"pullback", "pullback", "leader"},
	"mild-vol-start":       {"activity", "active", "growth"},
	"bottom-vol-yang":      {"reversal", "active", "value"},
	"yang-through-3ma":     {"breakout", "momentum", "growth"},
	"limit-up-pullback":    {"pullback", "pullback", "leader"},
	"strong-consolidation": {"consolidation", "pullback", "leader"},
	"rsi-strong-zone":      {"trend", "momentum", "growth"},
	"macd-gold-water":      {"trend", "momentum", "growth"},
	"macd-gold-under":      {"reversal", "pullback", "value"},
	"rsi-oversold-up":      {"reversal", "pullback", "value"},
	"boll-lower-bounce":    {"reversal", "pullback", "value"},
	"boll-break-up":        {"breakout", "momentum", "growth"},
	"ma-converge":          {"consolidation", "pullback", "leader"},
	"new-high-250":         {"breakout", "momentum", "growth"},
	"bull-align-trend":     {"trend", "momentum", "leader"},
	"year-line-stand":      {"trend", "pullback", "leader"},
	"steady-uptrend":       {"trend", "momentum", "growth"},
	"calm-consolidation":   {"consolidation", "pullback", "leader"},
	"deep-oversold-chip":   {"reversal", "pullback", "value"},
	"low-vol-trend":        {"quality", "pullback", "leader"},
}

var retailRecommendationProfiles = map[string]recommendationProfile{
	"low-price-steady": {"quality", "pullback", "leader"},
	"pullback-watch":   {"pullback", "pullback", "leader"},
	"volume-breakout":  {"breakout", "momentum", "growth"},
	"dividend-watch":   {"value", "value", "value"},
}

func (p recommendationProfile) scoreProfile(recType string) string {
	if recType == model.RecTypeLongTerm {
		return p.long
	}
	return p.short
}

func profileLabel(key string) string {
	switch key {
	case "momentum":
		return "趋势与突破"
	case "pullback":
		return "回踩与稳健"
	case "active":
		return "量价活跃"
	case "value":
		return "估值与盈利质量"
	case "growth":
		return "成长与趋势"
	case "leader":
		return "质量与稳定性"
	default:
		return "均衡技术评分"
	}
}

func profileIntent(key string) string {
	switch key {
	case "momentum":
		return "breakout"
	case "active":
		return "activity"
	case "leader":
		return "quality"
	case "pullback", "value", "growth":
		return key
	default:
		return "custom"
	}
}

func profileUsesFinance(key string) bool {
	return key == "value" || key == "growth" || key == "leader"
}
