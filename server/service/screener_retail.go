package service

import (
	"errors"
	"fmt"
	"quantvista/model"
	"sort"
)

const retailTemplateVersion = 1

type RetailTemplateParam struct {
	Key     string  `json:"key"`
	Label   string  `json:"label"`
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Step    float64 `json:"step"`
	Unit    string  `json:"unit,omitempty"`
}

type RetailTemplateView struct {
	Key              string                `json:"key"`
	Version          int                   `json:"version"`
	Name             string                `json:"name"`
	Scenario         string                `json:"scenario"`
	Risk             string                `json:"risk"`
	DataRequirements string                `json:"data_requirements"`
	Params           []RetailTemplateParam `json:"params"`
	Conditions       []string              `json:"conditions"`
	Period           string                `json:"period"`
	RiskLevel        string                `json:"risk_level"`
}

type retailTemplate struct {
	RetailTemplateView
	build func(map[string]float64) CondNode
}

var retailTemplates = []retailTemplate{
	{
		RetailTemplateView: RetailTemplateView{
			Key: "low-price-steady", Version: retailTemplateVersion, Name: "低价稳健观察",
			Scenario:         "想从价格不高、波动较小且仍在中期趋势上的股票开始观察。",
			Risk:             "低价不等于低估，也可能来自基本面恶化；这里只控制价格和历史波动，不判断公司质量。",
			DataRequirements: "至少 60 个交易日的日线、成交额和波动率数据。",
			Period:           "mid", RiskLevel: "low",
			Params: []RetailTemplateParam{{Key: "max_price", Label: "最高股价", Default: 30, Min: 2, Max: 100, Step: 1, Unit: "元"}, {Key: "max_volatility", Label: "最大日波动", Default: 3, Min: 1, Max: 6, Step: 0.5, Unit: "%"}},
		},
		build: func(p map[string]float64) CondNode {
			return allOf(leafV("close", "<=", p["max_price"]), leafV("volatility_20", "<=", p["max_volatility"]), leafTrue("above_ma60"), leafV("amount_yi", ">=", 1))
		},
	},
	{
		RetailTemplateView: RetailTemplateView{
			Key: "pullback-watch", Version: retailTemplateVersion, Name: "回调观察",
			Scenario:         "寻找此前有一定涨幅、近期回落但仍守住 20 日均线的观察对象。",
			Risk:             "趋势可能已经结束；若继续放量跌破均线，原有回调假设不再成立。",
			DataRequirements: "至少 20 个交易日的价格、均线和成交量数据。",
			Period:           "swing", RiskLevel: "mid",
			Params: []RetailTemplateParam{{Key: "min_prior_gain", Label: "此前最小涨幅", Default: 10, Min: 3, Max: 30, Step: 1, Unit: "%"}, {Key: "max_pullback", Label: "近 5 日最大回调", Default: 8, Min: 1, Max: 15, Step: 1, Unit: "%"}},
		},
		build: func(p map[string]float64) CondNode {
			return allOf(leafV("chg_20d", ">=", p["min_prior_gain"]), leafBetween("chg_5d", -p["max_pullback"], 0), leafTrue("above_ma20"), leafV("vol_5v20", "<", 1))
		},
	},
	{
		RetailTemplateView: RetailTemplateView{
			Key: "volume-breakout", Version: retailTemplateVersion, Name: "放量突破",
			Scenario:         "观察创 20 日新高且成交量明显放大的股票，等待突破是否得到延续。",
			Risk:             "突破可能失败，且接近涨停时追入难度和回撤风险都更高。命中不是买入建议。",
			DataRequirements: "至少 20 个交易日的价格、成交量和成交额数据。",
			Period:           "short", RiskLevel: "high",
			Params: []RetailTemplateParam{{Key: "min_volume", Label: "最小放量倍数", Default: 1.5, Min: 1.1, Max: 4, Step: 0.1, Unit: "倍"}, {Key: "max_day_gain", Label: "当日最大涨幅", Default: 8, Min: 3, Max: 9.5, Step: 0.5, Unit: "%"}},
		},
		build: func(p map[string]float64) CondNode {
			return allOf(leafTrue("high_20d"), leafBetween("vol_boost", p["min_volume"], 5), leafV("chg_pct", "<=", p["max_day_gain"]), leafV("amount_yi", ">=", 2))
		},
	},
	{
		RetailTemplateView: RetailTemplateView{
			Key: "dividend-watch", Version: retailTemplateVersion, Name: "红利观察",
			Scenario:         "从已有分红方案数据中寻找股息率达到要求、同时历史波动不过高的股票。",
			Risk:             "历史或方案股息率不保证未来分红，分红可能一次性变化；缺少分红数据的股票不会命中。",
			DataRequirements: "近 800 天有效分红方案、至少 20 个交易日的波动率和成交额数据。",
			Period:           "mid", RiskLevel: "mid",
			Params: []RetailTemplateParam{{Key: "min_dividend_yield", Label: "最低股息率", Default: 2, Min: 0.5, Max: 10, Step: 0.5, Unit: "%"}, {Key: "max_volatility", Label: "最大日波动", Default: 3.5, Min: 1, Max: 6, Step: 0.5, Unit: "%"}},
		},
		build: func(p map[string]float64) CondNode {
			return allOf(leafV("div_yield", ">=", p["min_dividend_yield"]), leafV("volatility_20", "<=", p["max_volatility"]), leafV("amount_yi", ">=", 1))
		},
	},
}

func retailTemplateByKey(key string) (retailTemplate, bool) {
	for _, t := range retailTemplates {
		if t.Key == key {
			return t, true
		}
	}
	return retailTemplate{}, false
}

func resolveRetailTemplate(key string, version int, supplied map[string]float64) (*resolvedScreenerStrategy, map[string]float64, error) {
	t, ok := retailTemplateByKey(key)
	if !ok {
		return nil, nil, fmt.Errorf("未知新手模板 %q", key)
	}
	if version != 0 && version != t.Version {
		return nil, nil, fmt.Errorf("新手模板版本 %d 不可用，请刷新模板", version)
	}
	values := map[string]float64{}
	known := map[string]RetailTemplateParam{}
	for _, p := range t.Params {
		known[p.Key] = p
		values[p.Key] = p.Default
	}
	for key, value := range supplied {
		def, ok := known[key]
		if !ok {
			return nil, nil, fmt.Errorf("模板参数 %q 不存在", key)
		}
		if value < def.Min || value > def.Max {
			return nil, nil, fmt.Errorf("%s须在 %g~%g%s 之间", def.Label, def.Min, def.Max, def.Unit)
		}
		values[key] = value
	}
	tree := t.build(values)
	canonical, treeJSON, err := canonicalCondTree(&tree)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	desc := t.Scenario + "|v=" + fmt.Sprint(t.Version)
	for _, key := range keys {
		desc += fmt.Sprintf("|%s=%g", key, values[key])
	}
	hash, err := modelScreenerHash(t.Name, desc, t.Period, t.RiskLevel, string(treeJSON))
	if err != nil {
		return nil, nil, err
	}
	return &resolvedScreenerStrategy{
		Tree: canonical, Name: t.Name, Revision: t.Version, Hash: hash,
		TemplateKey: t.Key, TemplateVersion: t.Version, TemplateParams: values,
	}, values, nil
}

// 包一层便于本文件保持模板定义聚焦，同时沿用现有版本哈希契约。
func modelScreenerHash(name, desc, period, risk, tree string) (string, error) {
	if name == "" {
		return "", errors.New("模板名称为空")
	}
	return model.ScreenerStrategyContentHash(name, desc, period, risk, tree)
}
