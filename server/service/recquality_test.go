package service

import (
	"strings"
	"testing"
	"time"

	"quantvista/model"
)

// ---------- S2-3 数据质量门控（影子）：判定纯函数表驱动 ----------

// qgFullCandidate 数据齐全的候选（各场景在此基础上抠字段）。
func qgFullCandidate() candidate {
	return candidate{
		Symbol: "600100", Market: "cn", Name: "甲", Price: 10, Amount: 5e8,
		SentiNews: 3, SentiScore: 0.2,
		Factors: &candFactors{BarCount: 90, Chg5d: 2},
		Fin:     &candFin{ROE: 12},
		lastBarDate: "2026-06-10",
	}
}

// TestComputeQualityGate 按策略分场景的缺失判定与 cap 映射（qg1）。
func TestComputeQualityGate(t *testing.T) {
	today := "2026-06-12" // 距 lastBarDate 2 天（新鲜）

	t.Run("数据齐全返回 nil 不落明细", func(t *testing.T) {
		if got := computeQualityGate(model.RecTypeShortTerm, qgFullCandidate(), today); got != nil {
			t.Fatalf("齐全应 nil: %+v", got)
		}
	})

	t.Run("长线缺财务=严重 cap 40", func(t *testing.T) {
		c := qgFullCandidate()
		c.Fin = nil
		got := computeQualityGate(model.RecTypeLongTerm, c, today)
		if got == nil || got.WouldBeConfidenceCap != qgCapLongNoFin {
			t.Fatalf("长线缺 fin 应 cap %d: %+v", qgCapLongNoFin, got)
		}
		if len(got.MissingCriticalFields) != 1 || got.MissingCriticalFields[0] != "fin" {
			t.Fatalf("缺失面应只有 fin: %+v", got.MissingCriticalFields)
		}
	})

	t.Run("短线缺财务不标记", func(t *testing.T) {
		c := qgFullCandidate()
		c.Fin = nil
		if got := computeQualityGate(model.RecTypeShortTerm, c, today); got != nil {
			t.Fatalf("短线缺 fin 不严重，应 nil: %+v", got)
		}
	})

	t.Run("日线过期属通用严重", func(t *testing.T) {
		c := qgFullCandidate()
		c.lastBarDate = "2026-06-01" // 距今 11 天 > 5
		got := computeQualityGate(model.RecTypeShortTerm, c, today)
		if got == nil || got.WouldBeConfidenceCap != qgCapStaleBars || got.DataAgeDays != 11 {
			t.Fatalf("过期应 cap %d 且 data_age=11: %+v", qgCapStaleBars, got)
		}
		if got.MissingCriticalFields[0] != "daily_bars_stale" {
			t.Fatalf("缺失面应 daily_bars_stale: %+v", got.MissingCriticalFields)
		}
	})

	t.Run("日线样本不足", func(t *testing.T) {
		c := qgFullCandidate()
		c.Factors.BarCount = 30
		got := computeQualityGate(model.RecTypeShortTerm, c, today)
		if got == nil || got.WouldBeConfidenceCap != qgCapShortBars {
			t.Fatalf("样本不足应 cap %d: %+v", qgCapShortBars, got)
		}
	})

	t.Run("因子缺席=最低档", func(t *testing.T) {
		c := qgFullCandidate()
		c.Factors = nil
		got := computeQualityGate(model.RecTypeShortTerm, c, today)
		if got == nil || got.WouldBeConfidenceCap != qgCapNoFactors || got.DataAgeDays != -1 {
			t.Fatalf("无因子应 cap %d 且 data_age=-1: %+v", qgCapNoFactors, got)
		}
	})

	t.Run("多项命中取最小 cap 且缺失面全记", func(t *testing.T) {
		c := qgFullCandidate()
		c.Fin = nil                  // 长线 40
		c.Amount = 0                 // 50
		c.Factors.BarCount = 30      // 60
		got := computeQualityGate(model.RecTypeLongTerm, c, today)
		if got == nil || got.WouldBeConfidenceCap != qgCapLongNoFin {
			t.Fatalf("多项命中应取最小 cap %d: %+v", qgCapLongNoFin, got)
		}
		if len(got.MissingCriticalFields) != 3 {
			t.Fatalf("缺失面应 3 项（amount/daily_bars_short/fin）: %+v", got.MissingCriticalFields)
		}
		if got.Version != qualityGateVersion {
			t.Fatalf("应带版本 %s: %+v", qualityGateVersion, got)
		}
	})

	t.Run("情绪缺失≠情绪中性：独立标记不降 cap", func(t *testing.T) {
		c := qgFullCandidate()
		c.SentiNews = 0 // 当日无相关新闻=数据缺失
		got := computeQualityGate(model.RecTypeShortTerm, c, today)
		if got == nil || !got.SentiMissing {
			t.Fatalf("情绪缺失应标记 senti_missing: %+v", got)
		}
		if got.WouldBeConfidenceCap != 100 || len(got.MissingCriticalFields) != 0 {
			t.Fatalf("情绪缺失不得降 cap 或进关键缺失面: %+v", got)
		}
		// 有新闻且情绪分为 0（真中性）不标缺失。
		c2 := qgFullCandidate()
		c2.SentiScore = 0
		if got := computeQualityGate(model.RecTypeShortTerm, c2, today); got != nil {
			t.Fatalf("情绪中性（有新闻）不是缺失: %+v", got)
		}
	})
}

