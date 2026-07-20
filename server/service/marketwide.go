package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// M1 全市场日线地基：
//   - 每日增量 SyncMarketWide：东财 clist 全市场快照（约 5500 只、56 页翻页）落当日 daily_bars，
//     并维护 market_sync_states 宇宙字典 + 除权初筛（f18 昨收 vs DB 上一交易日 close）；
//   - 历史初始化 initMarketWideHistory：对 states 内 pending 标的逐只拉 250 日 kline 建史，
//     断点续传（进度即表状态）、可暂停恢复；
//   - 除权重锚 rebaseStock：确认除权后整股删+插 250 根（前复权基准统一），记 adjust_epoch。
//
// 检测锚防"部分窗口重写"漏检（DEVELOPMENT_PLAN M1 对抗审查结论）：除权日若在线路径
// 先触发 GetDailyBars，最近窗口已被重锚，盘后"昨收单点比对"会吻合而漏检，窗口之外仍是
// 旧基准。两层防护：
//  1. persistDailyBars 挂 detectAndRebase——DB 内与本次拉取重叠的日期均匀采样 60 点比对
//     close，任一点偏差超阈值即判除权（在线路径当场抓住，头部旧基准区必被采样覆盖）；
//  2. 盘后 SyncMarketWide 的昨收初筛兜底无人访问的标的。

const (
	// wideBarLimit 历史初始化/除权重锚的 kline 根数（约一年，与因子宽表 250 日窗对齐）。
	wideBarLimit = 250
	// wideInitThrottle 历史初始化逐只拉取的节流间隔（5500 只全量一轮约 30~40 分钟）。
	wideInitThrottle = 300 * time.Millisecond
	// wideInitMaxFail 初始化连续失败达此值置 failed，不再重试（退市整理/长期停牌标的）。
	wideInitMaxFail = 3
	// wideInitBatch 每批从 states 取的行数（游标向前，防失败行在同轮内反复被取）。
	wideInitBatch = 100
	// wideInitAbortStreak 连续源类失败（网络/限流，非 ErrNoData）达此值判定"源故障"，
	// 本轮中止且不把失败记到标的头上——push2his 被限流时逐只硬扫会把全宇宙误标
	// failed，还等于加速打上游。中止后下一轮 job/手动再试。
	wideInitAbortStreak = 10
	// rebaseTolerance close 相对偏差超过该比例判除权（前复权重锚偏差通常 >1%；
	// 跨批浮点/decimal(20,4) 精度误差 <0.01%。更小的分红除权偏差会漏检，
	// 但量级 <0.5% 对因子影响可忽略，属承认的简化）。
	rebaseTolerance = 0.005
	// rebaseSamplePoints 多点检测的采样点数（均匀分布覆盖头中尾，抓半新半旧断层）。
	rebaseSamplePoints = 60
	// wideRebaseMaxPerSync 单轮增量允许的重锚上限：正常除权日几只~几十只，
	// 超过说明上游口径整体漂移（数据异常），拒绝批量重写并报警。
	wideRebaseMaxPerSync = 200
	// wideRebaseThrottle 增量轮内逐只重锚的节流。
	wideRebaseThrottle = 300 * time.Millisecond
)

// wideSyncRunning / wideInitRunning 并发防抖（手动触发与后台任务共用）。
var (
	wideSyncRunning atomic.Bool
	wideInitRunning atomic.Bool
	// wideInitCancel 当前初始化任务的取消函数（暂停用）；与 wideInitRunning 同受 CAS 保护。
	wideInitCancelMu sync.Mutex
	wideInitCancel   context.CancelFunc
	// rebaseInflight 除权重锚的进行中标记（key=market:symbol），防在线路径并发重复重拉。
	rebaseInflight sync.Map
)

// ---------- 纯函数（单测锚点） ----------

// relDiff 相对偏差 |a-b| / max(|b|, 0.01)。
func relDiff(a, b float64) float64 {
	den := math.Abs(b)
	if den < 0.01 {
		den = 0.01
	}
	return math.Abs(a-b) / den
}

