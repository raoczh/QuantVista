package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// FinanceService F1 财报日历与公告：
//   - 业绩预告/业绩快报/预约披露三类报表走 datacenter 网关，每日盘后按 NOTICE_DATE
//     增量刷新当前报告期（业绩预告全年零散发布，季度性刷新会让提醒滞后数月）；
//     报告期切换（游标里的 period 变化）时全量重拉。
//   - 公告按自选∪持仓每日增量拉取；个股详情按需实时补拉一次（冷却 1h）。
type FinanceService struct {
	em *datasource.EastMoneyAdapter

	annMu    sync.Mutex
	annFetch map[string]time.Time // 详情页按需补拉的冷却表 symbol -> 上次尝试时刻
}

func NewFinanceService() *FinanceService {
	return &FinanceService{em: datasource.NewEastMoneyAdapter(), annFetch: map[string]time.Time{}}
}

const (
	finReportForecast = "RPT_PUBLIC_OP_NEWPREDICT"
	finReportExpress  = "RPT_FCI_PERFORMANCEE"
	finReportAppoint  = "RPT_PUBLIC_BS_APPOIN"

	optFinCursorForecast = "fin_cursor_forecast"
	optFinCursorExpress  = "fin_cursor_express"
	optFinRefreshDay     = "fin_last_refresh_day"

	finPageSize       = 500
	finMaxPages       = 40        // 单类单期页数护栏（全市场预约披露约 12 页）
	annPageSize       = 30        // 每股公告单轮拉取条数
	annFetchCooldown  = time.Hour // 详情页按需补拉冷却
	annStaleDays      = 7         // P1 公告水位：最新公告老于该天数时允许按需补拉（旧记录不永久阻止更新）
	annSymbolsMax     = 80        // 每日公告采集的标的上限（自选∪持仓）
	forecastFreshDays = 7         // earn_fcst：预告发布后多少自然日内算「新预告」
)

// reportPeriodsAsOf 最近两个已结束的季度末（降序）。同时刷两期是为覆盖
// 年报（上年 12-31）与一季报（3-31）在 1~4 月并行披露的重叠季。纯函数可测。
func reportPeriodsAsOf(t time.Time) []string {
	quarterEnds := func(year int) []time.Time {
		return []time.Time{
			time.Date(year, 3, 31, 0, 0, 0, 0, time.Local),
			time.Date(year, 6, 30, 0, 0, 0, 0, time.Local),
			time.Date(year, 9, 30, 0, 0, 0, 0, time.Local),
			time.Date(year, 12, 31, 0, 0, 0, 0, time.Local),
		}
	}
	var ends []time.Time
	ends = append(ends, quarterEnds(t.Year()-1)...)
	ends = append(ends, quarterEnds(t.Year())...)
	var past []string
	for i := len(ends) - 1; i >= 0 && len(past) < 2; i-- {
		if ends[i].Before(t) {
			past = append(past, ends[i].Format("2006-01-02"))
		}
	}
	return past
}

// isSixDigits A 股 6 位纯数字代码口径（含北交所）。
func isSixDigits(s string) bool {
	if len(s) != 6 {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// --- 三类报表刷新 ---

// RefreshAll 每日盘后主流程：预告/快报增量 + 预约披露全量（当前两期）。
// 单类失败不阻断其余（上游偶发限流），返回聚合错误供日志。
func (s *FinanceService) RefreshAll(ctx context.Context) error {
	periods := reportPeriodsAsOf(time.Now())
	var errs []string
	if n, err := s.refreshIncremental(ctx, finReportForecast, optFinCursorForecast, periods, s.upsertForecastRows); err != nil {
		errs = append(errs, "业绩预告: "+err.Error())
	} else if n > 0 {
		common.SysLog("业绩预告刷新 %d 行", n)
	}
	if n, err := s.refreshIncremental(ctx, finReportExpress, optFinCursorExpress, periods, s.upsertExpressRows); err != nil {
		errs = append(errs, "业绩快报: "+err.Error())
	} else if n > 0 {
		common.SysLog("业绩快报刷新 %d 行", n)
	}
	if n, err := s.refreshAppoint(ctx, periods); err != nil {
		errs = append(errs, "预约披露: "+err.Error())
	} else if n > 0 {
		common.SysLog("预约披露刷新 %d 行", n)
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "；"))
	}
	return nil
}

