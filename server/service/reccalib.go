package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// P1-7 校准与后验标签（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.2/§9.2）：
// 把「程序合成置信度档位（SysConfidence high/medium/low）」与「模型口头置信度（0-100）」
// 当预测信号，对后验标签算校准——分档命中率、可靠性曲线、Brier/ECE、动作 precision/
// recall、标签 coverage、alpha 与成本前后收益分开统计。
//
// 三条纪律（改本文件前先读）：
//   - §6.3 不采纳边界：「模型口头 confidence 不当真实概率」——本报表恰是测量它有多不
//     校准的证据面（Brier/ECE/可靠性曲线），不是采信；程序合成置信度是有序档位，
//     不硬造概率，只算分档命中率与单调性描述。
//   - 零门控纯测量：只读标签/记录表做统计，不改写任何线上行为、不回写任何业务表。
//   - §8.1 样本门槛（个人低频模块分级）：总样本 < calibMinSample 时 evaluated=false
//     如实报「未评估」，不得把小样本当达标；单桶/单档 < calibMinBucket 前端标注样本不足。
//
// 后验标签口径：推荐侧只吃 entry_mode=next_open、recommendation_id>0、label_version=l2
// 的 matured 样本（统一执行模拟、当前执行语义版本；影子标签与用户执行事实不混入——
// 与归因报表 recattribution.go 同一铁律）；Forced（退市/长停强平）单列剔除；量化降级
// 条目（degraded_source=quant_fallback）的置信度是程序赋值非模型预测，单列剔除。
// 分析侧无落库标签，按 hindsight 同款口径现算：基准根（as_of 或创建日 ≤ 最近交易日）
// 收盘 → +calibAnalysisHorizon 交易日收盘的方向命中（bullish→涨/bearish→跌；neutral
// 无方向不判，单列计数）。

const (
	// calibMinSample 分级参考门槛（§8.1 低频模块分级）：分档/PR 表低于此仅供参考。
	calibMinSample = 30
	// calibEvalMinSample 「已评估」硬门槛（§8.1：推荐/个股分析每种数据状态至少 100 个
	// 样本，30 只允许 compare/screener 等低频模块——审查修复批对齐，evaluated 与
	// Brier/ECE 产出用同一门槛，杜绝「绿 tag 已评估但 Brier 未评估」的两层矛盾）。
	calibEvalMinSample = 100
	// calibMinBucket 单桶/单档最低样本，低于则前端标注「样本不足」（归因报表同值）。
	calibMinBucket = 5
	// calibAnalysisScanMax 分析侧最多回看的记录条数（每条要查两次日线，控制现算成本）。
	calibAnalysisScanMax = 400
	// calibAnalysisHorizon 分析评级方向核验窗口（交易日）——hindsight RatingHit 同款 20 日。
	calibAnalysisHorizon = 20
)

// calibRepHorizons 推荐侧代表持有期：短线 h=10 / 长线 h=20（反思记忆 rf1 与
// walk-forward 月度走查同款口径；其余 horizon 的标签仍在，需要时另行请求归因报表）。
var calibRepHorizons = map[string]int{
	model.RecTypeShortTerm: 10,
	model.RecTypeLongTerm:  20,
}

// CalibBucket 可靠性曲线单桶（口头置信度 0-100 固定 5 桶）。
type CalibBucket struct {
	Label      string  `json:"label"` // "0-20" / "20-40" / …
	Sample     int     `json:"sample"`
	AvgConf    float64 `json:"avg_conf"`     // 桶内平均口头置信度（0-100）
	HitRatePct float64 `json:"hit_rate_pct"` // 命中率 %（推荐=净收益>0；分析=方向命中）
	AvgNetPct  float64 `json:"avg_net_pct"`  // 桶内平均净收益 %（分析侧=平均 20 日收益）
	GapPct     float64 `json:"gap_pct"`      // 命中率 − 平均置信度（百分点；负=过度自信）
}