// closeMismatch 多点比对：fresh 序列与 DB close（按 trade_date 对齐）任一同日偏差
// 超容差即判除权。返回首个偏差日（便于日志定位）。双方均需 >0（0=缺失不比）。
func closeMismatch(dbClose map[string]float64, fresh []datasource.Bar, tolerance float64) (string, bool) {
	for _, b := range fresh {
		if db, ok := dbClose[b.TradeDate]; ok && db > 0 && b.Close > 0 {
			if relDiff(b.Close, db) > tolerance {
				return b.TradeDate, true
			}
		}
	}
	return "", false
}

// sampleDates 从日期序列均匀采样 n 个点（保序、必含末位）。检测锚必须覆盖头中尾——
// 只取尾部会漏掉"窗口外旧基准"的断层（Pos60 尾窗口径的反例）。
func sampleDates(dates []string, n int) []string {
	if len(dates) <= n {
		return dates
	}
	out := make([]string, 0, n)
	step := float64(len(dates)) / float64(n)
	for i := 0; i < n; i++ {
		out = append(out, dates[int(float64(i)*step)])
	}
	out[len(out)-1] = dates[len(dates)-1]
	return out
}

// spotTradeDate 从快照行取交易日：max(f124) 的日期（停牌股 f124 是旧值，取最大值
// 即最新成交时刻）。全部缺失返回错误——宁可整轮失败也不猜日期落错 bar。
func spotTradeDate(rows []datasource.SpotRow) (string, error) {
	var maxTS int64
	for _, r := range rows {
		if r.DataTime > maxTS {
			maxTS = r.DataTime
		}
	}
	if maxTS <= 0 {
		return "", errors.New("快照缺少行情时间戳（f124 口径漂移？）")
	}
	return time.Unix(maxTS, 0).Format("2006-01-02"), nil
}

// ---------- 除权检测与全量重锚 ----------

