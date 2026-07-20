package service

import (
	"database/sql"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// P1 数据健康总览（管理端）：各数据域的 expected/observed 日期、落后开市日数、覆盖率、
// 最近任务日志——「observed 只和库内自身 MAX 比较会把整库落后判成新鲜」的对账入口。
// 只读聚合，补跑走既有管理端接口（wide-sync / wide-init / sync-bars / snapshot /
// factor-rebuild / backfill-calendar）。

// DataHealthItem 一个数据域的健康行。
type DataHealthItem struct {
	Key          string             `json:"key"`
	Name         string             `json:"name"`
	ExpectedDate string             `json:"expected_date"`      // 按交易日历应有的最新日期
	ObservedDate string             `json:"observed_date"`      // 库内实际最新日期（空=无数据）
	LagOpenDays  int                `json:"lag_open_days"`      // 落后开市日数（-1=日历不可用无法判定）
	Tolerance    int                `json:"tolerance_open_days"` // 该域的容忍落后（T-1 类信号为 1~2）
	Status       string             `json:"status"`             // ok / behind / empty / unknown
	Coverage     string             `json:"coverage,omitempty"` // 覆盖率描述（如 done/total、fresh 行占比）
	LastRun      *model.DataSyncLog `json:"last_run,omitempty"` // 最近一次相关任务日志
	Note         string             `json:"note,omitempty"`
}

// DataHealthReport 数据健康总览。
type DataHealthReport struct {
	GeneratedAt string           `json:"generated_at"`
	Items       []DataHealthItem `json:"items"`
}

// dhMaxDate 取某表某日期列的 MAX（best-effort，查询失败返回空）。
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

// dhLastLog 最近一条指定任务日志。
func dhLastLog(task string) *model.DataSyncLog {
	if common.DB == nil {
		return nil
	}
	var l model.DataSyncLog
	if err := common.DB.Where("task = ?", task).Order("id DESC").First(&l).Error; err != nil {
		return nil
	}
	return &l
}

// dhStatus 按 observed/lag/tolerance 判定状态。
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

// BuildDataHealthReport 聚合各数据域健康行（读时计算，无缓存——管理端低频操作）。
func BuildDataHealthReport() *DataHealthReport {
	now := time.Now()
	rep := &DataHealthReport{GeneratedAt: now.Format("2006-01-02 15:04:05")}
	expectedWide := wideExpectedDate(now)          // 16:30 后=今日（交易日），否则上一开市日
	expectedPrev := prevOpenTradeDate(now.Format("2006-01-02")) // 恒为上一开市日（T-1 口径域）
	// 盘后采集类（mood 16:35 / intraday 16:00 / lhb 晚间）：当日 17:30 前按 T-1 口径对账。
	expectedEvening := expectedPrev
	if isTradingDayToday(now) && now.Hour()*60+now.Minute() >= 17*60+30 {
		expectedEvening = now.Format("2006-01-02")
	}

	add := func(key, name, observed, expected string, tolerance int, coverage string, lastRun *model.DataSyncLog, note string) {
		lag := 0
		if observed == "" {
			lag = -1
		} else {
			lag = openDaysBehind(observed, expected)
		}
		rep.Items = append(rep.Items, DataHealthItem{
			Key: key, Name: name,
			ExpectedDate: expected, ObservedDate: observed,
			LagOpenDays: lag, Tolerance: tolerance,
			Status:   dhStatus(observed, lag, tolerance),
			Coverage: coverage, LastRun: lastRun, Note: note,
		})
	}

	// 1) 全市场日线（M1 宇宙）。
	wideObserved := ""
	if d, err := wideFreshDate(); err == nil {
		wideObserved = d
	}
	wideCoverage := ""
	if common.DB != nil {
		var total, done int64
		common.DB.Model(&model.MarketSyncState{}).Where("market = ?", "cn").Count(&total)
		common.DB.Model(&model.MarketSyncState{}).Where("market = ? AND init_status = ?", "cn", "done").Count(&done)
		if total > 0 {
			wideCoverage = fmtCoverage(done, total, "已建史")
		}
	}
	add("marketwide", "全市场日线", wideObserved, expectedWide, 0, wideCoverage,
		dhLastLog("sync_market_wide"), "16:10 增量 job；落后时用管理端「全市场增量同步」补跑")

	// 2) 因子宽表（选股/策略信号地基）。
	if t := CurrentFactorTable(); t != nil {
		cov := fmtPctCoverage(t.FreshCoverage) + " fresh 行"
		add("factor_table", "因子宽表", t.TradeDate, orStr(t.ExpectedDate, expectedWide), 0, cov, nil,
			"构建于 "+t.BuiltAt.Format("01-02 15:04")+"；落后时先补全市场日线再「重建因子宽表」")
	} else {
		add("factor_table", "因子宽表", "", expectedWide, 0, "", nil, "进程内尚未构建（首次扫描/推荐会触发懒加载）")
	}

	// 3) 情绪温度计（涨停池，16:35 采集）。
	add("mood_pool", "涨停池/情绪温度计", dhMaxDate(&model.MarketMoodDaily{}, "trade_date", "market = ?", "cn"),
		expectedEvening, 1, "", nil, "16:35 job；上游不可回溯，错过窗口该日数据缺失")

	// 4) 人气榜（实时榜，交易日当日）。
	add("pop_rank", "股吧人气榜", dhMaxDate(&model.PopularityRank{}, "trade_date", "market = ?", "cn"),
		expectedEvening, 1, "", nil, "实时榜不可回溯")

	// 5) 龙虎榜（T-1 晚间披露）。
	add("lhb", "龙虎榜", dhMaxDate(&model.LhbEntry{}, "trade_date", "market = ?", "cn"),
		expectedPrev, 2, "", nil, "披露有延迟，容忍 2 个开市日")

	// 6) 盘中因子（M3b，T-1 口径）。
	add("intraday", "盘中因子", dhMaxDate(&model.IntradayFactorDaily{}, "trade_date", "market = ?", "cn"),
		expectedEvening, 1, "", nil, "5 分钟线派生；上游可回溯约 18 个交易日")

	// 7) 新闻（publish_time 按自然日）。
	newsMax := dhMaxDate(&model.News{}, "DATE(publish_time)", "")
	add("news", "新闻采集", newsMax, now.Format("2006-01-02"), 1, "", nil,
		"采集间隔见系统设置；observed 为最新一条发布日期")

	// 8) 公告（notice_date）。
	add("announcements", "公告采集", dhMaxDate(&model.Announcement{}, "notice_date", ""),
		expectedPrev, 2, "", nil, "自选∪持仓每日增量 + 详情页按需补拉")

	// 9) 交易日历。健康口径=历史覆盖到最近开市日（新鲜度判定只回看，「回填日历」
	// 按钮可修复）；未来覆盖只作提示不作告警——未来工作日节假日无公开数据源，
	// 回填按钮只能预写周末休市，把「未来覆盖不足」设为 behind 会是永远修不掉的
	// 假告警（第二轮复查：告警必须能被现有按钮修复）。
	calMax := dhMaxDate(&model.TradingCalendar{}, "trade_date", "market = ?", "cn")
	calStatus := "ok"
	calNote := "决定所有新鲜度判定的口径；历史缺失时用「回填日历」补"
	switch {
	case calMax == "":
		calStatus = "empty"
	case calMax < expectedPrev:
		calStatus = "behind"
		calNote = "日历未覆盖到最近开市日（" + expectedPrev + "），新鲜度判定退化为周一~五近似，请回填"
	case calMax < now.AddDate(0, 0, 14).Format("2006-01-02"):
		calNote = "历史覆盖正常。提示：未来覆盖仅到 " + calMax + "（回填可预写未来周末休市；工作日节假日无公开数据源，长假当日的新鲜度判定会按周一~五近似产生 stale 误报，属已知诚实退化）"
	}
	rep.Items = append(rep.Items, DataHealthItem{
		Key: "calendar", Name: "交易日历",
		ExpectedDate: expectedPrev, ObservedDate: calMax,
		Status: calStatus, Note: calNote,
	})
	return rep
}

func fmtCoverage(done, total int64, label string) string {
	return fmt.Sprintf("%s %d/%d", label, done, total)
}

func fmtPctCoverage(f float64) string {
	return fmt.Sprintf("%.0f%%", f*100)
}
