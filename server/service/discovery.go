package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// CandidateDiscovery 是共享的全市场候选发现事实。它只读取因子宽表一次，
// 不读取用户偏好，也不调用 LLM；用户规则在推荐消费阶段重新执行。
const (
	DiscoveryVersion       = "dd1"
	DiscoveryRunStatusProc = "processing"
	DiscoveryRunStatusOK   = "success"
	DiscoveryRunStatusPart = "partial"
	DiscoveryRunStatusFail = "failed"
	DiscoveryItemReady     = "ready"
	DiscoveryItemPartial   = "partial"
	DiscoveryItemUnknown   = "unknown"
	discoveryTopN          = 100
	discoveryTimeout       = 5 * time.Minute
)

var discoveryChannels = []string{"trend_breakout", "pullback_repair", "value_dividend", "strategy_signal"}
var discoveryRunMu sync.Mutex
var discoveryScheduleMu sync.Mutex
var discoverySchedulePending = map[string]bool{}

type discoveryJobRequest struct {
	Version       int    `json:"version"`
	Market        string `json:"market"`
	TradeDate     string `json:"trade_date,omitempty"`
	TriggerSource string `json:"trigger_source"`
	ParameterHash string `json:"parameter_hash"`
}

// DiscoverySignal 是纯程序发现的中间事实，便于表驱动测试锁定排序和通道配额。
type DiscoverySignal struct {
	Symbol        string
	Name          string
	Market        string
	Channel       string
	Score         float64
	SourceFacts   map[string]any
	FactorSummary map[string]float64
	DataStatus    string
	PartialReason string
}

type discoveryChannelSpec struct {
	name     string
	required []string
	match    func(*FactorTable, int) bool
	score    func(*FactorTable, int) float64
	facts    func(*FactorTable, int) map[string]any
}

