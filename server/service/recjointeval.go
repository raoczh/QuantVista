package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"gorm.io/gorm"

	"quantvista/common"
	"quantvista/model"
)

// P2-5 组合/回测联合评估（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3/§9）：
// 把推荐标签事实（reclabel，l2/next_open 统一执行模拟）升维到「组合视角」——收益、
// Alpha、最大回撤、换手、成本拖累、覆盖率与校准（Brier/ECE）同屏；并落地 §9.1 的
// 时间切分纪律：早段=开发集（dev，调参/观察随便看），末段=锁定测试集（locked test，
// 默认不出指标、显式请求才计算且每次读取登记审计——「发布前只读一次」以可见审计落地）。
//
// 三条纪律（改本文件前先读）：
//   - 零门控纯测量：只读标签/批次表，零 LLM 调用、不改写任何线上行为、不回写业务表
//     （唯一写入=锁定段读取审计的 options 行，那是对「读」这个动作的登记，非业务改写）。
//   - 样本口径与校准报表同源：loadRecCalibSamples 单一权威（l2/next_open/rec_id>0，
//     matured 中 forced/orphan/degraded 单列剔除）——两报表口径漂移会各说各话。
//   - 锁定段隔离：版本对照（prompt_version/provider 分组，实验飞轮晋级前后的收益对照
//     落点）只吃 dev 段样本——不给「以对照之名反复消费锁定段」留后门；include_locked
//     恒重算不走缓存（每次读取都必须被审计）。

const (
	// jointDevFrac dev 段唯一信号日占比（其余为 locked test；§9.1 时间切分不随机打散）。
	jointDevFrac = 0.7
	// jointMinSplitDays 唯一信号日不足此数不切分（全部归 dev，locked 如实缺席——
	// 硬切会让锁定段只有一两天，评审无意义还烧掉「只读一次」额度）。
	jointMinSplitDays = 10
	// jointLockedReadsKey 锁定段读取审计（options 表 KV）：每次 include_locked=1 计算
	// 成功后 +1 并记录时刻。审计只增不减；重置=手工删该行（有意不提供接口）。
	jointLockedReadsKey = "jointeval_locked_reads"
	// jointLockedLogMax 审计日志保留的最近读取时刻条数。
	jointLockedLogMax = 10
)

// JointEvalSegment 单段（dev / locked）统计。收益类指标限定 buy（与校准/归因「买入
// 建议质量」口径一致）；watch 样本计入 Sample 供对照，不进收益统计。
type JointEvalSegment struct {
	Segment    string `json:"segment"` // dev / locked
	DateStart  string `json:"date_start"`
	DateEnd    string `json:"date_end"`
	SignalDays int    `json:"signal_days"` // 段内唯一信号日数
	Sample     int    `json:"sample"`      // 段内可用成熟样本（buy+watch）
	BuySample  int    `json:"buy_sample"`

	// 收益（buy）。
	WinRatePct    float64 `json:"win_rate_pct"`
	AvgNetPct     float64 `json:"avg_net_pct"`
	MedianNetPct  float64 `json:"median_net_pct"`
	P10NetPct     float64 `json:"p10_net_pct"`
	SevereLossPct float64 `json:"severe_loss_pct"` // net < -5% 比例
	AvgGrossPct   float64 `json:"avg_gross_pct"`
	// CostDragPct 成本拖累=gross−net 均值（佣金万2.5最低5元+卖出印花税万5；执行模拟
	// 无滑点项——按次日开盘价成交的理想化假设，如实声明不硬造滑点数字）。
	CostDragPct float64 `json:"cost_drag_pct"`
	AvgAlphaPct float64 `json:"avg_alpha_pct"`
	AlphaSample int     `json:"alpha_sample"`

	// 组合视角（buy）。
	NavReturnPct   float64 `json:"nav_return_pct"`   // 信号日串联净值总收益
	MaxDrawdownPct float64 `json:"max_drawdown_pct"` // 串联净值最大回撤（正数=幅度）
	AvgMaePct      float64 `json:"avg_mae_pct"`      // 标签级持有期内最大不利波动均值（负）
	WorstMaePct    float64 `json:"worst_mae_pct"`

	// 校准（buy 口头置信度；与校准报表同门槛，样本不足 nil=未评估）。主口径=落库终值。
	CalibSample int      `json:"calib_sample"`
	Brier       *float64 `json:"brier,omitempty"`
	ECE         *float64 `json:"ece,omitempty"`
	// RawCalib 原始口径分列（第五十六批②，与校准报表 calibRawSummary 同实现同门槛）：
	// 复核改写前的模型预测快照单独测校准；旧记录无快照如实计 missing。
	RawCalib *CalibRawSummary `json:"raw_calib,omitempty"`
}