// CalibTierCell 程序合成置信度单档统计（有序档位，不硬造概率）。
type CalibTierCell struct {
	Tier          string  `json:"tier"` // high / medium / low /（未知）
	Sample        int     `json:"sample"`
	HitRatePct    float64 `json:"hit_rate_pct"`
	MedianNetPct  float64 `json:"median_net_pct"`
	AvgNetPct     float64 `json:"avg_net_pct"`
	AvgGrossPct   float64 `json:"avg_gross_pct"` // 成本前收益（与净收益分开统计）
	AvgAlphaPct   float64 `json:"avg_alpha_pct"`
	AlphaSample   int     `json:"alpha_sample"`
	SevereLossPct float64 `json:"severe_loss_pct"` // net < -5% 比例
}

// CalibCoverage 标签覆盖面（同一批标签的结算状态分布——「覆盖率」的事实基础）。
type CalibCoverage struct {
	Total           int     `json:"total"`   // 该口径下全部标签行
	Matured         int     `json:"matured"` // 其中已成熟（含被剔除的 forced/degraded）
	Pending         int     `json:"pending"`
	Skipped         int     `json:"skipped"`
	NoData          int     `json:"no_data"`
	Forced          int     `json:"forced"`        // 强平剔除（收益不可靠）
	DegradedExcl    int     `json:"degraded_excl"` // 量化降级条目剔除（置信度非模型预测）
	OrphanExcl      int     `json:"orphan_excl"`   // 孤儿标签剔除（推荐条目已删，置信度无从查证——混入会以 0 置信进桶）
	MaturedRatioPct float64 `json:"matured_ratio_pct"`
}

// CalibActionPR 动作 precision/recall（action=buy 当预测正类、结果为正当事实正类）。
type CalibActionPR struct {
	Sample          int      `json:"sample"` // buy+watch 成熟样本
	BuySample       int      `json:"buy_sample"`
	PrecisionNetPct *float64 `json:"precision_net_pct,omitempty"` // P(net>0 | buy)
	RecallNetPct    *float64 `json:"recall_net_pct,omitempty"`    // P(buy | net>0)
	WatchHitPct     *float64 `json:"watch_hit_pct,omitempty"`     // P(net>0 | watch) 对照
	PrecisionAlpha  *float64 `json:"precision_alpha_pct,omitempty"`
	RecallAlpha     *float64 `json:"recall_alpha_pct,omitempty"`
	AlphaSample     int      `json:"alpha_sample"`
}

// RecCalibReport 推荐侧校准报表（单 type×代表持有期）。
type RecCalibReport struct {
	Type        string `json:"type"`
	HorizonDays int    `json:"horizon_days"`
	Evaluated   bool   `json:"evaluated"` // 样本 ≥ calibMinSample 才 true
	Sample      int    `json:"sample"`    // 进入统计的成熟样本（buy+watch，剔 forced/degraded）
	BuySample   int    `json:"buy_sample"`

	Coverage CalibCoverage `json:"coverage"`

	// 口头置信度校准（buy 样本；y=净收益>0）。样本不足时为 nil（「未评估」非 0 分）。
	Brier       *float64      `json:"brier,omitempty"`
	ECE         *float64      `json:"ece,omitempty"`
	Reliability []CalibBucket `json:"reliability,omitempty"`

	// 程序合成置信度分档（buy 样本）。
	SysTiers     []CalibTierCell `json:"sys_tiers,omitempty"`
	TierMonotone string          `json:"tier_monotone,omitempty"` // 单调性描述（不下结论）

	ActionPR CalibActionPR `json:"action_pr"`
	Notes    []string      `json:"notes"`
}

// AnalysisCalibTier 分析侧 SysConfidence 单档（方向命中口径）。
type AnalysisCalibTier struct {
	Tier        string  `json:"tier"`
	Sample      int     `json:"sample"`
	HitRatePct  float64 `json:"hit_rate_pct"`
	AvgRet20Pct float64 `json:"avg_ret20_pct"` // 该档 +20 日平均收益（方向未调整，仅参考）
}

