package service

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// 阶段 2 市场数据补全：日线批量同步、交易日历回填、市场情绪快照，以及驱动它们的后台任务。

const (
	// 批量拉取时对每只股票之间的节流间隔，避免短时间打爆免费数据源。
	syncThrottle = 300 * time.Millisecond
	// 单次批量同步的安全上限，防止 DB 里意外堆积过多标的时任务失控。
	syncMaxStocks = 800
	// 交易日历回填的回看条数（≈4 年交易日）。
	calendarLookback = 1000
)

// ErrSyncInProgress 已有一轮日线批量同步在跑，拒绝并发重入（免打爆数据源）。
var ErrSyncInProgress = errors.New("已有批量同步任务在进行中")

// syncBarsRunning 保证同一时刻只有一轮批量日线同步（后台定时与手动触发共用）。
var syncBarsRunning atomic.Bool

// syncCursor 批量同步的轮转游标（进程内）：记录上一轮同步到的 stocks.id。
// 无游标时每轮都取同一前缀，标的数超过 syncMaxStocks（或任务中途超时取消）时
// 尾部标的永远轮不到；游标让各轮从上次断点继续、扫到尾自动回绕。
var syncCursor atomic.Int64

// SyncTrackedDailyBars 批量同步"已跟踪"股票（DB stocks 表内、即用户查过/持有的标的）的日线。
// 这是"全市场日线批量同步"的个人自用版：不主动抓全 5000 只（会长时间打免费源），
// 而是覆盖用户实际关心的标的；同步结果写入 data_sync_logs 供审计。
func (s *MarketService) SyncTrackedDailyBars(ctx context.Context, market string, barLimit int) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !syncBarsRunning.CompareAndSwap(false, true) {
		return nil, ErrSyncInProgress
	}
	defer syncBarsRunning.Store(false)

	if barLimit <= 0 || barLimit > 1000 {
		barLimit = 120
	}
	start := time.Now()

	fetch := func(afterID int64, limit int) ([]model.Stock, error) {
		var rows []model.Stock
		q := common.DB.Model(&model.Stock{}).Where("id > ?", afterID).Order("id")
		if market != "" {
			q = q.Where("market = ?", market)
		}
		err := q.Limit(limit).Find(&rows).Error
		return rows, err
	}
	stocks, err := fetch(syncCursor.Load(), syncMaxStocks)
	if err != nil {
		return nil, err
	}
	// 到尾回绕：从头补齐剩余额度。
	if len(stocks) < syncMaxStocks {
		head, err := fetch(0, syncMaxStocks-len(stocks))
		if err == nil {
			stocks = append(stocks, head...)
		}
	}

	log := &model.DataSyncLog{Task: "sync_daily_bars", Market: market, Total: len(stocks)}
	var firstErr string
	for _, st := range stocks {
		select {
		case <-ctx.Done():
			log.Message = truncate("任务取消: "+ctx.Err().Error(), 512)
			log.DurationMs = time.Since(start).Milliseconds()
			log.Status = statusOf(log)
			s.recordSyncLog(log)
			return log, ctx.Err()
		default:
		}
		if _, err := s.GetDailyBars(ctx, st.Market, st.Symbol, barLimit); err != nil {
			log.Failed++
			if firstErr == "" {
				firstErr = st.Symbol + ": " + err.Error()
			}
		} else {
			log.Succeeded++
		}
		syncCursor.Store(st.ID) // 中途取消也从断点续跑
		time.Sleep(syncThrottle)
	}

	log.DurationMs = time.Since(start).Milliseconds()
	log.Message = truncate(firstErr, 512)
	log.Status = statusOf(log)
	s.recordSyncLog(log)
	return log, nil
}

// BackfillCalendar 回填交易日历：用上证指数日线得到开市日集合，
// 再把区间内其余日期（周末/节假日）补为休市日（is_open=false），形成完整日历。
func (s *MarketService) BackfillCalendar(ctx context.Context, market string) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	start := time.Now()
	log := &model.DataSyncLog{Task: "backfill_calendar", Market: market}

	days, err := s.mgr.GetTradingDays(ctx, market, calendarLookback)
	if err != nil {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}

	open := make(map[string]struct{}, len(days))
	var minDate, maxDate string
	for _, d := range days {
		open[d] = struct{}{}
		if minDate == "" || d < minDate {
			minDate = d
		}
		if d > maxDate {
			maxDate = d
		}
	}
	from, err1 := time.ParseInLocation("2006-01-02", minDate, time.Local)
	to, err2 := time.ParseInLocation("2006-01-02", maxDate, time.Local)
	if err1 != nil || err2 != nil {
		log.Status = "failed"
		log.Message = "交易日日期解析失败"
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, errors.New(log.Message)
	}

	rows := make([]model.TradingCalendar, 0, 512)
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		ds := d.Format("2006-01-02")
		_, isOpen := open[ds]
		rows = append(rows, model.TradingCalendar{Market: market, TradeDate: ds, IsOpen: isOpen})
	}

	// 显式 Select 强制写入 is_open，即使历史 DB 列上仍残留 default:true 也不会漏写休市日。
	if err := common.DB.
		Select("Market", "TradeDate", "IsOpen").
		Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
		}).CreateInBatches(rows, 200).Error; err != nil {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}

	log.Total = len(rows)
	log.Succeeded = len(days) // 开市日数
	log.Status = "success"
	log.Message = truncate(minDate+" ~ "+maxDate+" 共 "+strconv.Itoa(len(rows))+" 天（开市 "+strconv.Itoa(len(days))+"）", 512)
	log.DurationMs = time.Since(start).Milliseconds()
	s.recordSyncLog(log)
	return log, nil
}

