package model

import (
	"encoding/json"
	"math"
)

// 位序是缓存表契约，只能追加。Known 让全缺失的新行也有明确的可用性记录。
const (
	FinanceFieldEPS uint32 = 1 << iota
	FinanceFieldBPS
	FinanceFieldOCFPS
	FinanceFieldRevenue
	FinanceFieldRevenueYoY
	FinanceFieldNetProfit
	FinanceFieldNetProfitYoY
	FinanceFieldDeductProfit
	FinanceFieldDeductProfitYoY
	FinanceFieldROE
	FinanceFieldGrossMargin
	FinanceFieldNetMargin
	FinanceFieldDebtRatio
	FinanceFieldsKnown uint32 = 1 << 30
)

// OptionalValue 对旧缓存仅承认有限非零值；不能替旧行猜测零值原本是否缺失。
func (r FinanceIndicator) OptionalValue(field uint32, value float64) *float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil
	}
	if r.ValueMask == 0 {
		if value == 0 {
			return nil
		}
	} else if r.ValueMask&field == 0 {
		return nil
	}
	return &value
}

// 数据库保留原数值列，JSON 用 null 表达未知，避免详情页或 AI 将缺失展示为 0。
func (r FinanceIndicator) MarshalJSON() ([]byte, error) {
	type raw FinanceIndicator
	return json.Marshal(struct {
		raw
		EPS             *float64 `json:"eps"`
		BPS             *float64 `json:"bps"`
		OCFPS           *float64 `json:"ocf_ps"`
		Revenue         *float64 `json:"revenue"`
		RevenueYoY      *float64 `json:"revenue_yoy"`
		NetProfit       *float64 `json:"net_profit"`
		NetProfitYoY    *float64 `json:"net_profit_yoy"`
		DeductProfit    *float64 `json:"deduct_profit"`
		DeductProfitYoY *float64 `json:"deduct_profit_yoy"`
		ROE             *float64 `json:"roe"`
		GrossMargin     *float64 `json:"gross_margin"`
		NetMargin       *float64 `json:"net_margin"`
		DebtRatio       *float64 `json:"debt_ratio"`
	}{
		raw: raw(r),
		EPS: r.OptionalValue(FinanceFieldEPS, r.EPS), BPS: r.OptionalValue(FinanceFieldBPS, r.BPS),
		OCFPS:   r.OptionalValue(FinanceFieldOCFPS, r.OCFPS),
		Revenue: r.OptionalValue(FinanceFieldRevenue, r.Revenue), RevenueYoY: r.OptionalValue(FinanceFieldRevenueYoY, r.RevenueYoY),
		NetProfit: r.OptionalValue(FinanceFieldNetProfit, r.NetProfit), NetProfitYoY: r.OptionalValue(FinanceFieldNetProfitYoY, r.NetProfitYoY),
		DeductProfit: r.OptionalValue(FinanceFieldDeductProfit, r.DeductProfit), DeductProfitYoY: r.OptionalValue(FinanceFieldDeductProfitYoY, r.DeductProfitYoY),
		ROE: r.OptionalValue(FinanceFieldROE, r.ROE), GrossMargin: r.OptionalValue(FinanceFieldGrossMargin, r.GrossMargin),
		NetMargin: r.OptionalValue(FinanceFieldNetMargin, r.NetMargin), DebtRatio: r.OptionalValue(FinanceFieldDebtRatio, r.DebtRatio),
	})
}
