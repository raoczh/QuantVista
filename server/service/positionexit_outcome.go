package service

import (
	"context"
	"errors"
	"sort"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// 卖出信号效果台账（问题 1 补强）：pea1 的 ATR 倍数、MA 周期与共振升级规则都是
// 保守拍定的基线，没有前向结果测量就永远无法基于证据调参。这里对每条盘后（close）
// 评估在 T+H 成熟后回填一次前向收益与最大偏移；normal 级同样回填作为对照分母。
// 纯测量、零 LLM、不改任何生产提醒行为——与推荐侧「先测量再调参」同一纪律。
const (
	exitOutcomeHorizonShort = 5
	exitOutcomeHorizonLong  = 10
	exitOutcomeBatchLimit   = 500
	exitOutcomeMinSamples   = 20
	exitOutcomeReportLimit  = 20000
)

var exitOutcomeHorizons = []int{exitOutcomeHorizonShort, exitOutcomeHorizonLong}

// BackfillPositionExitOutcomes 回填已成熟的评估前向结果。幂等：唯一键
// (assessment_id, horizon) + DoNothing；每轮至多处理 exitOutcomeBatchLimit 条
// 尚缺长窗口结果的评估（短窗口先成熟先补，长窗口补完后该行退出候选）。
func BackfillPositionExitOutcomes(ctx context.Context) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	var rows []model.PositionExitAssessment
	if err := common.DB.WithContext(ctx).
		Where("session = ? AND market = ?", model.PositionExitSessionClose, "cn").
		Where("NOT EXISTS (SELECT 1 FROM position_exit_outcomes o WHERE o.assessment_id = position_exit_assessments.id AND o.horizon = ?)",
			exitOutcomeHorizonLong).
		Where(`EXISTS (
			SELECT 1 FROM daily_bars anchor
			WHERE anchor.market = position_exit_assessments.market AND anchor.symbol = position_exit_assessments.symbol
			AND anchor.trade_date <= position_exit_assessments.trade_date AND anchor.close > 0
		)`).
		Where(`(
			NOT EXISTS (SELECT 1 FROM position_exit_outcomes short WHERE short.assessment_id = position_exit_assessments.id AND short.horizon = ?)
			AND (SELECT COUNT(*) FROM daily_bars future WHERE future.market = position_exit_assessments.market AND future.symbol = position_exit_assessments.symbol AND future.trade_date > position_exit_assessments.trade_date AND future.close > 0) >= ?
		) OR (
			SELECT COUNT(*) FROM daily_bars future WHERE future.market = position_exit_assessments.market AND future.symbol = position_exit_assessments.symbol AND future.trade_date > position_exit_assessments.trade_date AND future.close > 0
		) >= ?`, exitOutcomeHorizonShort, exitOutcomeHorizonShort, exitOutcomeHorizonLong).
		Order("trade_date ASC, id ASC").Limit(exitOutcomeBatchLimit).Find(&rows).Error; err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	minDate := rows[0].TradeDate
	symbolSet := map[string]bool{}
	for _, row := range rows {
		if row.TradeDate != "" && row.TradeDate < minDate {
			minDate = row.TradeDate
		}
		symbolSet[row.Symbol] = true
	}
	symbols := make([]string, 0, len(symbolSet))
	for symbol := range symbolSet {
		symbols = append(symbols, symbol)
	}
	var bars []model.DailyBar
	if err := common.DB.WithContext(ctx).
		Where("market = ? AND symbol IN ?", "cn", symbols).
		Where(`trade_date >= ? OR trade_date = (
			SELECT MAX(anchor.trade_date) FROM daily_bars anchor
			WHERE anchor.market = daily_bars.market AND anchor.symbol = daily_bars.symbol AND anchor.trade_date < ?
		)`, minDate, minDate).
		Order("symbol ASC, trade_date ASC").Find(&bars).Error; err != nil {
		return 0, err
	}
	barsBy := map[string][]model.DailyBar{}
	for _, b := range bars {
		if b.Close > 0 {
			barsBy[b.Symbol] = append(barsBy[b.Symbol], b)
		}
	}
	created := 0
	for _, row := range rows {
		series := barsBy[row.Symbol]
		// 锚定 T 日（或停牌时 <=T 的最近一根）：Base 与前向收盘同源于当前前复权
		// 序列，除权重锚不会造成两端口径错位。
		idx := -1
		for i := range series {
			if series[i].TradeDate <= row.TradeDate {
				idx = i
			} else {
				break
			}
		}
		if idx < 0 || series[idx].Close <= 0 {
			continue
		}
		base := series[idx].Close
		for _, horizon := range exitOutcomeHorizons {
			if idx+horizon >= len(series) {
				continue // 未成熟，等下一轮
			}
			mae, mfe := 0.0, 0.0
			for i := idx + 1; i <= idx+horizon; i++ {
				low, high := series[i].Low, series[i].High
				if low <= 0 {
					low = series[i].Close
				}
				if high <= 0 {
					high = series[i].Close
				}
				if pct := (low - base) / base * 100; pct < mae {
					mae = pct
				}
				if pct := (high - base) / base * 100; pct > mfe {
					mfe = pct
				}
			}
			outcome := model.PositionExitOutcome{
				AssessmentID: row.ID, Horizon: horizon,
				UserID: row.UserID, PositionID: row.PositionID,
				Symbol: row.Symbol, Market: row.Market, TradeDate: row.TradeDate,
				Level: row.Level, PrimarySignal: row.PrimarySignal, ParamsHash: row.ParamsHash,
				BasePrice:        round4(base),
				ForwardReturnPct: round2((series[idx+horizon].Close - base) / base * 100),
				MaePct:           round2(mae), MfePct: round2(mfe), BarsUsed: horizon,
			}
			res := common.DB.WithContext(ctx).Clauses(clause.OnConflict{
				Columns:   []clause.Column{{Name: "assessment_id"}, {Name: "horizon"}},
				DoNothing: true,
			}).Create(&outcome)
			if res.Error != nil {
				return created, res.Error
			}
			if res.RowsAffected == 1 {
				created++
			}
		}
	}
	if created > 0 {
		common.SysLog("卖出评估前向结果回填 %d 条", created)
	}
	return created, nil
}