// detectAndRebase persistDailyBars 的检测钩子：DB 内与 fresh 重叠的日期采样比对 close，
// 判定除权则全量重锚。返回 true 表示已重锚（调用方不必再写本窗）。
// 重锚失败时返回 false 退回旧行为（本窗照写，至少最新窗口是新基准），并把 states
// 置回 pending 交给初始化任务重试——那条路径拉全量 250 根，多点采样必能再次抓住断层。
func (s *MarketService) detectAndRebase(market, symbol string, fresh []datasource.Bar) bool {
	// 排除"今天"的根：当日 close 盘中持续变化（DB 里可能是早间价、fresh 是此刻价），
	// 拿它比对会把盘中波动误判成除权。历史日的收盘值只有除权重锚才会变，才是可靠锚点。
	today := time.Now().Format("2006-01-02")
	dates := make([]string, 0, len(fresh))
	for _, b := range fresh {
		if b.TradeDate != "" && b.TradeDate != today {
			dates = append(dates, b.TradeDate)
		}
	}
	if len(dates) == 0 {
		return false
	}
	var rows []model.DailyBar
	if err := common.DB.Select("trade_date", "close").
		Where("symbol = ? AND market = ? AND trade_date IN ?", symbol, market, sampleDates(dates, rebaseSamplePoints)).
		Find(&rows).Error; err != nil || len(rows) == 0 {
		return false
	}
	dbClose := make(map[string]float64, len(rows))
	for _, r := range rows {
		dbClose[r.TradeDate] = r.Close
	}
	day, mismatch := closeMismatch(dbClose, fresh, rebaseTolerance)
	if !mismatch {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.rebaseStock(ctx, market, symbol, fresh); err != nil {
		common.SysWarn("检测到除权 %s.%s（%s close 偏差）但重锚失败: %v；已标记待初始化任务重拉", market, symbol, day, err)
		s.markStatePending(market, symbol)
		return false
	}
	common.SysLog("检测到除权/送转 %s.%s（%s close 偏差超 %.1f%%），已全量重锚", market, symbol, day, rebaseTolerance*100)
	return true
}

// rebaseStock 全量重锚：fresh 不足全量时重拉东财 250 根（强制东财——mgr 路由的新浪
// 兜底不复权，会把基准锚坏），事务内整股删+插，记 adjust_epoch。
// 缓存说明：GetDailyBars 的 10 分钟缓存可能短暂残留旧基准序列（键带 limit 无法精确清），
// TTL 到期自愈；除权日在线路径拉到的本就是上游新基准，误差窗口有限。
func (s *MarketService) rebaseStock(ctx context.Context, market, symbol string, fresh []datasource.Bar) error {
	key := market + ":" + symbol
	if _, loaded := rebaseInflight.LoadOrStore(key, struct{}{}); loaded {
		return nil // 已有并发重锚在做，本次静默让路
	}
	defer rebaseInflight.Delete(key)

	// fresh 接近全量（≥96%）直接复用，免一次重复拉取（初始化路径命中检测时即此情形）。
	if len(fresh) < wideBarLimit*96/100 {
		var err error
		fresh, err = s.wideDailyBars(ctx, market, symbol, wideBarLimit)
		if err != nil {
			return fmt.Errorf("重拉 %d 根失败: %w", wideBarLimit, err)
		}
	}
	if len(fresh) == 0 {
		return errors.New("重拉结果为空")
	}

	rows := make([]model.DailyBar, 0, len(fresh))
	for _, b := range fresh {
		if b.TradeDate == "" {
			continue
		}
		rows = append(rows, model.DailyBar{
			Symbol: symbol, Market: market, TradeDate: b.TradeDate,
			Open: b.Open, High: b.High, Low: b.Low, Close: b.Close,
			Volume: b.Volume, Amount: b.Amount, TurnoverRate: b.TurnoverRate,
			Source: "eastmoney",
		})
	}
	if len(rows) == 0 {
		return errors.New("重拉序列无有效交易日")
	}
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		// 整股删+插：窗口之外的更老数据已无法对齐新基准，留着就是断层毒数据；
		// 因子宽表/筹码/回测的窗口均 ≤250 日，删除不损失有效信息。
		if err := tx.Where("symbol = ? AND market = ?", symbol, market).Delete(&model.DailyBar{}).Error; err != nil {
			return err
		}
		return tx.CreateInBatches(rows, 200).Error
	})
	if err != nil {
		return err
	}
	last := rows[len(rows)-1]
	s.touchStateAfterRebase(market, symbol, len(rows), last.TradeDate)
	return nil
}

// wideDailyBars 东财直连拉日线并补 Source（直连不经 manager，Source 不会被自动填充；
// persistDailyBars 的除权检测依赖 Source=="eastmoney" 判定复权口径）。
func (s *MarketService) wideDailyBars(ctx context.Context, market, symbol string, limit int) ([]datasource.Bar, error) {
	bars, err := s.wide.GetDailyBars(ctx, market, symbol, limit)
	if err != nil {
		return nil, err
	}
	for i := range bars {
		bars[i].Source = "eastmoney"
	}
	return bars, nil
}

// touchStateAfterRebase 重锚成功后更新宇宙状态（无 states 行的标的如 ETF 静默跳过——
// adjust_epoch 是审计字段，不影响正确性）。
func (s *MarketService) touchStateAfterRebase(market, symbol string, barsCount int, lastDate string) {
	today := time.Now().Format("2006-01-02")
	common.DB.Model(&model.MarketSyncState{}).
		Where("symbol = ? AND market = ?", symbol, market).
		Updates(map[string]any{
			"adjust_epoch": today, "bars_count": barsCount, "last_bar_date": lastDate,
			"init_status": "done", "fail_count": 0, "last_error": "",
		})
}