// AnalysisCalibReport 分析侧校准报表（个股标准分析评级方向 vs +20 交易日走势）。
type AnalysisCalibReport struct {
	Evaluated bool `json:"evaluated"`
	Scanned   int  `json:"scanned"` // 回看的记录数（≤ calibAnalysisScanMax）
	Judged    int  `json:"judged"`  // 可判定样本（非 neutral 且后验数据足）

	NeutralSkipped  int `json:"neutral_skipped"`  // neutral 无方向不判
	ImmatureSkipped int `json:"immature_skipped"` // 基准日后不足 20 根
	NoDataSkipped   int `json:"no_data_skipped"`  // 无本地日线
	NoSysConf       int `json:"no_sys_conf"`      // ResultJSON 无 sys_confidence（旧记录）
	DupSkipped      int `json:"dup_skipped"`      // 同标的同基准日重复分析（只计最新一条，重复生成不刷样本）

	Brier       *float64            `json:"brier,omitempty"` // 口头置信度 vs 方向命中
	ECE         *float64            `json:"ece,omitempty"`
	Reliability []CalibBucket       `json:"reliability,omitempty"`
	SysTiers    []AnalysisCalibTier `json:"sys_tiers,omitempty"`
	Notes       []string            `json:"notes"`
}

// LLMCalibrationReport 校准总报表（管理端只读）。
type LLMCalibrationReport struct {
	GeneratedAt    string               `json:"generated_at"`
	LabelVersion   string               `json:"label_version"`
	Recommendation []*RecCalibReport    `json:"recommendation"` // 短线 h=10 / 长线 h=20
	Analysis       *AnalysisCalibReport `json:"analysis"`
	ElapsedMs      int64                `json:"elapsed_ms"`
	Notes          []string             `json:"notes"`
}

// ---------- 纯计算内核 ----------

// calibObs 单条校准观测：口头置信度（0-100）与二元结果。
type calibObs struct {
	Conf float64
	Hit  bool
	Net  float64 // 附带收益（桶均值展示用；分析侧=20 日收益）
}

// calibBucketBounds 固定 5 桶边界（左闭右开，末桶右闭）。
var calibBucketBounds = [][2]float64{{0, 20}, {20, 40}, {40, 60}, {60, 80}, {80, 100}}

func calibBucketIdx(conf float64) int {
	for i, b := range calibBucketBounds {
		if conf >= b[0] && (conf < b[1] || (i == len(calibBucketBounds)-1 && conf <= b[1])) {
			return i
		}
	}
	if conf < 0 {
		return 0
	}
	return len(calibBucketBounds) - 1
}

// calibBrier Brier 分数：mean((conf/100 − hit)²)。空观测返回 nil（未评估 ≠ 0 分）。
func calibBrier(obs []calibObs) *float64 {
	if len(obs) == 0 {
		return nil
	}
	var sum float64
	for _, o := range obs {
		p := o.Conf / 100
		y := 0.0
		if o.Hit {
			y = 1
		}
		sum += (p - y) * (p - y)
	}
	v := round4(sum / float64(len(obs)))
	return &v
}

// calibECE 期望校准误差：Σ (n_b/N)·|acc_b − avgConf_b|（概率口径 0~1）。
func calibECE(obs []calibObs) *float64 {
	if len(obs) == 0 {
		return nil
	}
	type agg struct {
		n    int
		hits int
		conf float64
	}
	buckets := make([]agg, len(calibBucketBounds))
	for _, o := range obs {
		i := calibBucketIdx(o.Conf)
		buckets[i].n++
		buckets[i].conf += o.Conf
		if o.Hit {
			buckets[i].hits++
		}
	}
	var ece float64
	n := float64(len(obs))
	for _, b := range buckets {
		if b.n == 0 {
			continue
		}
		acc := float64(b.hits) / float64(b.n)
		avg := b.conf / float64(b.n) / 100
		diff := acc - avg
		if diff < 0 {
			diff = -diff
		}
		ece += float64(b.n) / n * diff
	}
	v := round4(ece)
	return &v
}