// PositionExitOutcomeBucket 单个 (horizon, level[, primary_signal]) 分组的聚合。
type PositionExitOutcomeBucket struct {
	Horizon          int     `json:"horizon"`
	Level            string  `json:"level"`
	PrimarySignal    string  `json:"primary_signal,omitempty"`
	ParamsHash       string  `json:"params_hash"`
	Samples          int     `json:"samples"`
	AvgForwardPct    float64 `json:"avg_forward_pct"`
	MedianForwardPct float64 `json:"median_forward_pct"`
	AvgMaePct        float64 `json:"avg_mae_pct"`
	DownRatioPct     float64 `json:"down_ratio_pct"` // 前向收益 <0 的占比
	Evaluated        bool    `json:"evaluated"`      // Samples >= exitOutcomeMinSamples
}

// PositionExitOutcomeReportView 跨用户脱敏聚合（无 symbol/user 明细）。
type PositionExitOutcomeReportView struct {
	GeneratedAt string                      `json:"generated_at"`
	Total       int                         `json:"total"`
	MinSamples  int                         `json:"min_samples"`
	Levels      []PositionExitOutcomeBucket `json:"levels"`
	Signals     []PositionExitOutcomeBucket `json:"signals"`
	Notes       []string                    `json:"notes"`
}

