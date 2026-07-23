package service

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm/clause"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P1-5 反思记忆影子层（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.2；表结构 S2-1 已建，
// RECOMMENDATION_ACCURACY_PLAN §5 S2-1 声明的启用门槛=成熟标签 ≥30 条在本文件落地）。
//
// 两半各自独立：
//   1. 反思生成（GenerateRecommendationReflections）：tracking job 在标签结算之后调用——
//      对新成熟的推荐标签（代表持有期：短线 10 / 长线 20 交易日）做一次轻量 LLM 反思
//      （固定三问：方向对不对·论点哪部分成立或失败·一条可迁移教训），落
//      recommendation_reflections。LLM 挂系统默认配置（resolveNewsLLM 同款语义：后台
//      任务非用户动作，token 记配置所有者审计、不扣次数配额）。
//   2. 影子检索（reflectionShadowJSON）：推荐生成时按 LLM 名单标的/策略检索适用教训，
//      快照落批次 ReflectionJSON——**三不纪律（regime/bear/quality 影子同款）**：
//      不注入 prompt（buildMessages 零改动）、不改写 action/置信度、拒选与降级批次也落。
//      注入转正必须凭「注入前后批次影子配对」评审（model/reflection.go 头注释），
//      不允许看表面效果拍脑袋启用。
//
// 防泄漏铁律：检索只返回 available_from <= 检索时点 的教训（AvailableFrom=反思生成时刻）。
// 线上实时链路该条件恒真，但它是历史回放（walk-forward/backtest 日后消费反思时）不把
// 未来结算结果带回过去的唯一防线——**严禁移除该过滤**（TestReflectionAvailableFromFilter 锁定）。
//
// flag `llm_reflection_shadow`（缺省开）：关闭停止生成与检索；已落库反思保留，重开即恢复。

const (
	// reflectionVersion 反思生成版本（prompt/三问结构/输入摘要口径变更时递增）。
	reflectionVersion = "rf1"
	// reflectionMinMatured 启用门槛：全库成熟标签（l2/next_open/真实推荐）不足此数不生成
	//（S2-1 排序原则「确定性统计先行」——样本太少时 LLM 教训是噪声放大器）。
	reflectionMinMatured = 30
	// reflectionBatchMax 每轮反思条数上限（2h 一轮，控 LLM 成本；积压由后续轮次消化）。
	reflectionBatchMax = 5
	// reflectionShadowMax 影子检索返回条数上限。
	reflectionShadowMax = 5
	// reflectionLessonMax 教训文本截断（rune）。
	reflectionLessonMax = 300
)

// 代表持有期（walk-forward 月度走查同款口径）：每条推荐只反思一个 horizon，
// 控制成本与教训密度（5 horizon 全反思会 ×5 膨胀且教训高度重复）。
func reflectionHorizonFor(recType string) int {
	if recType == model.RecTypeLongTerm {
		return 20
	}
	return 10
}

// reflectionOutcome 程序归类结算结局（LLM 只写教训，不判定输赢——可确定性计算的
// 不交给模型）：障碍先触分类优先，其余按净收益正负。
func reflectionOutcome(l model.RecommendationLabel) string {
	switch {
	case l.HitTakeProfit:
		return "take_profit"
	case l.HitStopLoss:
		return "stop_loss"
	case l.NetReturnPct > 0:
		return "win"
	default:
		return "loss"
	}
}

// reflectionSystemPrompt 反思生成系统提示：固定三问、短教训、禁复盘八股。
const reflectionSystemPrompt = `你是推荐结果的复盘反思器。对每条已结算的历史推荐，用 2-4 句中文回答固定三问并压成一条教训：
1. 方向对不对（结算结果相对推荐动作）；
2. 当时的论点哪部分成立、哪部分失败（对照给出的推荐理由与结算数据）；
3. 一条可迁移的教训（针对同类形态/策略下次可检查什么，不写「加强研究」类空话）。
铁律：只依据给出的数据，不得引入外部记忆或事后新闻解释涨跌；教训必须可操作可证伪。
只输出 JSON：{"reflections":[{"idx":0,"lesson":"教训文本"}]}，idx 为输入序号，逐条覆盖，不要任何解释或代码块标记。`

// reflectionCandidate 一条待反思的成熟标签及其推荐上下文（生成输入）。
type reflectionCandidate struct {
	label model.RecommendationLabel
	rec   model.Recommendation
}

