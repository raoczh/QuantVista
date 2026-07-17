package service

import (
	"encoding/json"
	"errors"
	"math"

	"quantvista/common"
	"quantvista/model"
)

// S3-1 每日因子快照落库（RECOMMENDATION_ACCURACY_PLAN §5 S3-1）：宽表重建成功后
// 把当日全部因子固化进 factor_snapshot_dailies——严格 PIT，消费端（S3~S5 历史
// 回放/walk-forward）后置、落库先行（同 S0-3 宇宙快照先例：越早积累越好）。
//
// 挂点：RebuildFactorTableAsync 成功发布新表后（16:10 增量完成 / 管理端手动重建 /
// 历史初始化推进都会走它）。首写胜的不可变纪律见 model/factorsnapshot.go 注释。

// factorSnapshotVersion 因子快照版本（factorDefs 清单/口径变更时递增）。
const factorSnapshotVersion = "fv1"

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
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if t == nil || t.Len() == 0 || t.TradeDate == "" {
		return 0, nil
	}
	var existing []string
	if err := common.DB.Model(&model.FactorSnapshotDaily{}).
		Where("trade_date = ?", t.TradeDate).Pluck("symbol", &existing).Error; err != nil {
		return 0, err
	}
	seen := make(map[string]bool, len(existing))
	for _, s := range existing {
		seen[s] = true
	}

	// 只固化历史初始化完成的 symbol（一次查询建 set）。
	var doneSyms []string
	if err := common.DB.Model(&model.MarketSyncState{}).
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
		if !initDone[t.Symbols[i]] {
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
		})
	}
	if len(rows) == 0 {
		return 0, nil
	}
	if err := common.DB.CreateInBatches(rows, 500).Error; err != nil {
		return 0, err
	}
	return len(rows), nil
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
