package service

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// P3a 机构观点服务：卖方研报评级 + 机构调研的按需拉取与缓存（照 F2 finance_f10 模式：
// 7 天新鲜期 + 1h 尝试冷却，包级共享状态，严禁全市场普查——只有个股详情/AI 快照访问触发）。
// 价值核心是 computeOrgView 数据组织纯函数：卖方评级九成是买入/增持，裸分布无区分度，
// 有信息量的是评级变动（下调=强信号）、目标价与现价偏离、调研密度变化。

var orgViewEM = datasource.NewEastMoneyAdapter()

// 注入点：单测替换。
var (
	fetchOrgReports = func(ctx context.Context, symbol string, days int) ([]datasource.ReportRow, error) {
		return orgViewEM.GetStockReports(ctx, symbol, days)
	}
	fetchOrgSurveys = func(ctx context.Context, symbol string, days int) ([]datasource.SurveyRow, error) {
		return orgViewEM.GetOrgSurveys(ctx, symbol, days)
	}
)

const (
	orgFreshTTL     = 7 * 24 * time.Hour // 缓存新鲜期（研报/调研按日披露，7 天足够及时）
	orgAttemptCool  = time.Hour          // 拉取尝试冷却（成功失败都算，防刷）
	orgFetchDays    = 400                // 拉取窗口：覆盖 180 天分布窗 + 90 天环比对照
	orgReportKeep   = 200                // 单股研报落库份数上限
	orgSurveySample = 5                  // org_names 取样家数
)

var (
	orgSyncMu  sync.Mutex
	orgSyncTry = map[string]time.Time{} // "rep:600519" / "svy:600519" → 上次尝试时刻
)

func orgTryAllowed(key string) bool {
	orgSyncMu.Lock()
	defer orgSyncMu.Unlock()
	if t, ok := orgSyncTry[key]; ok && time.Since(t) < orgAttemptCool {
		return false
	}
	orgSyncTry[key] = time.Now()
	return true
}

// ensureReportRatings 研报评级按需同步（best-effort：失败静默，消费方用缓存里有的）。
func ensureReportRatings(ctx context.Context, symbol string) {
	if common.DB == nil || !isSixDigits(symbol) {
		return
	}
	if finFresh(&model.ReportRating{}, symbol) || !orgTryAllowed("rep:"+symbol) {
		return
	}
	rows, err := fetchOrgReports(ctx, symbol, orgFetchDays)
	if err != nil {
		common.SysDebug("研报评级拉取失败 %s: %v", symbol, err)
		return
	}
	if len(rows) > orgReportKeep {
		rows = rows[:orgReportKeep]
	}
	recs := make([]model.ReportRating, 0, len(rows))
	for _, r := range rows {
		if r.InfoCode == "" || r.PublishDate == "" {
			continue
		}
		recs = append(recs, model.ReportRating{
			Symbol: symbol, Market: "cn", InfoCode: r.InfoCode,
			ReportDate: r.PublishDate, OrgName: truncateRunes(r.OrgName, 64),
			Researcher: truncateRunes(r.Researcher, 64), Title: truncateRunes(r.Title, 255),
			Rating: truncateRunes(r.Rating, 16), LastRating: truncateRunes(r.LastRating, 16),
			RatingChange: r.RatingChange, TargetPrice: r.TargetPrice,
		})
	}
	if len(recs) == 0 {
		return
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "info_code"}},
		DoUpdates: clause.AssignmentColumns([]string{"report_date", "org_name", "researcher", "title",
			"rating", "last_rating", "rating_change", "target_price", "updated_at"}),
	}).CreateInBatches(recs, 100).Error; err != nil {
		common.SysWarn("研报评级落库失败 %s: %v", symbol, err)
	}
}

// ensureOrgSurveys 机构调研按需同步：上游一机构一行明细，落库前按调研日聚合。
func ensureOrgSurveys(ctx context.Context, symbol string) {
	if common.DB == nil || !isSixDigits(symbol) {
		return
	}
	if finFresh(&model.OrgSurvey{}, symbol) || !orgTryAllowed("svy:"+symbol) {
		return
	}
	rows, err := fetchOrgSurveys(ctx, symbol, orgFetchDays)
	if err != nil {
		common.SysDebug("机构调研拉取失败 %s: %v", symbol, err)
		return
	}
	recs := aggregateSurveys(symbol, rows)
	if len(recs) == 0 {
		return
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "survey_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"notice_date", "org_count", "org_names",
			"receive_way", "updated_at"}),
	}).CreateInBatches(recs, 100).Error; err != nil {
		common.SysWarn("机构调研落库失败 %s: %v", symbol, err)
	}
}