func discoveryParameterHash() string {
	// 参数内容明确版本化，避免只改阈值却复用旧日事实。
	raw := strings.Join([]string{DiscoveryVersion, factorSnapshotVersion, "top=100", "trend=v1", "pullback=v1", "value=v1", "strategy=v1"}, "|")
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func finiteFactor(t *FactorTable, key string, i int) (float64, bool) {
	col := t.Col(key)
	if col == nil || i < 0 || i >= len(col) || math.IsNaN(col[i]) || math.IsInf(col[i], 0) {
		return 0, false
	}
	return col[i], true
}

func discoverySpecs() []discoveryChannelSpec {
	return []discoveryChannelSpec{
		{
			name:     "trend_breakout",
			required: []string{"close", "ma20", "bull_align", "high_20d", "chg_20d", "vol_boost", "bias_20"},
			match: func(t *FactorTable, i int) bool {
				close, cOK := finiteFactor(t, "close", i)
				ma20, mOK := finiteFactor(t, "ma20", i)
				bull, bOK := finiteFactor(t, "bull_align", i)
				high, hOK := finiteFactor(t, "high_20d", i)
				chg, chgOK := finiteFactor(t, "chg_20d", i)
				return cOK && mOK && bOK && hOK && chgOK && close >= ma20 && bull == 1 && (high == 1 || chg >= 8)
			},
			score: func(t *FactorTable, i int) float64 {
				chg, _ := finiteFactor(t, "chg_20d", i)
				vol, _ := finiteFactor(t, "vol_boost", i)
				bias, _ := finiteFactor(t, "bias_20", i)
				return clampDiscoveryScore(60 + chg*0.8 + minFloat(vol, 5)*3 - maxFloat(bias-12, 0)*1.2)
			},
			facts: func(t *FactorTable, i int) map[string]any {
				return map[string]any{"rule": "above_ma20_and_bull_align", "breakout": factorBool(t, "high_20d", i)}
			},
		},
		{
			name:     "pullback_repair",
			required: []string{"chg_20d", "chg_5d", "above_ma20", "vol_5v20"},
			match: func(t *FactorTable, i int) bool {
				chg20, a := finiteFactor(t, "chg_20d", i)
				chg5, b := finiteFactor(t, "chg_5d", i)
				above, c := finiteFactor(t, "above_ma20", i)
				vol, d := finiteFactor(t, "vol_5v20", i)
				return a && b && c && d && chg20 >= 10 && chg5 >= -8 && chg5 <= 1 && above == 1 && vol < 1.2
			},
			score: func(t *FactorTable, i int) float64 {
				chg20, _ := finiteFactor(t, "chg_20d", i)
				chg5, _ := finiteFactor(t, "chg_5d", i)
				vol, _ := finiteFactor(t, "vol_5v20", i)
				return clampDiscoveryScore(68 + minFloat(chg20, 30)*0.5 + (-chg5)*1.5 + maxFloat(1.2-vol, 0)*8)
			},
			facts: func(t *FactorTable, i int) map[string]any {
				return map[string]any{"rule": "strong_trend_repaired_above_ma20", "volume_contracting": true}
			},
		},
		{
			name:     "value_dividend",
			required: []string{"div_yield", "pos_60", "volatility_20", "close"},
			match: func(t *FactorTable, i int) bool {
				y, yOK := finiteFactor(t, "div_yield", i)
				pos, pOK := finiteFactor(t, "pos_60", i)
				vol, vOK := finiteFactor(t, "volatility_20", i)
				close, cOK := finiteFactor(t, "close", i)
				return yOK && pOK && vOK && cOK && y >= 2 && pos <= 75 && vol <= 5 && close > 0
			},
			score: func(t *FactorTable, i int) float64 {
				y, _ := finiteFactor(t, "div_yield", i)
				pos, _ := finiteFactor(t, "pos_60", i)
				vol, _ := finiteFactor(t, "volatility_20", i)
				return clampDiscoveryScore(55 + minFloat(y, 10)*3 + maxFloat(75-pos, 0)*0.18 + maxFloat(5-vol, 0)*1.5)
			},
			facts: func(t *FactorTable, i int) map[string]any {
				y, _ := finiteFactor(t, "div_yield", i)
				return map[string]any{"rule": "dividend_yield_and_stable_value", "div_yield": round2(y)}
			},
		},
		{
			name:     "strategy_signal",
			required: []string{"high_20d", "vol_boost", "chg_pct", "amount_yi", "chg_20d", "above_ma20", "chg_5d", "vol_5v20", "pos_60", "chip_profit", "chip_bars", "chg_60d", "volatility_20", "above_ma60", "bull_align"},
			match: func(t *FactorTable, i int) bool {
				return len(matchedDiscoveryStrategies(t, i)) > 0
			},
			score: func(t *FactorTable, i int) float64 {
				amount, _ := finiteFactor(t, "amount_yi", i)
				chg, _ := finiteFactor(t, "chg_20d", i)
				return clampDiscoveryScore(50 + minFloat(amount, 20)*1.5 + chg*0.4)
			},
			facts: func(t *FactorTable, i int) map[string]any {
				return map[string]any{"rule": "builtin_strategy_tree", "strategies": matchedDiscoveryStrategies(t, i)}
			},
		},
	}
}

var discoveryStrategyKeys = []string{"vol-break-20d", "shrink-pullback-ma20", "mild-vol-start", "deep-oversold-chip", "steady-uptrend", "bull-align-trend"}

func matchedDiscoveryStrategies(t *FactorTable, i int) []string {
	matched := make([]string, 0, len(discoveryStrategyKeys))
	for _, key := range discoveryStrategyKeys {
		b, ok := builtinScreenByKey(key)
		if ok && evalCondRow(t, &b.Tree, i) {
			matched = append(matched, key)
		}
	}
	return matched
}

// discoveryChannelDataIssue 区分“合法零命中”与“通道所需因子不可用”。只有后者
// 才把运行标为 partial；安静市场没有符合阈值的股票是完整的空结果。
func discoveryChannelDataIssue(t *FactorTable, spec discoveryChannelSpec) string {
	if t == nil || t.Len() == 0 {
		return "因子宽表为空"
	}
	for _, key := range spec.required {
		col := t.Col(key)
		if col == nil {
			return "缺少因子 " + key
		}
		available := false
		for i := range t.Symbols {
			if t.Fresh(i) {
				if _, ok := finiteFactor(t, key, i); ok {
					available = true
					break
				}
			}
		}
		if !available {
			return "因子无可用值 " + key
		}
	}
	return ""
}

func clampDiscoveryScore(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return round2(v)
}
func minFloat(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
func factorBool(t *FactorTable, key string, i int) bool {
	v, ok := finiteFactor(t, key, i)
	return ok && v == 1
}

// BuildDiscoverySignals 对宽表只做一次行遍历，然后各通道独立排序取 Top-N。
func BuildDiscoverySignals(t *FactorTable, topN int) map[string][]DiscoverySignal {
	if topN <= 0 {
		topN = discoveryTopN
	}
	specs := discoverySpecs()
	out := make(map[string][]DiscoverySignal, len(specs))
	for _, spec := range specs {
		out[spec.name] = nil
	}
	if t == nil {
		return out
	}
	st := t.Col("is_st")
	for i, symbol := range t.Symbols {
		if symbol == "" || !t.Fresh(i) || (st != nil && finiteOrZero(st[i]) == 1) {
			continue
		}
		for _, spec := range specs {
			if !spec.match(t, i) {
				continue
			}
			factors := map[string]float64{}
			for _, key := range []string{"close", "chg_pct", "chg_5d", "chg_20d", "ma20", "ma60", "vol_boost", "vol_5v20", "pos_60", "volatility_20", "div_yield", "atr_pct", "amount_yi", "turnover_rate"} {
				if v, ok := finiteFactor(t, key, i); ok {
					factors[key] = round2(v)
				}
			}
			status := DiscoveryItemReady
			reason := ""
			if t.LagOpenDays != 0 || t.FreshCoverage < 0.98 {
				status, reason = DiscoveryItemPartial, "全市场因子覆盖不完整或落后应有交易日"
			}
			out[spec.name] = append(out[spec.name], DiscoverySignal{Symbol: symbol, Name: t.Names[i], Market: "cn", Channel: spec.name, Score: spec.score(t, i), SourceFacts: spec.facts(t, i), FactorSummary: factors, DataStatus: status, PartialReason: reason})
		}
	}
	for channel, rows := range out {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].Score != rows[j].Score {
				return rows[i].Score > rows[j].Score
			}
			return rows[i].Symbol < rows[j].Symbol
		})
		if len(rows) > topN {
			rows = rows[:topN]
		}
		for i := range rows {
			rows[i].Score = round2(rows[i].Score)
		}
		out[channel] = rows
	}
	return out
}