// refreshIncremental 预告/快报的增量刷新：游标存 "period|date"。
// period 未变时 filter 加 NOTICE_DATE >= 游标日-1（重叠 1 天防同日晚间发布漏采，
// 重复行由 upsert 幂等吸收）；period 变化（报告期切换）全量重拉。
func (s *FinanceService) refreshIncremental(ctx context.Context, report, cursorKey string, periods []string, upsert func([]datasource.DcRow, string) (int, error)) (int, error) {
	if len(periods) == 0 {
		return 0, nil
	}
	curPeriod, curDate := readFinCursor(cursorKey)
	since := ""
	if curPeriod == periods[0] && curDate != "" {
		if d, err := time.ParseInLocation("2006-01-02", curDate, time.Local); err == nil {
			since = d.AddDate(0, 0, -1).Format("2006-01-02")
		}
	}
	total := 0
	for _, period := range periods {
		filter := fmt.Sprintf("(REPORT_DATE='%s')", period)
		if since != "" {
			filter += fmt.Sprintf("(NOTICE_DATE>='%s')", since)
		}
		n, err := s.pullReport(ctx, datasource.DataCenterQuery{
			ReportName: report, Filter: filter,
			SortColumns: "NOTICE_DATE", SortTypes: "-1", PageSize: finPageSize,
		}, period, upsert)
		if err != nil {
			return total, err
		}
		total += n
	}
	writeFinCursor(cursorKey, periods[0], time.Now().Format("2006-01-02"))
	return total, nil
}

// refreshAppoint 预约披露：无 NOTICE_DATE 可增量，但全市场一期约 12 页，
// 每日全量重拉当前两期 upsert（三次变更由 APPOINT_PUBLISH_DATE 自动合并）。
func (s *FinanceService) refreshAppoint(ctx context.Context, periods []string) (int, error) {
	total := 0
	for _, period := range periods {
		n, err := s.pullReport(ctx, datasource.DataCenterQuery{
			ReportName:  finReportAppoint,
			Filter:      fmt.Sprintf("(REPORT_DATE='%s')", period),
			SortColumns: "SECURITY_CODE", SortTypes: "1", PageSize: finPageSize,
		}, period, s.upsertAppointRows)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// pullReport 走 datacenter 迭代器逐页拉取并 upsert。ErrNoData 视为正常空轮。
func (s *FinanceService) pullReport(ctx context.Context, q datasource.DataCenterQuery, period string, upsert func([]datasource.DcRow, string) (int, error)) (int, error) {
	it := s.em.DataCenterQuery(q)
	total := 0
	for page := 0; page < finMaxPages; page++ {
		raws, err := it.Next(ctx)
		if err != nil {
			if errors.Is(err, datasource.ErrNoData) {
				return total, nil
			}
			return total, err
		}
		if raws == nil {
			return total, nil
		}
		rows := make([]datasource.DcRow, 0, len(raws))
		for _, raw := range raws {
			if row, perr := datasource.ParseDcRow(raw); perr == nil {
				rows = append(rows, row)
			}
		}
		n, err := upsert(rows, period)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

func (s *FinanceService) upsertForecastRows(rows []datasource.DcRow, period string) (int, error) {
	recs := make([]model.EarningsForecast, 0, len(rows))
	for _, r := range rows {
		sym := r.String("SECURITY_CODE")
		if !isSixDigits(sym) {
			continue
		}
		recs = append(recs, model.EarningsForecast{
			Symbol: sym, Market: "cn", ReportDate: r.Date("REPORT_DATE"),
			Name:        r.String("SECURITY_NAME_ABBR"),
			NoticeDate:  r.Date("NOTICE_DATE"),
			PredictType: r.String("PREDICT_TYPE"), PredictFinance: r.String("PREDICT_FINANCE"),
			AmtLower: r.Float("PREDICT_AMT_LOWER"), AmtUpper: r.Float("PREDICT_AMT_UPPER"),
			AmpLower: r.Float("ADD_AMP_LOWER"), AmpUpper: r.Float("ADD_AMP_UPPER"),
			Content: truncateRunes(r.String("PREDICT_CONTENT"), 500),
			Reason:  truncateRunes(r.String("CHANGE_REASON_EXPLAIN"), 1000),
		})
	}
	return upsertFinanceRows(recs, []string{"name", "notice_date", "predict_type", "predict_finance",
		"amt_lower", "amt_upper", "amp_lower", "amp_upper", "content", "reason", "updated_at"})
}

func (s *FinanceService) upsertExpressRows(rows []datasource.DcRow, period string) (int, error) {
	recs := make([]model.EarningsExpress, 0, len(rows))
	for _, r := range rows {
		sym := r.String("SECURITY_CODE")
		if !isSixDigits(sym) {
			continue
		}
		recs = append(recs, model.EarningsExpress{
			Symbol: sym, Market: "cn", ReportDate: r.Date("REPORT_DATE"),
			Name:       r.String("SECURITY_NAME_ABBR"),
			NoticeDate: r.Date("NOTICE_DATE"),
			EPS:        r.Float("BASIC_EPS"),
			Revenue:    r.Float("TOTAL_OPERATE_INCOME"), RevenueYoY: r.Float("YSTZ"),
			NetProfit: r.Float("PARENT_NETPROFIT"), NetProfitYoY: r.Float("JLRTBZCL"),
			ROE: r.Float("WEIGHTAVG_ROE"), DataType: r.String("DATATYPE"),
		})
	}
	// 注意物理列名：GORM 把 YoY 转成 yo_y（revenue_yo_y/net_profit_yo_y），
	// 此处必须用物理列名，否则冲突更新路径 SQL 报错、快报修正永远覆盖不进去。
	return upsertFinanceRows(recs, []string{"name", "notice_date", "eps", "revenue", "revenue_yo_y",
		"net_profit", "net_profit_yo_y", "roe", "data_type", "updated_at"})
}

func (s *FinanceService) upsertAppointRows(rows []datasource.DcRow, period string) (int, error) {
	recs := make([]model.DisclosureSchedule, 0, len(rows))
	for _, r := range rows {
		sym := r.String("SECURITY_CODE")
		if !isSixDigits(sym) {
			continue
		}
		recs = append(recs, model.DisclosureSchedule{
			Symbol: sym, Market: "cn", ReportDate: r.Date("REPORT_DATE"),
			Name:           r.String("SECURITY_NAME_ABBR"),
			AppointDate:    r.Date("APPOINT_PUBLISH_DATE"),
			FirstDate:      r.Date("FIRST_APPOINT_DATE"),
			ActualDate:     r.Date("ACTUAL_PUBLISH_DATE"),
			ReportTypeName: r.String("REPORT_TYPE_NAME"),
			IsPublished:    r.String("IS_PUBLISH") == "1",
		})
	}
	return upsertFinanceRows(recs, []string{"name", "appoint_date", "first_date", "actual_date",
		"report_type_name", "is_published", "updated_at"})
}

// upsertFinanceRows 泛型批量 upsert：唯一键 (symbol, market, report_date) 冲突时更新数据列。
func upsertFinanceRows[T any](recs []T, updateCols []string) (int, error) {
	if len(recs) == 0 || common.DB == nil {
		return 0, nil
	}
	res := common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "report_date"}},
		DoUpdates: clause.AssignmentColumns(updateCols),
	}).CreateInBatches(recs, 200)
	if res.Error != nil {
		return 0, res.Error
	}
	return len(recs), nil
}

