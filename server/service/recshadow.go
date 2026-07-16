package service

import (
	"errors"
	"fmt"

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

	q := common.DB.Where("user_id = ? AND horizon_days = ? AND entry_mode = ?",
		userID, horizon, model.EntryModeNextOpen)
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
	gatedBy := map[string]map[string]model.RecommendationCandidateEvent{}
	anyGate := map[string]bool{}
	for _, ev := range events {
		k := key(ev.BatchID, ev.Symbol)
		if gatedBy[ev.GateType] == nil {
			gatedBy[ev.GateType] = map[string]model.RecommendationCandidateEvent{}
		}
		gatedBy[ev.GateType][k] = ev
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
		if l.RecommendationID > 0 && !anyGate[k] && l.MaturityStatus == model.LabelMatured {
			ungatedMatured = append(ungatedMatured, l)
		}
	}
	ungatedCell := summarizeCell("shadow", "ungated", ungatedMatured)

	for _, gt := range shadowGateOrder {
		marks := gatedBy[gt]
		if len(marks) == 0 {
			continue
		}
		grp := ShadowGateGroup{GateType: gt, GateLabel: shadowGateLabelCN[gt], Ungated: ungatedCell}
		var gatedMatured []model.RecommendationLabel
		for _, l := range labels {
			ev, ok := marks[key(l.BatchID, l.Symbol)]
			if !ok {
				continue
			}
			grp.Marked++
			if ev.WouldBeAction != "" {
				grp.WouldRewrite++
			}
			if l.MaturityStatus == model.LabelMatured {
				gatedMatured = append(gatedMatured, l)
			}
		}
		if grp.Marked == 0 {
			continue // 该门控的标记都不在本报表口径（类型/持有期）内
		}
		grp.Gated = summarizeCell("shadow", "gated", gatedMatured)
		rep.Groups = append(rep.Groups, grp)
	}

	rep.Notes = append(rep.Notes,
		"口径：统一执行模拟标签（next_open）；被标记标的含入选（regime/反方/质量影子）与名单阶段被挤出者（相关性/行业），对照组=未被任何门控标记的入选标的",
		"覆盖率语义：would_rewrite=若强制执行会失去的 buy 数（质量门控只封顶置信度不改动作，恒 0）；picked_buy 为分母",
		"防回归纪律：影子门控转正凭本报表 gated vs ungated 配对评审——覆盖率下降但收益不改善即回退；评价协议须预注册，不允许看完结果再换阈值",
		fmt.Sprintf("单组成熟样本 <%d 时统计不稳定，仅供方向参考", attributionMinBucket),
	)
	return rep, nil
}
