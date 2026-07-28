package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// M3a 扩展数据：龙虎榜/机构统计/人气榜/涨停池 的盘后采集与消费查询。
//   - 涨停池族上游不可回溯历史（date 传旧日期会被静默回落，datasource 层已校验 qdate），
//     情绪序列靠每日盘后快照积累；龙虎榜 datacenter 可按日回查，首轮回填近 30 天。
//   - 错峰：16:35 涨停池+人气榜（收盘后即稳定，避开 16:10 全市场日线 job）；
//     18:45 龙虎榜+机构统计（datacenter 盘后逐步发布，18 点后基本齐全，避开 19:05 财报 job）。
//   - 游标（options）记「已成功同步的交易日」，启动补跑按目标日与游标比对，幂等。

const (
	optMoodPoolDay = "mood_pool_day" // 涨停池 已成功同步的交易日（沿用旧 key 保兼容）
	optMoodPopDay  = "mood_pop_day"  // 人气榜 已成功同步的交易日（与涨停池各自独立推进）
	// 旧 mood_lhb_day 可能由“主榜成功、机构榜失败”的旧实现错误推进，不能作为完整快照游标。
	// v2 首次为空时会强制重验近 30 天，之后只在主榜+机构榜原子提交后推进。
	optMoodLhbLegacyDay = "mood_lhb_day"
	optMoodLhbDay       = "mood_lhb_complete_day_v2"
	// 未解决缺口清单（JSON 数组）。游标只表达「该日之前都已处理」，不等于「都已完成」——
	// 恒定失败的历史日被登记在这里持续重试，直到主榜+机构榜原子成功或确认为空榜。
	optMoodLhbGaps = "mood_lhb_gaps_v1"

	moodPoolCutoffMin = 16*60 + 35 // 16:35 后当日涨停池数据视为可采
	moodLhbCutoffMin  = 18*60 + 45 // 18:45 后当日龙虎榜数据视为可采

	lhbBackfillDays  = 30               // 首轮部署回填龙虎榜/机构统计的自然日跨度（详情页上榜记录立即可用）
	lhbRetryInterval = 30 * time.Minute // 游标落后或当日尚未发布时的重试间隔
)

// MoodService 扩展数据采集与查询。
type MoodService struct {
	em               *datasource.EastMoneyAdapter
	fetchLhbDaily    func(context.Context, string) ([]datasource.LhbRow, error)
	fetchLhbOrgDaily func(context.Context, string) ([]datasource.LhbOrgRow, error)
	repairCalendar   func(context.Context) error
	now              func() time.Time
}

func NewMoodService() *MoodService {
	em := datasource.NewEastMoneyAdapter()
	return &MoodService{
		em:               em,
		fetchLhbDaily:    em.GetLhbDaily,
		fetchLhbOrgDaily: em.GetLhbOrgDaily,
		now:              time.Now,
	}
}

// ---------- 纯函数（单测锚点） ----------

// computeMoodDaily 由涨停池三接口聚合当日市场情绪温度计。
// zbCount 为炸板家数（炸板池可为空=0，正常态）；yzt 为空时昨涨停溢价字段缺席（保持 0）。
func computeMoodDaily(market, tradeDate string, zt []datasource.ZTPoolItem, zbCount int, yzt []datasource.YZTPoolItem) model.MarketMoodDaily {
	m := model.MarketMoodDaily{
		Market: market, TradeDate: tradeDate,
		LimitUpCount: len(zt), BrokenCount: zbCount,
	}
	if total := len(zt) + zbCount; total > 0 {
		m.BrokenRate = round2(float64(zbCount) / float64(total) * 100)
	}
	dist := map[int]int{}
	for _, it := range zt {
		streak := it.Streak
		if streak < 1 {
			streak = 1
		}
		dist[streak]++
		if streak > m.MaxStreak {
			m.MaxStreak = streak
		}
		if it.SealFund > m.SealFundTop {
			m.SealFundTop = it.SealFund
		}
	}
	if len(dist) > 0 {
		keys := make([]int, 0, len(dist))
		for k := range dist {
			keys = append(keys, k)
		}
		sort.Ints(keys)
		ordered := make(map[string]int, len(dist))
		for _, k := range keys {
			ordered[strconv.Itoa(k)] = dist[k]
		}
		if b, err := json.Marshal(ordered); err == nil {
			m.StreakDistJSON = string(b)
		}
	}
	if n := len(yzt); n > 0 {
		m.YztCount = n
		var sum float64
		up := 0
		for _, it := range yzt {
			sum += it.ChangePct
			if it.ChangePct > 0 {
				up++
			}
		}
		m.YztAvgChg = round2(sum / float64(n))
		m.YztUpRatio = round2(float64(up) / float64(n) * 100)
	}
	return m
}

// moodTargetDate 采集目标交易日：今天是交易日且已过 cutoff → 今天；否则最近一个已收盘交易日。
// 保证启动补跑永远指向「数据已可得」的那一天（周六启动补周五、盘中启动补昨日）。
func moodTargetDate(now time.Time, cutoffMin int) string {
	minutes := now.Hour()*60 + now.Minute()
	if isTradingDayToday(now) && minutes >= cutoffMin {
		return now.Format("2006-01-02")
	}
	return prevOpenTradeDate(now.Format("2006-01-02"))
}

// prevOpenTradeDate 严格早于 before 的最近开市日。无日历数据时回退「往前最近的周一~五」。
func prevOpenTradeDate(before string) string {
	if common.DB != nil {
		var dates []string
		if err := common.DB.Model(&model.TradingCalendar{}).
			Where("market = ? AND is_open = ? AND trade_date < ?", "cn", true, before).
			Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error; err == nil && len(dates) > 0 {
			return dates[0]
		}
	}
	d, err := time.ParseInLocation("2006-01-02", before, time.Local)
	if err != nil {
		return before
	}
	for {
		d = d.AddDate(0, 0, -1)
		if wd := d.Weekday(); wd >= time.Monday && wd <= time.Friday {
			return d.Format("2006-01-02")
		}
	}
}

// ---------- 盘后同步 ----------

