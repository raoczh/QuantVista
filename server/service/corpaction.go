package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
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

func corporateActionReferenced(tx *gorm.DB, actionID int64) (bool, error) {
	for _, table := range []any{
		&model.PositionCorpAdjust{},
		&model.PaperCorpAdjust{},
		&model.PositionTrade{},
	} {
		var count int64
		if err := tx.Model(table).Where("corporate_action_id = ?", actionID).Count(&count).Error; err != nil {
			return false, err
		}
		if count > 0 {
			return true, nil
		}
	}
	return false, nil
}

// storeCorporateActions 按稳定方案身份写入。ExDate 会从空值推进到实施日期，也可能被
// 上游订正，因此不能直接作为 upsert 身份；PLAN_NOTICE_DATE 相同的行必须就地更新。
// 上游偶发漏稳定字段时，只在 NoticeDate/ExDate 能唯一消歧且候选尚未被账本引用时接续；
// 两个日期都变化时无法区分“订正”与“独立方案”，必须报错，不能猜测复用或新建幽灵记录。
func storeCorporateActions(recs []model.CorporateAction) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		for i := range recs {
			rec := recs[i]
			base := tx.Where("symbol = ? AND market = ? AND report_date = ?",
				rec.Symbol, rec.Market, rec.ReportDate)
			var candidates []model.CorporateAction
			if err := base.Order("id ASC").Find(&candidates).Error; err != nil {
				return err
			}
			ambiguous := func(reason string, n int) error {
				return fmt.Errorf("公司行动身份歧义 %s/%s report=%s（%s，候选 %d 条）",
					rec.Market, rec.Symbol, rec.ReportDate, reason, n)
			}
			pickUnique := func(rows []model.CorporateAction, reason string, dst *model.CorporateAction) (bool, error) {
				switch len(rows) {
				case 0:
					return false, nil
				case 1:
					*dst = rows[0]
					return true, nil
				default:
					return false, ambiguous(reason, len(rows))
				}
			}
			filter := func(src []model.CorporateAction, keep func(model.CorporateAction) bool) []model.CorporateAction {
				out := make([]model.CorporateAction, 0, len(src))
				for _, row := range src {
					if keep(row) {
						out = append(out, row)
					}
				}
				return out
			}
			var existing model.CorporateAction
			found := false
			weakMatch := false
			if rec.PlanNoticeDate != "" {
				exact := filter(candidates, func(row model.CorporateAction) bool {
					return row.PlanNoticeDate == rec.PlanNoticeDate
				})
				legacyCandidates := filter(candidates, func(row model.CorporateAction) bool {
					return row.PlanNoticeDate == ""
				})
				var err error
				found, err = pickUnique(exact, "相同 PLAN_NOTICE_DATE", &existing)
				if err != nil {
					return err
				}
				if !found {
					// 升级前空身份行只能由公告日消歧：优先首次预案公告日，其次本次
					// 公告日，最后才接 NoticeDate 也为空的旧行。实施阶段若已有同 ExDate
					// 的兼容行，优先保留它的 ID（它可能已被审计引用），再清掉空日期预案。
					// 不能只凭同 ExDate 覆盖 NoticeDate 不同的未知方案。
					compatible := filter(legacyCandidates, func(row model.CorporateAction) bool {
						return row.NoticeDate == "" || row.NoticeDate == rec.PlanNoticeDate ||
							(rec.NoticeDate != "" && row.NoticeDate == rec.NoticeDate)
					})
					if rec.ExDate != "" {
						sameEx := filter(compatible, func(row model.CorporateAction) bool {
							return row.ExDate == rec.ExDate
						})
						found, err = pickUnique(sameEx, "同除权日旧行无法唯一消歧", &existing)
						if err != nil {
							return err
						}
					}
					if !found {
						pending := filter(compatible, func(row model.CorporateAction) bool {
							return row.ExDate == ""
						})
						found, err = pickUnique(pending, "空日期旧预案无法唯一消歧", &existing)
						if err != nil {
							return err
						}
					}
				}
				if !found && len(legacyCandidates) > 0 {
					return ambiguous("新稳定身份无法可靠关联同报告期旧弱身份行", len(legacyCandidates))
				}
				// 有稳定 Plan 的载荷若只靠“同 Ex、但旧行连公告日也没有”接上，
				// 仍是弱证据；旧 ID 已被账本引用时不得借此重新解释来源。
				weakMatch = found && existing.PlanNoticeDate == "" && existing.NoticeDate == ""
			} else {
				// 缺 PLAN_NOTICE_DATE：先用完整的 ExDate+NoticeDate 组合，再分别尝试
				// NoticeDate 与兼容的 ExDate。同 Ex 但两个非空公告日不同，必须视为
				// 独立方案，不能退化成「同 Ex 唯一」后误覆盖。
				exactPayload := filter(candidates, func(row model.CorporateAction) bool {
					return row.ExDate == rec.ExDate && row.NoticeDate == rec.NoticeDate
				})
				var pickErr error
				found, pickErr = pickUnique(exactPayload, "相同除权日/公告日无法唯一消歧", &existing)
				if pickErr != nil {
					return pickErr
				}
				if !found && rec.NoticeDate != "" {
					sameNotice := filter(candidates, func(row model.CorporateAction) bool {
						return row.NoticeDate == rec.NoticeDate
					})
					found, pickErr = pickUnique(sameNotice, "同公告日无法唯一消歧", &existing)
					if pickErr != nil {
						return pickErr
					}
				}
				if !found && rec.ExDate != "" {
					sameEx := filter(candidates, func(row model.CorporateAction) bool {
						return row.ExDate == rec.ExDate
					})
					compatibleEx := filter(sameEx, func(row model.CorporateAction) bool {
						return rec.NoticeDate == "" || row.NoticeDate == "" || row.NoticeDate == rec.NoticeDate
					})
					found, pickErr = pickUnique(compatibleEx, "同除权日存在多个兼容方案", &existing)
					if pickErr != nil {
						return pickErr
					}
				}
				if !found {
					legacy := filter(candidates, func(row model.CorporateAction) bool {
						return row.PlanNoticeDate == "" && (row.ExDate == rec.ExDate || row.ExDate == "")
					})
					if rec.NoticeDate != "" {
						legacy = filter(legacy, func(row model.CorporateAction) bool {
							return row.NoticeDate == rec.NoticeDate || row.NoticeDate == ""
						})
					}
					var pickErr error
					found, pickErr = pickUnique(legacy, "空身份旧行无法唯一消歧", &existing)
					if pickErr != nil {
						return pickErr
					}
				}
				if !found && len(candidates) > 0 {
					// 同 Ex、不同 Notice 也可能只是同一方案的实施公告更新；没有稳定
					// Plan 无法证明它是独立方案。只要已有候选却无法可靠关联就 fail closed。
					return ambiguous("缺 PLAN_NOTICE_DATE 且日期无法可靠关联", len(candidates))
				}
				weakMatch = found
			}
			if found {
				if weakMatch {
					referenced, err := corporateActionReferenced(tx, existing.ID)
					if err != nil {
						return err
					}
					if referenced {
						return ambiguous("弱身份候选已被账本引用，拒绝改写", 1)
					}
				}
				if rec.PlanNoticeDate == "" {
					rec.PlanNoticeDate = existing.PlanNoticeDate
				}
				if weakMatch {
					// 弱身份载荷的空日期表示“本次缺失”，不能把已经明确的实施/登记/
					// 公告日期回退为空，否则权益基数与后续匹配都会漂移。
					if rec.ExDate == "" {
						rec.ExDate = existing.ExDate
					}
					if rec.RecordDate == "" {
						rec.RecordDate = existing.RecordDate
					}
					if rec.NoticeDate == "" {
						rec.NoticeDate = existing.NoticeDate
					}
				}
				// 旧实现可能已经留下「空日期预案 + 实施行」两份。空日期行从未能生成折算，
				// 因而可安全清理；有日期行可能已被审计引用，绝不在这里删除。
				if rec.ExDate != "" && rec.PlanNoticeDate != "" {
					if err := tx.Where("symbol = ? AND market = ? AND report_date = ?",
						rec.Symbol, rec.Market, rec.ReportDate).Where(
						"id <> ? AND ex_date = '' AND COALESCE(plan_notice_date, '') = '' AND notice_date = ?",
						existing.ID, rec.PlanNoticeDate,
					).
						Delete(&model.CorporateAction{}).Error; err != nil {
						return err
					}
				}
				rec.ID, rec.CreatedAt = existing.ID, existing.CreatedAt
				if err := tx.Save(&rec).Error; err != nil {
					return err
				}
				continue
			}
			if err := tx.Create(&rec).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func liftStoreKey(r model.RestrictedRelease) string {
	return r.Symbol + "\x00" + r.Market + "\x00" + r.FreeDate + "\x00" + r.FreeType
}

// storeRestrictedReleases 把一次完整窗口查询当作权威集合：先 upsert 返回行，再删除窗口内
// 已不在上游集合的旧行。只有完整拉取成功才调用本函数，网络/分页/解析失败绝不清数据。
func storeRestrictedReleases(recs []model.RestrictedRelease, from, to string) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if len(recs) > 0 {
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "free_date"}, {Name: "free_type"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"name", "free_shares", "lift_market_cap", "free_ratio", "total_ratio", "updated_at",
				}),
			}).CreateInBatches(recs, 200).Error; err != nil {
				return err
			}
		}
		keep := make(map[string]struct{}, len(recs))
		for _, rec := range recs {
			keep[liftStoreKey(rec)] = struct{}{}
		}
		var old []model.RestrictedRelease
		if err := tx.Where("free_date BETWEEN ? AND ?", from, to).Find(&old).Error; err != nil {
			return err
		}
		var staleIDs []int64
		for _, row := range old {
			if _, ok := keep[liftStoreKey(row)]; !ok {
				staleIDs = append(staleIDs, row.ID)
			}
		}
		if len(staleIDs) > 0 {
			return tx.Where("id IN ?", staleIDs).Delete(&model.RestrictedRelease{}).Error
		}
		return nil
	})
}