// PositionExitOutcomeReport 聚合台账。level 层用于横比 normal（对照）与各风险级
// 的前向收益差；signal 层定位单信号预测力。样本不足的分组显式标未评估。
func PositionExitOutcomeReport() (*PositionExitOutcomeReportView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var rows []model.PositionExitOutcome
	if err := common.DB.Order("id DESC").Limit(exitOutcomeReportLimit).Find(&rows).Error; err != nil {
		return nil, err
	}
	type acc struct {
		forwards []float64
		maeSum   float64
		down     int
	}
	levelAcc := map[[3]string]*acc{}
	signalAcc := map[[4]string]*acc{}
	horizonKey := func(h int) string {
		if h == exitOutcomeHorizonShort {
			return "5"
		}
		return "10"
	}
	for _, row := range rows {
		lk := [3]string{horizonKey(row.Horizon), row.ParamsHash, row.Level}
		if levelAcc[lk] == nil {
			levelAcc[lk] = &acc{}
		}
		sk := [4]string{horizonKey(row.Horizon), row.ParamsHash, row.Level, row.PrimarySignal}
		if signalAcc[sk] == nil {
			signalAcc[sk] = &acc{}
		}
		for _, a := range []*acc{levelAcc[lk], signalAcc[sk]} {
			a.forwards = append(a.forwards, row.ForwardReturnPct)
			a.maeSum += row.MaePct
			if row.ForwardReturnPct < 0 {
				a.down++
			}
		}
	}
	build := func(horizon int, paramsHash, level, signal string, a *acc) PositionExitOutcomeBucket {
		n := len(a.forwards)
		sum := 0.0
		for _, v := range a.forwards {
			sum += v
		}
		sorted := append([]float64(nil), a.forwards...)
		sort.Float64s(sorted)
		median := 0.0
		if n > 0 {
			if n%2 == 1 {
				median = sorted[n/2]
			} else {
				median = (sorted[n/2-1] + sorted[n/2]) / 2
			}
		}
		b := PositionExitOutcomeBucket{Horizon: horizon, Level: level, PrimarySignal: signal, ParamsHash: paramsHash, Samples: n,
			Evaluated: n >= exitOutcomeMinSamples}
		if n > 0 {
			b.AvgForwardPct = round2(sum / float64(n))
			b.MedianForwardPct = round2(median)
			b.AvgMaePct = round2(a.maeSum / float64(n))
			b.DownRatioPct = round2(float64(a.down) / float64(n) * 100)
		}
		return b
	}
	parseHorizon := func(key string) int {
		if key == "5" {
			return exitOutcomeHorizonShort
		}
		return exitOutcomeHorizonLong
	}
	rep := &PositionExitOutcomeReportView{
		GeneratedAt: time.Now().Format("2006-01-02 15:04:05"), Total: len(rows),
		MinSamples: exitOutcomeMinSamples,
		Notes: []string{
			"研究口径：Base 与前向收盘同源于当前前复权日线序列，不代表真实可成交价；normal 级为对照分母",
			"不同 params_hash 独立聚合，禁止把不同参数版本的样本混为同一组",
			"样本不足分组标记未评估；在样本达标前不得据此调整 pea1 阈值",
		},
	}
	for key, a := range levelAcc {
		rep.Levels = append(rep.Levels, build(parseHorizon(key[0]), key[1], key[2], "", a))
	}
	for key, a := range signalAcc {
		rep.Signals = append(rep.Signals, build(parseHorizon(key[0]), key[1], key[2], key[3], a))
	}
	sort.Slice(rep.Levels, func(i, j int) bool {
		if rep.Levels[i].Horizon != rep.Levels[j].Horizon {
			return rep.Levels[i].Horizon < rep.Levels[j].Horizon
		}
		if rep.Levels[i].ParamsHash != rep.Levels[j].ParamsHash {
			return rep.Levels[i].ParamsHash < rep.Levels[j].ParamsHash
		}
		return positionExitRank(rep.Levels[i].Level) > positionExitRank(rep.Levels[j].Level)
	})
	sort.Slice(rep.Signals, func(i, j int) bool {
		a, b := rep.Signals[i], rep.Signals[j]
		if a.Horizon != b.Horizon {
			return a.Horizon < b.Horizon
		}
		if a.ParamsHash != b.ParamsHash {
			return a.ParamsHash < b.ParamsHash
		}
		if a.Level != b.Level {
			return positionExitRank(a.Level) > positionExitRank(b.Level)
		}
		return a.PrimarySignal < b.PrimarySignal
	})
	return rep, nil
}
