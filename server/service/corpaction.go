package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// 公司行动与打新日历同步（B8/B9 数据层）：每日 19:25 增量拉取四张东财 RPT_* 报表并幂等入库。
//
// 时点选择：19:25 错峰在 18:45 龙虎榜、19:05 财报+公告之后，19:35 盘后守护轮之前——
// **守护轮要消费本轮落库的解禁/除权数据，顺序不能倒**。
//
// 窗口：分红拉近 90 天**公告**窗口（覆盖预案→实施的全过程，除权日可能落在未来）；
// 解禁与申购拉未来 60 天（日历型数据，过去的对用户没有决策价值）。
//
// 失败纪律：四类各自独立，一类失败不影响其它三类落库；只有**四类全成功**才推进
// 「今日已同步」游标——部分失败时当天重启/下轮补跑守卫自然放行重试。
// 网络类失败永远只是「这次没查到」，绝不写成「今天没有公司行动」。

const (
	corpActionSyncHour = 19
	corpActionSyncMin  = 25

	corpActionBonusBackDays = 90 // 分红公告回看窗口（自然日）
	corpActionFutureDays    = 60 // 解禁/申购前瞻窗口（自然日）

	optCorpActionSyncDay = "corpaction_last_sync_day" // 「今日已同步」游标
)

// CorpActionService 公司行动数据同步与查询。
// 四张报表都只在 EastMoneyAdapter 上（非 Manager 能力路由），持自有实例——
// 与 FinanceService 同款先例。
type CorpActionService struct {
	em *datasource.EastMoneyAdapter
}

func NewCorpActionService() *CorpActionService {
	return &CorpActionService{em: datasource.NewEastMoneyAdapter()}
}

// emAdapter 取东财适配器（取不到即无法同步，如实报错不静默跳过）。
func (s *CorpActionService) emAdapter() (*datasource.EastMoneyAdapter, error) {
	if s.em == nil {
		return nil, errors.New("东财数据源不可用")
	}
	return s.em, nil
}

// SyncCorporateActions 同步分红送转（近 N 天公告窗口）。返回入库行数。
func (s *CorpActionService) SyncCorporateActions(ctx context.Context) (int, error) {
	em, err := s.emAdapter()
	if err != nil {
		return 0, err
	}
	since := time.Now().AddDate(0, 0, -corpActionBonusBackDays).Format("2006-01-02")
	rows, err := em.GetCorpActions(ctx, since)
	if err != nil {
		if errors.Is(err, datasource.ErrNoData) {
			return 0, nil // 窗口内确实无新方案：正常业务态
		}
		return 0, err
	}
	recs := make([]model.CorporateAction, 0, len(rows))
	for _, r := range rows {
		recs = append(recs, model.CorporateAction{
			Symbol: r.Symbol, Market: r.Market, Name: r.Name,
			ReportDate: r.ReportDate, ExDate: r.ExDate,
			RecordDate: r.RecordDate, NoticeDate: r.NoticeDate,
			BonusRatio: r.BonusRatio, TransferRatio: r.TransferRatio,
			DividendPretax: r.DividendPretax, DividendYield: r.DividendYield,
			Progress: r.Progress, PlanProfile: truncateRunes(r.PlanProfile, 250),
		})
	}
	if len(recs) == 0 {
		return 0, nil
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "report_date"}, {Name: "ex_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name", "record_date", "notice_date", "bonus_ratio", "transfer_ratio",
			"dividend_pretax", "dividend_yield", "progress", "plan_profile", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error; err != nil {
		return 0, err
	}
	return len(recs), nil
}

// SyncRestrictedReleases 同步未来 N 天的限售解禁。返回入库行数。
func (s *CorpActionService) SyncRestrictedReleases(ctx context.Context) (int, error) {
	em, err := s.emAdapter()
	if err != nil {
		return 0, err
	}
	now := time.Now()
	from := now.Format("2006-01-02")
	to := now.AddDate(0, 0, corpActionFutureDays).Format("2006-01-02")
	rows, err := em.GetLiftReleases(ctx, from, to)
	if err != nil {
		if errors.Is(err, datasource.ErrNoData) {
			return 0, nil
		}
		return 0, err
	}
	recs := make([]model.RestrictedRelease, 0, len(rows))
	for _, r := range rows {
		recs = append(recs, model.RestrictedRelease{
			Symbol: r.Symbol, Market: "cn", Name: r.Name,
			FreeDate: r.FreeDate, FreeType: r.FreeType,
			FreeShares: r.FreeShares, LiftMarketCap: r.LiftMarketCap,
			FreeRatio: r.FreeRatio, TotalRatio: r.TotalRatio,
		})
	}
	if len(recs) == 0 {
		return 0, nil
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "free_date"}, {Name: "free_type"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name", "free_shares", "lift_market_cap", "free_ratio", "total_ratio", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error; err != nil {
		return 0, err
	}
	return len(recs), nil
}

// SyncIpoSubscriptions 同步未来 N 天的新股 + 可转债申购。返回入库行数。
// 两类各自独立：新股失败不影响可转债落库（反之亦然），错误合并返回。
func (s *CorpActionService) SyncIpoSubscriptions(ctx context.Context) (int, error) {
	em, err := s.emAdapter()
	if err != nil {
		return 0, err
	}
	now := time.Now()
	from := now.Format("2006-01-02")
	to := now.AddDate(0, 0, corpActionFutureDays).Format("2006-01-02")

	var recs []model.IpoSubscription
	var errs []error

	stocks, serr := em.GetIpoSubscriptions(ctx, from, to)
	if serr != nil && !errors.Is(serr, datasource.ErrNoData) {
		errs = append(errs, fmt.Errorf("新股申购: %w", serr))
	}
	for _, r := range stocks {
		recs = append(recs, model.IpoSubscription{
			Kind: model.IpoKindStock, Code: r.Symbol, Name: r.Name,
			ApplyCode: r.ApplyCode, ApplyDate: r.ApplyDate,
			IssuePrice: r.IssuePrice, ApplyUpper: r.ApplyUpper,
			PayDate: r.PayDate, BallotDate: r.BallotDate, ListDate: r.ListDate,
			Board: r.Board,
		})
	}

	cbs, cerr := em.GetCbSubscriptions(ctx, from, to)
	if cerr != nil && !errors.Is(cerr, datasource.ErrNoData) {
		errs = append(errs, fmt.Errorf("可转债申购: %w", cerr))
	}
	for _, r := range cbs {
		recs = append(recs, model.IpoSubscription{
			Kind: model.IpoKindCb, Code: r.Symbol, Name: r.Name,
			ApplyCode: r.ApplyCode, ApplyDate: r.ApplyDate,
			IssuePrice: r.IssuePrice, ListDate: r.ListDate,
			StockCode: r.StockCode, StockName: r.StockName,
			Rating: r.Rating, IssueScaleYi: r.IssueScaleYi,
		})
	}

	if len(recs) > 0 {
		if err := common.DB.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "kind"}, {Name: "code"}, {Name: "apply_date"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"name", "apply_code", "issue_price", "apply_upper", "pay_date",
				"ballot_date", "list_date", "board", "stock_code", "stock_name",
				"rating", "issue_scale_yi", "updated_at",
			}),
		}).CreateInBatches(recs, 200).Error; err != nil {
			errs = append(errs, err)
		}
	}
	return len(recs), errors.Join(errs...)
}