func finiteOrZero(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return v
}

func discoveryScannedCount(t *FactorTable) int {
	if t == nil {
		return 0
	}
	st := t.Col("is_st")
	n := 0
	for i := range t.Symbols {
		if !t.Fresh(i) || (st != nil && finiteOrZero(st[i]) == 1) {
			continue
		}
		n++
	}
	return n
}

func discoveryFailedCount(t *FactorTable) int {
	if t == nil {
		return 0
	}
	st := t.Col("is_st")
	n := 0
	for i := range t.Symbols {
		if st != nil && finiteOrZero(st[i]) == 1 {
			continue // ST 是明确排除的宇宙成员，不是扫描失败。
		}
		if !t.Fresh(i) {
			n++
		}
	}
	return n
}

func discoveryDates(tradeDate string) []string {
	dates := recentOpenDates("cn", tradeDate, 6)
	if len(dates) == 0 || dates[len(dates)-1] != tradeDate {
		dates = append(dates, tradeDate)
	}
	if len(dates) > 6 {
		dates = dates[len(dates)-6:]
	}
	return dates
}

type discoveryHistoryState struct {
	FirstDate       string
	PrevDate        string
	PrevConsecutive int
	PrevRank        int
	PrevScore       float64
}

// loadDiscoveryHistory 一次读取本次信号涉及的历史行，避免按候选逐条查询。
// 连续天数沿用上一交易日的已固化值，因此不会被近 5 日窗口截断。
func loadDiscoveryHistory(tradeDate, parameterHash string, signals map[string][]DiscoverySignal) map[string]discoveryHistoryState {
	out := map[string]discoveryHistoryState{}
	if common.DB == nil {
		return out
	}
	keys := make([]string, 0)
	seen := map[string]bool{}
	symbolSet := map[string]bool{}
	for channel, rows := range signals {
		for _, row := range rows {
			key := channel + "\x00" + row.Symbol
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
			symbolSet[row.Symbol] = true
		}
	}
	if len(keys) == 0 {
		return out
	}
	channels := make([]string, 0, len(signals))
	for channel := range signals {
		channels = append(channels, channel)
	}
	symbols := make([]string, 0, len(symbolSet))
	for symbol := range symbolSet {
		symbols = append(symbols, symbol)
	}
	// 当前固定通道很少，按通道/代码成对读取，再在内存中筛选，避免 SQL 方言对复合 IN 的依赖。
	// 只需要每个 (channel,symbol) 的**最新一行**：FirstSeenDate 在写入时逐日传承，
	// 无须全史求最早日期。查询按 90 个自然日封窗保持有界（行数随运行天数线性膨胀，
	// 全史扫描是增长性隐患）；超过窗口未再入选的股重新起算首见日，属声明过的取舍。
	windowStart := tradeDate
	if t, err := time.ParseInLocation("2006-01-02", tradeDate, time.Local); err == nil {
		windowStart = t.AddDate(0, 0, -90).Format("2006-01-02")
	}
	var rows []model.CandidateDiscoveryItem
	query := common.DB.Joins("JOIN candidate_discovery_runs AS history_run ON history_run.id = candidate_discovery_items.run_id").Where("candidate_discovery_items.market = ? AND candidate_discovery_items.discovery_version = ? AND candidate_discovery_items.trade_date < ? AND candidate_discovery_items.trade_date >= ? AND candidate_discovery_items.channel IN ? AND candidate_discovery_items.symbol IN ?", "cn", DiscoveryVersion, tradeDate, windowStart, channels, symbols)
	if parameterHash != "" {
		query = query.Where("history_run.factor_version = ? AND history_run.parameter_hash = ?", factorSnapshotVersion, parameterHash)
	}
	if err := query.Order("candidate_discovery_items.trade_date DESC, candidate_discovery_items.id DESC").Find(&rows).Error; err != nil {
		return out
	}
	latest := map[string]bool{}
	for _, row := range rows {
		key := row.Channel + "\x00" + row.Symbol
		if !seen[key] || latest[key] {
			continue
		}
		latest[key] = true
		first := row.FirstSeenDate
		if first == "" {
			first = row.TradeDate
		}
		out[key] = discoveryHistoryState{FirstDate: first, PrevDate: row.TradeDate, PrevConsecutive: row.ConsecutiveDays, PrevRank: row.Rank, PrevScore: row.Score}
	}
	return out
}