// SyncZTPools 采集某交易日涨停池快照并聚合情绪温度计。tradeDate 形如 2026-07-08。
// 涨停池失败整轮失败（聚合缺主体无意义）；炸板/昨日涨停池 ErrNoData 是正常态
// （情绪冰点日可为空），按 0 家/缺失聚合；网络类失败则整轮失败防半截聚合。
func (s *MoodService) SyncZTPools(ctx context.Context, tradeDate string) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	dateCompact := compactDate(tradeDate)
	zt, err := s.em.GetZTPool(ctx, dateCompact)
	if err != nil {
		return fmt.Errorf("涨停池拉取失败: %w", err)
	}
	zb, err := s.em.GetZBPool(ctx, dateCompact)
	if err != nil && !errors.Is(err, datasource.ErrNoData) {
		return fmt.Errorf("炸板池拉取失败: %w", err)
	}
	yzt, err := s.em.GetYesterdayZTPool(ctx, dateCompact)
	if err != nil && !errors.Is(err, datasource.ErrNoData) {
		return fmt.Errorf("昨日涨停池拉取失败: %w", err)
	}

	// 明细先删后插（盘中手动重跑时池成员会变化，快照以最终拉取为准）。
	rows := make([]model.LimitUpStock, 0, len(zt))
	for _, it := range zt {
		rows = append(rows, model.LimitUpStock{
			Symbol: it.Symbol, Market: "cn", TradeDate: tradeDate, Name: it.Name,
			Price: it.Price, Amount: it.Amount, FloatCap: it.FloatCap, TurnoverRate: it.TurnoverRate,
			Streak: it.Streak, FirstSealAt: it.FirstSealAt, LastSealAt: it.LastSealAt,
			SealFund: it.SealFund, BreakCount: it.BreakCount,
			Industry: truncateRunes(it.Industry, 32), StatDays: it.StatDays, StatCount: it.StatCount,
		})
	}
	mood := computeMoodDaily("cn", tradeDate, zt, len(zb), yzt)
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("market = ? AND trade_date = ?", "cn", tradeDate).
			Delete(&model.LimitUpStock{}).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			if err := tx.CreateInBatches(rows, 200).Error; err != nil {
				return err
			}
		}
		return tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "market"}, {Name: "trade_date"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"limit_up_count", "broken_count", "broken_rate", "max_streak", "streak_dist_json",
				"yzt_count", "yzt_avg_chg", "yzt_up_ratio", "seal_fund_top", "updated_at",
			}),
		}).Create(&mood).Error
	})
}

// SyncPopularity 采集人气榜前 100 当日快照（实时榜无历史，非当日不可补）。
func (s *MoodService) SyncPopularity(ctx context.Context, tradeDate string) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	rows, err := datasource.GetPopularityTop(ctx)
	if err != nil {
		return err
	}
	recs := make([]model.PopularityRank, 0, len(rows))
	for _, r := range rows {
		recs = append(recs, model.PopularityRank{
			Symbol: r.Symbol, Market: "cn", TradeDate: tradeDate,
			Rank: r.Rank, PrevRank: r.PrevRank, IsNew: r.PrevRank <= 0,
		})
	}
	if len(recs) == 0 {
		return datasource.ErrNoData
	}
	return common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{"rank", "prev_rank", "is_new", "updated_at"}),
	}).CreateInBatches(recs, 200).Error
}

// SyncLhb 采集某交易日龙虎榜详情 + 机构买卖统计。返回主表行数。
// 两份上游数据均成功（机构 ErrNoData 表示当日确实无机构席位，按空集处理）后，才在
// 同一事务内删除重建该日两张表；抓取或任一落库失败都不会留下半份快照。
func (s *MoodService) SyncLhb(ctx context.Context, tradeDate string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	rows, err := s.fetchLhbRows(ctx, tradeDate)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, datasource.ErrNoData
	}
	orgRows, err := s.fetchLhbOrgRows(ctx, tradeDate)
	if errors.Is(err, datasource.ErrNoData) {
		orgRows = nil
	} else if errors.Is(err, datasource.ErrLhbNotReady) {
		now := time.Now()
		if s.now != nil {
			now = s.now()
		}
		if !lhbOrgNotReadyCanFinalizeEmpty(tradeDate, now) {
			return 0, err
		}
		orgRows = nil
	} else if err != nil {
		return 0, err
	}

	lhbRecs := makeLhbEntries(rows, tradeDate)
	orgRecs := makeLhbOrgEntries(orgRows, tradeDate)
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("market = ? AND trade_date = ?", "cn", tradeDate).
			Delete(&model.LhbEntry{}).Error; err != nil {
			return err
		}
		// 机构榜「先删后插」：原子替换语义——同步成功即让该日 DB 与上游一致。
		// 空结果也删是有意的（见 TestSyncLhbAtomicReplace）：上游 9201 对「尚未发布」
		// 与「确实为空」不可区分，故用时间守卫 lhbOrgNotReadyCanFinalizeEmpty 把风险
		// 限制在历史日——当天的 not-ready 一律整次失败重试，绝不收口为空榜。
		// 残余风险：历史日上游瞬时回 9201 会抹掉该日真实机构数据（游标推进后不回填）。
		if err := tx.Where("market = ? AND trade_date = ?", "cn", tradeDate).
			Delete(&model.LhbOrgDaily{}).Error; err != nil {
			return err
		}
		if len(lhbRecs) > 0 {
			if err := tx.CreateInBatches(lhbRecs, 200).Error; err != nil {
				return err
			}
		}
		if len(orgRecs) > 0 {
			if err := tx.CreateInBatches(orgRecs, 200).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(lhbRecs), nil
}

// lhbOrgNotReadyCanFinalizeEmpty 只允许把历史日期的“机构榜仍无结果”最终确认为空榜。
// 当日 9201 可能只是主榜先发布、机构榜仍在生成，必须继续重试而不能清表推进游标。
func lhbOrgNotReadyCanFinalizeEmpty(tradeDate string, now time.Time) bool {
	return isHistoricalTradeDate(tradeDate, now)
}

// isHistoricalTradeDate tradeDate 是否严格早于 now 所在自然日（即当日盘后发布窗口已翻页）。
func isHistoricalTradeDate(tradeDate string, now time.Time) bool {
	day, err := time.ParseInLocation("2006-01-02", tradeDate, time.Local)
	if err != nil {
		return false
	}
	local := now.In(time.Local)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.Local)
	return day.Before(today)
}

// nowOrDefault 服务时钟（测试可注入）。
func (s *MoodService) nowOrDefault() time.Time {
	if s != nil && s.now != nil {
		return s.now()
	}
	return time.Now()
}

func (s *MoodService) fetchLhbRows(ctx context.Context, tradeDate string) ([]datasource.LhbRow, error) {
	if s.fetchLhbDaily != nil {
		return s.fetchLhbDaily(ctx, tradeDate)
	}
	if s.em == nil {
		return nil, errors.New("龙虎榜数据源不可用")
	}
	return s.em.GetLhbDaily(ctx, tradeDate)
}