// JointLockedPreview 锁定段默认视图：只给范围与计数，不给任何收益/校准指标。
type JointLockedPreview struct {
	DateStart  string `json:"date_start"`
	DateEnd    string `json:"date_end"`
	SignalDays int    `json:"signal_days"`
	Sample     int    `json:"sample"` // 段内可用成熟样本数（buy+watch）
}

// JointTurnover 换手统计：同一用户同一类型的相邻批次 buy 名单对比。
type JointTurnover struct {
	Pairs         int     `json:"pairs"`           // 参与计算的相邻批次对数
	AvgNewPct     float64 `json:"avg_new_pct"`     // 本批新进标的占比均值（|cur−prev|/|cur|）
	AvgOverlapPct float64 `json:"avg_overlap_pct"` // 相邻批次重合率均值（|∩|/|∪|）
}

// JointEvalSection 单 type×代表持有期的联合评估。
type JointEvalSection struct {
	Type        string `json:"type"`
	HorizonDays int    `json:"horizon_days"`

	Coverage      CalibCoverage       `json:"coverage"`
	Dev           *JointEvalSegment   `json:"dev,omitempty"`
	Locked        *JointEvalSegment   `json:"locked,omitempty"` // include_locked=1 才计算
	LockedPreview *JointLockedPreview `json:"locked_preview,omitempty"`

	Turnover JointTurnover `json:"turnover"`
	// Slices 版本/来源对照（prompt_version / provider·model；实验飞轮晋级前后收益对照
	// 的落点）——只吃 dev 段样本（锁定段隔离纪律）。
	Slices []CalibSliceGroup `json:"slices,omitempty"`
	Notes  []string          `json:"notes"`
}

// JointLockedAudit 锁定段读取审计（options 表持久化，跨重启累计）。
type JointLockedAudit struct {
	Count  int      `json:"count"`
	LastAt string   `json:"last_at"`
	Log    []string `json:"log,omitempty"` // 最近 jointLockedLogMax 次读取时刻
}

// JointEvalReport 联合评估总报表。
type JointEvalReport struct {
	GeneratedAt   string              `json:"generated_at"`
	LabelVersion  string              `json:"label_version"`
	IncludeLocked bool                `json:"include_locked"`
	LockedAudit   *JointLockedAudit   `json:"locked_audit,omitempty"`
	Sections      []*JointEvalSection `json:"sections"`
	ElapsedMs     int64               `json:"elapsed_ms"`
	Notes         []string            `json:"notes"`
}

// ---------- 纯计算内核 ----------

// jointSplitDays 时间切分（§9.1）：唯一信号日升序，前 jointDevFrac 归 dev、其余归
// locked；不足 jointMinSplitDays 天不切（全 dev）。切分点只依赖日期集合，同一数据集
// 上稳定可复现。
func jointSplitDays(days []string) (dev, locked []string) {
	sorted := append([]string(nil), days...)
	sort.Strings(sorted)
	if len(sorted) < jointMinSplitDays {
		return sorted, nil
	}
	cut := int(float64(len(sorted)) * jointDevFrac)
	if cut < 1 {
		cut = 1
	}
	if cut >= len(sorted) {
		cut = len(sorted) - 1
	}
	return sorted[:cut], sorted[cut:]
}

// jointNavStats 信号日串联净值：每个信号日的组合收益=当日 buy 标签平均净收益，
// nav=Π(1+r_i/100)。返回总收益 % 与最大回撤 %（正数）。口径声明：等权、按持有期末
// 结算串联，忽略持有期重叠——是「逐批全仓换仓」的近似，非逐日盯市净值。
func jointNavStats(perDayAvgNet []float64) (navRetPct, maxDDPct float64) {
	nav, peak := 1.0, 1.0
	for _, r := range perDayAvgNet {
		nav *= 1 + r/100
		if nav > peak {
			peak = nav
		}
		if peak > 0 {
			if dd := (peak - nav) / peak * 100; dd > maxDDPct {
				maxDDPct = dd
			}
		}
	}
	return round2((nav - 1) * 100), round2(maxDDPct)
}