// calibReliability 可靠性曲线：固定 5 桶逐桶命中率 vs 平均置信度（空桶不输出）。
func calibReliability(obs []calibObs) []CalibBucket {
	if len(obs) == 0 {
		return nil
	}
	type agg struct {
		n    int
		hits int
		conf float64
		net  float64
	}
	buckets := make([]agg, len(calibBucketBounds))
	for _, o := range obs {
		i := calibBucketIdx(o.Conf)
		buckets[i].n++
		buckets[i].conf += o.Conf
		buckets[i].net += o.Net
		if o.Hit {
			buckets[i].hits++
		}
	}
	var out []CalibBucket
	for i, b := range buckets {
		if b.n == 0 {
			continue
		}
		hit := float64(b.hits) / float64(b.n) * 100
		avg := b.conf / float64(b.n)
		out = append(out, CalibBucket{
			Label:      fmt.Sprintf("%d-%d", int(calibBucketBounds[i][0]), int(calibBucketBounds[i][1])),
			Sample:     b.n,
			AvgConf:    round2(avg),
			HitRatePct: round2(hit),
			AvgNetPct:  round2(b.net / float64(b.n)),
			GapPct:     round2(hit - avg),
		})
	}
	return out
}

// calibTierOrder SysConfidence 展示序（high → low →（未知））。
var calibTierOrder = []string{"high", "medium", "low"}

// calibTierMonotone 单调性描述：三档样本都 ≥ calibMinBucket 且命中率 high≥medium≥low
// 时描述「区分度成立」，否则如实描述——只描述观察，不下达任何门控结论。
func calibTierMonotone(tiers []CalibTierCell) string {
	byTier := map[string]CalibTierCell{}
	for _, t := range tiers {
		byTier[t.Tier] = t
	}
	h, hasH := byTier["high"]
	m, hasM := byTier["medium"]
	l, hasL := byTier["low"]
	if !hasH || !hasM || !hasL || h.Sample < calibMinBucket || m.Sample < calibMinBucket || l.Sample < calibMinBucket {
		return "档位样本不足，单调性暂无法判读"
	}
	if h.HitRatePct >= m.HitRatePct && m.HitRatePct >= l.HitRatePct {
		return "high ≥ medium ≥ low 命中率单调，档位区分度在当前样本上成立"
	}
	return "命中率未按档位单调（当前样本上档位区分度不成立），仅陈述观察"
}

// ---------- 推荐侧 ----------

// calibPickMeta DetailJSON 中本报表消费的字段（recPick 子集）。
type calibPickMeta struct {
	SysConfidence  string `json:"sys_confidence"`
	DegradedSource string `json:"degraded_source"`
}

