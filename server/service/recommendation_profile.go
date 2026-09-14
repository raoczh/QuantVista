package service

const recommendationProfileVersion = "sp3"

type recommendationProfile struct {
	intent  string
	profile string
}

// 每个内置形态明确声明排序侧重，适用周期只用于持有期与结果评估。
// 增加策略时必须登记；完整性测试检查与策略目录一一对应。
var builtinRecommendationProfiles = map[string]recommendationProfile{
	"vol-break-20d":        {"breakout", "momentum"},
	"shrink-pullback-ma20": {"pullback", "pullback"},
	"mild-vol-start":       {"activity", "active"},
	"bottom-vol-yang":      {"reversal", "active"},
	"yang-through-3ma":     {"breakout", "momentum"},
	"limit-up-pullback":    {"pullback", "pullback"},
	"strong-consolidation": {"consolidation", "pullback"},
	"rsi-strong-zone":      {"trend", "momentum"},
	"macd-gold-water":      {"trend", "momentum"},
	"macd-gold-under":      {"reversal", "pullback"},
	"rsi-oversold-up":      {"reversal", "pullback"},
	"boll-lower-bounce":    {"reversal", "pullback"},
	"boll-break-up":        {"breakout", "momentum"},
	"ma-converge":          {"consolidation", "pullback"},
	"new-high-250":         {"breakout", "momentum"},
	"bull-align-trend":     {"trend", "momentum"},
	"year-line-stand":      {"trend", "pullback"},
	"steady-uptrend":       {"trend", "momentum"},
	"calm-consolidation":   {"consolidation", "pullback"},
	"deep-oversold-chip":   {"reversal", "pullback"},
	"low-vol-trend":        {"quality", "pullback"},
	"ma20-cross-ma60":      {"trend", "momentum"},
	"breakout-retest":      {"pullback", "pullback"},
	"boll-squeeze-break":   {"breakout", "momentum"},
	"donchian-55":          {"breakout", "momentum"},
	"kdj-low-cross":        {"reversal", "pullback"},
	"dmi-trend-confirm":    {"trend", "momentum"},
	"nr7-breakout":         {"breakout", "momentum"},
	"rsi2-trend-reclaim":   {"pullback", "pullback"},
	"long-trend-template":  {"trend", "momentum"},
}

var retailRecommendationProfiles = map[string]recommendationProfile{
	"low-price-steady": {"quality", "pullback"},
	"pullback-watch":   {"pullback", "pullback"},
	"volume-breakout":  {"breakout", "momentum"},
	"dividend-watch":   {"value", "value"},
}

// 技术形态不会因为用户改成长线而变成价值/成长财务策略。周期只改变执行与评估窗口。
func (p recommendationProfile) scoreProfile(_ string) string {
	return p.profile
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