func (s *MoodService) fetchLhbOrgRows(ctx context.Context, tradeDate string) ([]datasource.LhbOrgRow, error) {
	if s.fetchLhbOrgDaily != nil {
		return s.fetchLhbOrgDaily(ctx, tradeDate)
	}
	if s.em == nil {
		return nil, errors.New("机构龙虎榜数据源不可用")
	}
	return s.em.GetLhbOrgDaily(ctx, tradeDate)
}

func makeLhbEntries(rows []datasource.LhbRow, tradeDate string) []model.LhbEntry {
	recs := make([]model.LhbEntry, 0, len(rows))
	for _, r := range rows {
		date := tradeDate
		if date == "" {
			date = r.TradeDate
		}
		recs = append(recs, model.LhbEntry{
			Symbol: r.Symbol, Market: "cn", TradeDate: date,
			ChangeType: truncateRunes(r.ChangeType, 24), Name: r.Name,
			Reason: truncateRunes(r.Reason, 128), Note: truncateRunes(r.Note, 128),
			Close: r.Close, ChangePct: round2(r.ChangePct),
			NetBuy: r.NetBuy, BuyAmt: r.BuyAmt, SellAmt: r.SellAmt, DealAmt: r.DealAmt,
			NetRatio: round2(r.NetRatio), TurnoverRate: round2(r.TurnoverRate),
		})
	}
	return recs
}

func makeLhbOrgEntries(rows []datasource.LhbOrgRow, tradeDate string) []model.LhbOrgDaily {
	recs := make([]model.LhbOrgDaily, 0, len(rows))
	for _, r := range rows {
		date := tradeDate
		if date == "" {
			date = r.TradeDate
		}
		recs = append(recs, model.LhbOrgDaily{
			Symbol: r.Symbol, Market: "cn", TradeDate: date, Name: r.Name,
			Close: r.Close, ChangePct: round2(r.ChangePct),
			BuyTimes: r.BuyTimes, SellTimes: r.SellTimes,
			BuyAmt: r.BuyAmt, SellAmt: r.SellAmt, NetBuy: r.NetBuy,
			NetRatio: round2(r.NetRatio), Reason: truncateRunes(r.Reason, 128),
		})
	}
	return recs
}

// upsertLhbRows 龙虎榜主表批量 upsert（键 symbol+market+trade_date+change_type 幂等）。
func upsertLhbRows(rows []datasource.LhbRow) (int, error) {
	recs := makeLhbEntries(rows, "")
	if len(recs) == 0 {
		return 0, nil
	}
	if err := common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}, {Name: "change_type"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name", "reason", "note", "close", "change_pct", "net_buy", "buy_amt",
			"sell_amt", "deal_amt", "net_ratio", "turnover_rate", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error; err != nil {
		return 0, err
	}
	return len(recs), nil
}

// upsertLhbOrgRows 机构买卖统计批量 upsert（键 symbol+market+trade_date 幂等）。
func upsertLhbOrgRows(rows []datasource.LhbOrgRow) error {
	recs := makeLhbOrgEntries(rows, "")
	if len(recs) == 0 {
		return nil
	}
	return common.DB.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "symbol"}, {Name: "market"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"name", "close", "change_pct", "buy_times", "sell_times",
			"buy_amt", "sell_amt", "net_buy", "net_ratio", "reason", "updated_at",
		}),
	}).CreateInBatches(recs, 200).Error
}

// lhbPendingDatesWithCoverage 返回游标之后、目标日之前（含目标日）的开市日及日历是否完整。
// 周末是确定休市；任一工作日缺行则 complete=false，调用方必须先修复日历，不能猜成开市
// （法定节假日会卡死）或休市（真实开市日会被越过）。
func lhbPendingDatesWithCoverage(cursor, target string) ([]string, bool) {
	pending := []string{}
	targetDay, err := time.ParseInLocation("2006-01-02", target, time.Local)
	if err != nil {
		return pending, false
	}
	// 下界统一用开区间；多减一天以保留旧逻辑的「目标日前 30 天 + 目标日」边界。
	start := targetDay.AddDate(0, 0, -lhbBackfillDays-1)
	if cursor != "" {
		cursorDay, err := time.ParseInLocation("2006-01-02", cursor, time.Local)
		if err == nil {
			if !cursorDay.Before(targetDay) {
				return pending, true
			}
			start = cursorDay
		}
	}
	startDate := start.Format("2006-01-02")
	calendarByDate := map[string]bool{}
	if common.DB != nil {
		var calendar []model.TradingCalendar
		if err := common.DB.Select("trade_date", "is_open").
			Where("market = ? AND trade_date > ? AND trade_date <= ?", "cn", startDate, target).
			Order("trade_date ASC").Find(&calendar).Error; err == nil {
			for _, day := range calendar {
				calendarByDate[day.TradeDate] = day.IsOpen
			}
		}
	}
	complete := true
	for day := start.AddDate(0, 0, 1); !day.After(targetDay); day = day.AddDate(0, 0, 1) {
		date := day.Format("2006-01-02")
		if isOpen, known := calendarByDate[date]; known {
			if isOpen {
				pending = append(pending, date)
			}
			continue
		}
		if wd := day.Weekday(); wd >= time.Monday && wd <= time.Friday {
			complete = false
		}
	}
	return pending, complete
}

func lhbPendingDates(cursor, target string) []string {
	pending, _ := lhbPendingDatesWithCoverage(cursor, target)
	return pending
}

// compactDate 2026-07-08 → 20260708（涨停池接口的 date 参数格式）。
func compactDate(d string) string {
	out := make([]byte, 0, 8)
	for i := 0; i < len(d); i++ {
		if d[i] != '-' {
			out = append(out, d[i])
		}
	}
	return string(out)
}

// ---------- 消费查询 ----------

// MoodTrendPoint 情绪趋势单点。历史不可回溯，缺勤日不补造。
type MoodTrendPoint struct {
	TradeDate    string  `json:"trade_date"`
	LimitUpCount int     `json:"limit_up_count"`
	BrokenCount  int     `json:"broken_count"`
	BrokenRate   float64 `json:"broken_rate"`
	MaxStreak    int     `json:"max_streak"`
	YztAvgChg    float64 `json:"yzt_avg_chg"`
	YztUpRatio   float64 `json:"yzt_up_ratio"`
}

// StreakLadder 连板梯队，按高度降序，每档保留全部当日涨停股。
type StreakLadder struct {
	Streak int                  `json:"streak"`
	Count  int                  `json:"count"`
	Stocks []model.LimitUpStock `json:"stocks"`
}

