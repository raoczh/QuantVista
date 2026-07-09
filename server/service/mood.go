package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
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
	optMoodPoolDay = "mood_pool_day" // 涨停池+人气榜 已成功同步的交易日
	optMoodLhbDay  = "mood_lhb_day"  // 龙虎榜+机构统计 已成功同步的交易日

	moodPoolCutoffMin = 16*60 + 35 // 16:35 后当日涨停池数据视为可采
	moodLhbCutoffMin  = 18*60 + 45 // 18:45 后当日龙虎榜数据视为可采

	lhbBackfillDays = 30 // 首轮部署回填龙虎榜/机构统计的自然日跨度（详情页上榜记录立即可用）
)

// MoodService 扩展数据采集与查询。
type MoodService struct {
	em *datasource.EastMoneyAdapter
}

func NewMoodService() *MoodService {
	return &MoodService{em: datasource.NewEastMoneyAdapter()}
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
//（情绪冰点日可为空），按 0 家/缺失聚合；网络类失败则整轮失败防半截聚合。
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
// ErrNoData 透传给调用方（可能是「当日未发布」或「非交易日」，游标不推进，下次再试）。
func (s *MoodService) SyncLhb(ctx context.Context, tradeDate string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	rows, err := s.em.GetLhbDaily(ctx, tradeDate)
	if err != nil {
		return 0, err
	}
	n, err := upsertLhbRows(rows)
	if err != nil {
		return 0, err
	}
	// 机构统计 best-effort：当日无机构上榜（ErrNoData）是正常态，其余失败仅告警不阻断。
	orgRows, oerr := s.em.GetLhbOrgDaily(ctx, tradeDate)
	if oerr != nil && !errors.Is(oerr, datasource.ErrNoData) {
		common.SysWarn("机构买卖统计拉取失败 %s: %v", tradeDate, oerr)
	}
	if err := upsertLhbOrgRows(orgRows); err != nil {
		common.SysWarn("机构买卖统计落库失败 %s: %v", tradeDate, err)
	}
	return n, nil
}

// upsertLhbRows 龙虎榜主表批量 upsert（键 symbol+market+trade_date+change_type 幂等）。
func upsertLhbRows(rows []datasource.LhbRow) (int, error) {
	recs := make([]model.LhbEntry, 0, len(rows))
	for _, r := range rows {
		recs = append(recs, model.LhbEntry{
			Symbol: r.Symbol, Market: "cn", TradeDate: r.TradeDate,
			ChangeType: truncateRunes(r.ChangeType, 24), Name: r.Name,
			Reason: truncateRunes(r.Reason, 128), Note: truncateRunes(r.Note, 128),
			Close: r.Close, ChangePct: round2(r.ChangePct),
			NetBuy: r.NetBuy, BuyAmt: r.BuyAmt, SellAmt: r.SellAmt, DealAmt: r.DealAmt,
			NetRatio: round2(r.NetRatio), TurnoverRate: round2(r.TurnoverRate),
		})
	}
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
	recs := make([]model.LhbOrgDaily, 0, len(rows))
	for _, r := range rows {
		recs = append(recs, model.LhbOrgDaily{
			Symbol: r.Symbol, Market: "cn", TradeDate: r.TradeDate, Name: r.Name,
			Close: r.Close, ChangePct: round2(r.ChangePct),
			BuyTimes: r.BuyTimes, SellTimes: r.SellTimes,
			BuyAmt: r.BuyAmt, SellAmt: r.SellAmt, NetBuy: r.NetBuy,
			NetRatio: round2(r.NetRatio), Reason: truncateRunes(r.Reason, 128),
		})
	}
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

// backfillLhb 首轮部署回填近 days 个自然日的龙虎榜（详情页上榜记录立即可用）。
// 逐日拉取由 datacenter 包级令牌桶自然限速（QPS≤2）；单日失败跳过不阻断。
func (s *MoodService) backfillLhb(ctx context.Context, endDate string, days int) {
	end, err := time.ParseInLocation("2006-01-02", endDate, time.Local)
	if err != nil {
		return
	}
	total := 0
	for i := 1; i <= days; i++ {
		if ctx.Err() != nil {
			return
		}
		d := end.AddDate(0, 0, -i)
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday {
			continue
		}
		n, err := s.SyncLhb(ctx, d.Format("2006-01-02"))
		if err != nil && !errors.Is(err, datasource.ErrNoData) {
			common.SysDebug("龙虎榜回填跳过 %s: %v", d.Format("2006-01-02"), err)
			continue
		}
		total += n
	}
	if total > 0 {
		common.SysLog("龙虎榜历史回填完成：近 %d 天共 %d 行", days, total)
	}
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

// lhbSignalsFor 批量查询候选的最近龙虎榜信号。
func lhbSignalsFor(symbols []string) map[string]lhbSignal {
	out := map[string]lhbSignal{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	var latest string
	common.DB.Model(&model.LhbEntry{}).Where("market = ?", "cn").
		Select("MAX(trade_date)").Scan(&latest)
	if latest == "" {
		return out
	}
	var rows []model.LhbEntry
	common.DB.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).Find(&rows)
	for _, r := range rows {
		sig, ok := out[r.Symbol]
		if !ok || absF(r.NetBuy) > absF(sig.NetBuyYi*1e8) {
			out[r.Symbol] = lhbSignal{
				TradeDate: r.TradeDate, NetBuyYi: round2(r.NetBuy / 1e8),
				Reason: r.Reason, OrgNetYi: sig.OrgNetYi, OrgBuys: sig.OrgBuys,
			}
		}
	}
	var orgRows []model.LhbOrgDaily
	common.DB.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).Find(&orgRows)
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

// popSignalsFor 批量查询候选的最近人气榜名次。
func popSignalsFor(symbols []string) map[string]popSignal {
	out := map[string]popSignal{}
	if common.DB == nil || len(symbols) == 0 {
		return out
	}
	var latest string
	common.DB.Model(&model.PopularityRank{}).Where("market = ?", "cn").
		Select("MAX(trade_date)").Scan(&latest)
	if latest == "" {
		return out
	}
	var rows []model.PopularityRank
	common.DB.Where("market = ? AND trade_date = ? AND symbol IN ?", "cn", latest, symbols).Find(&rows)
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
func (s *MoodService) StockLhbRecords(symbol string, limit int) []LhbRecordView {
	if common.DB == nil || !isSixDigits(symbol) {
		return []LhbRecordView{}
	}
	if limit <= 0 || limit > 30 {
		limit = 10
	}
	var rows []model.LhbEntry
	if err := common.DB.Where("symbol = ? AND market = ?", symbol, "cn").
		Order("trade_date DESC, id ASC").Limit(limit).Find(&rows).Error; err != nil {
		return []LhbRecordView{}
	}
	if len(rows) == 0 {
		return []LhbRecordView{}
	}
	dates := make([]string, 0, len(rows))
	for _, r := range rows {
		dates = append(dates, r.TradeDate)
	}
	orgBy := map[string]model.LhbOrgDaily{}
	var orgRows []model.LhbOrgDaily
	common.DB.Where("symbol = ? AND market = ? AND trade_date IN ?", symbol, "cn", dates).Find(&orgRows)
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
	return out
}

// ---------- 后台任务 ----------

// StartMoodJobs 盘后错峰采集：16:35 涨停池+人气榜、18:45 龙虎榜+机构统计。
// 启动 3 分钟后按游标补跑缺口（涨停池上游不可回溯，补跑失败即诚实缺失该日）。
func StartMoodJobs() *MoodService {
	svc := NewMoodService()
	runPools := func() {
		target := moodTargetDate(time.Now(), moodPoolCutoffMin)
		if optionValue(optMoodPoolDay) == target {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := svc.SyncZTPools(ctx, target); err != nil {
			// 涨停池上游不可回溯：错过采集窗口后（如次日盘中补跑昨日）该日数据
			// 已翻页，ErrNoData 是预期的诚实缺失，不当故障刷警告。
			if errors.Is(err, datasource.ErrNoData) {
				common.SysDebug("涨停池 %s 数据不可得（上游不可回溯/非交易日）: %v", target, err)
			} else {
				common.SysWarn("涨停池采集失败 %s: %v", target, err)
			}
			return
		}
		// 人气榜是实时榜（无历史），仅当目标日=今天时采集；补昨日无意义。
		if target == time.Now().Format("2006-01-02") {
			if err := svc.SyncPopularity(ctx, target); err != nil {
				common.SysWarn("人气榜采集失败: %v", err)
			}
		}
		_ = model.UpsertOption(optMoodPoolDay, target)
		common.SysLog("市场情绪数据采集完成 %s", target)
	}
	runLhb := func() {
		target := moodTargetDate(time.Now(), moodLhbCutoffMin)
		if optionValue(optMoodLhbDay) == target {
			return
		}
		firstRun := optionValue(optMoodLhbDay) == ""
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		n, err := svc.SyncLhb(ctx, target)
		if err != nil {
			if errors.Is(err, datasource.ErrNoData) {
				common.SysDebug("龙虎榜 %s 暂无数据（未发布或非交易日），下次再试", target)
			} else {
				common.SysWarn("龙虎榜采集失败 %s: %v", target, err)
			}
			return
		}
		_ = model.UpsertOption(optMoodLhbDay, target)
		common.SysLog("龙虎榜采集完成 %s：%d 行", target, n)
		if firstRun {
			svc.backfillLhb(ctx, target, lhbBackfillDays)
		}
	}
	// 启动补跑 + 每日定时（自然日循环，交易日判定在 moodTargetDate/游标里消化）。
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(3 * time.Minute)
		runPools()
		runLhb()
		for {
			now := time.Now()
			nextPool := nextDailyAt(now, 16, 35)
			nextLhb := nextDailyAt(now, 18, 45)
			if nextPool.Before(nextLhb) {
				time.Sleep(time.Until(nextPool))
				runPools()
			} else {
				time.Sleep(time.Until(nextLhb))
				runLhb()
			}
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