func readFinCursor(key string) (period, date string) {
	if common.DB == nil {
		return "", ""
	}
	var opt model.Option
	if err := common.DB.Where("`key` = ?", key).First(&opt).Error; err != nil {
		return "", ""
	}
	parts := strings.SplitN(opt.Value, "|", 2)
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func writeFinCursor(key, period, date string) {
	if err := model.UpsertOption(key, period+"|"+date); err != nil {
		common.SysWarn("写财报游标失败: %v", err)
	}
}

// --- 公告采集与查询 ---

// CollectAnnouncements 自选∪持仓 distinct 标的逐只拉最新一页公告，(symbol, art_code)
// 冲突静默忽略即天然增量。单只失败跳过（公告源稳定，无需整轮降级）。
func (s *FinanceService) CollectAnnouncements(ctx context.Context) (inserted int) {
	if common.DB == nil {
		return 0
	}
	var syms []string
	// 已平仓持仓排除（不再采其公告）；稳定排序保证 LIMIT 截断可复现。
	common.DB.Raw(`SELECT DISTINCT symbol FROM (
		SELECT symbol FROM watchlist_items
		UNION SELECT symbol FROM positions WHERE status = 'holding'
	) t ORDER BY symbol LIMIT ?`, annSymbolsMax).Scan(&syms)
	for _, sym := range syms {
		if !isSixDigits(sym) {
			continue
		}
		inserted += s.fetchAnnouncements(ctx, sym)
		select {
		case <-ctx.Done():
			return inserted
		case <-time.After(500 * time.Millisecond): // 节流，敬畏免费源
		}
	}
	return inserted
}

// fetchAnnouncements 拉单只公告落库，返回新插入条数。
func (s *FinanceService) fetchAnnouncements(ctx context.Context, symbol string) int {
	items, err := datasource.GetEMAnnouncements(ctx, symbol, annPageSize)
	if err != nil {
		if !errors.Is(err, datasource.ErrNoData) {
			common.SysDebug("公告采集跳过 %s: %v", symbol, err)
		}
		return 0
	}
	inserted := 0
	for _, it := range items {
		rec := model.Announcement{
			Symbol: it.Symbol, Market: "cn", ArtCode: it.ArtCode, Name: it.Name,
			Title: truncateRunes(it.Title, 500), NoticeType: truncateRunes(it.NoticeType, 60),
			NoticeDate: it.NoticeDate.Format("2006-01-02"), URL: it.URL,
		}
		res := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&rec)
		if res.Error == nil && res.RowsAffected > 0 {
			inserted++
		}
	}
	return inserted
}