// MoodOverviewView 盘面情绪页聚合响应。
type MoodOverviewView struct {
	Market        string                 `json:"market"`
	Latest        *model.MarketMoodDaily `json:"latest"`
	StreakDist    map[string]int         `json:"streak_dist"`
	StreakLadders []StreakLadder         `json:"streak_ladders"`
	Trend         []MoodTrendPoint       `json:"trend"`
	SealFundTop   []model.LimitUpStock   `json:"seal_fund_top"`
}

// MoodOverview 返回最近情绪快照、当日连板梯队、近 N 日趋势和封板资金 Top。
// 多表读取放在同一快照事务，避免恰逢盘后同步时 Latest/Trend/梯队跨交易日混合。
func (s *MoodService) MoodOverview(ctx context.Context, market string, days int) (*MoodOverviewView, error) {
	market = strings.ToLower(strings.TrimSpace(market))
	if market != "cn" {
		return nil, errors.New("盘面情绪仅支持 A 股（market=cn）")
	}
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if days <= 0 {
		days = 20
	}
	if days > 120 {
		days = 120
	}
	out := &MoodOverviewView{
		Market: market, StreakDist: map[string]int{},
		StreakLadders: []StreakLadder{}, Trend: []MoodTrendPoint{},
		SealFundTop: []model.LimitUpStock{},
	}

	var latest model.MarketMoodDaily
	var moodRows []model.MarketMoodDaily
	var stocks []model.LimitUpStock
	if ctx == nil {
		ctx = context.Background()
	}
	found := false
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Where("market = ?", market).Order("trade_date DESC").First(&latest).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		if err := tx.Where("market = ?", market).Order("trade_date DESC").Limit(days).Find(&moodRows).Error; err != nil {
			return err
		}
		return tx.Where("market = ? AND trade_date = ?", market, latest.TradeDate).
			Order("streak DESC, seal_fund DESC, symbol ASC").Find(&stocks).Error
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return out, nil
	}
	out.Latest = &latest
	if latest.StreakDistJSON != "" {
		_ = json.Unmarshal([]byte(latest.StreakDistJSON), &out.StreakDist)
	}
	for i := len(moodRows) - 1; i >= 0; i-- {
		r := moodRows[i]
		out.Trend = append(out.Trend, MoodTrendPoint{
			TradeDate: r.TradeDate, LimitUpCount: r.LimitUpCount,
			BrokenCount: r.BrokenCount, BrokenRate: r.BrokenRate, MaxStreak: r.MaxStreak,
			YztAvgChg: r.YztAvgChg, YztUpRatio: r.YztUpRatio,
		})
	}

	byStreak := make(map[int][]model.LimitUpStock)
	for _, stock := range stocks {
		streak := stock.Streak
		if streak < 1 {
			streak = 1
		}
		byStreak[streak] = append(byStreak[streak], stock)
	}
	heights := make([]int, 0, len(byStreak))
	for height := range byStreak {
		heights = append(heights, height)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(heights)))
	for _, height := range heights {
		group := byStreak[height]
		out.StreakLadders = append(out.StreakLadders, StreakLadder{Streak: height, Count: len(group), Stocks: group})
	}

	sort.SliceStable(stocks, func(i, j int) bool {
		if stocks[i].SealFund == stocks[j].SealFund {
			return stocks[i].Symbol < stocks[j].Symbol
		}
		return stocks[i].SealFund > stocks[j].SealFund
	})
	if len(stocks) > 10 {
		stocks = stocks[:10]
	}
	out.SealFundTop = stocks
	return out, nil
}

// LhbDailyItem 全市场龙虎榜一行，并入同股同日机构席位统计。
type LhbDailyItem struct {
	Symbol       string  `json:"symbol"`
	Name         string  `json:"name"`
	Reason       string  `json:"reason"`
	Note         string  `json:"note,omitempty"`
	Close        float64 `json:"close"`
	ChangePct    float64 `json:"change_pct"`
	NetBuy       float64 `json:"net_buy"`
	BuyAmt       float64 `json:"buy_amt"`
	SellAmt      float64 `json:"sell_amt"`
	DealAmt      float64 `json:"deal_amt"`
	NetRatio     float64 `json:"net_ratio"`
	TurnoverRate float64 `json:"turnover_rate"`
	OrgNetBuy    float64 `json:"org_net_buy"`
	OrgBuyTimes  int     `json:"org_buy_times"`
	OrgSellTimes int     `json:"org_sell_times"`
}

type LhbDailyView struct {
	Market    string         `json:"market"`
	TradeDate string         `json:"trade_date"`
	Items     []LhbDailyItem `json:"items"`
}