// buildRecCalibReport 单 type×horizon 的推荐校准（全库口径：管理端报表不分用户，
// 样本才够门槛；Notes 声明）。
func buildRecCalibReport(recType string, horizon int) (*RecCalibReport, error) {
	rep := &RecCalibReport{Type: recType, HorizonDays: horizon}

	var labels []model.RecommendationLabel
	if err := common.DB.
		Where("horizon_days = ? AND entry_mode = ? AND recommendation_id > 0 AND label_version = ? AND type = ?",
			horizon, model.EntryModeNextOpen, labelVersion, recType).
		Find(&labels).Error; err != nil {
		return nil, err
	}
	rep.Coverage.Total = len(labels)

	// 关联推荐条目：口头置信度列 + DetailJSON 的 sys_confidence/degraded_source。
	ids := make([]int64, 0, len(labels))
	seen := map[int64]bool{}
	for _, l := range labels {
		if !seen[l.RecommendationID] {
			seen[l.RecommendationID] = true
			ids = append(ids, l.RecommendationID)
		}
	}
	type recMeta struct {
		conf     int
		sysConf  string
		degraded bool
	}
	metas := map[int64]recMeta{}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		var recs []model.Recommendation
		if err := common.DB.Select("id", "confidence", "detail_json").
			Where("id IN ?", ids[start:end]).Find(&recs).Error; err != nil {
			return nil, err
		}
		for _, r := range recs {
			var pm calibPickMeta
			_ = json.Unmarshal([]byte(r.DetailJSON), &pm)
			metas[r.ID] = recMeta{conf: r.Confidence, sysConf: pm.SysConfidence, degraded: pm.DegradedSource != ""}
		}
	}

	type sample struct {
		label model.RecommendationLabel
		meta  recMeta
	}
	var usable []sample
	for _, l := range labels {
		switch l.MaturityStatus {
		case model.LabelMatured:
			rep.Coverage.Matured++
			if l.Forced {
				rep.Coverage.Forced++
				continue
			}
			m, ok := metas[l.RecommendationID]
			if !ok {
				// 孤儿标签（推荐条目已被用户删除）：置信度/SysConfidence 无从查证，
				// 混入会以 confidence=0 进桶把 Brier/ECE 拉向失真——单列剔除（审查修复批）。
				rep.Coverage.OrphanExcl++
				continue
			}
			if m.degraded {
				// 量化降级条目：置信度 35/SysConfidence low 是程序赋值，不是模型预测，
				// 混入会把「程序规则」误测成「模型校准」。
				rep.Coverage.DegradedExcl++
				continue
			}
			usable = append(usable, sample{label: l, meta: m})
		case model.LabelPending:
			rep.Coverage.Pending++
		case model.LabelSkipped:
			rep.Coverage.Skipped++
		case model.LabelNoData:
			rep.Coverage.NoData++
		}
	}
	if rep.Coverage.Total > 0 {
		rep.Coverage.MaturedRatioPct = round2(float64(rep.Coverage.Matured) / float64(rep.Coverage.Total) * 100)
	}

	rep.Sample = len(usable)

	// 动作 precision/recall（buy∪watch 全量；alpha 口径只吃 HasBench 样本）。
	var buyN, buyHit, posN, watchN, watchHit int
	var aN, aBuyN, aBuyHit, aPosN int
	for _, s := range usable {
		hit := s.label.NetReturnPct > 0
		isBuy := s.label.Action == model.RecActionBuy
		if isBuy {
			buyN++
			if hit {
				buyHit++
			}
		} else {
			watchN++
			if hit {
				watchHit++
			}
		}
		if hit {
			posN++
		}
		if s.label.HasBench {
			aN++
			aHit := s.label.AlphaPct > 0
			if isBuy {
				aBuyN++
				if aHit {
					aBuyHit++
				}
			}
			if aHit {
				aPosN++
			}
		}
	}
	pr := CalibActionPR{Sample: len(usable), BuySample: buyN, AlphaSample: aN}
	if buyN > 0 {
		v := round2(float64(buyHit) / float64(buyN) * 100)
		pr.PrecisionNetPct = &v
	}
	if posN > 0 {
		v := round2(float64(buyHit) / float64(posN) * 100)
		pr.RecallNetPct = &v
	}
	if watchN > 0 {
		v := round2(float64(watchHit) / float64(watchN) * 100)
		pr.WatchHitPct = &v
	}
	if aBuyN > 0 {
		v := round2(float64(aBuyHit) / float64(aBuyN) * 100)
		pr.PrecisionAlpha = &v
	}
	if aPosN > 0 {
		// alpha 口径 recall：buy 且 alpha>0 / 全部 alpha>0。
		var aBuyPos int
		for _, s := range usable {
			if s.label.HasBench && s.label.Action == model.RecActionBuy && s.label.AlphaPct > 0 {
				aBuyPos++
			}
		}
		v := round2(float64(aBuyPos) / float64(aPosN) * 100)
		pr.RecallAlpha = &v
	}
	rep.ActionPR = pr
	rep.BuySample = buyN

	// 口头置信度校准 + SysConfidence 分档：限定 buy（买入建议的校准最有意义；
	// watch 是弱信号语义不同，其命中率已在 ActionPR 对照展示）。
	var obs []calibObs
	tierRows := map[string][]model.RecommendationLabel{}
	tierGross := map[string]float64{}
	for _, s := range usable {
		if s.label.Action != model.RecActionBuy {
			continue
		}
		obs = append(obs, calibObs{Conf: float64(s.meta.conf), Hit: s.label.NetReturnPct > 0, Net: s.label.NetReturnPct})
		tier := s.meta.sysConf
		if tier == "" {
			tier = "（未知）"
		}
		tierRows[tier] = append(tierRows[tier], s.label)
		tierGross[tier] += s.label.GrossReturnPct
	}
	// evaluated 与 Brier/ECE 同门槛（审查修复批）：口径统一为「buy 口头置信度样本
	// ≥ calibEvalMinSample」——分档/PR 表任何样本量都展示（前端按 calibMinBucket 标注
	// 样本不足），但报表级「已评估」只认校准指标真正产出。
	rep.Evaluated = len(obs) >= calibEvalMinSample
	if rep.Evaluated {
		rep.Brier = calibBrier(obs)
		rep.ECE = calibECE(obs)
	}
	rep.Reliability = calibReliability(obs)

	appendTier := func(tier string) {
		rows, ok := tierRows[tier]
		if !ok {
			return
		}
		cell := CalibTierCell{Tier: tier, Sample: len(rows)}
		nets := make([]float64, 0, len(rows))
		var sumNet, sumAlpha float64
		hits, severe := 0, 0
		for _, l := range rows {
			nets = append(nets, l.NetReturnPct)
			sumNet += l.NetReturnPct
			if l.NetReturnPct > 0 {
				hits++
			}
			if l.NetReturnPct < -5 {
				severe++
			}
			if l.HasBench {
				cell.AlphaSample++
				sumAlpha += l.AlphaPct
			}
		}
		sort.Float64s(nets)
		n := float64(len(rows))
		cell.HitRatePct = round2(float64(hits) / n * 100)
		cell.MedianNetPct = round2(median(nets))
		cell.AvgNetPct = round2(sumNet / n)
		cell.AvgGrossPct = round2(tierGross[tier] / n)
		cell.SevereLossPct = round2(float64(severe) / n * 100)
		if cell.AlphaSample > 0 {
			cell.AvgAlphaPct = round2(sumAlpha / float64(cell.AlphaSample))
		}
		rep.SysTiers = append(rep.SysTiers, cell)
	}
	for _, t := range calibTierOrder {
		appendTier(t)
	}
	appendTier("（未知）")
	if len(rep.SysTiers) > 0 {
		rep.TierMonotone = calibTierMonotone(rep.SysTiers)
	}

	rep.Notes = append(rep.Notes,
		"全库口径（不分用户）：管理端测量报表，样本才够门槛；口径=统一执行模拟 next_open、label_version="+labelVersion+"、成熟非强平非降级",
		"口头置信度校准仅限 buy 样本；y=净收益>0。Brier/ECE 是「模型口头 confidence 不当真实概率」（§6.3）的测量证据，非采信",
		"程序合成置信度是有序档位不硬造概率，只报分档命中率/收益与单调性观察；成本前（gross）与成本后（net）收益分开统计",
		fmt.Sprintf("「已评估」硬门槛=buy 口头置信度样本 ≥%d（§8.1 推荐模块每数据状态 100；未评估≠0 分）；分档/PR 表 <%d 仅供分级参考、单桶/单档 <%d 统计不稳定", calibEvalMinSample, calibMinSample, calibMinBucket),
		"口径限制（如实声明）：未按 provider/model/prompt/策略/regime 分层（先分层再汇总的完整口径待 P2-5 联合评估）；confidence 为落库终值（复核 reject 降级条目的置信度已被程序改写，混合了模型原始预测与复核修正）",
	)
	return rep, nil
}