// GenerateRecommendationReflections 反思生成主入口（tracking job 每 2h 调用，幂等）：
// 门槛校验 → 挑未反思的新成熟标签（代表持有期）→ 批量一次 LLM → 落库。
// best-effort：LLM 不可用/解析失败只记日志，下轮再试（无未消化状态残留）。
func GenerateRecommendationReflections(ctx context.Context) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if !setting.LLMReflectionShadow() {
		return 0, nil
	}
	// 启用门槛：成熟标签（l2 / next_open / 真实推荐）总量 ≥30。
	var matured int64
	if err := common.DB.Model(&model.RecommendationLabel{}).
		Where("maturity_status = ? AND entry_mode = ? AND recommendation_id > 0 AND label_version = ?",
			model.LabelMatured, model.EntryModeNextOpen, labelVersion).
		Count(&matured).Error; err != nil {
		return 0, err
	}
	if matured < reflectionMinMatured {
		return 0, nil
	}

	cands, err := loadReflectionCandidates(reflectionBatchMax)
	if err != nil || len(cands) == 0 {
		return 0, err
	}

	// 系统默认 LLM（resolveNewsLLM：管理后台指定回退配置优先，否则首个管理员默认配置）。
	// 反思与新闻情绪同为系统后台任务：token 记配置所有者审计、不扣次数配额。
	cfg, apiKey, adminID, err := resolveNewsLLM()
	if err != nil {
		common.SysWarn("反思生成跳过：%v", err)
		return 0, nil
	}

	items, usage, err := callReflectionLLM(ctx, adminID, cfg, apiKey, cands)
	if usage.TotalTokens > 0 {
		consumeQuota(adminID, usage.TotalTokens, false)
	}
	if err != nil {
		common.SysWarn("反思生成 LLM 失败（下轮再试）：%v", err)
		return 0, nil
	}

	now := time.Now()
	saved := 0
	for idx, lesson := range items {
		if idx < 0 || idx >= len(cands) {
			continue // idx 越界=模型伪造关联，丢弃
		}
		c := cands[idx]
		digest, _ := json.Marshal(map[string]any{
			"strategy": c.label.Strategy, "source": c.label.Source, "regime": c.label.Regime,
			"industry": c.label.Industry, "entry_chg_5d_pct": c.label.EntryChg5dPct,
			"entry_turnover": c.label.EntryTurnover, "entry_score": c.label.EntryScore,
		})
		row := model.RecommendationReflection{
			RecommendationID: c.label.RecommendationID,
			HorizonDays:      c.label.HorizonDays,
			UserID:           c.label.UserID,
			Symbol:           c.label.Symbol,
			Strategy:         c.label.Strategy,
			RecType:          c.label.Type,
			Outcome:          reflectionOutcome(c.label),
			ReturnPct:        c.label.NetReturnPct,
			AlphaPct:         c.label.AlphaPct,
			Lesson:           truncateRunes(strings.TrimSpace(lesson), reflectionLessonMax),
			FactorDigest:     string(digest),
			// LabelMaturedAt=标签结算时刻；AvailableFrom=教训生成时刻（防回放泄漏的
			// 注入下界——教训在生成之前不存在，回放注入早于此即未来泄漏）。
			LabelMaturedAt:    c.label.UpdatedAt,
			AvailableFrom:     now,
			ReflectionVersion: reflectionVersion,
		}
		if row.Lesson == "" {
			continue
		}
		// 唯一键 (recommendation_id, horizon_days) 冲突忽略：候选查询已排除已反思行，
		// 冲突只可能来自并发重入，DoNothing 保幂等。
		res := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
		if res.Error != nil {
			common.SysWarn("反思落库失败 rec=%d h=%d: %v", row.RecommendationID, row.HorizonDays, res.Error)
			continue
		}
		if res.RowsAffected > 0 {
			saved++
		}
	}
	if saved > 0 {
		common.SysLog("推荐反思生成完成：本轮 %d 条（累计成熟标签 %d）", saved, matured)
	}
	return saved, nil
}