// LhbDaily 返回指定交易日全市场龙虎榜；date 为空时回退最近有数据日。
func (s *MoodService) LhbDaily(ctx context.Context, market, date string, limit int) (*LhbDailyView, error) {
	market = strings.ToLower(strings.TrimSpace(market))
	if market != "cn" {
		return nil, errors.New("龙虎榜仅支持 A 股（market=cn）")
	}
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	date = strings.TrimSpace(date)
	if date != "" {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return nil, errors.New("date 格式应为 YYYY-MM-DD")
		}
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []model.LhbEntry
	var orgRows []model.LhbOrgDaily
	if ctx == nil {
		ctx = context.Background()
	}
	if err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if date == "" {
			var dates []string
			if err := tx.Model(&model.LhbEntry{}).Where("market = ?", market).
				Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error; err != nil {
				return err
			}
			if len(dates) > 0 {
				date = dates[0]
			}
		}
		if date == "" {
			return nil
		}
		if err := tx.Where("market = ? AND trade_date = ?", market, date).
			Order("net_buy DESC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		return tx.Where("market = ? AND trade_date = ?", market, date).Find(&orgRows).Error
	}); err != nil {
		return nil, err
	}
	out := &LhbDailyView{Market: market, TradeDate: date, Items: []LhbDailyItem{}}
	orgBySymbol := make(map[string]model.LhbOrgDaily, len(orgRows))
	for _, row := range orgRows {
		orgBySymbol[row.Symbol] = row
	}
	for _, row := range rows {
		item := LhbDailyItem{
			Symbol: row.Symbol, Name: row.Name, Reason: row.Reason, Note: row.Note,
			Close: row.Close, ChangePct: row.ChangePct, NetBuy: row.NetBuy,
			BuyAmt: row.BuyAmt, SellAmt: row.SellAmt, DealAmt: row.DealAmt,
			NetRatio: row.NetRatio, TurnoverRate: row.TurnoverRate,
		}
		if org, ok := orgBySymbol[row.Symbol]; ok {
			item.OrgNetBuy = org.NetBuy
			item.OrgBuyTimes = org.BuyTimes
			item.OrgSellTimes = org.SellTimes
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

type PopularityDailyItem struct {
	Symbol   string `json:"symbol"`
	Name     string `json:"name"`
	Rank     int    `json:"rank"`
	PrevRank int    `json:"prev_rank"`
	IsNew    bool   `json:"is_new"`
}

type PopularityDailyView struct {
	Market    string                `json:"market"`
	TradeDate string                `json:"trade_date"`
	Items     []PopularityDailyItem `json:"items"`
}

// PopularityDaily 返回指定日人气榜；date 为空时回退最近快照，并标记新上榜。
func (s *MoodService) PopularityDaily(ctx context.Context, market, date string) (*PopularityDailyView, error) {
	market = strings.ToLower(strings.TrimSpace(market))
	if market != "cn" {
		return nil, errors.New("人气榜仅支持 A 股（market=cn）")
	}
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	date = strings.TrimSpace(date)
	if date != "" {
		if _, err := time.Parse("2006-01-02", date); err != nil {
			return nil, errors.New("date 格式应为 YYYY-MM-DD")
		}
	}
	var rows []model.PopularityRank
	var universe []model.StockUniverseDaily
	var states []model.MarketSyncState
	var stocks []model.Stock
	if ctx == nil {
		ctx = context.Background()
	}
	if err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if date == "" {
			var dates []string
			if err := tx.Model(&model.PopularityRank{}).Where("market = ?", market).
				Order("trade_date DESC").Limit(1).Pluck("trade_date", &dates).Error; err != nil {
				return err
			}
			if len(dates) > 0 {
				date = dates[0]
			}
		}
		if date == "" {
			return nil
		}
		// rank 是 MySQL 8.0.2+ 保留字（窗口函数 RANK()）：GORM 的 Order(string) 走
		// clause.Column{Raw:true} 原样拼接不加引号，裸 `ORDER BY rank` 在 MySQL 上是
		// ERROR 1064，而 SQLite 不把 rank 当保留字 → 单测全绿、生产必挂。
		// 用 OrderByColumn 让方言各自加引号（写入侧 clause.AssignmentColumns 本就正确）。
		if err := tx.Where("market = ? AND trade_date = ?", market, date).
			Order(clause.OrderByColumn{Column: clause.Column{Name: "rank"}}).
			Order(clause.OrderByColumn{Column: clause.Column{Name: "symbol"}}).
			Find(&rows).Error; err != nil {
			return err
		}
		symbols := make([]string, 0, len(rows))
		for _, row := range rows {
			symbols = append(symbols, row.Symbol)
		}
		if len(symbols) == 0 {
			return nil
		}
		var universeDates []string
		if err := tx.Model(&model.StockUniverseDaily{}).Where("market = ?", market).
			Order("trade_date DESC").Limit(1).Pluck("trade_date", &universeDates).Error; err != nil {
			return err
		}
		if len(universeDates) > 0 {
			if err := tx.Select("symbol", "name").
				Where("market = ? AND trade_date = ? AND symbol IN ?", market, universeDates[0], symbols).
				Find(&universe).Error; err != nil {
				return err
			}
		}
		if err := tx.Select("symbol", "name").
			Where("market = ? AND symbol IN ?", market, symbols).Find(&states).Error; err != nil {
			return err
		}
		return tx.Select("symbol", "name").
			Where("market = ? AND symbol IN ?", market, symbols).Find(&stocks).Error
	}); err != nil {
		return nil, err
	}
	out := &PopularityDailyView{Market: market, TradeDate: date, Items: []PopularityDailyItem{}}
	nameBySymbol := make(map[string]string, len(rows))
	for _, stock := range stocks {
		if stock.Name != "" {
			nameBySymbol[stock.Symbol] = stock.Name
		}
	}
	for _, state := range states {
		if state.Name != "" {
			nameBySymbol[state.Symbol] = state.Name
		}
	}
	for _, stock := range universe {
		if stock.Name != "" {
			nameBySymbol[stock.Symbol] = stock.Name
		}
	}
	for _, row := range rows {
		out.Items = append(out.Items, PopularityDailyItem{
			Symbol: row.Symbol, Name: nameBySymbol[row.Symbol], Rank: row.Rank,
			PrevRank: row.PrevRank, IsNew: row.IsNew || row.PrevRank <= 0,
		})
	}
	return out, nil
}

// moodBrief 最近一日情绪温度计（市场分析快照/日报快照的 mood 段）。无数据返回 nil。
// 连板分布转成可读 map；日期随行返回，供 prompt 声明数据归属日。
func moodBrief() map[string]any {
	if common.DB == nil {
		return nil
	}
	var row model.MarketMoodDaily
	if err := common.DB.Where("market = ?", "cn").
		Order("trade_date DESC").First(&row).Error; err != nil {
		return nil
	}
	out := map[string]any{
		"trade_date":     row.TradeDate,
		"limit_up_count": row.LimitUpCount,
		"broken_count":   row.BrokenCount,
		"broken_rate":    row.BrokenRate,
		"max_streak":     row.MaxStreak,
		"note":           "涨停/炸板口径为东财涨停池盘后快照；broken_rate=炸板/(涨停+炸板)；yzt_avg_chg 为昨日涨停股今日平均涨跌幅（打板溢价）",
	}
	if row.StreakDistJSON != "" {
		var dist map[string]int
		if json.Unmarshal([]byte(row.StreakDistJSON), &dist) == nil {
			out["streak_dist"] = dist
		}
	}
	if row.YztCount > 0 {
		out["yzt_count"] = row.YztCount
		out["yzt_avg_chg"] = row.YztAvgChg
		out["yzt_up_ratio"] = row.YztUpRatio
	}
	return out
}

// lhbSignal 推荐候选的龙虎榜信号（最近一个有数据交易日的口径，T-1 信息）。
type lhbSignal struct {
	TradeDate string
	NetBuyYi  float64 // 龙虎榜净买额（亿元，同股多原因行取净买额绝对值最大的一条）
	Reason    string
	OrgNetYi  float64 // 机构净买额（亿元；0=无机构行）
	OrgBuys   int     // 机构买入次数
}

// signalStaleMaxOpenDays M3a/M3b 信号消费水位：库内最新交易日落后应有交易日超过该
// 开市日数时视为过期，不再作「最近信号」喂给推荐加分（这类信号本就是 T-1 口径，
// 容忍 2 个开市日；再旧的龙虎榜/人气/盘中形态冒充近期信号会污染加分项）。
const signalStaleMaxOpenDays = 2

