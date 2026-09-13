package service

import (
	"encoding/json"
	"errors"
	"math"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
)

// S3-1 每日因子快照落库（RECOMMENDATION_ACCURACY_PLAN §5 S3-1）：宽表重建成功后
// 把当日全部因子固化进 factor_snapshot_dailies——严格 PIT，消费端（S3~S5 历史
// 回放/walk-forward）后置、落库先行（同 S0-3 宇宙快照先例：越早积累越好）。
//
// 挂点：RebuildFactorTableAsync 成功发布新表后（16:10 增量完成 / 管理端手动重建 /
// 历史初始化推进都会走它）。首写胜的不可变纪律见 model/factorsnapshot.go 注释。

// factorSnapshotVersion 因子快照版本（factorDefs 清单/口径变更时递增）。
// fv2（2026-07-29，C10+C12）：新增 div_yield 估值因子与 10 个 K 线形态布尔因子。
// **历史 fv1 行不重写**（首写胜的不可变纪律）——fv1 快照里天然没有这些键，
// 消费方按「键缺失=该日无该因子」处理，不得把缺键当成 0/false。
// fv3：剔除不复权/混源序列，记录独立质量标记；旧版本发现结果不能沿用此质量口径。
// fv4：保留价格与均线精度，避免三/四位报价的趋势和比较条件被两位舍入改变。
// fv5：无成交额保持未知，避免上游未提供时被解释成真实零成交额。
// fv6：冻结机会质量与原始技术维度；历史评估直接读取当时观测，不事后重算特征。
// fv7：完整窗口/20个收益、严格新高及顺序形态，冻结突破起点 ATR；历史行不重写。
const factorSnapshotVersion = "fv7"

// SnapshotFactorTable 把宽表 t 固化落库。已有行不可变（重建/重跑不覆盖——daily_bars
// 前复权重锚会整股重写，覆盖=把重写后的值伪装成当时快照，PIT 泄漏）；同一 trade_date
// 只**补缺失的 symbol**：分批历史初始化会让首批先落一部分快照，后续批次补齐的股票
// 若被「首写胜整日跳过」将永久缺席，形成不完整的全市场快照。返回本次实际落库行数。
//
// 落库门槛：只落 market_sync_states.init_status=done 的 symbol。首次部署当天
// SyncMarketWide 先给每股落 1 根当日 bar → rebuild → 此时全股仅 1 根短史、因子几乎全
// NaN，若当场落快照会因「同日已有行不可覆盖」把这份低质量快照永久冻结；历史初始化补齐
// 250 根后再 rebuild 也补不进去。故只固化历史已初始化完成（done）的 symbol，pending 股
// 待其 init done 后由后续 rebuild 的「补缺失 symbol」逻辑自然补上（IPO 新股 init done
// 后天然短史，属可接受的缺席）。
func SnapshotFactorTable(t *FactorTable) (int, error) {
	return snapshotFactorTableDB(common.DB, t)
}