// ---------- 分析侧 ----------

// calibAnalysisSysConf ResultJSON 中本报表消费的字段（AnalysisResult 子集）。
type calibAnalysisSysConf struct {
	SysConfidence string `json:"sys_confidence"`
}

// buildAnalysisCalibReport 个股标准分析评级方向 vs +20 交易日走势（hindsight 同款
// 口径现算：无落库标签，个人量级逐条查询可承受，上限 calibAnalysisScanMax 条）。
func buildAnalysisCalibReport(ctx context.Context) (*AnalysisCalibReport, error) {
	rep := &AnalysisCalibReport{}

	var recs []model.AnalysisRecord
	if err := common.DB.
		Select("id", "symbol", "market", "rating", "confidence", "as_of", "created_at", "result_json").
		// as_of 回溯诊断记录排除（审查修复批）：回溯分析是「事后视角的历史解释」，与
		// 实时分析混测会污染 PIT 纪律（同一历史日期可反复生成刷样本）；只测实时记录。
		Where("module = ? AND market = ? AND status = ? AND mode = '' AND symbol <> '' AND (as_of = '' OR as_of IS NULL)",
			model.AnalysisModuleStock, "cn", model.AnalysisStatusSuccess).
		Order("id DESC").Limit(calibAnalysisScanMax).
		Find(&recs).Error; err != nil {
		return nil, err
	}
	rep.Scanned = len(recs)

	// symbol×基准日去重（审查修复批）：同标的同日重复分析只计最新一条——重复生成
	// 不得刷样本量（§9.1 事件粒度）。
	seenEvent := map[string]bool{}

	var obs []calibObs
	type tierAgg struct {
		n    int
		hits int
		ret  float64
	}
	tiers := map[string]*tierAgg{}

	for _, r := range recs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if r.Rating == model.AnalysisRatingNeutral || r.Rating == "" {
			rep.NeutralSkipped++
			continue
		}
		baseDate := r.AsOf
		if baseDate == "" {
			baseDate = r.CreatedAt.In(time.Local).Format("2006-01-02")
		}
		if key := r.Symbol + "|" + baseDate; seenEvent[key] {
			rep.DupSkipped++
			continue
		} else {
			seenEvent[key] = true
		}
		base := cnBarsUpTo(r.Symbol, baseDate, 1)
		if len(base) == 0 || base[0].Close <= 0 {
			rep.NoDataSkipped++
			continue
		}
		var after []model.DailyBar
		if err := common.DB.Select("trade_date", "close").
			Where("market = ? AND symbol = ? AND trade_date > ?", "cn", r.Symbol, baseDate).
			Order("trade_date").Limit(calibAnalysisHorizon).
			Find(&after).Error; err != nil {
			return nil, err
		}
		if len(after) < calibAnalysisHorizon || after[calibAnalysisHorizon-1].Close <= 0 {
			rep.ImmatureSkipped++
			continue
		}
		ret := (after[calibAnalysisHorizon-1].Close/base[0].Close - 1) * 100
		hit := (r.Rating == model.AnalysisRatingBullish && ret > 0) ||
			(r.Rating == model.AnalysisRatingBearish && ret < 0)
		rep.Judged++
		obs = append(obs, calibObs{Conf: float64(r.Confidence), Hit: hit, Net: round2(ret)})

		var sc calibAnalysisSysConf
		_ = json.Unmarshal([]byte(r.ResultJSON), &sc)
		tier := sc.SysConfidence
		if tier == "" {
			rep.NoSysConf++
			tier = "（未知）"
		}
		a := tiers[tier]
		if a == nil {
			a = &tierAgg{}
			tiers[tier] = a
		}
		a.n++
		a.ret += ret
		if hit {
			a.hits++
		}
	}

	rep.Evaluated = rep.Judged >= calibEvalMinSample
	if rep.Evaluated {
		rep.Brier = calibBrier(obs)
		rep.ECE = calibECE(obs)
	}
	rep.Reliability = calibReliability(obs)

	appendTier := func(tier string) {
		a, ok := tiers[tier]
		if !ok {
			return
		}
		rep.SysTiers = append(rep.SysTiers, AnalysisCalibTier{
			Tier: tier, Sample: a.n,
			HitRatePct:  round2(float64(a.hits) / float64(a.n) * 100),
			AvgRet20Pct: round2(a.ret / float64(a.n)),
		})
	}
	for _, t := range calibTierOrder {
		appendTier(t)
	}
	appendTier("（未知）")

	rep.Notes = append(rep.Notes,
		fmt.Sprintf("口径：个股标准分析（非 panel）评级方向 vs 基准日（as_of 或创建日）后 +%d 交易日收盘（hindsight 同款）；neutral 无方向不判", calibAnalysisHorizon),
		fmt.Sprintf("最多回看最近 %d 条记录；后验不足 %d 根/无本地日线的单列跳过（未评估≠失败）", calibAnalysisScanMax, calibAnalysisHorizon),
		"方向命中只验证「涨跌方向」，不代表按分析操作可获得该收益（无执行模拟/无成本）；与推荐侧净收益口径不可比",
	)
	return rep, nil
}