// aggregateSurveys 明细按调研日聚合（纯函数）：org_count=当日参与机构行数，
// org_names 取样前若干家。
func aggregateSurveys(symbol string, rows []datasource.SurveyRow) []model.OrgSurvey {
	byDate := map[string]*model.OrgSurvey{}
	names := map[string][]string{}
	for _, r := range rows {
		if r.SurveyDate == "" {
			continue
		}
		rec, ok := byDate[r.SurveyDate]
		if !ok {
			rec = &model.OrgSurvey{
				Symbol: symbol, Market: "cn", SurveyDate: r.SurveyDate,
				NoticeDate: r.NoticeDate, ReceiveWay: truncateRunes(r.ReceiveWay, 128),
			}
			byDate[r.SurveyDate] = rec
		}
		rec.OrgCount++
		if r.OrgName != "" && len(names[r.SurveyDate]) < orgSurveySample {
			names[r.SurveyDate] = append(names[r.SurveyDate], r.OrgName)
		}
	}
	out := make([]model.OrgSurvey, 0, len(byDate))
	for date, rec := range byDate {
		rec.OrgNames = truncateRunes(strings.Join(names[date], ","), 255)
		out = append(out, *rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SurveyDate > out[j].SurveyDate })
	return out
}

// orgRatingBucket 东财归一评级 → 分布桶键。
func orgRatingBucket(rating string) string {
	switch rating {
	case "买入":
		return "buy"
	case "增持":
		return "overweight"
	case "中性":
		return "neutral"
	case "减持":
		return "reduce"
	case "卖出":
		return "sell"
	case "":
		return ""
	default:
		return "other"
	}
}

// 上游 ratingChange 枚举（2026-07-10 实测锚定）。
const (
	ratingChangeUp    = 0 // 上调
	ratingChangeDown  = 1 // 下调
	ratingChangeFirst = 2 // 首次覆盖
	ratingChangeKeep  = 3 // 维持
)

// computeOrgView 机构观点数据组织纯函数（本批价值核心）。
// reports 按 report_date 降序、surveys 按 survey_date 降序；price<=0 时不算目标价偏离。
// 输出为 AI 快照 org_view 段（数值叶子经 snapshotValueSet 自动进核验值域，
// 文本字段刻意不含小数数字）。
func computeOrgView(reports []model.ReportRating, surveys []model.OrgSurvey, price float64, now time.Time) map[string]any {
	if len(reports) == 0 && len(surveys) == 0 {
		return nil
	}
	day := func(back int) string { return now.AddDate(0, 0, -back).Format("2006-01-02") }
	d30, d90, d180 := day(30), day(90), day(180)

	out := map[string]any{
		"note": "卖方研报评级普遍乐观（九成为买入/增持），参考价值在评级变动（尤其下调）、目标价与现价的偏离、调研密度变化，而非买入家数本身",
	}

	if len(reports) > 0 {
		dist := func(since string) map[string]any {
			m := map[string]int{}
			total := 0
			for _, r := range reports {
				if r.ReportDate < since {
					continue
				}
				if b := orgRatingBucket(r.Rating); b != "" {
					m[b]++
					total++
				}
			}
			d := map[string]any{"total": total}
			for k, v := range m {
				d[k] = v
			}
			return d
		}
		out["rating_dist_90d"] = dist(d90)
		out["rating_dist_180d"] = dist(d180)

		// 评级变动检测：上游 ratingChange 枚举为准（0=上调 1=下调 2=首次 3=维持）。
		up, down, first := 0, 0, 0
		var latestChange map[string]any
		for _, r := range reports { // 降序：首个命中即最近一次变动
			if r.ReportDate < d90 {
				break
			}
			switch r.RatingChange {
			case ratingChangeUp:
				up++
			case ratingChangeDown:
				down++
			case ratingChangeFirst:
				first++
			}
			if latestChange == nil && (r.RatingChange == ratingChangeUp || r.RatingChange == ratingChangeDown) {
				kind := "上调"
				if r.RatingChange == ratingChangeDown {
					kind = "下调"
				}
				from := r.LastRating
				if from == "" {
					from = "未知"
				}
				latestChange = map[string]any{
					"date": r.ReportDate, "org": r.OrgName, "kind": kind,
					"from": from, "to": r.Rating,
				}
			}
		}
		out["rating_changes_90d"] = map[string]any{
			"upgrades": up, "downgrades": down, "first_covers": first,
			"note": "评级下调是强信号（卖方极少下调）；首次覆盖代表关注度提升",
		}
		if latestChange != nil {
			out["latest_rating_change"] = latestChange
		}

		// 目标价：近 180 天有目标价的研报取 min/中位/max 与现价偏离（缺失容错）。
		var targets []float64
		for _, r := range reports {
			if r.ReportDate >= d180 && r.TargetPrice > 0 {
				targets = append(targets, r.TargetPrice)
			}
		}
		if len(targets) > 0 {
			sort.Float64s(targets)
			median := targets[len(targets)/2]
			if len(targets)%2 == 0 {
				median = (targets[len(targets)/2-1] + targets[len(targets)/2]) / 2
			}
			tp := map[string]any{
				"count":  len(targets),
				"min":    round2(targets[0]),
				"median": round2(median),
				"max":    round2(targets[len(targets)-1]),
				"note":   "近 180 天给出目标价的研报统计（多数研报不给目标价，样本有限）",
			}
			if price > 0 {
				tp["median_vs_price_pct"] = round2((median - price) / price * 100)
			}
			out["target_price"] = tp
		}
	}

	if len(surveys) > 0 {
		b30, b90, prev90 := 0, 0, 0
		for _, s := range surveys {
			switch {
			case s.SurveyDate >= d30:
				b30++
				b90++
			case s.SurveyDate >= d90:
				b90++
			case s.SurveyDate >= d180:
				prev90++
			}
		}
		out["survey"] = map[string]any{
			"batches_30d":      b30,
			"batches_90d":      b90,
			"batches_prev_90d": prev90,
			"latest_date":      surveys[0].SurveyDate,
			"latest_org_count": surveys[0].OrgCount,
			"note":             "机构调研按日计批；近 90 天批次数环比上一个 90 天可看关注度变化",
		}
	}
	return out
}