// RunCorpActionSync 跑一轮完整同步。四类互不阻断，返回是否**全部成功**（供游标推进判定）。
func (s *CorpActionService) RunCorpActionSync(ctx context.Context) bool {
	if common.DB == nil {
		return false
	}
	allOK := true
	if n, err := s.SyncCorporateActions(ctx); err != nil {
		common.SysWarn("分红送转同步失败: %v", err)
		allOK = false
	} else if n > 0 {
		common.SysLog("分红送转同步入库 %d 行", n)
	}
	if n, err := s.SyncRestrictedReleases(ctx); err != nil {
		common.SysWarn("限售解禁同步失败: %v", err)
		allOK = false
	} else if n > 0 {
		common.SysLog("限售解禁同步入库 %d 行", n)
	}
	if n, err := s.SyncIpoSubscriptions(ctx); err != nil {
		common.SysWarn("打新日历同步失败: %v", err)
		allOK = false
	} else if n > 0 {
		common.SysLog("打新日历同步入库 %d 行", n)
	}
	// 模拟盘除权自动调整：数据落库后立刻跑（真实持仓走用户确认，不在这里动）。
	if n := RunPaperCorpAdjust(); n > 0 {
		common.SysLog("模拟盘除权除息自动调整 %d 笔", n)
	}
	return allOK
}

// StartCorpActionJobs 每日 19:25 同步公司行动与打新日历。
//
//   - 防重入：单 goroutine 串行（同一时刻只有一轮），单轮 15 分钟超时；
//   - 启动补跑：启动 5 分钟后若「今日已同步」游标不是今天则补跑一轮
//     （错峰在 finance 的 2 分钟、快照的 4 分钟之后）；
//   - 每天都跑（不判交易日）：分红/解禁公告周末也发布，窗口查询天然幂等。
func StartCorpActionJobs() *CorpActionService {
	svc := NewCorpActionService()
	run := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		if svc.RunCorpActionSync(ctx) {
			_ = model.UpsertOption(optCorpActionSyncDay, time.Now().Format("2006-01-02"))
		}
	}
	go func() {
		time.Sleep(5 * time.Minute)
		if common.DB != nil && optionValue(optCorpActionSyncDay) != time.Now().Format("2006-01-02") {
			run()
		}
		for {
			time.Sleep(time.Until(nextDailyAt(time.Now(), corpActionSyncHour, corpActionSyncMin)))
			run()
		}
	}()
	return svc
}