// markStatePending 重锚失败时把标的踢回 pending，让历史初始化任务下轮全量重拉修复断层。
func (s *MarketService) markStatePending(market, symbol string) {
	common.DB.Model(&model.MarketSyncState{}).
		Where("symbol = ? AND market = ?", symbol, market).
		Updates(map[string]any{"init_status": "pending", "fail_count": 0})
}

// ---------- 每日增量 ----------

// SyncMarketWide 全市场日线每日增量：clist 快照 → 当日 bar 批量 upsert → 宇宙字典
// 维护 → 除权初筛与重锚。盘后（16:10 job）与手动触发共用，防并发重入。
func (s *MarketService) SyncMarketWide(ctx context.Context) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !wideSyncRunning.CompareAndSwap(false, true) {
		return nil, ErrSyncInProgress
	}
	defer wideSyncRunning.Store(false)

	start := time.Now()
	log := &model.DataSyncLog{Task: "sync_market_wide", Market: "cn"}
	fail := func(err error) (*model.DataSyncLog, error) {
		log.Status = "failed"
		log.Message = truncate(err.Error(), 512)
		log.DurationMs = time.Since(start).Milliseconds()
		s.recordSyncLog(log)
		return log, err
	}

	rows, err := s.wide.GetCNSpotSnapshot(ctx)
	if err != nil {
		return fail(fmt.Errorf("全市场快照失败: %w", err))
	}
	tradeDate, err := spotTradeDate(rows)
	if err != nil {
		return fail(err)
	}
	log.Total = len(rows)

	// 1. 当日 bar：有效成交行（停牌 Price=0/Volume=0 不落当日 bar）。
	bars := make([]model.DailyBar, 0, len(rows))
	for _, r := range rows {
		if r.Price <= 0 || r.Volume <= 0 {
			continue
		}
		bars = append(bars, model.DailyBar{
			Symbol: r.Symbol, Market: "cn", TradeDate: tradeDate,
			Open: r.Open, High: r.High, Low: r.Low, Close: r.Price,
			Volume: r.Volume, Amount: r.Amount, TurnoverRate: r.TurnoverRate,
			Source: "eastmoney",
		})
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"open", "high", "low", "close", "volume", "amount", "turnover_rate", "source",
		}),
	}).CreateInBatches(bars, 500).Error; err != nil && err != gorm.ErrEmptySlice {
		return fail(fmt.Errorf("落库当日 bar 失败: %w", err))
	}
	log.Succeeded = len(bars)

	// 交易日历顺手补当日（快照日必为开市日）。
	if err := common.DB.Select("Market", "TradeDate", "IsOpen").Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"is_open"}),
	}).Create(&model.TradingCalendar{Market: "cn", TradeDate: tradeDate, IsOpen: true}).Error; err != nil {
		common.SysWarn("补交易日历失败 %s: %v", tradeDate, err)
	}

	// 2. 宇宙字典：全部行（含停牌）upsert；有效成交行额外刷 last_bar_date。
	if err := s.upsertSyncStates(rows, tradeDate); err != nil {
		return fail(fmt.Errorf("维护宇宙字典失败: %w", err))
	}

	// 2b. S0-3 PIT 宇宙快照：全市场当日状态逐日固化（退市/ST/停牌/行业/估值），
	// 历史回放的候选宇宙按 as_of 日重建的地基。best-effort：失败只记日志。
	if err := s.upsertUniverseDaily(rows, tradeDate); err != nil {
		common.SysWarn("宇宙快照落库失败 %s: %v", tradeDate, err)
	}

	// 3. 除权初筛 + 重锚。
	rebased, suspects := s.rebaseSuspects(ctx, rows, tradeDate)
	log.Failed = suspects - rebased // 初筛命中但重锚失败的（已标 pending 由初始化任务兜底）

	log.Status = statusOf(log)
	log.Message = truncate(fmt.Sprintf("%s：快照 %d 只，落 bar %d，除权重锚 %d/%d", tradeDate, len(rows), len(bars), rebased, suspects), 512)
	log.DurationMs = time.Since(start).Milliseconds()
	s.recordSyncLog(log)
	// M1 第二部分：增量落库完成后异步重建因子宽表（选股/推荐策略信号的数据地基）。
	if log.Succeeded > 0 {
		RebuildFactorTableAsync("每日增量完成")
		// P3b：快照已带 f9/f115/f23/f100 估值字段，顺手按行业聚合板块估值（best-effort）。
		AggregateBoardValuationAsync(rows, tradeDate)
	}
	return log, nil
}