// jointBatchPicks 换手计算的输入单元：一个批次的 buy 名单。
type jointBatchPicks struct {
	UserID     int64
	BatchID    int64
	SignalDate string
	Buys       map[string]bool // symbol 集合
}

// jointTurnoverStats 相邻批次换手：按 user 分组、批次按（信号日, 批次 id）升序，
// 相邻两批 buy 名单非空才成一对。跨用户批次序列没有换手语义，不混排。
func jointTurnoverStats(batches []jointBatchPicks) JointTurnover {
	byUser := map[int64][]jointBatchPicks{}
	for _, b := range batches {
		byUser[b.UserID] = append(byUser[b.UserID], b)
	}
	var newSum, overlapSum float64
	pairs := 0
	for _, list := range byUser {
		sort.Slice(list, func(i, j int) bool {
			if list[i].SignalDate != list[j].SignalDate {
				return list[i].SignalDate < list[j].SignalDate
			}
			return list[i].BatchID < list[j].BatchID
		})
		for i := 1; i < len(list); i++ {
			prev, cur := list[i-1].Buys, list[i].Buys
			if len(prev) == 0 || len(cur) == 0 {
				continue
			}
			inter := 0
			for s := range cur {
				if prev[s] {
					inter++
				}
			}
			union := len(prev) + len(cur) - inter
			newSum += float64(len(cur)-inter) / float64(len(cur)) * 100
			overlapSum += float64(inter) / float64(union) * 100
			pairs++
		}
	}
	t := JointTurnover{Pairs: pairs}
	if pairs > 0 {
		t.AvgNewPct = round2(newSum / float64(pairs))
		t.AvgOverlapPct = round2(overlapSum / float64(pairs))
	}
	return t
}

// jointSegStats 单段统计：收益/组合视角/校准（输入=段内可用成熟样本）。
func jointSegStats(segment string, days []string, list []calibSample) *JointEvalSegment {
	seg := &JointEvalSegment{Segment: segment, SignalDays: len(days), Sample: len(list)}
	if len(days) > 0 {
		seg.DateStart, seg.DateEnd = days[0], days[len(days)-1]
	}

	var nets []float64
	var sumNet, sumGross, sumAlpha, sumMae float64
	var hits, severe int
	worstMae := 0.0
	var cobs []calibObs
	perDay := map[string][]float64{}
	for _, s := range list {
		if s.label.Action != model.RecActionBuy {
			continue
		}
		seg.BuySample++
		l := s.label
		nets = append(nets, l.NetReturnPct)
		sumNet += l.NetReturnPct
		sumGross += l.GrossReturnPct
		sumMae += l.MaePct
		if l.MaePct < worstMae {
			worstMae = l.MaePct
		}
		if l.NetReturnPct > 0 {
			hits++
		}
		if l.NetReturnPct < -5 {
			severe++
		}
		if l.HasBench {
			seg.AlphaSample++
			sumAlpha += l.AlphaPct
		}
		cobs = append(cobs, calibObs{Conf: float64(s.meta.conf), Hit: l.NetReturnPct > 0, Net: l.NetReturnPct})
		perDay[l.SignalDate] = append(perDay[l.SignalDate], l.NetReturnPct)
	}
	if seg.BuySample > 0 {
		n := float64(seg.BuySample)
		sort.Float64s(nets)
		seg.WinRatePct = round2(float64(hits) / n * 100)
		seg.AvgNetPct = round2(sumNet / n)
		seg.MedianNetPct = round2(median(nets))
		seg.P10NetPct = round2(percentileSorted(nets, 0.10))
		seg.SevereLossPct = round2(float64(severe) / n * 100)
		seg.AvgGrossPct = round2(sumGross / n)
		seg.CostDragPct = round2((sumGross - sumNet) / n)
		seg.AvgMaePct = round2(sumMae / n)
		seg.WorstMaePct = round2(worstMae)
		if seg.AlphaSample > 0 {
			seg.AvgAlphaPct = round2(sumAlpha / float64(seg.AlphaSample))
		}

		dayKeys := make([]string, 0, len(perDay))
		for d := range perDay {
			dayKeys = append(dayKeys, d)
		}
		sort.Strings(dayKeys)
		perDayAvg := make([]float64, 0, len(dayKeys))
		for _, d := range dayKeys {
			var s float64
			for _, v := range perDay[d] {
				s += v
			}
			perDayAvg = append(perDayAvg, s/float64(len(perDay[d])))
		}
		seg.NavReturnPct, seg.MaxDrawdownPct = jointNavStats(perDayAvg)
	}
	seg.CalibSample = len(cobs)
	if len(cobs) >= calibEvalMinSample {
		seg.Brier = calibBrier(cobs)
		seg.ECE = calibECE(cobs)
	}
	// 原始口径按复核前动作取样；即使最终 buy 为空也保留被 reject 的原始 buy。
	rawCalib := calibRawSummary(list)
	if rawCalib.Sample > 0 || rawCalib.Missing > 0 {
		seg.RawCalib = rawCalib
	}
	return seg
}

