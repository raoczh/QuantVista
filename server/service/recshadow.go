package service

import (
	"errors"
	"fmt"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// S2-4 影子对照报表（RECOMMENDATION_ACCURACY_PLAN §5 S2 验收 / §9）：一切影子门控
// （regime_shadow / bear_shadow / quality_shadow）与名单裁剪（correlation / industry_cap）
// **转正评审的数据地基**——基于 recommendation_labels + recommendation_candidate_events
// 按 gate_type 分组对比「被门控标记标的」与「未被任何门控标记的入选标的」的成熟
// 净收益/alpha 分布，以及若强制执行的推荐覆盖率损失（risk-coverage 语义）。
//
// 口径说明：
//   - 标的收益一律取 entry_mode=next_open 统一模拟标签（picked 挂 rec 行、未入选挂
//     事件行的影子标签，同一执行语义可比）；
//   - regime/bear/quality 标记的是入选（picked）条目；correlation/industry_cap 标记的
//     是名单阶段被挤出的候选——对照组统一为「未被任何门控标记的入选标的」；
//   - defense 批的 buy 会被 regime_shadow 全量标记，其对照天然是跨批次的
//     （闸门避开的收益 vs 正常放行的收益），样本少时只有方向参考意义；
//   - 转正评价协议（评价窗口/最低样本量/覆盖率下降上限）须在影子期开始前预注册
//     （文档 §5 S5），本报表只供出数，不做转正判定。

// ShadowGateGroup 单一 gate_type 的 gated vs ungated 对照。
type ShadowGateGroup struct {
	GateType  string `json:"gate_type"`
	GateLabel string `json:"gate_label"`
	// Marked 被该门控标记、且在本报表口径（类型/持有期）下有标签的标的·批次数（含未成熟）。
	Marked int `json:"marked"`
	// WouldRewrite 其中若强制执行会改写动作的数量（would_be_action 非空；质量门控只
	// 封顶置信度不改动作，恒 0）。
	WouldRewrite int             `json:"would_rewrite"`
	Gated        AttributionCell `json:"gated"`   // 被标记标的成熟统计
	Ungated      AttributionCell `json:"ungated"` // 未标记入选标的成熟统计（对照组，各组共用）
}

// ShadowReport 影子门控对照报表（单一持有期口径）。
type ShadowReport struct {
	Type        string `json:"type"`
	HorizonDays int    `json:"horizon_days"`
	// PickedBuy 入选 buy 标签总数（覆盖率分母，含未成熟）；PickedBuyMatured 其中已成熟数。
	PickedBuy        int               `json:"picked_buy"`
	PickedBuyMatured int               `json:"picked_buy_matured"`
	Groups           []ShadowGateGroup `json:"groups"`
	Notes            []string          `json:"notes"`
}

// shadowGateLabelCN gate_type 中文标签。
var shadowGateLabelCN = map[string]string{
	model.GateRegimeShadow:  "大盘闸门（影子）",
	model.GateBearShadow:    "反方研究员（影子）",
	model.GateQualityShadow: "数据质量门控（影子）",
	model.GateCorrelation:   "相关性去重",
	model.GateIndustryCap:   "行业名额上限",
}

// shadowGateOrder 报表分组展示顺序（有事件才出组）。
var shadowGateOrder = []string{
	model.GateRegimeShadow, model.GateBearShadow, model.GateQualityShadow,
	model.GateCorrelation, model.GateIndustryCap,
}

// RecShadowReport 生成影子对照报表。recType 可空（全部）；horizon ∈ LabelHorizons。
func RecShadowReport(userID int64, recType string, horizon int) (*ShadowReport, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	valid := false
	for _, h := range model.LabelHorizons {
		if h == horizon {
			valid = true
			break
		}
	}
	if !valid {
		return nil, fmt.Errorf("持有期须为 %v 之一", model.LabelHorizons)
	}

	// label_version 过滤与归因报表同款：新旧执行结算语义（l1 的 T+0 缺陷）不混池。
	q := common.DB.Where("user_id = ? AND horizon_days = ? AND entry_mode = ? AND label_version = ?",
		userID, horizon, model.EntryModeNextOpen, labelVersion)
	if recType == model.RecTypeShortTerm || recType == model.RecTypeLongTerm {
		q = q.Where("type = ?", recType)
	}
	var labels []model.RecommendationLabel
	if err := q.Find(&labels).Error; err != nil {
		return nil, err
	}
	var events []model.RecommendationCandidateEvent
	if err := common.DB.Where("user_id = ? AND gate_type <> ''", userID).
		Find(&events).Error; err != nil {
		return nil, err
	}

	key := func(batchID int64, symbol string) string { return fmt.Sprintf("%d|%s", batchID, symbol) }
	// 门控标记集合：gate_type → key 集合；anyGate 供对照组排除「被任何门控标记者」。
	// 一条事件可能命中多个门控（GateTypes 逗号全量，旧行回退单值 GateType）——
	// 每个命中门控都各自归组，任何门控不因合并折叠丢失样本。
	gatedBy := map[string]map[string]model.RecommendationCandidateEvent{}
	anyGate := map[string]bool{}
	for _, ev := range events {
		k := key(ev.BatchID, ev.Symbol)
		types := strings.Split(ev.GateTypes, ",")
		if ev.GateTypes == "" {
			types = []string{ev.GateType}
		}
		for _, gt := range types {
			gt = strings.TrimSpace(gt)
			if gt == "" {
				continue
			}
			if gatedBy[gt] == nil {
				gatedBy[gt] = map[string]model.RecommendationCandidateEvent{}
			}
			gatedBy[gt][k] = ev
		}
		anyGate[k] = true
	}

	rep := &ShadowReport{Type: recType, HorizonDays: horizon}
	var ungatedMatured []model.RecommendationLabel
	for _, l := range labels {
		k := key(l.BatchID, l.Symbol)
		if l.RecommendationID > 0 && l.Action == model.RecActionBuy {
			rep.PickedBuy++
			if l.MaturityStatus == model.LabelMatured {
				rep.PickedBuyMatured++
			}
		}
		// 对照组限定 buy：入选 watch 的成熟收益与「被门控标记的 buy」不可比（watch
		// 本就是更弱的信号），混入会让 gated vs ungated 的转正判断失真。
		if l.RecommendationID > 0 && l.Action == model.RecActionBuy &&
			!anyGate[k] && l.MaturityStatus == model.LabelMatured {
			ungatedMatured = append(ungatedMatured, l)
		}
	}
	ungatedCell := summarizeCell("shadow", "ungated", ungatedMatured)

	for _, gt := range shadowGateOrder {
		marks := gatedBy[gt]
		if len(marks) == 0 {
			continue
		}
		// 入选类影子门控（regime/bear/quality）标记的是 picked 条目：gated 侧同样
		// 限定 buy 与对照组同口径；名单裁剪类（correlation/industry_cap）标记的是
		// 未入选者（影子标签 action=shadow），无动作概念不过滤。
		pickedGate := gt == model.GateRegimeShadow || gt == model.GateBearShadow || gt == model.GateQualityShadow
		grp := ShadowGateGroup{GateType: gt, GateLabel: shadowGateLabelCN[gt], Ungated: ungatedCell}
		var gatedMatured []model.RecommendationLabel
		for _, l := range labels {
			ev, ok := marks[key(l.BatchID, l.Symbol)]
			if !ok {
				continue
			}
			if pickedGate && l.Action != model.RecActionBuy {
				continue // watch 被门控标记不进对照口径（分母分子一致排除）
			}
			grp.Marked++
			// would_rewrite=若强制执行会失去的 buy：只有会改写动作的门控（regime/bear)
			// 才计——quality 只封顶置信度，同一事件携带的 would_be_action 属于主门控，
			// 不得记到 quality 组头上。
			if ev.WouldBeAction != "" && (gt == model.GateRegimeShadow || gt == model.GateBearShadow) {
				grp.WouldRewrite++
			}
			if l.MaturityStatus == model.LabelMatured {
				gatedMatured = append(gatedMatured, l)
			}
		}
		if grp.Marked == 0 {
			continue // 该门控的标记都不在本报表口径（类型/持有期/动作）内
		}
		grp.Gated = summarizeCell("shadow", "gated", gatedMatured)
		rep.Groups = append(rep.Groups, grp)
	}

	rep.Notes = append(rep.Notes,
		"口径：统一执行模拟标签（next_open）；gated 与对照组统一限定 buy（入选类门控），名单阶段被挤出者（相关性/行业）吃影子标签；对照组=未被任何门控标记的入选 buy",
		"correlation/industry_cap 组的影子侧为无止盈止损的固定持有，对照侧为带障碍的入选 buy，该两组对照同时反映选股与退出策略差异，仅作方向参考",
		"覆盖率语义：would_rewrite=若强制执行会失去的 buy 数（只计会改写动作的 regime/反方门控；质量门控只封顶置信度恒 0）；picked_buy 为分母",
		"同一标的命中多个门控时各门控分别归组（事件行 gate_types 全量），样本可在多组重复出现——组间不可加总",
		"防回归纪律：影子门控转正凭本报表 gated vs ungated 配对评审——覆盖率下降但收益不改善即回退；评价协议须预注册，不允许看完结果再换阈值",
		fmt.Sprintf("单组成熟样本 <%d 时统计不稳定，仅供方向参考", attributionMinBucket),
		fmt.Sprintf("仅统计当前执行结算语义（label_version=%s）的标签，旧口径样本不混入", labelVersion),
	)
	return rep, nil
}