// SnapshotMarket 拉取当前涨跌家数并落库为一条市场情绪快照，形成历史序列。
// 与上一条完全相同（同交易日且各家数未变，典型如收盘后）则跳过，避免非交易时段堆积重复行。
func (s *MarketService) SnapshotMarket(ctx context.Context, market string) (*model.MarketSnapshot, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	b, err := s.mgr.GetBreadth(ctx, market)
	if err != nil {
		return nil, err
	}
	snap := &model.MarketSnapshot{
		Market:    market,
		TradeDate: b.TradeDate,
		Advances:  b.Advances,
		Declines:  b.Declines,
		Unchanged: b.Unchanged,
		LimitUp:   b.LimitUp,
		LimitDown: b.LimitDown,
		Source:    b.Source,
		DataTime:  b.DataTime,
	}
	if last, err := s.LatestSnapshot(market); err == nil && last != nil && sameBreadth(last, snap) {
		return last, nil // 数据未变，复用上一条
	}
	if err := common.DB.Create(snap).Error; err != nil {
		return nil, err
	}
	return snap, nil
}

// sameBreadth 判断两条快照的涨跌家数是否一致（用于去重）。
func sameBreadth(a, b *model.MarketSnapshot) bool {
	return a.TradeDate == b.TradeDate &&
		a.Advances == b.Advances && a.Declines == b.Declines && a.Unchanged == b.Unchanged &&
		a.LimitUp == b.LimitUp && a.LimitDown == b.LimitDown
}

// LatestSnapshot 返回某市场最近一条情绪快照（无则 nil）。
func (s *MarketService) LatestSnapshot(market string) (*model.MarketSnapshot, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var snap model.MarketSnapshot
	err := common.DB.Where("market = ?", market).Order("data_time DESC").First(&snap).Error
	if err != nil {
		return nil, err
	}
	return &snap, nil
}

// RecentSyncLogs 返回最近的数据同步任务日志（供管理员排查数据缺口）。
func (s *MarketService) RecentSyncLogs(limit int) ([]model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var logs []model.DataSyncLog
	err := common.DB.Order("created_at DESC").Limit(limit).Find(&logs).Error
	return logs, err
}

func (s *MarketService) recordSyncLog(log *model.DataSyncLog) {
	if common.DB == nil || log == nil {
		return
	}
	if err := common.DB.Create(log).Error; err != nil {
		common.SysWarn("写 data_sync_logs 失败: %v", err)
	}
}

// statusOf 依据成功/失败计数判定同步状态。
func statusOf(log *model.DataSyncLog) string {
	switch {
	case log.Total == 0:
		return "success"
	case log.Failed == 0:
		return "success"
	case log.Succeeded == 0:
		return "failed"
	default:
		return "partial"
	}
}

// StartMarketJobs 启动市场数据后台任务：
//   - 启动时若日历为空则回填一次；
//   - 每 10 分钟落一条市场情绪快照（数据源不可用时静默跳过）；
//   - 每 6 小时批量同步已跟踪股票日线。
//
// 均为个人自用低频任务，失败仅记日志不影响主流程。
func StartMarketJobs(mgr *datasource.Manager) {
	svc := NewMarketService(mgr)
	const market = "cn"

	// 启动时：日历为空才回填（避免每次重启都全量刷）。
	go func() {
		if common.DB == nil {
			return
		}
		var n int64
		common.DB.Model(&model.TradingCalendar{}).Where("market = ?", market).Count(&n)
		if n > 0 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := svc.BackfillCalendar(ctx, market); err != nil {
			common.SysWarn("启动回填交易日历失败: %v", err)
		} else {
			common.SysLog("启动回填交易日历完成")
		}
	}()

	// 市场情绪快照：每 10 分钟一次。
	go func() {
		snapshot := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if _, err := svc.SnapshotMarket(ctx, market); err != nil {
				common.SysDebug("市场情绪快照跳过（数据源不可用）: %v", err)
			}
		}
		snapshot()
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			snapshot()
		}
	}()

	// 已跟踪股票日线批量同步：每 6 小时一次。
	// 超时预算：800 只 × (300ms 节流 + 抓取) 最坏可到 20 分钟以上，给足 30 分钟；
	// 即便中途取消，游标也保证下一轮从断点续跑。
	go func() {
		t := time.NewTicker(6 * time.Hour)
		defer t.Stop()
		for range t.C {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			if log, err := svc.SyncTrackedDailyBars(ctx, market, 120); err != nil {
				common.SysWarn("批量同步日线失败: %v", err)
			} else if log.Total > 0 {
				common.SysLog("批量同步日线完成: 共 %d 成功 %d 失败 %d", log.Total, log.Succeeded, log.Failed)
			}
			cancel()
		}
	}()
}
