package service

import (
	"context"

	"quantvista/common"
	"quantvista/model"
)

// 只推进仍存在的 pending 行，不用 Save 的 upsert 回建已删除事实，也不覆盖并发终态。
func commitLabelOutcome(ctx context.Context, label *model.RecommendationLabel) (int64, error) {
	result := common.DB.WithContext(ctx).Model(&model.RecommendationLabel{}).
		Where("id = ? AND maturity_status = ?", label.ID, model.LabelPending).
		Updates(map[string]any{
			"maturity_status": label.MaturityStatus, "skip_reason": label.SkipReason,
			"entry_date": label.EntryDate, "entry_price": label.EntryPrice,
			"exit_date": label.ExitDate, "exit_price": label.ExitPrice,
			"gross_return_pct": label.GrossReturnPct, "net_return_pct": label.NetReturnPct,
			"bench_return_pct": label.BenchReturnPct, "alpha_pct": label.AlphaPct, "has_bench": label.HasBench,
			"mfe_pct": label.MfePct, "mae_pct": label.MaePct,
			"hit_take_profit": label.HitTakeProfit, "hit_stop_loss": label.HitStopLoss,
			"forced": label.Forced, "label_version": label.LabelVersion,
		})
	return result.RowsAffected, result.Error
}