// upsertSyncStates 宇宙字典维护：新标的建 pending 行，已有行只刷名称（与 last_bar_date）。
func (s *MarketService) upsertSyncStates(rows []datasource.SpotRow, tradeDate string) error {
	trading := make([]model.MarketSyncState, 0, len(rows))
	suspended := make([]model.MarketSyncState, 0, 64)
	for _, r := range rows {
		st := model.MarketSyncState{Symbol: r.Symbol, Market: "cn", Name: r.Name, InitStatus: "pending"}
		if r.Price > 0 && r.Volume > 0 {
			st.LastBarDate = tradeDate
			trading = append(trading, st)
		} else {
			suspended = append(suspended, st)
		}
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "last_bar_date", "updated_at"}),
	}).CreateInBatches(trading, 500).Error; err != nil && err != gorm.ErrEmptySlice {
		return err
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}},
		DoUpdates: clause.AssignmentColumns([]string{"name", "updated_at"}),
	}).CreateInBatches(suspended, 500).Error; err != nil && err != gorm.ErrEmptySlice {
		return err
	}
	return nil
}

// rebaseSuspects 除权初筛：快照 f18 昨收 vs DB 上一交易日 close，偏差超阈值判疑似除权，
// 逐只全量重锚。返回 (重锚成功数, 疑似总数)。
// 该初筛只兜底"无人访问"的标的——被在线路径重写过窗口的股票已由 persistDailyBars
// 的多点检测当场重锚，此处比对自然吻合，不会重复。
func (s *MarketService) rebaseSuspects(ctx context.Context, rows []datasource.SpotRow, tradeDate string) (rebased, suspects int) {
	var prevDates []string
	if err := common.DB.Model(&model.TradingCalendar{}).
		Where("market = ? AND is_open = ? AND trade_date < ?", "cn", true, tradeDate).
		Order("trade_date DESC").Limit(1).Pluck("trade_date", &prevDates).Error; err != nil || len(prevDates) == 0 {
		common.SysDebug("除权初筛跳过：无上一交易日日历")
		return 0, 0
	}
	prevDate := prevDates[0]
	var dbRows []model.DailyBar
	if err := common.DB.Select("symbol", "close").
		Where("market = ? AND trade_date = ?", "cn", prevDate).Find(&dbRows).Error; err != nil {
		return 0, 0
	}
	spotPrev := make(map[string]float64, len(rows))
	for _, r := range rows {
		if r.PrevClose > 0 {
			spotPrev[r.Symbol] = r.PrevClose
		}
	}
	var list []string
	for _, r := range dbRows {
		if pc, ok := spotPrev[r.Symbol]; ok && r.Close > 0 && relDiff(pc, r.Close) > rebaseTolerance {
			list = append(list, r.Symbol)
		}
	}
	if len(list) == 0 {
		return 0, 0
	}
	if len(list) > wideRebaseMaxPerSync {
		common.SysWarn("除权初筛命中 %d 只超上限 %d（上游口径整体漂移？），本轮拒绝批量重锚", len(list), wideRebaseMaxPerSync)
		return 0, 0
	}
	suspects = len(list)
	for _, sym := range list {
		if ctx.Err() != nil {
			break
		}
		if err := s.rebaseStock(ctx, "cn", sym, nil); err != nil {
			common.SysWarn("除权重锚失败 cn.%s: %v；已标记待初始化任务重拉", sym, err)
			s.markStatePending("cn", sym)
		} else {
			rebased++
		}
		time.Sleep(wideRebaseThrottle)
	}
	if suspects > 0 {
		common.SysLog("除权初筛：疑似 %d 只，重锚成功 %d", suspects, rebased)
	}
	return rebased, suspects
}