// ExecuteDailyDiscovery 在单个全局互斥段内创建/重试同日运行。expectedResultID 是
// JobRun 已绑定的 CandidateDiscoveryRun.ID，只用于防止任务快照串到别的结果事实。
func ExecuteDailyDiscovery(ctx context.Context, expectedResultID int64, market, tradeDate, parameterHash string) (*model.CandidateDiscoveryRun, error) {
	discoveryRunMu.Lock()
	defer discoveryRunMu.Unlock()
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if market == "" {
		market = "cn"
	}
	t := CurrentFactorTable()
	if t == nil || (tradeDate != "" && t.TradeDate < tradeDate) {
		var err error
		t, err = ensureFactorTable(ctx)
		if err != nil {
			return nil, err
		}
	}
	if tradeDate == "" {
		tradeDate = t.TradeDate
	}
	if tradeDate == "" || t.TradeDate != tradeDate {
		// 当前宽表是单日内存快照，不能拿较新的表冒充延迟作业请求的旧交易日。
		// 旧日若已错过，只能明确失败并由最新快照的启动补跑接管。
		return nil, fmt.Errorf("因子宽表交易日不匹配: 当前 %s，目标 %s", t.TradeDate, tradeDate)
	}
	if parameterHash == "" {
		parameterHash = discoveryParameterHash()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	now := time.Now()
	var run model.CandidateDiscoveryRun
	created := false
	err := common.DB.Where("market = ? AND trade_date = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ?", market, tradeDate, DiscoveryVersion, factorSnapshotVersion, parameterHash).First(&run).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		run = model.CandidateDiscoveryRun{OwnerType: model.JobOwnerSystem, Market: market, TradeDate: tradeDate, AsOf: now, DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion, ParameterHash: parameterHash, Status: DiscoveryRunStatusProc, StartedAt: now}
		if err := common.DB.Create(&run).Error; err != nil {
			return nil, err
		}
		created = true
	} else if err != nil {
		return nil, err
	}
	// 结果引用错配必须在复用或重置事实之前拒绝：错配时旧运行的 items 不能先被清掉。
	if expectedResultID > 0 && run.ID != expectedResultID {
		return nil, fmt.Errorf("发现作业结果引用错配: expected=%d actual=%d", expectedResultID, run.ID)
	}
	if !created && run.Status == DiscoveryRunStatusOK && run.FinishedAt != nil {
		return &run, nil
	}
	if !created {
		run.Status, run.Error, run.PartialReason, run.AsOf, run.StartedAt, run.FinishedAt = DiscoveryRunStatusProc, "", "", now, now, nil
		if err := common.DB.Model(&run).Updates(map[string]any{"status": run.Status, "error": "", "partial_reason": "", "as_of": now, "started_at": now, "finished_at": nil}).Error; err != nil {
			return nil, err
		}
		if err := common.DB.Where("run_id = ?", run.ID).Delete(&model.CandidateDiscoveryItem{}).Error; err != nil {
			return nil, err
		}
	}
	signals := BuildDiscoverySignals(t, discoveryTopN)
	history := loadDiscoveryHistory(tradeDate, parameterHash, signals)
	items := make([]model.CandidateDiscoveryItem, 0, len(discoveryChannels)*discoveryTopN)
	eligible := 0
	channelIssues := make([]string, 0, len(discoveryChannels))
	for _, spec := range discoverySpecs() {
		channel := spec.name
		rows := signals[channel]
		if len(rows) > 0 {
			eligible += len(rows)
		}
		if issue := discoveryChannelDataIssue(t, spec); issue != "" {
			channelIssues = append(channelIssues, channel+":"+issue)
		}
		for rank, signal := range rows {
			state := history[channel+"\x00"+signal.Symbol]
			first := state.FirstDate
			if first == "" {
				first = tradeDate
			}
			consecutive := 1
			if state.PrevDate != "" {
				openDates := discoveryDates(tradeDate)
				if len(openDates) >= 2 && state.PrevDate == openDates[len(openDates)-2] {
					consecutive = state.PrevConsecutive + 1
				}
			}
			prevRank, prevScore := state.PrevRank, state.PrevScore
			rankChange, scoreChange := 0, 0.0
			if state.PrevDate != "" {
				// 首次入选没有“变化”可言：PrevRank/PrevScore 零值会伪造出
				// “排名变化 -N / 分数变化 +score”，只有真实历史才计算变化量。
				rankChange = prevRank - (rank + 1)
				scoreChange = round2(signal.Score - prevScore)
			}
			facts, _ := json.Marshal(signal.SourceFacts)
			factors, _ := json.Marshal(signal.FactorSummary)
			items = append(items, model.CandidateDiscoveryItem{RunID: run.ID, TradeDate: tradeDate, Market: signal.Market, DiscoveryVersion: DiscoveryVersion, Channel: channel, Symbol: signal.Symbol, Name: signal.Name, Rank: rank + 1, Score: signal.Score, FirstSeenDate: first, ConsecutiveDays: consecutive, RankChange: rankChange, ScoreChange: scoreChange, SourceFacts: string(facts), FactorSummary: string(factors), DataStatus: signal.DataStatus, PartialReason: signal.PartialReason, AsOf: now})
		}
	}
	scanned := discoveryScannedCount(t)
	failed := discoveryFailedCount(t)
	status := DiscoveryRunStatusOK
	reason := ""
	if t.Len() == 0 {
		status, reason = DiscoveryRunStatusFail, "全市场因子宽表为空"
	} else if t.LagOpenDays != 0 || t.FreshCoverage < 0.98 {
		status, reason = DiscoveryRunStatusPart, fmt.Sprintf("因子覆盖 %.1f%%，数据截至 %s，应有 %s", t.FreshCoverage*100, t.TradeDate, t.ExpectedDate)
	} else if len(channelIssues) > 0 {
		status, reason = DiscoveryRunStatusPart, "通道数据不完整："+strings.Join(channelIssues, ",")
	}
	finished := time.Now()
	if err := ctx.Err(); err != nil {
		common.DB.Model(&model.CandidateDiscoveryRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": DiscoveryRunStatusFail, "error": truncate(err.Error(), 512), "finished_at": finished})
		return nil, err
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if len(items) > 0 {
			if err := tx.CreateInBatches(items, 500).Error; err != nil {
				return err
			}
		}
		sourceVersions, _ := json.Marshal(map[string]any{"factor_snapshot": factorSnapshotVersion, "factor_trade_date": t.TradeDate, "discovery": DiscoveryVersion, "channels": discoveryChannels})
		return tx.Model(&model.CandidateDiscoveryRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": status, "universe_count": t.Len(), "scanned_count": scanned, "eligible_count": eligible, "success_count": len(items), "failed_count": failed, "partial_reason": reason, "error": "", "factor_version": factorSnapshotVersion, "source_versions": string(sourceVersions), "finished_at": finished, "updated_at": finished}).Error
	})
	if err != nil {
		common.DB.Model(&model.CandidateDiscoveryRun{}).Where("id = ?", run.ID).Updates(map[string]any{"status": DiscoveryRunStatusFail, "error": truncate(err.Error(), 512), "finished_at": finished})
		return nil, err
	}
	common.DB.First(&run, run.ID)
	if run.Status == DiscoveryRunStatusFail {
		common.SysWarn("全市场候选发现失败 market=%s trade_date=%s run_id=%d reason=%s", market, tradeDate, run.ID, run.PartialReason)
		return &run, errors.New(run.PartialReason)
	}
	common.SysLog("全市场候选发现完成 market=%s trade_date=%s run_id=%d status=%s scanned=%d items=%d failed=%d", market, tradeDate, run.ID, run.Status, run.ScannedCount, run.SuccessCount, run.FailedCount)
	return &run, nil
}

func registerDiscoveryBinding() durableJobBinding {
	return durableJobBinding{resultType: JobResultDiscovery, resultCommittedByHandler: true,
		create: func(tx *gorm.DB, run *model.JobRun, raw json.RawMessage) (int64, error) {
			var req discoveryJobRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return 0, err
			}
			if req.Market == "" {
				req.Market = "cn"
			}
			if req.Version == 0 {
				req.Version = 1
			}
			if req.ParameterHash == "" {
				// 旧队列快照可能缺参数 hash；执行侧会补当前默认，这里同步补齐，
				// 否则 binding 以空 hash 建行、执行侧按默认 hash 查询会身份错开。
				req.ParameterHash = discoveryParameterHash()
			}
			var row model.CandidateDiscoveryRun
			err := tx.Where("market = ? AND trade_date = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ?", req.Market, req.TradeDate, DiscoveryVersion, factorSnapshotVersion, req.ParameterHash).First(&row).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				row = model.CandidateDiscoveryRun{OwnerType: model.JobOwnerSystem, JobRunID: &run.ID, Market: req.Market, TradeDate: req.TradeDate, AsOf: time.Now(), DiscoveryVersion: DiscoveryVersion, FactorVersion: factorSnapshotVersion, ParameterHash: req.ParameterHash, Status: DiscoveryRunStatusProc, StartedAt: time.Now()}
				if err := tx.Create(&row).Error; err != nil {
					return 0, err
				}
			} else if err != nil {
				return 0, err
			} else if row.Status != DiscoveryRunStatusOK {
				if err := tx.Model(&row).Updates(map[string]any{"status": DiscoveryRunStatusProc, "job_run_id": run.ID, "error": "", "partial_reason": "", "finished_at": nil}).Error; err != nil {
					return 0, err
				}
			}
			if row.JobRunID == nil || row.Status != DiscoveryRunStatusOK {
				if err := tx.Model(&row).Updates(map[string]any{"job_run_id": run.ID}).Error; err != nil {
					return 0, err
				}
			}
			return row.ID, nil
		},
		persistSuccess: func(tx *gorm.DB, run *model.JobRun, result DurableJobResult, _ []byte, _ time.Time) error {
			if run.ResultID == nil {
				return errors.New("发现作业缺少结果引用")
			}
			var row model.CandidateDiscoveryRun
			if err := tx.First(&row, *run.ResultID).Error; err != nil {
				return err
			}
			if row.Status != DiscoveryRunStatusOK && row.Status != DiscoveryRunStatusPart {
				return errors.New("发现结果尚未落库")
			}
			_ = result
			return nil
		},
		finishFailure: func(tx *gorm.DB, run *model.JobRun, _ string, _ string, message string, now time.Time) error {
			if run.ResultID == nil {
				return errors.New("发现作业缺少结果引用")
			}
			return tx.Model(&model.CandidateDiscoveryRun{}).Where("id = ?", *run.ResultID).Updates(map[string]any{"status": DiscoveryRunStatusFail, "error": truncate(message, 512), "finished_at": now}).Error
		},
	}
}

