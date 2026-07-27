package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
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

// CalibSliceRow 分层维度单行（P2-5 批落地第五十一批声明的口径限制：先分层再汇总）。
// 口径与主校准一致：buy 成熟样本（剔 forced/degraded/orphan）；每层 Brier/ECE 与报表级
// 同门槛（≥ calibEvalMinSample 才产出，小样本层只报命中率/收益供参考）。
type CalibSliceRow struct {
	Key          string   `json:"key"`
	Sample       int      `json:"sample"` // buy 成熟样本
	HitRatePct   float64  `json:"hit_rate_pct"`
	AvgNetPct    float64  `json:"avg_net_pct"`
	MedianNetPct float64  `json:"median_net_pct"`
	AvgAlphaPct  float64  `json:"avg_alpha_pct"`
	AlphaSample  int      `json:"alpha_sample"`
	Brier        *float64 `json:"brier,omitempty"`
	ECE          *float64 `json:"ece,omitempty"`
}

// CalibSliceGroup 单一分层维度的全部取值行。
type CalibSliceGroup struct {
	Dim   string          `json:"dim"`   // strategy / regime / provider_model / prompt_version
	Label string          `json:"label"` // 展示名
	Rows  []CalibSliceRow `json:"rows"`
}

// calibSliceObs 分层观测（label 收益 + 口头置信度 + 维度键；联合评估复用同内核）。
type calibSliceObs struct {
	Key      string
	Conf     float64
	Hit      bool
	Net      float64
	Alpha    float64
	HasBench bool
}

// calibSliceMaxRows 单维度最多展示的取值行；超出按样本降序保留、其余合并「（其他）」
// 行（防止长尾 prompt_version/provider 撑爆报表；合并行不产出 Brier/ECE——混合层无解释意义）。
const calibSliceMaxRows = 12

// calibSliceRows 把观测按维度取值分组为统计行：样本降序（同样本按 key 字典序稳定），
// 每层样本 ≥ calibEvalMinSample 才产出 Brier/ECE（与报表级「已评估」同门槛）。
func calibSliceRows(obs []calibSliceObs) []CalibSliceRow {
	groups := map[string][]calibSliceObs{}
	for _, o := range obs {
		k := o.Key
		if k == "" {
			k = "（未知）"
		}
		groups[k] = append(groups[k], o)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(groups[keys[i]]) != len(groups[keys[j]]) {
			return len(groups[keys[i]]) > len(groups[keys[j]])
		}
		return keys[i] < keys[j]
	})

	build := func(key string, list []calibSliceObs, withCalib bool) CalibSliceRow {
		row := CalibSliceRow{Key: key, Sample: len(list)}
		nets := make([]float64, 0, len(list))
		var sumNet, sumAlpha float64
		hits := 0
		var cobs []calibObs
		for _, o := range list {
			nets = append(nets, o.Net)
			sumNet += o.Net
			if o.Hit {
				hits++
			}
			if o.HasBench {
				row.AlphaSample++
				sumAlpha += o.Alpha
			}
			cobs = append(cobs, calibObs{Conf: o.Conf, Hit: o.Hit, Net: o.Net})
		}
		sort.Float64s(nets)
		n := float64(len(list))
		if n > 0 {
			row.HitRatePct = round2(float64(hits) / n * 100)
			row.AvgNetPct = round2(sumNet / n)
			row.MedianNetPct = round2(median(nets))
		}
		if row.AlphaSample > 0 {
			row.AvgAlphaPct = round2(sumAlpha / float64(row.AlphaSample))
		}
		if withCalib && len(cobs) >= calibEvalMinSample {
			row.Brier = calibBrier(cobs)
			row.ECE = calibECE(cobs)
		}
		return row
	}

	var rows []CalibSliceRow
	var restList []calibSliceObs
	restKinds := 0
	for i, k := range keys {
		if i < calibSliceMaxRows {
			rows = append(rows, build(k, groups[k], true))
			continue
		}
		restKinds++
		restList = append(restList, groups[k]...)
	}
	if restKinds > 0 {
		rows = append(rows, build(fmt.Sprintf("（其他 %d 项）", restKinds), restList, false))
	}
	return rows
}

