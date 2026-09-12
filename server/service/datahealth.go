package service

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// P0-3A 数据健康报告只读本地库。交易日窗口固定在 30~60 日，所有明细查询带日期
// 范围、可用索引和结果硬上限；不得从该 GET 触发上游扫描。
const (
	DataHealthDefaultDays = 45
	DataHealthMinDays     = 30
	DataHealthMaxDays     = 60
)

type DataHealthDay struct {
	Date          string `json:"date"`
	Status        string `json:"status"` // covered / missing / partial / suspended / closed / unknown
	Observed      int64  `json:"observed"`
	Expected      int64  `json:"expected"`
	Suspended     int64  `json:"suspended,omitempty"`
	RecoveryClass string `json:"recovery_class"` // backfillable / unrecoverable / partial / unknown
	Note          string `json:"note,omitempty"`
}

type DataHealthFailureSummary struct {
	Task      string    `json:"task"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// DataHealthItem 一个数据域的健康行。Coverage 保留旧客户端展示，数值分子/分母是新契约。
type DataHealthItem struct {
	Key                 string                    `json:"key"`
	Name                string                    `json:"name"`
	ExpectedDate        string                    `json:"expected_date"`
	ObservedDate        string                    `json:"observed_date"`
	LagOpenDays         int                       `json:"lag_open_days"`
	Tolerance           int                       `json:"tolerance_open_days"`
	Status              string                    `json:"status"` // ok / behind / empty / partial / unknown
	Coverage            string                    `json:"coverage,omitempty"`
	CoverageNumerator   int64                     `json:"coverage_numerator"`
	CoverageDenominator int64                     `json:"coverage_denominator"`
	CoverageUnit        string                    `json:"coverage_unit,omitempty"`
	RecoveryClass       string                    `json:"recovery_class"`
	GapCalendar         []DataHealthDay           `json:"gap_calendar,omitempty"`
	LastRun             *model.DataSyncLog        `json:"last_run,omitempty"`
	RecentFailure       *DataHealthFailureSummary `json:"recent_failure,omitempty"`
	Note                string                    `json:"note,omitempty"`
}

type DataHealthReport struct {
	GeneratedAt  string           `json:"generated_at"`
	WindowDays   int              `json:"window_days"`
	WindowStart  string           `json:"window_start,omitempty"`
	WindowEnd    string           `json:"window_end,omitempty"`
	QueryHardMax int              `json:"query_hard_max"`
	Items        []DataHealthItem `json:"items"`
}

func normalizeDataHealthDays(days int) int {
	if days == 0 {
		return DataHealthDefaultDays
	}
	if days < DataHealthMinDays {
		return DataHealthMinDays
	}
	if days > DataHealthMaxDays {
		return DataHealthMaxDays
	}
	return days
}

// dhMaxDate 只接受代码内固定列名；调用点都应让 where 命中模型现有索引。
func (r *dataHealthReader) dhMaxDate(modelPtr any, dateCol, where string, args ...any) string {
	if r.db == nil {
		return ""
	}
	var d sql.NullString
	q := r.db.Model(modelPtr).Select("MAX(" + dateCol + ")")
	if _, calendar := modelPtr.(*model.TradingCalendar); !calendar {
		q = q.Where(dateCol+" <= ?", r.now.Format("2006-01-02"))
	}
	if where != "" {
		q = q.Where(where, args...)
	}
	if r.failed(q.Scan(&d).Error) || !d.Valid {
		return ""
	}
	return d.String
}

func (r *dataHealthReader) dhNewsMax() string {
	if r.db == nil {
		return ""
	}
	var row model.News
	if r.missingOrFailed(r.db.Select("publish_time").Where("publish_time <= ?", r.now).Order("publish_time DESC").Limit(1).Take(&row).Error) {
		return ""
	}
	return row.PublishTime.Format("2006-01-02")
}

func (r *dataHealthReader) dhLastLog(tasks ...string) *model.DataSyncLog {
	if r.db == nil || len(tasks) == 0 {
		return nil
	}
	var row model.DataSyncLog
	if r.missingOrFailed(r.db.Where("task IN ?", tasks).Order("created_at DESC, id DESC").Limit(1).Take(&row).Error) {
		return nil
	}
	return &row
}

func (r *dataHealthReader) dhRecentFailure(tasks ...string) *DataHealthFailureSummary {
	if r.db == nil || len(tasks) == 0 {
		return nil
	}
	var row model.DataSyncLog
	if r.missingOrFailed(r.db.Select("task", "status", "message", "created_at").
		Where("task IN ? AND status IN ?", tasks, []string{"failed", "partial"}).
		Order("created_at DESC, id DESC").Limit(1).Take(&row).Error) {
		return nil
	}
	return &DataHealthFailureSummary{Task: row.Task, Status: row.Status, Message: row.Message, CreatedAt: row.CreatedAt}
}

func dhStatus(observed string, lag, tolerance int) string {
	switch {
	case observed == "":
		return "empty"
	case lag < 0:
		return "unknown"
	case lag <= tolerance:
		return "ok"
	default:
		return "behind"
	}
}

func recentOpenDates(market, end string, days int) []string {
	dates, _ := recentOpenDatesDB(common.DB, market, end, days)
	return dates
}

type dhDateCount struct {
	Date string
	N    int64
}

func (r *dataHealthReader) dhCounts(modelPtr any, dateCol, market, from, to string, limit int) map[string]int64 {
	out := map[string]int64{}
	if r.db == nil || from == "" || to == "" {
		return out
	}
	var rows []dhDateCount
	q := r.db.Model(modelPtr).Select(dateCol+" AS date, COUNT(*) AS n").
		Where(dateCol+" >= ? AND "+dateCol+" <= ?", from, to).
		Where(dateCol+" IN ?", r.tradeDates)
	if market != "" {
		q = q.Where("market = ?", market)
	}
	if r.failed(q.Group(dateCol).Order(dateCol).Limit(limit+1).Find(&rows).Error) || r.tooMany(len(rows), limit) {
		return out
	}
	for _, row := range rows {
		out[row.Date] = row.N
	}
	return out
}

func (r *dataHealthReader) dhNewsCounts(from, to string, limit int) map[string]int64 {
	out := map[string]int64{}
	if r.db == nil || from == "" || to == "" {
		return out
	}
	end, err := time.ParseInLocation("2006-01-02", to, time.Local)
	if err != nil {
		return out
	}
	var rows []dhDateCount
	dateExpr := "DATE(publish_time)"
	if r.db.Dialector.Name() == "sqlite" {
		// SQLite DATE 会把带时区的本地午夜先换成 UTC，落到前一天；
		// 与 MySQL DATETIME 一致，按入库时间的日历日期聚合。
		dateExpr = "substr(publish_time, 1, 10)"
	}
	if r.failed(r.db.Model(&model.News{}).
		Select(dateExpr+" AS date, COUNT(*) AS n").
		Where("publish_time >= ? AND publish_time < ?", from+" 00:00:00", end.AddDate(0, 0, 1)).
		Where(dateExpr+" IN ?", r.tradeDates).
		Group(dateExpr).Order("date").Limit(limit+1).Find(&rows).Error) || r.tooMany(len(rows), limit) {
		return out
	}
	for _, row := range rows {
		out[row.Date] = row.N
	}
	return out
}

func countCalendar(dates []string, counts map[string]int64, missingStatus, recovery, missingRecovery string) ([]DataHealthDay, int64, int64) {
	calendar := make([]DataHealthDay, 0, len(dates))
	var numerator int64
	for _, date := range dates {
		n := counts[date]
		day := DataHealthDay{Date: date, Observed: n, Expected: 1, RecoveryClass: recovery}
		if n > 0 {
			day.Status = "covered"
			numerator++
		} else {
			day.Status = missingStatus
			day.RecoveryClass = missingRecovery
		}
		calendar = append(calendar, day)
	}
	return calendar, numerator, int64(len(dates))
}

func coverageText(numerator, denominator int64, unit string) string {
	if denominator <= 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d %s", numerator, denominator, unit)
}

func hasCoverageGap(days []DataHealthDay) (anyObserved, gap bool) {
	for _, day := range days {
		if day.Status == "covered" || day.Status == "suspended" || day.Status == "closed" {
			anyObserved = true
		}
		if day.Status == "missing" || day.Status == "partial" {
			gap = true
		}
	}
	return
}

func finalizeHealthItem(item *DataHealthItem) {
	item.Coverage = coverageText(item.CoverageNumerator, item.CoverageDenominator, item.CoverageUnit)
	observed, gap := hasCoverageGap(item.GapCalendar)
	if item.Status == "ok" && gap {
		item.Status = "partial"
	}
	if item.Status == "empty" && observed {
		item.Status = "partial"
	}
}

type dhUniverseCoverage struct {
	Date      string
	Total     int64
	Suspended int64
}

func (r *dataHealthReader) dhActiveBarCounts(from, to string, limit int) map[string]int64 {
	out := map[string]int64{}
	if r.db == nil || from == "" || to == "" {
		return out
	}
	var rows []dhDateCount
	err := r.db.Table("daily_bars AS b").
		Select("b.trade_date AS date, COUNT(*) AS n").
		Joins("JOIN stock_universe_dailies AS u ON u.market = b.market AND u.symbol = b.symbol AND u.trade_date = b.trade_date").
		Where("b.market = ? AND b.trade_date >= ? AND b.trade_date <= ? AND u.suspended = ?", "cn", from, to, false).
		Where("b.trade_date IN ?", r.tradeDates).
		Group("b.trade_date").Order("b.trade_date").Limit(limit + 1).Find(&rows).Error
	if r.failed(err) || r.tooMany(len(rows), limit) {
		return out
	}
	for _, row := range rows {
		out[row.Date] = row.N
	}
	return out
}

func (r *dataHealthReader) wideGapCalendar(dates []string) ([]DataHealthDay, int64, int64) {
	if len(dates) == 0 || r.db == nil {
		return nil, 0, 0
	}
	from, to := dates[0], dates[len(dates)-1]
	barCounts := r.dhCounts(&model.DailyBar{}, "trade_date", "cn", from, to, len(dates))
	activeBarCounts := r.dhActiveBarCounts(from, to, len(dates))
	var universeRows []dhUniverseCoverage
	if r.failed(r.db.Model(&model.StockUniverseDaily{}).
		Select("trade_date AS date, COUNT(*) AS total, SUM(CASE WHEN suspended THEN 1 ELSE 0 END) AS suspended").
		Where("market = ? AND trade_date >= ? AND trade_date <= ?", "cn", from, to).
		Where("trade_date IN ?", dates).
		Group("trade_date").Order("trade_date").Limit(len(dates)+1).Find(&universeRows).Error) || r.tooMany(len(universeRows), len(dates)) {
		return nil, 0, 0
	}
	universe := make(map[string]dhUniverseCoverage, len(universeRows))
	for _, row := range universeRows {
		universe[row.Date] = row
	}
	var currentTotal int64
	if r.failed(r.db.Model(&model.MarketSyncState{}).Where("market = ?", "cn").Count(&currentTotal).Error) {
		return nil, 0, 0
	}
	calendar := make([]DataHealthDay, 0, len(dates))
	var numerator, denominator int64
	for _, date := range dates {
		observed := barCounts[date]
		u, hasPIT := universe[date]
		expected, suspended := currentTotal, int64(0)
		note := ""
		if hasPIT {
			expected = u.Total - u.Suspended
			suspended = u.Suspended
			observed = activeBarCounts[date]
		} else {
			note = "当日 PIT 宇宙缺失，停牌分母未知，暂按当前宇宙估计"
		}
		if expected == 0 && observed > 0 {
			expected = observed
		}
		day := DataHealthDay{Date: date, Observed: observed, Expected: expected, Suspended: suspended, RecoveryClass: "backfillable", Note: note}
		switch {
		case expected == 0 && suspended > 0:
			day.Status = "suspended"
		case expected == 0:
			day.Status = "unknown"
			day.RecoveryClass = "unknown"
		case observed == 0:
			day.Status = "missing"
		case !hasPIT:
			day.Status = "partial"
			day.RecoveryClass = "partial"
		case observed < expected:
			day.Status = "partial"
		default:
			day.Status = "covered"
		}
		denominator += expected
		if observed > expected {
			numerator += expected
		} else {
			numerator += observed
		}
		calendar = append(calendar, day)
	}
	return calendar, numerator, denominator
}

func (r *dataHealthReader) calendarCoverage(from, to string, hardMax int) ([]DataHealthDay, int64, int64) {
	if r.db == nil || from == "" || to == "" {
		return nil, 0, 0
	}
	f, err1 := time.ParseInLocation("2006-01-02", from, time.Local)
	t, err2 := time.ParseInLocation("2006-01-02", to, time.Local)
	if err1 != nil || err2 != nil {
		return nil, 0, 0
	}
	naturalDays := int(t.Sub(f).Hours()/24) + 1
	if naturalDays > hardMax {
		f = t.AddDate(0, 0, -(hardMax - 1))
		naturalDays = hardMax
	}
	var rows []model.TradingCalendar
	if r.failed(r.db.Select("trade_date", "is_open").Where("market = ? AND trade_date >= ? AND trade_date <= ?", "cn", f.Format("2006-01-02"), to).
		Order("trade_date").Limit(naturalDays+1).Find(&rows).Error) || r.tooMany(len(rows), naturalDays) {
		return nil, 0, 0
	}
	known := make(map[string]bool, len(rows))
	for _, row := range rows {
		known[row.TradeDate] = row.IsOpen
	}
	calendar := make([]DataHealthDay, 0, naturalDays)
	var numerator int64
	for d := f; !d.After(t); d = d.AddDate(0, 0, 1) {
		date := d.Format("2006-01-02")
		isOpen, ok := known[date]
		day := DataHealthDay{Date: date, Expected: 1, RecoveryClass: "backfillable"}
		if !ok {
			day.Status = "missing"
		} else {
			day.Observed = 1
			numerator++
			if isOpen {
				day.Status = "covered"
			} else {
				day.Status = "closed"
			}
		}
		calendar = append(calendar, day)
	}
	return calendar, numerator, int64(naturalDays)
}

func (r *dataHealthReader) build(now time.Time, requestedDays int) *DataHealthReport {
	days := normalizeDataHealthDays(requestedDays)
	rep := &DataHealthReport{
		GeneratedAt: now.Format("2006-01-02 15:04:05"), WindowDays: days,
		QueryHardMax: DataHealthMaxDays,
	}
	expectedWide := r.wideExpectedDate(now)
	expectedPrev := r.prevOpenTradeDate(now.Format("2006-01-02"))
	expectedEvening := expectedPrev
	if r.isTradingDayToday(now) && now.Hour()*60+now.Minute() >= 17*60+30 {
		expectedEvening = now.Format("2006-01-02")
	}
	tradeDates := r.recentOpenDates("cn", expectedWide, days)
	r.tradeDates = tradeDates
	if len(tradeDates) > 0 {
		rep.WindowStart, rep.WindowEnd = tradeDates[0], tradeDates[len(tradeDates)-1]
		rep.WindowDays = len(tradeDates)
	}

	add := func(item DataHealthItem) {
		if item.ObservedDate == "" {
			item.LagOpenDays = -1
		} else {
			item.LagOpenDays = r.openDaysBehind(item.ObservedDate, item.ExpectedDate)
		}
		item.Status = dhStatus(item.ObservedDate, item.LagOpenDays, item.Tolerance)
		if item.Key != "calendar" && item.ObservedDate > now.Format("2006-01-02") {
			item.Status, item.LagOpenDays = "unknown", -1
		}
		finalizeHealthItem(&item)
		rep.Items = append(rep.Items, item)
	}

	wideObserved := r.dhMaxDate(&model.MarketSyncState{}, "last_bar_date", "market = ? AND last_bar_date <> ''", "cn")
	if wideObserved == "" && r.err == nil {
		wideObserved = r.dhMaxDate(&model.DailyBar{}, "trade_date", "market = ?", "cn")
	}
	wideCalendar, wideN, wideD := r.wideGapCalendar(tradeDates)
	add(DataHealthItem{
		Key: "marketwide", Name: "全市场日线", ExpectedDate: expectedWide, ObservedDate: wideObserved,
		RecoveryClass: "backfillable", GapCalendar: wideCalendar,
		CoverageNumerator: wideN, CoverageDenominator: wideD, CoverageUnit: "股票交易日",
		LastRun: r.dhLastLog("sync_market_wide", "init_market_history"), RecentFailure: r.dhRecentFailure("sync_market_wide", "init_market_history"),
		Note: "按 PIT 宇宙扣除已知停牌；PIT 缺失日按当前宇宙估计并明确标 partial",
	})

	// 只取不可变的进程内快照，期望日期仍由本报告的时点与数据库快照决定。
	// CurrentFactorTable 会按墙上时钟另查数据库，不能在这里混用两个读取时点。
	factorTableMu.RLock()
	table := factorTableCur
	factorTableMu.RUnlock()
	if table != nil {
		expected := int64(len(table.Symbols))
		fresh := int64(float64(expected) * table.FreshCoverage)
		fresh = max(int64(0), min(fresh, expected))
		item := DataHealthItem{
			Key: "factor_table", Name: "因子宽表", ExpectedDate: expectedWide, ObservedDate: table.TradeDate,
			RecoveryClass: "partial", CoverageNumerator: fresh, CoverageDenominator: expected, CoverageUnit: "标的",
			Note: "进程内当前快照，不提供伪造的历史日历；落后时先补日线再重建",
		}
		if table.TradeDate != "" {
			status := "covered"
			if expected == 0 || fresh == 0 {
				status = "missing"
			} else if fresh < expected {
				status = "partial"
			}
			item.GapCalendar = []DataHealthDay{{Date: table.TradeDate, Status: status, Observed: fresh, Expected: expected, RecoveryClass: "partial"}}
		}
		add(item)
	} else {
		add(DataHealthItem{Key: "factor_table", Name: "因子宽表", ExpectedDate: expectedWide, RecoveryClass: "unknown", CoverageDenominator: 1, CoverageUnit: "当前快照", Note: "进程内尚未构建"})
	}

	from, to := rep.WindowStart, rep.WindowEnd
	appendCountDomain := func(key, name, observed, expected string, tolerance int, counts map[string]int64, missingStatus, recovery, missingRecovery, note string, tasks ...string) {
		cal, n, d := countCalendar(tradeDates, counts, missingStatus, recovery, missingRecovery)
		add(DataHealthItem{
			Key: key, Name: name, ExpectedDate: expected, ObservedDate: observed, Tolerance: tolerance,
			RecoveryClass: recovery, GapCalendar: cal, CoverageNumerator: n, CoverageDenominator: d, CoverageUnit: "交易日",
			LastRun: r.dhLastLog(tasks...), RecentFailure: r.dhRecentFailure(tasks...), Note: note,
		})
	}
	appendCountDomain("mood_pool", "涨停池/情绪温度计", r.dhMaxDate(&model.MarketMoodDaily{}, "trade_date", "market = ?", "cn"), expectedEvening, 1,
		r.dhCounts(&model.MarketMoodDaily{}, "trade_date", "cn", from, to, len(tradeDates)), "missing", "unrecoverable", "unrecoverable", "上游不可回溯；休市日不进入分母")
	appendCountDomain("pop_rank", "股吧人气榜", r.dhMaxDate(&model.PopularityRank{}, "trade_date", "market = ?", "cn"), expectedEvening, 1,
		r.dhCounts(&model.PopularityRank{}, "trade_date", "cn", from, to, len(tradeDates)), "missing", "unrecoverable", "unrecoverable", "实时榜不可回溯")
	appendCountDomain("lhb", "龙虎榜", r.dhMaxDate(&model.LhbEntry{}, "trade_date", "market = ?", "cn"), expectedPrev, 2,
		r.dhCounts(&model.LhbEntry{}, "trade_date", "cn", from, to, len(tradeDates)), "unknown", "unknown", "unknown", "无上榜记录与未采集无法仅凭本地稀疏事件表区分")

	intradayCounts := r.dhCounts(&model.IntradayFactorDaily{}, "trade_date", "cn", from, to, len(tradeDates))
	intradayCalendar, intradayN, intradayD := countCalendar(tradeDates, intradayCounts, "missing", "partial", "backfillable")
	for i := range intradayCalendar {
		if intradayCalendar[i].Status == "missing" && len(intradayCalendar)-i > 18 {
			intradayCalendar[i].RecoveryClass = "unrecoverable"
			intradayCalendar[i].Note = "超出约 18 个交易日上游回溯窗"
		}
	}
	add(DataHealthItem{
		Key: "intraday", Name: "盘中因子", ExpectedDate: expectedEvening,
		ObservedDate: r.dhMaxDate(&model.IntradayFactorDaily{}, "trade_date", "market = ?", "cn"), Tolerance: 1,
		RecoveryClass: "partial", GapCalendar: intradayCalendar, CoverageNumerator: intradayN, CoverageDenominator: intradayD, CoverageUnit: "交易日",
		Note: "近约 18 个交易日可补，超窗不可回溯",
	})

	appendCountDomain("news", "新闻采集", r.dhNewsMax(), now.Format("2006-01-02"), 1,
		r.dhNewsCounts(from, to, len(tradeDates)), "unknown", "partial", "unknown", "事件稀疏；零条不能单凭本地表判定为采集失败")
	appendCountDomain("announcements", "公告采集", r.dhMaxDate(&model.Announcement{}, "notice_date", ""), expectedPrev, 2,
		r.dhCounts(&model.Announcement{}, "notice_date", "", from, to, len(tradeDates)), "unknown", "partial", "unknown", "按需覆盖；零公告与未采集需结合任务日志判断")

	calMax := r.dhMaxDate(&model.TradingCalendar{}, "trade_date", "market = ?", "cn")
	calFrom, calTo := rep.WindowStart, rep.WindowEnd
	if calFrom == "" || calTo == "" {
		calTo = expectedPrev
		if parsed, err := time.ParseInLocation("2006-01-02", calTo, time.Local); err == nil {
			calFrom = parsed.AddDate(0, 0, -(maintenanceMaxNaturalDays - 1)).Format("2006-01-02")
		}
	}
	calCalendar, calN, calD := r.calendarCoverage(calFrom, calTo, maintenanceMaxNaturalDays)
	calItem := DataHealthItem{
		Key: "calendar", Name: "交易日历", ExpectedDate: expectedPrev, ObservedDate: calMax,
		RecoveryClass: "backfillable", GapCalendar: calCalendar, CoverageNumerator: calN, CoverageDenominator: calD, CoverageUnit: "自然日",
		LastRun: r.dhLastLog("backfill_calendar"), RecentFailure: r.dhRecentFailure("backfill_calendar"),
		Note: "休市日显示 closed 且不计作业务缺口；未来工作日节假日无来源时保持 unknown",
	}
	add(calItem)

	// 输出顺序固定，便于前端稳定 diff；防御未来调用点意外重复 key。
	seen := make(map[string]struct{}, len(rep.Items))
	filtered := rep.Items[:0]
	for _, item := range rep.Items {
		if _, ok := seen[item.Key]; ok {
			continue
		}
		seen[item.Key] = struct{}{}
		filtered = append(filtered, item)
	}
	rep.Items = filtered
	return rep
}

// 兼容旧同包调用；生产 HTTP 使用有 error 返回值的 context 入口。
func buildDataHealthReport(now time.Time, days int) *DataHealthReport {
	report, _ := buildDataHealthReportContext(context.Background(), now, days)
	return report
}

// BuildDataHealthReport 保留默认调用入口。
func BuildDataHealthReport() *DataHealthReport {
	return buildDataHealthReport(time.Now(), DataHealthDefaultDays)
}

// BuildDataHealthReportForDays 供现有 GET 的 days 查询参数使用。
func BuildDataHealthReportForDays(days int) *DataHealthReport {
	return buildDataHealthReport(time.Now(), days)
}

// fmtCoverage/fmtPctCoverage 保留给同包旧调用或测试，避免口径散落。
func fmtCoverage(done, total int64, label string) string {
	return fmt.Sprintf("%s %d/%d", label, done, total)
}

func fmtPctCoverage(f float64) string {
	return fmt.Sprintf("%.0f%%", f*100)
}

// stableDataHealthDays 供测试核对日期输出不因 map 遍历漂移。
func stableDataHealthDays(days []DataHealthDay) {
	sort.Slice(days, func(i, j int) bool { return strings.Compare(days[i].Date, days[j].Date) < 0 })
}