// TestApplyQualityGateShadow 影子接线：明细写入但不改写 confidence/action；
// 仅 would-be cap 低于该条置信度时产出 quality_shadow 事件。
func TestApplyQualityGateShadow(t *testing.T) {
	fresh := time.Now().AddDate(0, 0, -2).Format("2006-01-02") // 接线函数内部取今天，日线须动态新鲜
	full := qgFullCandidate()
	full.lastBarDate = fresh
	noFin := qgFullCandidate()
	noFin.Symbol = "600200"
	noFin.Fin = nil
	noFin.lastBarDate = fresh
	lowConf := qgFullCandidate()
	lowConf.Symbol = "600300"
	lowConf.Fin = nil
	lowConf.lastBarDate = fresh
	pool := map[string]candidate{"600100": full, "600200": noFin, "600300": lowConf}

	picks := []recPick{
		{Symbol: "600100", Action: model.RecActionBuy, Confidence: 80},  // 齐全：无明细无事件
		{Symbol: "600200", Action: model.RecActionBuy, Confidence: 80},  // cap 40 < 80：明细+事件
		{Symbol: "600300", Action: model.RecActionWatch, Confidence: 30}, // cap 40 ≥ 30：明细、无事件
	}
	gates := applyQualityGateShadow(model.RecTypeLongTerm, picks, pool)

	if picks[0].QualityGate != nil {
		t.Fatalf("数据齐全不应落明细: %+v", picks[0].QualityGate)
	}
	if picks[1].QualityGate == nil || picks[1].QualityGate.WouldBeConfidenceCap != qgCapLongNoFin {
		t.Fatalf("缺财务应落明细 cap %d: %+v", qgCapLongNoFin, picks[1].QualityGate)
	}
	// 影子铁律：不实际封顶、不改写动作。
	if picks[1].Confidence != 80 || picks[1].Action != model.RecActionBuy {
		t.Fatalf("影子模式不得改写 confidence/action: %+v", picks[1])
	}
	if len(gates) != 1 || gates[0].Symbol != "600200" || gates[0].GateType != model.GateQualityShadow {
		t.Fatalf("应只有 600200 产出 quality_shadow 事件: %+v", gates)
	}
	if gates[0].GateVersion != qualityGateVersion || gates[0].WouldBeAction != "" {
		t.Fatalf("质量门控事件应带 qg 版本且不改动作: %+v", gates[0])
	}
	if !strings.Contains(gates[0].Reason, "80 将封顶至 40") {
		t.Fatalf("事件 reason 应说明 would-be 封顶: %s", gates[0].Reason)
	}
	if picks[2].QualityGate == nil {
		t.Fatalf("cap 未构成约束也应落明细（缺失面透明）: %+v", picks[2])
	}
}