// ---------- 报表入口（进程内缓存 + 互斥，factoric/walkforward 同款模式） ----------

var (
	calibMu       sync.Mutex
	calibInflight bool
	calibCacheMu  sync.RWMutex
	calibCache    *LLMCalibrationReport
)

// CachedLLMCalibrationReport 返回进程内缓存的校准报表（可能为 nil）。
func CachedLLMCalibrationReport() *LLMCalibrationReport {
	calibCacheMu.RLock()
	defer calibCacheMu.RUnlock()
	return calibCache
}

// RunLLMCalibration 计算校准报表（P1-7）。纯测量：只读标签/记录/日线表，
// 不写任何业务表、零 LLM 调用、零门控。全局互斥防并发重算。
func RunLLMCalibration(ctx context.Context) (*LLMCalibrationReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	calibMu.Lock()
	if calibInflight {
		calibMu.Unlock()
		return nil, errors.New("校准报表正在计算中，请稍候")
	}
	calibInflight = true
	calibMu.Unlock()
	defer func() {
		calibMu.Lock()
		calibInflight = false
		calibMu.Unlock()
	}()

	start := time.Now()
	rep := &LLMCalibrationReport{
		GeneratedAt:  time.Now().In(time.Local).Format("2006-01-02 15:04:05"),
		LabelVersion: labelVersion,
	}
	for _, t := range []string{model.RecTypeShortTerm, model.RecTypeLongTerm} {
		r, err := buildRecCalibReport(t, calibRepHorizons[t])
		if err != nil {
			return nil, err
		}
		rep.Recommendation = append(rep.Recommendation, r)
	}
	a, err := buildAnalysisCalibReport(ctx)
	if err != nil {
		return nil, err
	}
	rep.Analysis = a
	rep.ElapsedMs = time.Since(start).Milliseconds()
	rep.Notes = append(rep.Notes,
		"P1-7 纯测量报表：零门控零 LLM 调用，不改写任何线上行为；数据不足如实标「未评估」",
		fmt.Sprintf("推荐侧代表持有期：短线 h=%d / 长线 h=%d（与反思记忆、月度走查同口径）；其余持有期见归因报表", calibRepHorizons[model.RecTypeShortTerm], calibRepHorizons[model.RecTypeLongTerm]),
	)

	calibCacheMu.Lock()
	calibCache = rep
	calibCacheMu.Unlock()
	return rep, nil
}