// ListAnnouncements 个股公告查询（详情页「公告」块）。库中该股无记录、或最新公告已老于
// annStaleDays（P1：有旧记录不能永久阻止更新——一年前的旧公告会让该股公告面永远停更）
// 时按需实时补拉一次（冷却 1h，防详情页被刷打上游），采集范围外的股也能看到公告。
func (s *FinanceService) ListAnnouncements(ctx context.Context, symbol string, limit int) ([]model.Announcement, error) {
	symbol = strings.TrimSpace(symbol)
	if !isSixDigits(symbol) {
		return []model.Announcement{}, nil // 非 A 股口径自然为空（同 N1 新闻先例）
	}
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	var cnt int64
	common.DB.Model(&model.Announcement{}).Where("symbol = ?", symbol).Count(&cnt)
	needFetch := cnt == 0
	if !needFetch {
		var latest sql.NullString
		common.DB.Model(&model.Announcement{}).Where("symbol = ?", symbol).
			Select("MAX(notice_date)").Scan(&latest)
		if latest.Valid && latest.String != "" &&
			latest.String < time.Now().AddDate(0, 0, -annStaleDays).Format("2006-01-02") {
			needFetch = true
		}
	}
	if needFetch && s.annFetchAllowed(symbol) {
		fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		s.fetchAnnouncements(fctx, symbol)
		cancel()
	}
	var rows []model.Announcement
	err := common.DB.Where("symbol = ?", symbol).
		Order("notice_date DESC, id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// annFetchAllowed 冷却检查：同一 symbol 1h 内只允许一次按需补拉。
func (s *FinanceService) annFetchAllowed(symbol string) bool {
	s.annMu.Lock()
	defer s.annMu.Unlock()
	if t, ok := s.annFetch[symbol]; ok && time.Since(t) < annFetchCooldown {
		return false
	}
	s.annFetch[symbol] = time.Now()
	return true
}

// latestAnnouncementBriefs 某标的最近 limit 条公告（标题+类型+日期），供个股分析/问答
// prompt 注入（AI 证据池的文本型合法来源）。best-effort：无 DB / 无公告返回空。
type announcementBrief struct {
	Title string `json:"title"`
	Type  string `json:"type,omitempty"`
	Date  string `json:"date"`
}

func latestAnnouncementBriefs(symbol string, limit int) []announcementBrief {
	if common.DB == nil || !isSixDigits(symbol) {
		return nil
	}
	var rows []model.Announcement
	if err := common.DB.Select("title, notice_type, notice_date").
		Where("symbol = ?", symbol).
		Order("notice_date DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]announcementBrief, 0, len(rows))
	for _, a := range rows {
		out = append(out, announcementBrief{Title: a.Title, Type: a.NoticeType, Date: a.NoticeDate})
	}
	return out
}

// announcementTitleTexts 从个股快照的 announcements 块提取标题（信任层：公告标题里的
// 小数是文本型合法来源，须并入证据核验值域——同 N2 新闻标题前例）。
func announcementTitleTexts(snapshot map[string]any) []string {
	blk, ok := snapshot["announcements"].(map[string]any)
	if !ok {
		return nil
	}
	if items, ok := blk["items"].([]announcementBrief); ok {
		out := make([]string, 0, len(items))
		for _, it := range items {
			out = append(out, it.Title)
		}
		return out
	}
	// 快照经 JSON 反序列化后（问答复用落库快照）结构是 []any/map[string]any。
	arr, ok := blk["items"].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if m, ok := v.(map[string]any); ok {
			if t, ok := m["title"].(string); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// --- 财报日历消费查询 ---

// UpcomingDisclosure 某标的最近一个未披露的预约披露安排（earn_date 提醒评估用）。
// 取 appoint_date >= today 且未实际披露的最早一条；无则返回 nil。
func UpcomingDisclosure(symbol string, today string) *model.DisclosureSchedule {
	if common.DB == nil {
		return nil
	}
	var row model.DisclosureSchedule
	err := common.DB.Where("symbol = ? AND appoint_date >= ? AND is_published = ?", symbol, today, false).
		Order("appoint_date ASC").First(&row).Error
	if err != nil {
		return nil
	}
	return &row
}

// LatestForecast 某标的最新一条业绩预告（earn_fcst 提醒评估用）。
func LatestForecast(symbol string) *model.EarningsForecast {
	if common.DB == nil {
		return nil
	}
	var row model.EarningsForecast
	err := common.DB.Where("symbol = ?", symbol).
		Order("notice_date DESC, id DESC").First(&row).Error
	if err != nil {
		return nil
	}
	return &row
}

// TomorrowDisclosures 自选∪持仓中次日预约披露的标的文案（日报「明日披露名单」）。
func TomorrowDisclosures(userID int64, tomorrow string) []string {
	if common.DB == nil {
		return nil
	}
	var syms []string
	// 已平仓持仓排除：明日披露名单直接面向用户，不该出现已清仓标的。
	common.DB.Raw(`SELECT DISTINCT symbol FROM (
		SELECT symbol FROM watchlist_items WHERE user_id = ?
		UNION SELECT symbol FROM positions WHERE user_id = ? AND status = 'holding'
	) t`, userID, userID).Scan(&syms)
	if len(syms) == 0 {
		return nil
	}
	var rows []model.DisclosureSchedule
	if err := common.DB.Where("symbol IN ? AND appoint_date = ? AND is_published = ?", syms, tomorrow, false).
		Order("symbol ASC").Limit(30).Find(&rows).Error; err != nil {
		return nil
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		name := r.Name
		if name == "" {
			name = r.Symbol
		}
		out = append(out, fmt.Sprintf("%s(%s) 明日预约披露 %s", name, r.Symbol, r.ReportTypeName))
	}
	return out
}

// --- 后台任务 ---

// StartFinanceJobs 每日盘后 19:05 刷新三类财报数据 + 采集公告 + 财报类提醒每日一评
// （盘中 15min 行情评估显式排除财报类 kind，见 alert.go）。启动 2 分钟后若当日尚未
// 跑过则补一轮（首次部署全量建库；重启幂等靠游标与 upsert）。
func StartFinanceJobs(mgr *datasource.Manager) *FinanceService {
	svc := NewFinanceService()
	alertSvc := NewAlertService(NewMarketService(mgr))
	run := func() {
		if common.DB == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		refreshErr := svc.RefreshAll(ctx)
		if refreshErr != nil {
			common.SysWarn("财报数据刷新部分失败: %v", refreshErr)
		}
		if n := svc.CollectAnnouncements(ctx); n > 0 {
			common.SysLog("公告采集入库: %d", n)
		}
		if n := alertSvc.EvaluateEarningsAll(); n > 0 {
			common.SysLog("财报类提醒命中 %d 条", n)
		}
		// 仅在三类财报刷新全部成功才推进「今日已刷新」游标：部分失败时不写，
		// 当天重启/下轮补跑守卫自然放行重试（每类游标本身也只在成功时推进）。
		if refreshErr == nil {
			_ = model.UpsertOption(optFinRefreshDay, time.Now().Format("2006-01-02"))
		}
	}
	go func() {
		time.Sleep(2 * time.Minute)
		var opt model.Option
		if common.DB != nil {
			if err := common.DB.Where("`key` = ?", optFinRefreshDay).First(&opt).Error; err != nil ||
				opt.Value != time.Now().Format("2006-01-02") {
				run()
			}
		}
		for {
			now := time.Now()
			next := time.Date(now.Year(), now.Month(), now.Day(), 19, 5, 0, 0, now.Location())
			if !next.After(now) {
				next = next.Add(24 * time.Hour)
			}
			time.Sleep(time.Until(next))
			run()
		}
	}()
	return svc
}