// ---------- 锁定段读取审计 ----------

// jointLockedAuditLoad 读取审计记录（无记录返回零值非错误）。
func jointLockedAuditLoad() (*JointLockedAudit, error) {
	var row model.Option
	err := common.DB.Where("`key` = ?", jointLockedReadsKey).First(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &JointLockedAudit{}, nil
		}
		return nil, err
	}
	var a JointLockedAudit
	if json.Unmarshal([]byte(row.Value), &a) != nil {
		// 值损坏按零值重新开始计（审计尽力而为，不阻断报表）。
		return &JointLockedAudit{}, nil
	}
	return &a, nil
}

// jointLockedAuditBump 登记一次锁定段读取（计数+时刻；持久化失败不阻断报表但记日志）。
func jointLockedAuditBump(now time.Time) *JointLockedAudit {
	a, err := jointLockedAuditLoad()
	if err != nil {
		common.SysWarn("联合评估锁定段审计读取失败: %v", err)
		a = &JointLockedAudit{}
	}
	a.Count++
	a.LastAt = now.In(time.Local).Format("2006-01-02 15:04:05")
	a.Log = append(a.Log, a.LastAt)
	if len(a.Log) > jointLockedLogMax {
		a.Log = a.Log[len(a.Log)-jointLockedLogMax:]
	}
	if b, err := json.Marshal(a); err == nil {
		if err := model.UpsertOption(jointLockedReadsKey, string(b)); err != nil {
			common.SysWarn("联合评估锁定段审计写入失败: %v", err)
		}
	}
	return a
}

// ---------- 报表组装 ----------