// signalDateUsable 库内最新信号日期是否仍在可用水位内（P1：不能只取库内 MAX——
// 采集停摆时旧记录会永远冒充「最近信号」）。
func signalDateUsable(latest string) bool {
	if latest == "" {
		return false
	}
	expected := prevOpenTradeDate(time.Now().Format("2006-01-02"))
	lag := openDaysBehind(latest, expected)
	return lag >= 0 && lag <= signalStaleMaxOpenDays
}

// lhbSignalsFor 批量查询候选的最近龙虎榜信号。主榜和机构榜必须来自同一读事务快照。
func lhbSignalsFor(ctx context.Context, symbols []string) map[string]lhbSignal {
	out := map[string]lhbSignal{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows []model.LhbEntry
	var orgRows []model.LhbOrgDaily
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest string
		if err := tx.Model(&model.LhbEntry{}).Where("market = ?", "cn").
			Select("MAX(trade_date)").Scan(&latest).Error; err != nil {
			return err
		}
		if !signalDateUsable(latest) {
			return nil
		}
		if err := tx.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).
			Find(&rows).Error; err != nil {
			return err
		}
		return tx.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).
			Find(&orgRows).Error
	})
	if err != nil {
		return out
	}
	for _, r := range rows {
		sig, ok := out[r.Symbol]
		if !ok || absF(r.NetBuy) > absF(sig.NetBuyYi*1e8) {
			out[r.Symbol] = lhbSignal{
				TradeDate: r.TradeDate, NetBuyYi: round2(r.NetBuy / 1e8),
				Reason: r.Reason, OrgNetYi: sig.OrgNetYi, OrgBuys: sig.OrgBuys,
			}
		}
	}
	for _, r := range orgRows {
		sig := out[r.Symbol]
		if sig.TradeDate == "" {
			sig.TradeDate = r.TradeDate
		}
		sig.OrgNetYi = round2(r.NetBuy / 1e8)
		sig.OrgBuys = r.BuyTimes
		out[r.Symbol] = sig
	}
	return out
}

// popSignal 推荐候选的人气榜信号。
type popSignal struct {
	TradeDate string
	Rank      int
	PrevRank  int // <=0 = 新上榜
	IsNew     bool
}

// popSignalsFor 批量查询候选的最近人气榜名次（MAX 日期与明细同一快照）。
func popSignalsFor(ctx context.Context, symbols []string) map[string]popSignal {
	out := map[string]popSignal{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows []model.PopularityRank
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var latest string
		if err := tx.Model(&model.PopularityRank{}).Where("market = ?", "cn").
			Select("MAX(trade_date)").Scan(&latest).Error; err != nil {
			return err
		}
		if !signalDateUsable(latest) {
			return nil
		}
		return tx.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).
			Find(&rows).Error
	})
	if err != nil {
		return out
	}
	for _, r := range rows {
		out[r.Symbol] = popSignal{TradeDate: r.TradeDate, Rank: r.Rank, PrevRank: r.PrevRank, IsNew: r.IsNew}
	}
	return out
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// LhbRecordView 个股详情页「龙虎榜上榜记录」行（同日机构净买合并展示）。
type LhbRecordView struct {
	TradeDate string  `json:"trade_date"`
	Reason    string  `json:"reason"`
	Note      string  `json:"note,omitempty"`
	ChangePct float64 `json:"change_pct"`
	NetBuy    float64 `json:"net_buy"`            // 龙虎榜净买额（元）
	DealAmt   float64 `json:"deal_amt"`           // 龙虎榜成交额（元）
	OrgNetBuy float64 `json:"org_net_buy"`        // 机构净买额（元；0=当日无机构行）
	OrgBuys   int     `json:"org_buys,omitempty"` // 机构买入次数
}

// StockLhbRecords 个股最近上榜记录（详情页）。按日期降序，同日多原因各自成行。
// 主榜与机构榜在同一读事务内查询，避免同步事务提交夹缝产生跨版本混合。
// DB/context 错误如实返回：静默吞掉会让「查询失败」与「近期无上榜」在前端长得一模一样。
func (s *MoodService) StockLhbRecords(ctx context.Context, symbol string, limit int) ([]LhbRecordView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if !isSixDigits(symbol) {
		// 非 A 股 6 位代码本就不可能有龙虎榜记录，属诚实空集不是错误。
		return []LhbRecordView{}, nil
	}
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	if ctx == nil {
		ctx = context.Background()
	}
	var rows []model.LhbEntry
	var orgRows []model.LhbOrgDaily
	if err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("symbol = ? AND market = ?", symbol, "cn").
			Order("trade_date DESC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		dates := make([]string, 0, len(rows))
		for _, r := range rows {
			dates = append(dates, r.TradeDate)
		}
		return tx.Where("symbol = ? AND market = ? AND trade_date IN ?", symbol, "cn", dates).
			Find(&orgRows).Error
	}); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return []LhbRecordView{}, nil
	}
	orgBy := map[string]model.LhbOrgDaily{}
	for _, r := range orgRows {
		orgBy[r.TradeDate] = r
	}
	out := make([]LhbRecordView, 0, len(rows))
	for _, r := range rows {
		v := LhbRecordView{
			TradeDate: r.TradeDate, Reason: r.Reason, Note: r.Note,
			ChangePct: r.ChangePct, NetBuy: r.NetBuy, DealAmt: r.DealAmt,
		}
		if org, ok := orgBy[r.TradeDate]; ok {
			v.OrgNetBuy = org.NetBuy
			v.OrgBuys = org.BuyTimes
		}
		out = append(out, v)
	}
	return out, nil
}

// ---------- 后台任务 ----------

// runMoodPools 执行当日涨停池 + 人气榜采集，两者各自游标独立推进（抽出供测试）。
// 涨停池与人气榜早前共用一个游标：人气榜是实时榜（不可回溯，错过=真丢失），失败会被
// 涨停池成功带过、当天不再重试。拆成两个 option key 后各自成功才推进、各自判断是否需跑。
func (s *MoodService) runMoodPools(ctx context.Context, target, today string) {
	if optionValue(optMoodPoolDay) != target {
		if err := s.SyncZTPools(ctx, target); err != nil {
			// 涨停池上游不可回溯：错过采集窗口后（如次日盘中补跑昨日）该日数据
			// 已翻页，ErrNoData 是预期的诚实缺失，不当故障刷警告。
			if errors.Is(err, datasource.ErrNoData) {
				common.SysDebug("涨停池 %s 数据不可得（上游不可回溯/非交易日）: %v", target, err)
			} else {
				common.SysWarn("涨停池采集失败 %s: %v", target, err)
			}
		} else {
			_ = model.UpsertOption(optMoodPoolDay, target)
			common.SysLog("涨停池情绪数据采集完成 %s", target)
		}
	}
	// 人气榜是实时榜（无历史），仅当目标日=今天时采集；补昨日无意义。
	if target == today && optionValue(optMoodPopDay) != target {
		if err := s.SyncPopularity(ctx, target); err != nil {
			common.SysWarn("人气榜采集失败: %v", err)
		} else {
			_ = model.UpsertOption(optMoodPopDay, target)
			common.SysLog("人气榜采集完成 %s", target)
		}
	}
}