// storeIpoSubscriptions 以 (kind, code) 作为稳定发行身份就地更新时间，并对该来源的完整
// 查询窗口单独对账。股票源和转债源互不借用结果，避免一源失败时误清另一源数据。
func storeIpoSubscriptions(kind string, recs []model.IpoSubscription, from, to string) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		for i := range recs {
			rec := recs[i]
			var existing []model.IpoSubscription
			if err := tx.Where("kind = ? AND code = ?", kind, rec.Code).Order("id ASC").Find(&existing).Error; err != nil {
				return err
			}
			if len(existing) > 0 {
				keeper := existing[0]
				for _, row := range existing {
					if row.ApplyDate == rec.ApplyDate {
						keeper = row
						break
					}
				}
				if len(existing) > 1 {
					ids := make([]int64, 0, len(existing)-1)
					for _, dup := range existing {
						if dup.ID != keeper.ID {
							ids = append(ids, dup.ID)
						}
					}
					if err := tx.Where("id IN ?", ids).Delete(&model.IpoSubscription{}).Error; err != nil {
						return err
					}
				}
				rec.ID, rec.CreatedAt = keeper.ID, keeper.CreatedAt
				if err := tx.Save(&rec).Error; err != nil {
					return err
				}
				continue
			}
			if err := tx.Create(&rec).Error; err != nil {
				return err
			}
		}
		keep := make(map[string]struct{}, len(recs))
		for _, rec := range recs {
			keep[rec.Code] = struct{}{}
		}
		var old []model.IpoSubscription
		if err := tx.Where("kind = ? AND apply_date BETWEEN ? AND ?", kind, from, to).Find(&old).Error; err != nil {
			return err
		}
		var staleIDs []int64
		for _, row := range old {
			if _, ok := keep[row.Code]; !ok {
				staleIDs = append(staleIDs, row.ID)
			}
		}
		if len(staleIDs) > 0 {
			return tx.Where("id IN ?", staleIDs).Delete(&model.IpoSubscription{}).Error
		}
		return nil
	})
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
			RecordDate: r.RecordDate, PlanNoticeDate: r.PlanNoticeDate, NoticeDate: r.NoticeDate,
			BonusRatio: r.BonusRatio, TransferRatio: r.TransferRatio,
			DividendPretax: r.DividendPretax, DividendYield: r.DividendYield,
			Progress: r.Progress, PlanProfile: truncateRunes(r.PlanProfile, 250),
		})
	}
	if len(recs) == 0 {
		return 0, nil
	}
	if err := storeCorporateActions(recs); err != nil {
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
	if err != nil && !errors.Is(err, datasource.ErrNoData) {
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
	if err := storeRestrictedReleases(recs, from, to); err != nil {
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

	var errs []error
	total := 0

	stocks, serr := em.GetIpoSubscriptions(ctx, from, to)
	if serr != nil && !errors.Is(serr, datasource.ErrNoData) {
		errs = append(errs, fmt.Errorf("新股申购: %w", serr))
	} else {
		recs := make([]model.IpoSubscription, 0, len(stocks))
		for _, r := range stocks {
			recs = append(recs, model.IpoSubscription{
				Kind: model.IpoKindStock, Code: r.Symbol, Name: r.Name,
				ApplyCode: r.ApplyCode, ApplyDate: r.ApplyDate,
				IssuePrice: r.IssuePrice, ApplyUpper: r.ApplyUpper,
				PayDate: r.PayDate, BallotDate: r.BallotDate, ListDate: r.ListDate,
				Board: r.Board,
			})
		}
		if err := storeIpoSubscriptions(model.IpoKindStock, recs, from, to); err != nil {
			errs = append(errs, fmt.Errorf("新股申购入库: %w", err))
		} else {
			total += len(recs)
		}
	}

	cbs, cerr := em.GetCbSubscriptions(ctx, from, to)
	if cerr != nil && !errors.Is(cerr, datasource.ErrNoData) {
		errs = append(errs, fmt.Errorf("可转债申购: %w", cerr))
	} else {
		recs := make([]model.IpoSubscription, 0, len(cbs))
		for _, r := range cbs {
			recs = append(recs, model.IpoSubscription{
				Kind: model.IpoKindCb, Code: r.Symbol, Name: r.Name,
				ApplyCode: r.ApplyCode, ApplyDate: r.ApplyDate,
				IssuePrice: r.IssuePrice, ListDate: r.ListDate,
				StockCode: r.StockCode, StockName: r.StockName,
				Rating: r.Rating, IssueScaleYi: r.IssueScaleYi,
			})
		}
		if err := storeIpoSubscriptions(model.IpoKindCb, recs, from, to); err != nil {
			errs = append(errs, fmt.Errorf("可转债申购入库: %w", err))
		} else {
			total += len(recs)
		}
	}
	return total, errors.Join(errs...)
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