// orgViewBrief 个股 AI 快照的机构观点段（分析/问答共用）。缓存缺失时按需拉取
// （研报 1~2 请求 + 调研 1 请求，interactive 路径可承受）。无数据返回 nil。
func orgViewBrief(ctx context.Context, symbol string, price float64) map[string]any {
	if common.DB == nil || !isSixDigits(symbol) {
		return nil
	}
	ensureReportRatings(ctx, symbol)
	ensureOrgSurveys(ctx, symbol)
	reports, surveys := loadOrgRows(symbol, 0, 0)
	return computeOrgView(reports, surveys, price, time.Now())
}

// loadOrgRows 读库（降序）；repLimit/svyLimit <=0 时取组织窗口所需的全量上限。
func loadOrgRows(symbol string, repLimit, svyLimit int) ([]model.ReportRating, []model.OrgSurvey) {
	if repLimit <= 0 {
		repLimit = orgReportKeep
	}
	if svyLimit <= 0 {
		svyLimit = 100
	}
	var reports []model.ReportRating
	common.DB.Where("symbol = ?", symbol).Order("report_date DESC").Limit(repLimit).Find(&reports)
	var surveys []model.OrgSurvey
	common.DB.Where("symbol = ?", symbol).Order("survey_date DESC").Limit(svyLimit).Find(&surveys)
	return reports, surveys
}

// OrgViewService 详情页机构观点块的薄服务壳。
type OrgViewService struct{}

func NewOrgViewService() *OrgViewService { return &OrgViewService{} }

// Overview 详情页机构观点：汇总 + 研报/调研明细列表。非 A 股股票返回空结构。
func (s *OrgViewService) Overview(ctx context.Context, market, symbol string, price float64) map[string]any {
	empty := map[string]any{"summary": nil, "reports": []model.ReportRating{}, "surveys": []model.OrgSurvey{}}
	symbol = strings.TrimSpace(symbol)
	if common.DB == nil || market != "cn" || !isSixDigits(symbol) || isCNFund(symbol) {
		return empty
	}
	ensureReportRatings(ctx, symbol)
	ensureOrgSurveys(ctx, symbol)
	// summary 基于全量窗口（180 天分布不能被明细截断），列表另行截前 30/20。
	reports, surveys := loadOrgRows(symbol, 0, 0)
	summary := computeOrgView(reports, surveys, price, time.Now())
	if len(reports) > 30 {
		reports = reports[:30]
	}
	if len(surveys) > 20 {
		surveys = surveys[:20]
	}
	if reports == nil {
		reports = []model.ReportRating{}
	}
	if surveys == nil {
		surveys = []model.OrgSurvey{}
	}
	return map[string]any{
		"summary": summary,
		"reports": reports,
		"surveys": surveys,
	}
}