// upsertUniverseDaily S0-3 每日宇宙快照：clist 全市场行原样固化（同一次上游请求
// 零额外成本）。唯一键 (trade_date, symbol) upsert 幂等，盘中重跑覆盖为最新快照。
func (s *MarketService) upsertUniverseDaily(rows []datasource.SpotRow, tradeDate string) error {
	snap := make([]model.StockUniverseDaily, 0, len(rows))
	for _, r := range rows {
		snap = append(snap, model.StockUniverseDaily{
			TradeDate: tradeDate, Symbol: r.Symbol, Market: "cn", Name: r.Name,
			IsST:      isSTName(r.Name),
			Suspended: r.Price <= 0 || r.Volume <= 0,
			Close:     r.Price, PrevClose: r.PrevClose,
			Amount: r.Amount, TurnoverRate: r.TurnoverRate,
			PE: r.PE, PETTM: r.PETTM, PB: r.PB, Industry: r.Industry,
		})
	}
	err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "trade_date"}, {Name: "symbol"}},
		DoUpdates: clause.AssignmentColumns([]string{
			// PETTM 的 GORM 蛇形化列名是 pettm（连续大写视为一词，F1 YoY→yo_y 同款坑）；
			// 写 pe_ttm 会让整条 upsert SQL 无效——快照整批落库失败而非单列失败。
			"name", "is_st", "suspended", "close", "prev_close",
			"amount", "turnover_rate", "pe", "pettm", "pb", "industry",
		}),
	}).CreateInBatches(snap, 500).Error
	if err != nil && err != gorm.ErrEmptySlice {
		return err
	}
	return nil
}

// ---------- 历史初始化（断点续传/暂停恢复） ----------

// StartMarketWideInit 异步启动历史初始化（已在跑返回 ErrSyncInProgress）。
// CAS 与取消函数注册在同一入口，保证 Pause 取消的一定是正在跑的那轮。
func (s *MarketService) StartMarketWideInit() error {
	if !wideInitRunning.CompareAndSwap(false, true) {
		return ErrSyncInProgress
	}
	ctx, cancel := context.WithCancel(context.Background())
	wideInitCancelMu.Lock()
	wideInitCancel = cancel
	wideInitCancelMu.Unlock()
	go func() {
		defer wideInitRunning.Store(false)
		defer cancel()
		if log, err := s.initMarketWideHistory(ctx); err != nil && !errors.Is(err, context.Canceled) {
			common.SysWarn("全市场历史初始化异常退出: %v", err)
		} else if log != nil {
			common.SysLog("全市场历史初始化轮结束: 处理 %d 成功 %d 失败 %d（pending 未清空时下轮 job 自动续跑）",
				log.Total, log.Succeeded, log.Failed)
		}
	}()
	return nil
}

// PauseMarketWideInit 暂停当前初始化任务。进度在 states 表内，重新 Start 即从断点续跑。
func (s *MarketService) PauseMarketWideInit() bool {
	wideInitCancelMu.Lock()
	defer wideInitCancelMu.Unlock()
	if wideInitCancel != nil && wideInitRunning.Load() {
		wideInitCancel()
		return true
	}
	return false
}