// buildJointEvalSection 单 type×horizon 的联合评估段。
func buildJointEvalSection(recType string, horizon int, includeLocked bool) (*JointEvalSection, error) {
	sec := &JointEvalSection{Type: recType, HorizonDays: horizon}

	usable, coverage, err := loadRecCalibSamples(recType, horizon)
	if err != nil {
		return nil, err
	}
	sec.Coverage = coverage

	// 唯一信号日（可用成熟样本口径）升序切分。
	daySet := map[string]bool{}
	for _, s := range usable {
		if s.label.SignalDate != "" {
			daySet[s.label.SignalDate] = true
		}
	}
	days := make([]string, 0, len(daySet))
	for d := range daySet {
		days = append(days, d)
	}
	devDays, lockedDays := jointSplitDays(days)
	inDev := map[string]bool{}
	for _, d := range devDays {
		inDev[d] = true
	}
	inLocked := map[string]bool{}
	for _, d := range lockedDays {
		inLocked[d] = true
	}

	var devList, lockedList []calibSample
	for _, s := range usable {
		switch {
		case inDev[s.label.SignalDate]:
			devList = append(devList, s)
		case inLocked[s.label.SignalDate]:
			lockedList = append(lockedList, s)
		}
	}
	sec.Dev = jointSegStats("dev", devDays, devList)
	if len(lockedDays) > 0 {
		sec.LockedPreview = &JointLockedPreview{
			DateStart: lockedDays[0], DateEnd: lockedDays[len(lockedDays)-1],
			SignalDays: len(lockedDays), Sample: len(lockedList),
		}
		if includeLocked {
			sec.Locked = jointSegStats("locked", lockedDays, lockedList)
		}
	}

	// 换手：该 type×horizon 的标签行（含 pending——名单构成不需要成熟）按批次聚合
	// buy 名单。独立轻量查询（只取四列）。**锁定段隔离（审查修复批）**：默认视图只吃
	// 开发段信号日的批次——修复前无条件全量查询让常规页面请求消费了 locked 日期的名单
	// 数据（换手指标受 locked 影响却无审计记录），违反「默认只显示 locked 范围与样本数」；
	// include_locked（已登记审计的显式请求）才纳入其他日期批次。默认路径严格使用
	// signal_date IN devDays，而不是“<= dev 末日”：无成熟 dev 日期时结果必须为空，少日
	// 不切分时也不能让 devDays 之外的 pending 批次混入换手。
	var turnRows []model.RecommendationLabel
	if includeLocked || len(devDays) > 0 {
		turnQ := common.DB.Select("user_id", "batch_id", "signal_date", "symbol", "action").
			Where("horizon_days = ? AND entry_mode = ? AND recommendation_id > 0 AND label_version = ? AND type = ?",
				horizon, model.EntryModeNextOpen, labelVersion, recType)
		if !includeLocked {
			turnQ = turnQ.Where("signal_date IN ?", devDays)
		}
		if err := turnQ.Find(&turnRows).Error; err != nil {
			return nil, err
		}
	}
	batchMap := map[int64]*jointBatchPicks{}
	for _, l := range turnRows {
		if l.Action != model.RecActionBuy {
			continue
		}
		b := batchMap[l.BatchID]
		if b == nil {
			b = &jointBatchPicks{UserID: l.UserID, BatchID: l.BatchID, SignalDate: l.SignalDate, Buys: map[string]bool{}}
			batchMap[l.BatchID] = b
		}
		b.Buys[l.Symbol] = true
	}
	batches := make([]jointBatchPicks, 0, len(batchMap))
	for _, b := range batchMap {
		batches = append(batches, *b)
	}
	sec.Turnover = jointTurnoverStats(batches)

	// 版本/来源对照（实验飞轮落点）：只吃 dev 段 buy 样本——锁定段隔离。
	{
		var buys []calibSample
		batchIDs := make([]int64, 0, 16)
		seenBatch := map[int64]bool{}
		for _, s := range devList {
			if s.label.Action != model.RecActionBuy {
				continue
			}
			buys = append(buys, s)
			if s.label.BatchID > 0 && !seenBatch[s.label.BatchID] {
				seenBatch[s.label.BatchID] = true
				batchIDs = append(batchIDs, s.label.BatchID)
			}
		}
		if len(buys) > 0 {
			bmetas, err := calibBatchMetaByID(batchIDs)
			if err != nil {
				return nil, err
			}
			mkObs := func(keyOf func(calibSample) string) []calibSliceObs {
				out := make([]calibSliceObs, 0, len(buys))
				for _, s := range buys {
					out = append(out, calibSliceObs{
						Key: keyOf(s), Conf: float64(s.meta.conf),
						Hit: s.label.NetReturnPct > 0, Net: s.label.NetReturnPct,
						Alpha: s.label.AlphaPct, HasBench: s.label.HasBench,
					})
				}
				return out
			}
			pvRows := calibSliceRows(mkObs(func(s calibSample) string {
				m, ok := bmetas[s.label.BatchID]
				if !ok {
					return ""
				}
				return m.PromptVersion
			}))
			if len(pvRows) > 0 {
				sec.Slices = append(sec.Slices, CalibSliceGroup{Dim: "prompt_version", Label: "Prompt 版本（晋级对照）", Rows: pvRows})
			}
			pmRows := calibSliceRows(mkObs(func(s calibSample) string {
				m, ok := bmetas[s.label.BatchID]
				return calibProviderModelKey(m, ok)
			}))
			if len(pmRows) > 0 {
				sec.Slices = append(sec.Slices, CalibSliceGroup{Dim: "provider_model", Label: "Provider·Model", Rows: pmRows})
			}
		}
	}

	sec.Notes = append(sec.Notes,
		fmt.Sprintf("切分（§9.1 时间切分不随机打散）：唯一信号日升序前 %.0f%% 归开发段、其余归锁定段；不足 %d 个信号日不切分（锁定段如实缺席）", jointDevFrac*100, jointMinSplitDays),
		"净值口径：按信号日串联各期组合平均净收益（等权、持有期末结算、忽略持有期重叠）——「逐批全仓换仓」的近似而非逐日盯市；标签级最大不利波动（MAE）并列供回撤参照",
		"成本拖累=毛收益−净收益均值（佣金万2.5最低5元+卖出印花税万5）；执行模拟按次日开盘价成交、无滑点项，属理想化假设（如实声明，不硬造滑点数字）",
		"换手=同一用户同类型相邻批次 buy 名单对比（新进占比 |新面孔|/|本批|、重合率 |∩|/|∪|），跨用户不混排；名单构成不需要标签成熟；默认视图只统计开发段日期界内的批次（晚于开发段末日的一律不进——锁定段隔离），读取锁定段时才含全量日期",
		"版本/来源对照只吃开发段样本（锁定段隔离——不给「以对照之名反复消费锁定段」留后门）；prompt_version 对照即 champion/challenger 晋级前后的收益对照落点（P2-2 声明的晋级后评估）",
	)
	return sec, nil
}