// RegisterCandidateDiscoveryJobHandler 注册 system/global 日更发现作业。
func RegisterCandidateDiscoveryJobHandler() {
	binding := registerDiscoveryBinding()
	registerDurableBusinessJobHandler(JobKindDailyDiscovery, discoveryTimeout, binding,
		func(ctx context.Context, _ int64, _ bool, raw json.RawMessage) (DurableJobResult, error) {
			var req discoveryJobRequest
			if err := json.Unmarshal(raw, &req); err != nil {
				return DurableJobResult{}, err
			}
			runID, ok := currentJobResultID(ctx)
			if !ok {
				return DurableJobResult{}, errors.New("发现作业缺少结果定位")
			}
			row, err := ExecuteDailyDiscovery(ctx, runID, req.Market, req.TradeDate, req.ParameterHash)
			if err != nil {
				return DurableJobResult{}, err
			}
			status := model.JobStatusSuccess
			if row.Status == DiscoveryRunStatusPart {
				status = model.JobStatusDegraded
			}
			// 只有当前交易日发现事实已经持久化，才尝试审计严格相邻的上一有效交易日。
			ScheduleCandidateAudit(row.TradeDate, "discovery")
			return DurableJobResult{Status: status, Total: row.UniverseCount, Succeeded: row.SuccessCount, Failed: row.FailedCount}, nil
		})
}