// initMarketWideHistory 对 pending 标的逐只拉 250 日 kline 建史。
// 断点续传：每只完成即落表状态，取消/崩溃后重跑自动跳过已 done 的。
// 游标只向前：失败但未达 failed 阈值的行本轮不再重取（防拉不动的标的把一轮卡成死循环），
// 留给下一轮 job/手动重试。
func (s *MarketService) initMarketWideHistory(ctx context.Context) (*model.DataSyncLog, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	start := time.Now()
	log := &model.DataSyncLog{Task: "init_market_history", Market: "cn"}

	var pendingTotal int64
	common.DB.Model(&model.MarketSyncState{}).
		Where("market = ? AND init_status = ?", "cn", "pending").Count(&pendingTotal)
	if pendingTotal == 0 {
		log.Status = "success"
		log.Message = "无待初始化标的"
		s.recordSyncLog(log)
		return log, nil
	}
	common.SysLog("全市场历史初始化开跑: 待处理 %d 只（每只 %d 根，节流 %v）", pendingTotal, wideBarLimit, wideInitThrottle)

	var lastID int64
	canceled, srcAborted := false, false
	// 源类失败（网络/限流）与标的失败（ErrNoData/代码非法）分开记：前者攒着，
	// 有成功夹杂说明源活着才落库（零散网络失败也算标的一次尝试）；连续达阈值
	// 判源故障，整批丢弃不怪标的。
	srcFailStreak := 0
	var srcFails []model.MarketSyncState
	var srcFailErrs []string
	recordFail := func(st *model.MarketSyncState, msg string) {
		st.FailCount++
		st.LastError = truncate(msg, 256)
		status := st.InitStatus
		if st.FailCount >= wideInitMaxFail {
			status = "failed"
		}
		common.DB.Model(st).Updates(map[string]any{
			"fail_count": st.FailCount, "last_error": st.LastError, "init_status": status,
		})
		log.Failed++
	}
	flushSrcFails := func() {
		for i := range srcFails {
			recordFail(&srcFails[i], srcFailErrs[i])
		}
		srcFails, srcFailErrs = nil, nil
	}
loop:
	for {
		var batch []model.MarketSyncState
		if err := common.DB.Where("market = ? AND init_status = ? AND id > ?", "cn", "pending", lastID).
			Order("id").Limit(wideInitBatch).Find(&batch).Error; err != nil {
			return nil, err
		}
		if len(batch) == 0 {
			break
		}
		for i := range batch {
			st := &batch[i]
			if ctx.Err() != nil {
				canceled = true
				break loop
			}
			lastID = st.ID
			bars, err := s.wideDailyBars(ctx, "cn", st.Symbol, wideBarLimit)
			switch {
			case err != nil && ctx.Err() != nil: // 预算用尽/暂停不是标的问题，不记失败
				canceled = true
				break loop
			case errors.Is(err, datasource.ErrNoData) || errors.Is(err, datasource.ErrSymbolInvalid):
				// 上游明确"无数据/代码非法"（退市整理、长期停牌）：直接记标的失败。
				flushSrcFails()
				srcFailStreak = 0
				if errors.Is(err, datasource.ErrSymbolInvalid) {
					st.FailCount = wideInitMaxFail - 1 // 代码非法重试无意义，本次即 failed
				}
				recordFail(st, err.Error())
			case err != nil:
				srcFailStreak++
				srcFails = append(srcFails, *st)
				srcFailErrs = append(srcFailErrs, err.Error())
				if srcFailStreak >= wideInitAbortStreak {
					srcFails, srcFailErrs = nil, nil // 源故障不怪标的
					srcAborted = true
					break loop
				}
			default:
				flushSrcFails()
				srcFailStreak = 0
				// 落库失败不标 done：否则该股永久缺口（宽表/因子/回测都读不到）。
				// 保持 pending，走 recordFail 计一次尝试，下一轮 job/手动重试。
				if perr := s.persistDailyBars("cn", st.Symbol, bars); perr != nil { // 内部含除权检测：有旧基准数据时自动全量重锚
					recordFail(st, perr.Error())
					break
				}
				last := bars[len(bars)-1]
				common.DB.Model(st).Updates(map[string]any{
					"init_status": "done", "fail_count": 0, "last_error": "",
					"bars_count": len(bars), "last_bar_date": last.TradeDate,
				})
				log.Succeeded++
			}
			done := log.Succeeded + log.Failed
			if done > 0 && done%50 == 0 {
				common.SysLog("全市场历史初始化进度: %d/%d（失败 %d）", done, pendingTotal, log.Failed)
			}
			select {
			case <-ctx.Done():
				canceled = true
				break loop
			case <-time.After(wideInitThrottle):
			}
		}
	}
	flushSrcFails()

	log.Total = log.Succeeded + log.Failed
	log.Status = statusOf(log)
	switch {
	case srcAborted:
		log.Status = "failed"
		log.Message = truncate(fmt.Sprintf("东财日线连续 %d 只失败，疑似源限流，本轮中止（标的状态未记失败，断点续传）: 本轮处理 %d/%d",
			wideInitAbortStreak, log.Total, pendingTotal), 512)
		common.SysWarn("全市场历史初始化中止：东财日线疑似被限流（连续 %d 只失败）", wideInitAbortStreak)
	case canceled:
		log.Message = truncate(fmt.Sprintf("已暂停/超时（断点续传）: 本轮处理 %d/%d", log.Total, pendingTotal), 512)
	default:
		log.Message = truncate(fmt.Sprintf("本轮处理 %d/%d", log.Total, pendingTotal), 512)
	}
	log.DurationMs = time.Since(start).Milliseconds()
	s.recordSyncLog(log)
	// 本轮有新建史标的即重建宽表（含暂停中断的部分进度——尽快让选股覆盖它们）。
	if log.Succeeded > 0 {
		RebuildFactorTableAsync("历史初始化推进")
	}
	if canceled {
		return log, context.Canceled
	}
	return log, nil
}