// loadReflectionCandidates 挑未反思的成熟标签：l2 / next_open / 真实推荐 / 代表持有期，
// 按结算时间倒序（最新成熟优先），LEFT JOIN 排除已反思行。
func loadReflectionCandidates(limit int) ([]reflectionCandidate, error) {
	var labels []model.RecommendationLabel
	if err := common.DB.
		Where("maturity_status = ? AND entry_mode = ? AND recommendation_id > 0 AND label_version = ?",
			model.LabelMatured, model.EntryModeNextOpen, labelVersion).
		Where("(type = ? AND horizon_days = 10) OR (type = ? AND horizon_days = 20)",
			model.RecTypeShortTerm, model.RecTypeLongTerm).
		Where("NOT EXISTS (SELECT 1 FROM recommendation_reflections rr WHERE rr.recommendation_id = recommendation_labels.recommendation_id AND rr.horizon_days = recommendation_labels.horizon_days)").
		// 孤儿标签排除（审查修复批）：推荐条目被用户删除后标签仍在（标签是测量事实
		// 不级联删），若只在取出后逐条跳过，「最新 5 条全是孤儿」会让每轮都重复选中
		// 它们，更早的有效候选永远排不上队——必须在 SQL 侧排除孤儿行。
		Where("EXISTS (SELECT 1 FROM recommendations r WHERE r.id = recommendation_labels.recommendation_id)").
		// id ASC 是确定性 tiebreaker：同一轮结算的标签 updated_at 常落同一毫秒，
		// 无 tiebreaker 时顺序退化为物理页序（不可复现，测试也曾因此 flaky）。
		Order("updated_at DESC, id ASC").Limit(limit).
		Find(&labels).Error; err != nil {
		return nil, err
	}
	out := make([]reflectionCandidate, 0, len(labels))
	for _, l := range labels {
		var rec model.Recommendation
		if err := common.DB.First(&rec, l.RecommendationID).Error; err != nil {
			continue // 推荐条目已删：无理由上下文，跳过不反思
		}
		out = append(out, reflectionCandidate{label: l, rec: rec})
	}
	return out, nil
}

// callReflectionLLM 批量一次 LLM 反思调用。返回 idx→lesson（程序校验 idx，越界丢弃在
// 调用方）；错误=本轮放弃（候选行未消耗，下轮重挑）。
func callReflectionLLM(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, cands []reflectionCandidate) (map[int]string, chatUsage, error) {
	var usage chatUsage
	rows := make([]map[string]any, 0, len(cands))
	for i, c := range cands {
		rows = append(rows, map[string]any{
			"idx": i, "symbol": c.label.Symbol, "name": c.rec.Name,
			"action": c.label.Action, "strategy": c.label.Strategy, "rec_type": c.label.Type,
			"reason_then": c.rec.Summary, // 推荐当时的首条理由（生成时点固化）
			"outcome": map[string]any{
				"horizon_days": c.label.HorizonDays, "net_return_pct": c.label.NetReturnPct,
				"alpha_pct": c.label.AlphaPct, "mfe_pct": c.label.MfePct, "mae_pct": c.label.MaePct,
				"hit_take_profit": c.label.HitTakeProfit, "hit_stop_loss": c.label.HitStopLoss,
				"result": reflectionOutcome(c.label),
			},
			"entry_context": map[string]any{
				"regime": c.label.Regime, "entry_chg_5d_pct": c.label.EntryChg5dPct,
				"entry_turnover": c.label.EntryTurnover, "entry_score": c.label.EntryScore,
			},
		})
	}
	inputJSON, err := json.Marshal(rows)
	if err != nil {
		return nil, usage, err
	}

	convo := []chatMessage{
		{Role: "system", Content: reflectionSystemPrompt},
		{Role: "user", Content: "已结算的历史推荐如下（JSON 数组，reason_then 为推荐当时的理由，outcome 为统一执行模拟的结算结果）：\n" + string(inputJSON)},
	}
	run := newLLMRun(newLLMTraceID(), "", "reflection", "reflection.v1", reflectionVersion)
	run.hashData(string(inputJSON))
	run.hashPrompt(convo)
	type reflOut struct {
		Reflections []struct {
			Idx    int    `json:"idx"`
			Lesson string `json:"lesson"`
		} `json:"reflections"`
	}
	var lastErr error
	for attempt := 0; attempt <= moduleRepairAttempts("reflection"); attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			Temperature: cfg.Temperature, MaxTokens: moduleTokenCap("reflection", cfg.MaxTokens),
			Messages: convo, JSONMode: true, AllowPrivate: llmAllowPrivate(false, cfg),
			Repair: attempt > 0,
			Meta:   run.chatMeta(userID, cfg, attempt+1),
		})
		run.record(res, err)
		if err != nil {
			if res != nil {
				usage.PromptTokens += res.Usage.PromptTokens
				usage.CompletionTokens += res.Usage.CompletionTokens
				usage.TotalTokens += res.Usage.TotalTokens
			}
			return nil, usage, err
		}
		usage.PromptTokens += res.Usage.PromptTokens
		usage.CompletionTokens += res.Usage.CompletionTokens
		usage.TotalTokens += res.Usage.TotalTokens

		var out reflOut
		if jerr := json.Unmarshal([]byte(extractJSONObject(res.Content)), &out); jerr == nil && len(out.Reflections) > 0 {
			items := make(map[int]string, len(out.Reflections))
			for _, r := range out.Reflections {
				if strings.TrimSpace(r.Lesson) == "" {
					continue
				}
				items[r.Idx] = r.Lesson
			}
			if len(items) > 0 {
				return items, usage, nil
			}
		}
		lastErr = errors.New("反思输出解析失败")
		convo = append(convo,
			chatMessage{Role: "assistant", Content: moduleRepairFeed("reflection", res.Content)},
			chatMessage{Role: "user", Content: `上一条输出不合格。请只输出 JSON：{"reflections":[{"idx":0,"lesson":"..."}]}，idx 为输入序号。`},
		)
	}
	run.DegradedReason = "llm_output_invalid"
	return nil, usage, lastErr
}