// ScheduleDailyDiscovery 在因子宽表成功发布后调用；系统 owner 的在途作业天然全局去重。
func ScheduleDailyDiscovery(tradeDate, trigger string) {
	if common.DB == nil || tradeDate == "" {
		return
	}
	req := discoveryJobRequest{Version: 1, Market: "cn", TradeDate: tradeDate, TriggerSource: trigger, ParameterHash: discoveryParameterHash()}
	var existing model.CandidateDiscoveryRun
	if err := common.DB.Where("market = ? AND trade_date = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ?", req.Market, req.TradeDate, DiscoveryVersion, factorSnapshotVersion, req.ParameterHash).First(&existing).Error; err == nil && existing.Status == DiscoveryRunStatusOK && existing.FinishedAt != nil {
		return
	}
	// 同 kind 在途去重不能丢掉另一个交易日：队列繁忙、已有运行或被合并到不同
	// 身份的在途作业时，都记一次有界延迟补交；若同日任务已经成功，重试入口会
	// 在查库后直接返回。
	scheduleRetry := func() {
		key := tradeDate + ":" + req.ParameterHash
		discoveryScheduleMu.Lock()
		if !discoverySchedulePending[key] {
			discoverySchedulePending[key] = true
			time.AfterFunc(systemScheduleRetryDelay, func() {
				discoveryScheduleMu.Lock()
				delete(discoverySchedulePending, key)
				discoveryScheduleMu.Unlock()
				ScheduleDailyDiscovery(tradeDate, trigger)
			})
		}
		discoveryScheduleMu.Unlock()
	}
	run, err := defaultJobRuntime.startSystemWithBinding(nil, JobKindDailyDiscovery, req, nil)
	if err != nil {
		if errors.Is(err, ErrJobQueueBusy) || errors.Is(err, ErrJobAlreadyRunning) {
			scheduleRetry()
			return
		}
		common.SysWarn("提交全市场发现作业失败: %v", err)
		return
	}
	// system 同 kind 全局互斥：命中在途作业时 runtime 返回的是**原 run**（可能是
	// 另一交易日/参数的请求），静默把它当“已受理”会让本交易日的发现永久缺失、
	// 相邻日审计也不再触发。比对快照身份，不一致必须延迟补交。
	var accepted discoveryJobRequest
	if run == nil || json.Unmarshal(snapshotJSONRequest([]byte(run.RequestSnapshot)), &accepted) != nil ||
		accepted.TradeDate != req.TradeDate || accepted.ParameterHash != req.ParameterHash {
		scheduleRetry()
	}
}