// ---------- 状态查询 ----------

// MarketWideStatusView 管理端全市场覆盖状态。
type MarketWideStatusView struct {
	Total       int64              `json:"total"`
	Pending     int64              `json:"pending"`
	Done        int64              `json:"done"`
	Failed      int64              `json:"failed"`
	SyncRunning bool               `json:"sync_running"`
	InitRunning bool               `json:"init_running"`
	LastSync    *model.DataSyncLog `json:"last_sync"`
	LastInit    *model.DataSyncLog `json:"last_init"`

	// P1 数据水位（对照交易日历，非库内自身 MAX）。
	ObservedDate string `json:"observed_date"` // 库内最新交易日（states/daily_bars MAX）
	ExpectedDate string `json:"expected_date"` // 按交易日历应有的最新交易日
	LagOpenDays  int    `json:"lag_open_days"` // 落后开市日数（0=齐平，-1=日历不可用）
}

// MarketWideStatus 聚合宇宙字典状态与最近任务日志。
func (s *MarketService) MarketWideStatus() (*MarketWideStatusView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	v := &MarketWideStatusView{
		SyncRunning: wideSyncRunning.Load(),
		InitRunning: wideInitRunning.Load(),
	}
	type cnt struct {
		InitStatus string
		N          int64
	}
	var cs []cnt
	if err := common.DB.Model(&model.MarketSyncState{}).Select("init_status, COUNT(*) AS n").
		Where("market = ?", "cn").Group("init_status").Find(&cs).Error; err != nil {
		return nil, err
	}
	for _, c := range cs {
		v.Total += c.N
		switch c.InitStatus {
		case "pending":
			v.Pending = c.N
		case "done":
			v.Done = c.N
		case "failed":
			v.Failed = c.N
		}
	}
	lastLog := func(task string) *model.DataSyncLog {
		var l model.DataSyncLog
		if err := common.DB.Where("task = ?", task).Order("id DESC").First(&l).Error; err != nil {
			return nil
		}
		return &l
	}
	v.LastSync = lastLog("sync_market_wide")
	v.LastInit = lastLog("init_market_history")
	if d, err := wideFreshDate(); err == nil {
		v.ObservedDate = d
	}
	v.ExpectedDate = wideExpectedDate(time.Now())
	v.LagOpenDays = openDaysBehind(v.ObservedDate, v.ExpectedDate)
	return v, nil
}