// calibBatchMeta 批次归因元数据（分层维度来源：批次表 Provider/Model/PromptVersion 列）。
type calibBatchMeta struct {
	Provider      string
	Model         string
	PromptVersion string
}

// calibBatchMetaByID 批量查批次归因列（500 一批防 IN 过长；旧批次空值层归「（未知）」）。
func calibBatchMetaByID(ids []int64) (map[int64]calibBatchMeta, error) {
	out := map[int64]calibBatchMeta{}
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		var rows []model.RecommendationBatch
		if err := common.DB.Select("id", "provider", "model", "prompt_version").
			Where("id IN ?", ids[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		for _, b := range rows {
			out[b.ID] = calibBatchMeta{Provider: b.Provider, Model: b.Model, PromptVersion: b.PromptVersion}
		}
	}
	return out, nil
}

// calibProviderModelKey provider/model 分层键（批次已删或旧行空值时如实「（未知）」）。
func calibProviderModelKey(m calibBatchMeta, ok bool) string {
	if !ok || (m.Provider == "" && m.Model == "") {
		return ""
	}
	if m.Model == "" {
		return m.Provider
	}
	if m.Provider == "" {
		return m.Model
	}
	return m.Provider + "/" + m.Model
}

// CalibRawSummary 原始口头置信度口径分列（第五十六批②，清「confidence 为落库终值」
// 口径限制）：RawConfidence=复核 reject 级联/复核覆盖改写前的模型预测快照。与主校准
// （终值口径）分开测——终值口径测「用户实际看到的置信度」的校准性，原始口径测「模型
// 自身预测」的校准性，两者混在一列会把复核修正误记到模型头上。纯测量零门控。
type CalibRawSummary struct {
	Sample  int `json:"sample"`  // 有原始快照的原始 buy 样本（Brier/ECE 的分母池）
	Missing int `json:"missing"` // 旧记录无原始快照（如实单列剔除，不硬造=不拿终值冒充原始值）
	// Diverged 原始动作或置信度与终值不同的样本数（复核真实改写过的面）。动作差异
	// 在 raw confidence 缺失时仍可判定，因此该数不受 Sample（可计算校准的样本）约束。
	Diverged int      `json:"diverged"`
	Brier    *float64 `json:"brier,omitempty"` // ≥ calibEvalMinSample 才产出（与主口径同门槛）
	ECE      *float64 `json:"ece,omitempty"`
}

func normalizeCalibRawAction(action string) string {
	action = strings.ToLower(strings.TrimSpace(action))
	if action == model.RecActionBuy || action == model.RecActionWatch {
		return action
	}
	return ""
}

// calibRawAction 返回模型原始动作。新记录优先使用 DetailJSON 快照；旧记录由加载层尝试
// 从同批次同标的 picked 事件回填，仍缺失时才回退标签终值。
func calibRawAction(s calibSample) string {
	if s.meta.rawAction != nil {
		if action := normalizeCalibRawAction(*s.meta.rawAction); action != "" {
			return action
		}
	}
	return s.label.Action
}

// calibRawSummary 从样本池聚合原始口径分列：主口径按最终 buy，raw 口径按原始 buy。
// 校准报表与联合评估共用同一实现，避免两处口径漂移。
func calibRawSummary(samples []calibSample) *CalibRawSummary {
	raw := &CalibRawSummary{}
	var robs []calibObs
	for _, s := range samples {
		rawAction := calibRawAction(s)
		if rawAction != model.RecActionBuy {
			continue
		}
		actionDiverged := rawAction != s.label.Action
		if actionDiverged {
			raw.Diverged++
		}
		if s.meta.rawConf == nil {
			raw.Missing++
			continue
		}
		raw.Sample++
		if !actionDiverged && *s.meta.rawConf != s.meta.conf {
			raw.Diverged++
		}
		robs = append(robs, calibObs{Conf: float64(*s.meta.rawConf), Hit: s.label.NetReturnPct > 0, Net: s.label.NetReturnPct})
	}
	if len(robs) >= calibEvalMinSample {
		raw.Brier = calibBrier(robs)
		raw.ECE = calibECE(robs)
	}
	return raw
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
	// 主口径=落库终值（用户实际看到的置信度，含复核 reject 级联/复核覆盖修正）。
	Brier       *float64      `json:"brier,omitempty"`
	ECE         *float64      `json:"ece,omitempty"`
	Reliability []CalibBucket `json:"reliability,omitempty"`

	// RawCalib 原始口径分列（第五十六批②）：复核改写前的模型预测快照单独测校准。
	// 旧记录无快照如实进 Missing 不硬造。
	RawCalib *CalibRawSummary `json:"raw_calib,omitempty"`

	// 程序合成置信度分档（buy 样本）。
	SysTiers     []CalibTierCell `json:"sys_tiers,omitempty"`
	TierMonotone string          `json:"tier_monotone,omitempty"` // 单调性描述（不下结论）

	// Slices P2-5 批分层维度（策略/regime/provider·model/prompt_version；buy 口径）——
	// 落地第五十一批 Notes 声明的「先分层再汇总」口径限制。
	Slices []CalibSliceGroup `json:"slices,omitempty"`

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
	// RawAction 与 RawConfidence 同点快照；nil=旧记录无原始动作。
	RawAction *string `json:"raw_action"`
	// RawConfidence 模型原始口头置信度快照（第五十六批②；复核 reject 级联/复核覆盖
	// 改写前的值）。nil=旧记录无快照（单列 raw_missing，不硬造）。
	RawConfidence *int `json:"raw_confidence"`
}

// calibRecMeta 推荐条目关联元数据（口头置信度列 + DetailJSON 摘取）。
type calibRecMeta struct {
	conf      int
	rawAction *string // 原始动作快照（DetailJSON 优先，旧记录可由 picked 事件回填）
	rawConf   *int    // 原始口头置信度快照（nil=旧记录缺席）
	sysConf   string
	degraded  bool
}

type calibRawActionRef struct {
	recID   int64
	batchID int64
	symbol  string
}

type calibRawActionKey struct {
	batchID int64
	symbol  string
}

// calibPickedRawActions 为 DetailJSON 尚无 raw_action 的历史推荐查权威原动作。
// candidate_events 无唯一索引，异常重试可能产生重复行；按 id 倒序取最新有效 picked 行。
func calibPickedRawActions(refs []calibRawActionRef) (map[int64]string, error) {
	out := make(map[int64]string)
	if len(refs) == 0 {
		return out, nil
	}

	recIDsByKey := make(map[calibRawActionKey][]int64, len(refs))
	batchIDs := make([]int64, 0, len(refs))
	seenBatch := make(map[int64]bool, len(refs))
	for _, ref := range refs {
		if ref.batchID <= 0 || ref.symbol == "" {
			continue
		}
		key := calibRawActionKey{batchID: ref.batchID, symbol: ref.symbol}
		recIDsByKey[key] = append(recIDsByKey[key], ref.recID)
		if !seenBatch[ref.batchID] {
			seenBatch[ref.batchID] = true
			batchIDs = append(batchIDs, ref.batchID)
		}
	}

	for start := 0; start < len(batchIDs); start += 500 {
		end := start + 500
		if end > len(batchIDs) {
			end = len(batchIDs)
		}
		var events []model.RecommendationCandidateEvent
		if err := common.DB.Select("id", "batch_id", "symbol", "raw_action").
			Where("batch_id IN ? AND candidate_stage = ?", batchIDs[start:end], model.CandStagePicked).
			Order("id DESC").Find(&events).Error; err != nil {
			return nil, err
		}
		for _, ev := range events {
			action := normalizeCalibRawAction(ev.RawAction)
			if action == "" {
				continue
			}
			key := calibRawActionKey{batchID: ev.BatchID, symbol: ev.Symbol}
			for _, recID := range recIDsByKey[key] {
				if _, exists := out[recID]; !exists {
					out[recID] = action
				}
			}
		}
	}
	return out, nil
}

// calibSample 单条可用校准样本（成熟、非强平、非降级、非孤儿）。
type calibSample struct {
	label model.RecommendationLabel
	meta  calibRecMeta
}

// loadRecCalibSamples 加载单 type×horizon 的标签样本与覆盖面分类（校准报表与 P2-5
// 联合评估共用同一口径：l2/next_open/rec_id>0；matured 中 forced/orphan/degraded 单列
// 剔除——两报表口径漂移会让「校准」与「联合评估」各说各话，收口在此）。
func loadRecCalibSamples(recType string, horizon int) (usable []calibSample, coverage CalibCoverage, err error) {
	var labels []model.RecommendationLabel
	if err = common.DB.
		Where("horizon_days = ? AND entry_mode = ? AND recommendation_id > 0 AND label_version = ? AND type = ?",
			horizon, model.EntryModeNextOpen, labelVersion, recType).
		Find(&labels).Error; err != nil {
		return nil, coverage, err
	}
	coverage.Total = len(labels)

	// 关联推荐条目：口头置信度列 + DetailJSON 元数据；旧 raw_action 再从 picked 事件回填。
	ids := make([]int64, 0, len(labels))
	seen := map[int64]bool{}
	for _, l := range labels {
		if !seen[l.RecommendationID] {
			seen[l.RecommendationID] = true
			ids = append(ids, l.RecommendationID)
		}
	}
	metas := map[int64]calibRecMeta{}
	var legacyRawActionRefs []calibRawActionRef
	for start := 0; start < len(ids); start += 500 {
		end := start + 500
		if end > len(ids) {
			end = len(ids)
		}
		var recs []model.Recommendation
		if err = common.DB.Select("id", "batch_id", "symbol", "confidence", "detail_json").
			Where("id IN ?", ids[start:end]).Find(&recs).Error; err != nil {
			return nil, coverage, err
		}
		for _, r := range recs {
			var pm calibPickMeta
			_ = json.Unmarshal([]byte(r.DetailJSON), &pm)
			metas[r.ID] = calibRecMeta{conf: r.Confidence, rawAction: pm.RawAction, rawConf: pm.RawConfidence, sysConf: pm.SysConfidence, degraded: pm.DegradedSource != ""}
			if pm.RawAction == nil || normalizeCalibRawAction(*pm.RawAction) == "" {
				legacyRawActionRefs = append(legacyRawActionRefs, calibRawActionRef{recID: r.ID, batchID: r.BatchID, symbol: r.Symbol})
			}
		}
	}
	pickedRawActions, loadErr := calibPickedRawActions(legacyRawActionRefs)
	if loadErr != nil {
		return nil, coverage, fmt.Errorf("加载历史推荐原始动作失败: %w", loadErr)
	}
	for recID, action := range pickedRawActions {
		m := metas[recID]
		rawAction := action
		m.rawAction = &rawAction
		metas[recID] = m
	}

	for _, l := range labels {
		switch l.MaturityStatus {
		case model.LabelMatured:
			coverage.Matured++
			if l.Forced {
				coverage.Forced++
				continue
			}
			m, ok := metas[l.RecommendationID]
			if !ok {
				// 孤儿标签（推荐条目已被用户删除）：置信度/SysConfidence 无从查证，
				// 混入会以 confidence=0 进桶把 Brier/ECE 拉向失真——单列剔除（审查修复批）。
				coverage.OrphanExcl++
				continue
			}
			if m.degraded {
				// 量化降级条目：置信度 35/SysConfidence low 是程序赋值，不是模型预测，
				// 混入会把「程序规则」误测成「模型校准」。
				coverage.DegradedExcl++
				continue
			}
			usable = append(usable, calibSample{label: l, meta: m})
		case model.LabelPending:
			coverage.Pending++
		case model.LabelSkipped:
			coverage.Skipped++
		case model.LabelNoData:
			coverage.NoData++
		}
	}
	if coverage.Total > 0 {
		coverage.MaturedRatioPct = round2(float64(coverage.Matured) / float64(coverage.Total) * 100)
	}
	return usable, coverage, nil
}

// buildRecCalibReport 单 type×horizon 的推荐校准（全库口径：管理端报表不分用户，
// 样本才够门槛；Notes 声明）。
func buildRecCalibReport(recType string, horizon int) (*RecCalibReport, error) {
	rep := &RecCalibReport{Type: recType, HorizonDays: horizon}

	usable, coverage, err := loadRecCalibSamples(recType, horizon)
	if err != nil {
		return nil, err
	}
	rep.Coverage = coverage
	type sample = calibSample

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
	// 原始口径分列按复核前动作取样；即使全部原始 buy 都被 reject 成 watch 也必须输出。
	rawCalib := calibRawSummary(usable)
	if rawCalib.Sample > 0 || rawCalib.Missing > 0 {
		rep.RawCalib = rawCalib
	}

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

	// P2-5 分层维度（buy 口径，与口头置信度校准同样本池）：策略/regime 用标签归因冗余，
	// provider·model/prompt_version 关联批次归因列（批次已删/旧行空值层归「（未知）」）。
	// 分层是观察不是采信：每层样本 ≥ calibEvalMinSample 才产出该层 Brier/ECE。
	{
		var buys []sample
		batchIDs := make([]int64, 0, 16)
		seenBatch := map[int64]bool{}
		for _, s := range usable {
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
			sliceObs := func(keyOf func(sample) string) []calibSliceObs {
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
			dims := []struct {
				dim   string
				label string
				keyOf func(sample) string
			}{
				{"strategy", "策略", func(s sample) string { return s.label.Strategy }},
				{"regime", "市场状态", func(s sample) string { return s.label.Regime }},
				{"provider_model", "Provider·Model", func(s sample) string {
					m, ok := bmetas[s.label.BatchID]
					return calibProviderModelKey(m, ok)
				}},
				{"prompt_version", "Prompt 版本", func(s sample) string {
					m, ok := bmetas[s.label.BatchID]
					if !ok {
						return ""
					}
					return m.PromptVersion
				}},
			}
			for _, d := range dims {
				rows := calibSliceRows(sliceObs(d.keyOf))
				if len(rows) == 0 {
					continue
				}
				rep.Slices = append(rep.Slices, CalibSliceGroup{Dim: d.dim, Label: d.label, Rows: rows})
			}
		}
	}

	rep.Notes = append(rep.Notes,
		"全库口径（不分用户）：管理端测量报表，样本才够门槛；口径=统一执行模拟 next_open、label_version="+labelVersion+"、成熟非强平非降级",
		"口头置信度校准仅限 buy 样本；y=净收益>0。Brier/ECE 是「模型口头 confidence 不当真实概率」（§6.3）的测量证据，非采信",
		"程序合成置信度是有序档位不硬造概率，只报分档命中率/收益与单调性观察；成本前（gross）与成本后（net）收益分开统计",
		fmt.Sprintf("「已评估」硬门槛=buy 口头置信度样本 ≥%d（§8.1 推荐模块每数据状态 100；未评估≠0 分）；分档/PR 表 <%d 仅供分级参考、单桶/单档 <%d 统计不稳定", calibEvalMinSample, calibMinSample, calibMinBucket),
		fmt.Sprintf("分层维度（P2-5）：策略/市场状态取标签归因冗余，provider·model/prompt_version 关联批次归因列（prompt_version 分层即 champion/challenger 晋级前后的对照落点）；每层样本 ≥%d 才产出该层 Brier/ECE，小样本层只报命中率/收益供参考，分层是观察不是采信", calibEvalMinSample),
		"双口径（第五十六批）：主校准按最终 buy 与落库终值；raw_calib 按复核前原始 buy 与原始口头置信度。两者共用成熟、非强平、非降级标签总体及评估门槛；旧记录无原始动作时先按同批次同标的 picked 事件回填，事件也缺失才回退最终动作；无原始置信度时计 missing",
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