// --- 影子检索（推荐生成侧） ---

// reflectionMatch 影子检索命中的一条历史教训（批次 ReflectionJSON.matched 元素）。
type reflectionMatch struct {
	ID            int64     `json:"id"`
	Symbol        string    `json:"symbol"`
	Strategy      string    `json:"strategy"`
	RecType       string    `json:"rec_type"`
	HorizonDays   int       `json:"horizon_days"`
	Outcome       string    `json:"outcome"`
	ReturnPct     float64   `json:"return_pct"`
	Lesson        string    `json:"lesson"`
	AvailableFrom time.Time `json:"available_from"`
	MatchedBy     string    `json:"matched_by"` // symbol=同标的教训 | strategy=同策略教训
}

// reflectionShadowSnapshot 批次 ReflectionJSON 的整体形态。
type reflectionShadowSnapshot struct {
	Version   string            `json:"version"`
	CheckedAt time.Time         `json:"checked_at"`
	Matched   []reflectionMatch `json:"matched"`
	Note      string            `json:"note"`
}

// lookupReflections 检索适用教训（纯查询，可测）：available_from <= asOf 是防回放泄漏
// 铁律；同标的教训优先，名额未满再补同策略教训（symbol 命中语义更强）。
// userID 过滤是用户隔离铁律（审查修复批）：反思行携带来源用户的标的/收益结局/教训文本，
// 跨用户返回=把 A 的持仓线索泄漏进 B 的批次快照并在前端展示——严禁移除该条件。
func lookupReflections(asOf time.Time, userID int64, recType, strategy string, symbols []string) []reflectionMatch {
	if common.DB == nil || len(symbols) == 0 {
		return nil
	}
	out := make([]reflectionMatch, 0, reflectionShadowMax)
	seen := map[int64]bool{}
	appendRows := func(rows []model.RecommendationReflection, matchedBy string) {
		for _, r := range rows {
			if seen[r.ID] || len(out) >= reflectionShadowMax {
				continue
			}
			seen[r.ID] = true
			out = append(out, reflectionMatch{
				ID: r.ID, Symbol: r.Symbol, Strategy: r.Strategy, RecType: r.RecType,
				HorizonDays: r.HorizonDays, Outcome: r.Outcome, ReturnPct: r.ReturnPct,
				Lesson: truncateRunes(r.Lesson, 120), AvailableFrom: r.AvailableFrom,
				MatchedBy: matchedBy,
			})
		}
	}
	var bySymbol []model.RecommendationReflection
	if err := common.DB.Where("user_id = ? AND available_from <= ? AND symbol IN ?", userID, asOf, symbols).
		Order("available_from DESC").Limit(reflectionShadowMax).Find(&bySymbol).Error; err == nil {
		appendRows(bySymbol, "symbol")
	}
	if len(out) < reflectionShadowMax {
		var byStrategy []model.RecommendationReflection
		if err := common.DB.Where("user_id = ? AND available_from <= ? AND rec_type = ? AND strategy = ?", userID, asOf, recType, strategy).
			Order("available_from DESC").Limit(reflectionShadowMax).Find(&byStrategy).Error; err == nil {
			appendRows(byStrategy, "strategy")
		}
	}
	return out
}

// reflectionShadowJSON 推荐生成时的影子检索快照（runGeneration 调用，best-effort）：
// flag 关/无匹配返回空串（批次列保持空，前端零噪声）。**只读不写、不进 prompt、
// 不碰 picks**——影子纪律的代码形态就是「返回值只赋给 batch.ReflectionJSON」。
func reflectionShadowJSON(userID int64, recType, strategy string, llmCands []candidate) string {
	if !setting.LLMReflectionShadow() {
		return ""
	}
	symbols := make([]string, 0, len(llmCands))
	for _, c := range llmCands {
		symbols = append(symbols, c.Symbol)
	}
	matched := lookupReflections(time.Now(), userID, recType, strategy, symbols)
	if len(matched) == 0 {
		return ""
	}
	snap := reflectionShadowSnapshot{
		Version: reflectionVersion, CheckedAt: time.Now(), Matched: matched,
		Note: "影子层：历史教训仅记录未注入 prompt，不影响本批推荐结果；注入转正需影子配对评审",
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	return string(b)
}