// lhbSkipAfterFails 历史日连续失败达此次数即「不再阻塞后续日期」。重试间隔 30min，
// 3 次≈1.5 小时持续失败——足以区分瞬时故障与恒定失败。
//
// 为什么必须能继续往后走：pending 按交易日**升序**逐日补，任一天失败就 return false、
// 游标不动，下轮重试的还是同一个最早缺口。而恒定失败是可达的——该日东财确无榜
// （ErrNoData）、或 parseLhbRowStrict 因缺必填字段整天作废——于是最老的一天会把其后
// 所有天连同**今天**的榜一起永久卡死。今天（target）不在此列：盘后逐步落库，
// 18:00 前未发布是正常态，必须继续等。
//
// **但「继续往后走」≠「该日已完成」**：游标推进的同时必须把该日登记进
// optMoodLhbGaps 未解决缺口清单并每轮持续重试，只有下面两种结局才算终结：
//   - 主榜+机构榜在同一事务原子提交成功；
//   - 可证明的历史空榜（上游明确回 ErrNoData、该日已成历史、且连续 lhbSkipAfterFails
//     轮仍为空——「查得到且确实没有」，区别于网络/解析类的「查不到」）。
const lhbSkipAfterFails = 3

// lhbGapRetryPerRound 每轮补跑最多重试的历史缺口数（防单轮被长缺口清单拖满 10 分钟预算）。
const lhbGapRetryPerRound = 5

// lhbGapMaxEntries 未解决缺口清单容量上限。触顶说明上游长期异常，丢最老的一条并告警，
// 避免 option 值无界增长（正常运行永远达不到）。
const lhbGapMaxEntries = 200

// lhbDayFails 同一交易日连续失败计数（进程内；重启后重新计数，宁可多试几轮）。
var (
	lhbFailMu   sync.Mutex
	lhbDayFails = map[string]int{}
)

func bumpLhbDayFail(date string) int {
	lhbFailMu.Lock()
	defer lhbFailMu.Unlock()
	lhbDayFails[date]++
	return lhbDayFails[date]
}

func clearLhbDayFail(date string) {
	lhbFailMu.Lock()
	defer lhbFailMu.Unlock()
	delete(lhbDayFails, date)
}