// ScheduleDiscoveryStartupBackfill 在启动恢复完成后补跑最近一份已发布因子快照。
// 已成功的同版本运行不重扫；failed/partial 会复用同一运行事实安全重试。
func discoveryNeedsBackfill(tradeDate string) bool {
	if common.DB == nil || tradeDate == "" {
		return false
	}
	var run model.CandidateDiscoveryRun
	err := common.DB.Where("market = ? AND trade_date = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ?", "cn", tradeDate, DiscoveryVersion, factorSnapshotVersion, discoveryParameterHash()).First(&run).Error
	return err != nil || run.Status != DiscoveryRunStatusOK || run.FinishedAt == nil
}

func ScheduleDiscoveryStartupBackfill() {
	if common.DB == nil {
		return
	}
	go func() {
		var tradeDate string
		if err := common.DB.Model(&model.FactorSnapshotDaily{}).Select("MAX(trade_date)").Scan(&tradeDate).Error; err != nil || tradeDate == "" {
			return
		}
		if !discoveryNeedsBackfill(tradeDate) {
			return
		}
		common.SysLog("启动补跑：因子快照 %s 尚无完整全市场发现，提交 system/global 作业", tradeDate)
		ScheduleDailyDiscovery(tradeDate, "startup")
	}()
}

// LatestDiscoveryStatus 返回全局最新发现运行及其短名单，供管理/推荐状态页使用。
func LatestDiscoveryStatus(limit int) (*model.CandidateDiscoveryRun, []model.CandidateDiscoveryItem, error) {
	if common.DB == nil {
		return nil, nil, errors.New("数据库不可用")
	}
	if limit <= 0 || limit > 400 {
		limit = 100
	}
	var run model.CandidateDiscoveryRun
	if err := common.DB.Where("owner_type = ? AND discovery_version = ? AND factor_version = ? AND parameter_hash = ?", model.JobOwnerSystem, DiscoveryVersion, factorSnapshotVersion, discoveryParameterHash()).Order("trade_date DESC, id DESC").First(&run).Error; err != nil {
		return nil, nil, err
	}
	var items []model.CandidateDiscoveryItem
	// rank 是 MySQL 8.0.2+ 保留字，须 OrderByColumn 加引号（同 mood.go 先例）。
	// limit 按**每通道**生效：单一全局 limit 会在满池时只返回字母序靠前的通道，
	// 把其余通道显示成空池、与 run.success_count 矛盾。
	perChannel := limit
	var rows []model.CandidateDiscoveryItem
	if err := common.DB.Where("run_id = ?", run.ID).Order("channel").
		Order(clause.OrderByColumn{Column: clause.Column{Name: "rank"}}).
		Order("symbol").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	counts := map[string]int{}
	for _, row := range rows {
		if counts[row.Channel] >= perChannel {
			continue
		}
		counts[row.Channel]++
		items = append(items, row)
	}
	return &run, items, nil
}

func DiscoveryChannelKeys() []string {
	return append([]string(nil), discoveryChannels...)
}
