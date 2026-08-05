package service

import (
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
func dhMaxDate(modelPtr any, dateCol, where string, args ...any) string {
	if common.DB == nil {
		return ""
	}
	var d sql.NullString
	q := common.DB.Model(modelPtr).Select("MAX(" + dateCol + ")")
	if where != "" {
		q = q.Where(where, args...)
	}
	if err := q.Scan(&d).Error; err != nil || !d.Valid {
		return ""
	}
	return d.String
}

func dhNewsMax() string {
	if common.DB == nil {
		return ""
	}
	var row model.News
	if err := common.DB.Select("publish_time").Order("publish_time DESC").Limit(1).Take(&row).Error; err != nil {
		return ""
	}
	return row.PublishTime.Format("2006-01-02")
}

func dhLastLog(tasks ...string) *model.DataSyncLog {
	if common.DB == nil || len(tasks) == 0 {
		return nil
	}
	var row model.DataSyncLog
	if err := common.DB.Where("task IN ?", tasks).Order("created_at DESC, id DESC").Limit(1).Take(&row).Error; err != nil {
		return nil
	}
	return &row
}

func dhRecentFailure(tasks ...string) *DataHealthFailureSummary {
	if common.DB == nil || len(tasks) == 0 {
		return nil
	}
	var row model.DataSyncLog
	if err := common.DB.Select("task", "status", "message", "created_at").
		Where("task IN ? AND status IN ?", tasks, []string{"failed", "partial"}).
		Order("created_at DESC, id DESC").Limit(1).Take(&row).Error; err != nil {
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
	if common.DB == nil {
		return nil
	}
	var dates []string
	if err := common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date <= ?", market, true, end).
		Order("trade_date DESC").Limit(days).Pluck("trade_date", &dates).Error; err != nil {
		return nil
	}
	reverseStrings(dates)
	return dates
}

type dhDateCount struct {
	Date string
	N    int64
}

func dhCounts(modelPtr any, dateCol, market, from, to string, limit int) map[string]int64 {
	out := map[string]int64{}
	if common.DB == nil || from == "" || to == "" {
		return out
	}
	var rows []dhDateCount
	q := common.DB.Model(modelPtr).Select(dateCol+" AS date, COUNT(*) AS n").
		Where(dateCol+" >= ? AND "+dateCol+" <= ?", from, to)
	if market != "" {
		q = q.Where("market = ?", market)
	}
	if err := q.Group(dateCol).Order(dateCol).Limit(limit + 1).Find(&rows).Error; err != nil || len(rows) > limit {
		return out
	}
	for _, row := range rows {
		out[row.Date] = row.N
	}
	return out
}

func dhNewsCounts(from, to string, limit int) map[string]int64 {
	out := map[string]int64{}
	if common.DB == nil || from == "" || to == "" {
		return out
	}
	end, err := time.ParseInLocation("2006-01-02", to, time.Local)
	if err != nil {
		return out
	}
	var rows []dhDateCount
	if err := common.DB.Model(&model.News{}).
		Select("DATE(publish_time) AS date, COUNT(*) AS n").
		Where("publish_time >= ? AND publish_time < ?", from+" 00:00:00", end.AddDate(0, 0, 1)).
		Group("DATE(publish_time)").Order("date").Limit(limit + 1).Find(&rows).Error; err != nil || len(rows) > limit {
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

func dhActiveBarCounts(from, to string, limit int) map[string]int64 {
	out := map[string]int64{}
	if common.DB == nil || from == "" || to == "" {
		return out
	}
	var rows []dhDateCount
	err := common.DB.Table("daily_bars AS b").
		Select("b.trade_date AS date, COUNT(*) AS n").
		Joins("JOIN stock_universe_dailies AS u ON u.market = b.market AND u.symbol = b.symbol AND u.trade_date = b.trade_date").
		Where("b.market = ? AND b.trade_date >= ? AND b.trade_date <= ? AND u.suspended = ?", "cn", from, to, false).
		Group("b.trade_date").Order("b.trade_date").Limit(limit + 1).Find(&rows).Error
	if err != nil || len(rows) > limit {
		return out
	}
	for _, row := range rows {
		out[row.Date] = row.N
	}
	return out
}

func wideGapCalendar(dates []string) ([]DataHealthDay, int64, int64) {
	if len(dates) == 0 || common.DB == nil {
		return nil, 0, 0
	}
	from, to := dates[0], dates[len(dates)-1]
	barCounts := dhCounts(&model.DailyBar{}, "trade_date", "cn", from, to, len(dates))
	activeBarCounts := dhActiveBarCounts(from, to, len(dates))
	var universeRows []dhUniverseCoverage
	common.DB.Model(&model.StockUniverseDaily{}).
		Select("trade_date AS date, COUNT(*) AS total, SUM(CASE WHEN suspended THEN 1 ELSE 0 END) AS suspended").
		Where("market = ? AND trade_date >= ? AND trade_date <= ?", "cn", from, to).
		Group("trade_date").Order("trade_date").Limit(len(dates) + 1).Find(&universeRows)
	universe := make(map[string]dhUniverseCoverage, len(universeRows))
	for _, row := range universeRows {
		universe[row.Date] = row
	}
	var currentTotal int64
	common.DB.Model(&model.MarketSyncState{}).Where("market = ?", "cn").Count(&currentTotal)
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
		} else if currentTotal > 0 {
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

func calendarCoverage(from, to string, hardMax int) ([]DataHealthDay, int64, int64) {
	if common.DB == nil || from == "" || to == "" {
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
	common.DB.Select("trade_date", "is_open").Where("market = ? AND trade_date >= ? AND trade_date <= ?", "cn", f.Format("2006-01-02"), to).
		Order("trade_date").Limit(naturalDays + 1).Find(&rows)
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

func buildDataHealthReport(now time.Time, requestedDays int) *DataHealthReport {
	days := normalizeDataHealthDays(requestedDays)
	rep := &DataHealthReport{
		GeneratedAt: now.Format("2006-01-02 15:04:05"), WindowDays: days,
		QueryHardMax: DataHealthMaxDays,
	}
	expectedWide := wideExpectedDate(now)
	expectedPrev := prevOpenTradeDate(now.Format("2006-01-02"))
	expectedEvening := expectedPrev
	if isTradingDayToday(now) && now.Hour()*60+now.Minute() >= 17*60+30 {
		expectedEvening = now.Format("2006-01-02")
	}
	tradeDates := recentOpenDates("cn", expectedWide, days)
	if len(tradeDates) > 0 {
		rep.WindowStart, rep.WindowEnd = tradeDates[0], tradeDates[len(tradeDates)-1]
		rep.WindowDays = len(tradeDates)
	}

	add := func(item DataHealthItem) {
		if item.ObservedDate == "" {
			item.LagOpenDays = -1
		} else {
			item.LagOpenDays = openDaysBehind(item.ObservedDate, item.ExpectedDate)
		}
		item.Status = dhStatus(item.ObservedDate, item.LagOpenDays, item.Tolerance)
		finalizeHealthItem(&item)
		rep.Items = append(rep.Items, item)
	}

	wideObserved := ""
	if d, err := wideFreshDate(); err == nil {
		wideObserved = d
	}
	wideCalendar, wideN, wideD := wideGapCalendar(tradeDates)
	add(DataHealthItem{
		Key: "marketwide", Name: "全市场日线", ExpectedDate: expectedWide, ObservedDate: wideObserved,
		RecoveryClass: "backfillable", GapCalendar: wideCalendar,
		CoverageNumerator: wideN, CoverageDenominator: wideD, CoverageUnit: "股票交易日",
		LastRun: dhLastLog("sync_market_wide", "init_market_history"), RecentFailure: dhRecentFailure("sync_market_wide", "init_market_history"),
		Note: "按 PIT 宇宙扣除已知停牌；PIT 缺失日按当前宇宙估计并明确标 partial",
	})

	if table := CurrentFactorTable(); table != nil {
		expected := int64(len(table.Symbols))
		fresh := int64(float64(expected) * table.FreshCoverage)
		item := DataHealthItem{
			Key: "factor_table", Name: "因子宽表", ExpectedDate: orStr(table.ExpectedDate, expectedWide), ObservedDate: table.TradeDate,
			RecoveryClass: "partial", CoverageNumerator: fresh, CoverageDenominator: expected, CoverageUnit: "标的",
			Note: "进程内当前快照，不提供伪造的历史日历；落后时先补日线再重建",
		}
		if table.TradeDate != "" {
			item.GapCalendar = []DataHealthDay{{Date: table.TradeDate, Status: "covered", Observed: fresh, Expected: expected, RecoveryClass: "partial"}}
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
			LastRun: dhLastLog(tasks...), RecentFailure: dhRecentFailure(tasks...), Note: note,
		})
	}
	appendCountDomain("mood_pool", "涨停池/情绪温度计", dhMaxDate(&model.MarketMoodDaily{}, "trade_date", "market = ?", "cn"), expectedEvening, 1,
		dhCounts(&model.MarketMoodDaily{}, "trade_date", "cn", from, to, len(tradeDates)), "missing", "unrecoverable", "unrecoverable", "上游不可回溯；休市日不进入分母")
	appendCountDomain("pop_rank", "股吧人气榜", dhMaxDate(&model.PopularityRank{}, "trade_date", "market = ?", "cn"), expectedEvening, 1,
		dhCounts(&model.PopularityRank{}, "trade_date", "cn", from, to, len(tradeDates)), "missing", "unrecoverable", "unrecoverable", "实时榜不可回溯")
	appendCountDomain("lhb", "龙虎榜", dhMaxDate(&model.LhbEntry{}, "trade_date", "market = ?", "cn"), expectedPrev, 2,
		dhCounts(&model.LhbEntry{}, "trade_date", "cn", from, to, len(tradeDates)), "unknown", "unknown", "unknown", "无上榜记录与未采集无法仅凭本地稀疏事件表区分")

	intradayCounts := dhCounts(&model.IntradayFactorDaily{}, "trade_date", "cn", from, to, len(tradeDates))
	intradayCalendar, intradayN, intradayD := countCalendar(tradeDates, intradayCounts, "missing", "partial", "backfillable")
	for i := range intradayCalendar {
		if intradayCalendar[i].Status == "missing" && len(intradayCalendar)-i > 18 {
			intradayCalendar[i].RecoveryClass = "unrecoverable"
			intradayCalendar[i].Note = "超出约 18 个交易日上游回溯窗"
		}
	}
	add(DataHealthItem{
		Key: "intraday", Name: "盘中因子", ExpectedDate: expectedEvening,
		ObservedDate: dhMaxDate(&model.IntradayFactorDaily{}, "trade_date", "market = ?", "cn"), Tolerance: 1,
		RecoveryClass: "partial", GapCalendar: intradayCalendar, CoverageNumerator: intradayN, CoverageDenominator: intradayD, CoverageUnit: "交易日",
		Note: "近约 18 个交易日可补，超窗不可回溯",
	})

	appendCountDomain("news", "新闻采集", dhNewsMax(), now.Format("2006-01-02"), 1,
		dhNewsCounts(from, to, len(tradeDates)), "unknown", "partial", "unknown", "事件稀疏；零条不能单凭本地表判定为采集失败")
	appendCountDomain("announcements", "公告采集", dhMaxDate(&model.Announcement{}, "notice_date", ""), expectedPrev, 2,
		dhCounts(&model.Announcement{}, "notice_date", "", from, to, len(tradeDates)), "unknown", "partial", "unknown", "按需覆盖；零公告与未采集需结合任务日志判断")

	calMax := dhMaxDate(&model.TradingCalendar{}, "trade_date", "market = ?", "cn")
	calFrom, calTo := rep.WindowStart, rep.WindowEnd
	if calFrom == "" || calTo == "" {
		calTo = expectedPrev
		if parsed, err := time.ParseInLocation("2006-01-02", calTo, time.Local); err == nil {
			calFrom = parsed.AddDate(0, 0, -(maintenanceMaxNaturalDays - 1)).Format("2006-01-02")
		}
	}
	calCalendar, calN, calD := calendarCoverage(calFrom, calTo, maintenanceMaxNaturalDays)
	calItem := DataHealthItem{
		Key: "calendar", Name: "交易日历", ExpectedDate: expectedPrev, ObservedDate: calMax,
		RecoveryClass: "backfillable", GapCalendar: calCalendar, CoverageNumerator: calN, CoverageDenominator: calD, CoverageUnit: "自然日",
		LastRun: dhLastLog("backfill_calendar"), RecentFailure: dhRecentFailure("backfill_calendar"),
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