// loadLhbGaps 读未解决缺口清单（升序去重；解析失败按空处理，不因脏值卡死采集）。
func loadLhbGaps() []string {
	raw := optionValue(optMoodLhbGaps)
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var dates []string
	if err := json.Unmarshal([]byte(raw), &dates); err != nil {
		common.SysWarn("龙虎榜缺口清单解析失败（按空处理）: %v", err)
		return nil
	}
	out := make([]string, 0, len(dates))
	seen := map[string]bool{}
	for _, d := range dates {
		d = strings.TrimSpace(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

// saveLhbGaps 落库未解决缺口清单（空清单落空串，读侧等价于无缺口）。
func saveLhbGaps(dates []string) error {
	if len(dates) == 0 {
		return model.UpsertOption(optMoodLhbGaps, "")
	}
	b, err := json.Marshal(dates)
	if err != nil {
		return err
	}
	return model.UpsertOption(optMoodLhbGaps, string(b))
}

// appendLhbGap 登记一个未解决缺口（升序去重、容量有界）。
func appendLhbGap(gaps []string, date string) []string {
	for _, d := range gaps {
		if d == date {
			return gaps
		}
	}
	out := append(append([]string{}, gaps...), date)
	sort.Strings(out)
	if len(out) > lhbGapMaxEntries {
		common.SysWarn("龙虎榜未解决缺口超过 %d 条，丢弃最早的 %s（上游长期异常，请人工核查）",
			lhbGapMaxEntries, out[0])
		out = out[len(out)-lhbGapMaxEntries:]
	}
	return out
}

// lhbDayOutcome 单个交易日的同步结局。
type lhbDayOutcome int

const (
	lhbDayDone        lhbDayOutcome = iota // 主榜+机构榜原子提交成功
	lhbDayEmptyProven                      // 可证明的历史空榜（上游明确回空且已成历史）
	lhbDayRetry                            // 尚未终结，需继续重试
	lhbDayAborted                          // ctx 用尽，本轮预算问题不归因于该日
)

// syncLhbDay 同步单日并归类结局。第二个返回值：Done 时为主榜行数，Retry 时为该日连续失败次数。
func (s *MoodService) syncLhbDay(ctx context.Context, date string) (lhbDayOutcome, int, error) {
	n, err := s.SyncLhb(ctx, date)
	if err == nil {
		clearLhbDayFail(date)
		return lhbDayDone, n, nil
	}
	// ctx 用尽属本轮预算问题不是该日的问题，不计入失败次数。
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return lhbDayAborted, 0, err
	}
	fails := bumpLhbDayFail(date)
	// 可证明的历史空榜：上游明确回「查得到且确实没有」（ErrNoData），该日已成历史
	// （当日未发布不能当空），且连续多轮仍为空。网络/解析类失败不适用——那是「查不到」。
	if errors.Is(err, datasource.ErrNoData) && fails >= lhbSkipAfterFails &&
		isHistoricalTradeDate(date, s.nowOrDefault()) {
		clearLhbDayFail(date)
		return lhbDayEmptyProven, 0, nil
	}
	return lhbDayRetry, fails, err
}

// retryLhbGaps 每轮优先重试已登记的未解决缺口。补齐/确认空榜即从清单移除，
// 其余原样保留继续下轮重试（**绝不因为重试过就当完成**）。
func (s *MoodService) retryLhbGaps(ctx context.Context, gaps []string) ([]string, bool) {
	if len(gaps) == 0 {
		return gaps, false
	}
	remain := make([]string, 0, len(gaps))
	changed, aborted, tried := false, false, 0
	for i := 0; i < len(gaps); i++ {
		date := gaps[i]
		if tried >= lhbGapRetryPerRound || ctx.Err() != nil {
			remain = append(remain, gaps[i:]...)
			break
		}
		tried++
		outcome, _, err := s.syncLhbDay(ctx, date)
		switch outcome {
		case lhbDayDone:
			changed = true
			common.SysLog("龙虎榜历史缺口 %s 已补齐", date)
		case lhbDayEmptyProven:
			changed = true
			common.SysLog("龙虎榜历史缺口 %s 经上游多轮确认为空榜，收口", date)
		case lhbDayAborted:
			aborted = true
			remain = append(remain, gaps[i:]...)
		default:
			remain = append(remain, date)
			common.SysDebug("龙虎榜历史缺口 %s 仍未补齐（继续重试）: %v", date, err)
		}
		if aborted {
			break
		}
	}
	if changed {
		if err := saveLhbGaps(remain); err != nil {
			common.SysWarn("龙虎榜缺口清单落库失败: %v", err)
			return gaps, aborted
		}
	}
	return remain, aborted
}

// runMoodLhb 从游标之后按交易日升序补到 target，并在每轮开头重试历史未解决缺口。
// 单日失败即停确保下轮仍从最早缺口开始；历史日连续失败达 lhbSkipAfterFails 次时，
// **登记进未解决缺口清单**后才推进游标继续后续日期——缺口持续重试，不冒充完成。
// 每个成功日单独推进游标，长回填即使超时也能从已提交位置续跑。
// 返回值 true 仅当「游标已达 target 且无未解决缺口」，false 会让调度循环 30 分钟后重试。
func (s *MoodService) runMoodLhb(ctx context.Context, target string) bool {
	if ctx == nil {
		ctx = context.Background()
	}
	gaps, aborted := s.retryLhbGaps(ctx, loadLhbGaps())
	if aborted {
		common.SysWarn("龙虎榜历史缺口重试中止：本轮预算用尽")
		return false
	}
	cursor := optionValue(optMoodLhbDay)
	if cursor == target {
		return len(gaps) == 0
	}
	pending, complete := lhbPendingDatesWithCoverage(cursor, target)
	if !complete && s.repairCalendar != nil {
		if err := s.repairCalendar(ctx); err != nil {
			common.SysWarn("龙虎榜补缺口前修复交易日历失败: %v", err)
			return false
		}
		pending, complete = lhbPendingDatesWithCoverage(cursor, target)
	}
	if !complete {
		common.SysWarn("龙虎榜补缺口暂停：%s 至 %s 的交易日历不完整", cursor, target)
		return false
	}
	for _, date := range pending {
		if err := ctx.Err(); err != nil {
			common.SysWarn("龙虎榜补缺口中止 %s: %v", date, err)
			return false
		}
		outcome, info, err := s.syncLhbDay(ctx, date)
		switch outcome {
		case lhbDayAborted:
			common.SysWarn("龙虎榜补缺口中止 %s: %v", date, err)
			return false
		case lhbDayRetry:
			if date != target && info >= lhbSkipAfterFails {
				// 继续补后续日期，但该日**登记为未解决缺口**持续重试——游标推进只表示
				// 「已处理到这里」，缺口清单非空时 runMoodLhb 永不宣称追平。
				gaps = appendLhbGap(gaps, date)
				if serr := saveLhbGaps(gaps); serr != nil {
					common.SysWarn("龙虎榜缺口清单落库失败 %s: %v", date, serr)
					return false
				}
				clearLhbDayFail(date)
				if uerr := model.UpsertOption(optMoodLhbDay, date); uerr != nil {
					common.SysWarn("龙虎榜游标推进失败 %s: %v", date, uerr)
					return false
				}
				cursor = date
				common.SysWarn("龙虎榜 %s 连续 %d 次失败，登记为未解决缺口并继续补后续（将持续重试；最后错误: %v）",
					date, info, err)
				continue
			}
			if errors.Is(err, datasource.ErrNoData) {
				common.SysDebug("龙虎榜 %s 暂无数据（未发布或日历有误），下次再试（第 %d 次）", date, info)
			} else {
				common.SysWarn("龙虎榜采集失败 %s（第 %d 次）: %v", date, info, err)
			}
			return false
		case lhbDayEmptyProven:
			common.SysLog("龙虎榜 %s 经上游多轮确认为空榜，收口", date)
		default:
			common.SysLog("龙虎榜采集完成 %s：%d 行", date, info)
		}
		if err := model.UpsertOption(optMoodLhbDay, date); err != nil {
			common.SysWarn("龙虎榜游标推进失败 %s: %v", date, err)
			return false
		}
		cursor = date
	}
	return cursor == target && len(gaps) == 0
}

// StartMoodJobs 盘后错峰采集：16:35 涨停池+人气榜、18:45 龙虎榜+机构统计。
// 涨停池与龙虎榜使用独立循环：前者每日执行，后者按游标升序补缺口且落后时约 30 分钟重试。
func StartMoodJobs(mgr *datasource.Manager) *MoodService {
	svc := NewMoodService()
	marketSvc := NewMarketService(mgr)
	svc.repairCalendar = func(ctx context.Context) error {
		_, err := marketSvc.BackfillCalendar(ctx, "cn")
		return err
	}
	runPools := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		svc.runMoodPools(ctx, moodTargetDate(time.Now(), moodPoolCutoffMin), time.Now().Format("2006-01-02"))
	}

	// 涨停池/人气榜每日循环。上游不可回溯，失败仍保持诚实缺失，不与龙虎榜重试互相阻塞。
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(3 * time.Minute)
		for {
			runPools()
			time.Sleep(time.Until(nextDailyAt(time.Now(), 16, 35)))
		}
	}()

	// 龙虎榜独立循环。成功追平后睡到下个 18:45；失败、超时或任务跨过 cutoff
	// 导致目标日变化时，约 30 分钟后重试最早缺口。
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(3 * time.Minute)
		for {
			target := moodTargetDate(time.Now(), moodLhbCutoffMin)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			caughtUp := svc.runMoodLhb(ctx, target)
			cancel()
			currentTarget := moodTargetDate(time.Now(), moodLhbCutoffMin)
			if !caughtUp || optionValue(optMoodLhbDay) != currentTarget {
				time.Sleep(lhbRetryInterval)
				continue
			}
			time.Sleep(time.Until(nextDailyAt(time.Now(), 18, 45)))
		}
	}()
	return svc
}

// nextDailyAt 下一个每日 hh:mm 时点（已过则明天）。
func nextDailyAt(now time.Time, hour, min int) time.Time {
	next := time.Date(now.Year(), now.Month(), now.Day(), hour, min, 0, 0, now.Location())
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next
}

// optionValue 读 options 表单值（不存在返回空串）。
func optionValue(key string) string {
	if common.DB == nil {
		return ""
	}
	var opt model.Option
	if err := common.DB.Where("`key` = ?", key).First(&opt).Error; err != nil {
		return ""
	}
	return opt.Value
}