func snapshotFactorTableDB(db *gorm.DB, t *FactorTable) (int, error) {
	if db == nil {
		return 0, errors.New("数据库不可用")
	}
	if t == nil || t.Len() == 0 || t.TradeDate == "" {
		return 0, nil
	}
	var existing []string
	if err := db.Model(&model.FactorSnapshotDaily{}).
		Where("trade_date = ?", t.TradeDate).Pluck("symbol", &existing).Error; err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(existing))
	for _, s := range existing {
		seen[s] = true
	}

	// 只固化历史初始化完成的 symbol（一次查询建 set）。
	var doneSyms []string
	if err := db.Model(&model.MarketSyncState{}).
		Where("market = ? AND init_status = ?", "cn", "done").Pluck("symbol", &doneSyms).Error; err != nil {
		return 0, err
	}
	initDone := make(map[string]bool, len(doneSyms))
	for _, s := range doneSyms {
		initDone[s] = true
	}

	rows := make([]model.FactorSnapshotDaily, 0, t.Len())
	for i := 0; i < t.Len(); i++ {
		if seen[t.Symbols[i]] {
			continue // 不可变：已有行不覆盖（值已变也不覆盖）
		}
		if !t.snapshotReady[t.Symbols[i]] || !initDone[t.Symbols[i]] || t.LastDates[i] == "" || t.LastDates[i] > t.TradeDate {
			continue // 历史未初始化完成：短史低质量因子不冻结，待 done 后由后续 rebuild 补上
		}
		vals := make(map[string]float64, len(factorDefs))
		for _, d := range factorDefs {
			v := t.cols[d.Key][i]
			if !math.IsNaN(v) && !math.IsInf(v, 0) {
				vals[d.Key] = v
			}
		}
		buf, err := json.Marshal(vals)
		if err != nil {
			continue // 单行序列化失败不阻断整日快照（float 已滤 NaN/Inf，实际不会发生）
		}
		rows = append(rows, model.FactorSnapshotDaily{
			TradeDate: t.TradeDate, Symbol: t.Symbols[i], Market: "cn",
			Name: t.Names[i], LastBarDate: t.LastDates[i],
			FactorsJSON: string(buf), FactorVersion: factorSnapshotVersion,
			DataQuality: "verified_adjustment",
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := db.CreateInBatches(rows, 500).Error; err != nil {
		return 0, err
	}
	return len(rows), nil
}

// usableFactorSnapshots 排除已标记的问题快照，以及尚未重建的新浪来源历史。
// 重建事务会先补记独立质量标记，因此清除旧日线后也不会重新信任原来的因子值。
func usableFactorSnapshots(db *gorm.DB) *gorm.DB {
	return db.Where("COALESCE(factor_snapshot_dailies.data_quality, '') <> ?", model.FactorQualityUnverifiedAdjustment).
		Where(`NOT EXISTS (SELECT 1 FROM daily_bars qv_bad
			WHERE qv_bad.market = factor_snapshot_dailies.market AND qv_bad.symbol = factor_snapshot_dailies.symbol
			AND qv_bad.source = 'sina' AND qv_bad.trade_date <= factor_snapshot_dailies.last_bar_date)`)
}

func markUnadjustedFactorSnapshotsTx(tx *gorm.DB, market, symbol string) error {
	var dates []string
	if err := tx.Model(&model.DailyBar{}).Where("market = ? AND symbol = ? AND source = ?", market, symbol, "sina").
		Order("trade_date").Limit(1).Pluck("trade_date", &dates).Error; err != nil {
		return err
	}
	if len(dates) == 0 {
		return nil
	}
	return markUnadjustedDerivedSinceTx(tx, market, symbol, dates[0])
}

// 在删除问题日线之前保留已发现的来源证据；与重锚和保留期清理共用同一事务。
func markUnadjustedDerivedSinceTx(tx *gorm.DB, market, symbol, firstBadDate string) error {
	if err := tx.Model(&model.FactorSnapshotDaily{}).Where("market = ? AND symbol = ? AND last_bar_date >= ?", market, symbol, firstBadDate).
		UpdateColumn("data_quality", model.FactorQualityUnverifiedAdjustment).Error; err != nil {
		return err
	}
	// 退出效果历史缺少完整输入窗口审计；对同标的旧结果标记为待核验，不改写收益值。
	if err := tx.Model(&model.PositionExitOutcome{}).Where("market = ? AND symbol = ?", market, symbol).
		UpdateColumn("data_quality", model.FactorQualityUnverifiedAdjustment).Error; err != nil {
		return err
	}
	return tx.Model(&model.Position{}).Where("market = ? AND symbol = ? AND peak_price > 0", market, symbol).
		Where("(COALESCE(peak_date, '') > COALESCE(peak_from, '') OR peak_backfilled = ?)", true).
		Where(`EXISTS (SELECT 1 FROM daily_bars bad WHERE bad.market = positions.market AND bad.symbol = positions.symbol
			AND bad.source = 'sina' AND bad.trade_date > COALESCE(positions.peak_from, '')
			AND (COALESCE(positions.peak_date, '') = '' OR bad.trade_date <= positions.peak_date))`).
		UpdateColumn("peak_data_quality", model.FactorQualityUnverifiedAdjustment).Error
}

// FactorSnapshotDays 已积累快照的交易日数（状态展示：S3 消费端就绪度的进度条）。
func FactorSnapshotDays() int {
	if common.DB == nil {
		return 0
	}
	var n int64
	if err := common.DB.Model(&model.FactorSnapshotDaily{}).
		Distinct("trade_date").Count(&n).Error; err != nil {
		return 0
	}
	return int(n)
}