// ---------- 报表入口（进程内缓存 + 互斥；include_locked 恒重算不走缓存） ----------

var (
	jointMu       sync.Mutex
	jointInflight bool
	jointCacheMu  sync.RWMutex
	jointCache    *JointEvalReport
)

// CachedJointEvalReport 返回进程内缓存的联合评估报表（可能为 nil；只缓存不含锁定段
// 指标的常规视图——include_locked 请求恒重算，保证每次锁定段读取都被审计）。
func CachedJointEvalReport() *JointEvalReport {
	jointCacheMu.RLock()
	defer jointCacheMu.RUnlock()
	return jointCache
}

// RunJointEval 计算联合评估报表（P2-5）。纯测量：只读标签/批次表，零 LLM 调用、
// 零门控；includeLocked=true 时计算锁定段指标并登记读取审计。
func RunJointEval(includeLocked bool) (*JointEvalReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	jointMu.Lock()
	if jointInflight {
		jointMu.Unlock()
		return nil, errors.New("联合评估正在计算中，请稍候")
	}
	jointInflight = true
	jointMu.Unlock()
	defer func() {
		jointMu.Lock()
		jointInflight = false
		jointMu.Unlock()
	}()

	start := time.Now()
	rep := &JointEvalReport{
		GeneratedAt:   time.Now().In(time.Local).Format("2006-01-02 15:04:05"),
		LabelVersion:  labelVersion,
		IncludeLocked: includeLocked,
	}
	hasLockedData := false
	for _, t := range []string{model.RecTypeShortTerm, model.RecTypeLongTerm} {
		sec, err := buildJointEvalSection(t, calibRepHorizons[t], includeLocked)
		if err != nil {
			return nil, err
		}
		if sec.LockedPreview != nil {
			hasLockedData = true
		}
		rep.Sections = append(rep.Sections, sec)
	}
	if includeLocked && hasLockedData {
		rep.LockedAudit = jointLockedAuditBump(time.Now())
	} else {
		a, err := jointLockedAuditLoad()
		if err == nil && a.Count > 0 {
			rep.LockedAudit = a
		}
	}
	rep.ElapsedMs = time.Since(start).Milliseconds()
	rep.Notes = append(rep.Notes,
		"P2-5 纯测量报表：零门控零 LLM 调用，不改写任何线上行为；样本口径与校准报表同源（l2/next_open/成熟非强平非降级非孤儿）",
		fmt.Sprintf("锁定测试集纪律（§9.1/§9.3）：锁定段默认只显示范围与样本数；显式请求才计算指标且每次读取登记审计（已读 %d 次可见）——调参迭代只看开发段，锁定段留给发布前验收", func() int {
			if rep.LockedAudit != nil {
				return rep.LockedAudit.Count
			}
			return 0
		}()),
		fmt.Sprintf("代表持有期：短线 h=%d / 长线 h=%d（与校准报表、反思记忆、月度走查同口径）", calibRepHorizons[model.RecTypeShortTerm], calibRepHorizons[model.RecTypeLongTerm]),
	)

	// 只缓存常规视图：锁定段指标不落缓存（防止缓存命中绕过审计拿到锁定段数字）。
	if !includeLocked {
		jointCacheMu.Lock()
		jointCache = rep
		jointCacheMu.Unlock()
	}
	return rep, nil
}
