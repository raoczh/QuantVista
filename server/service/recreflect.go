package service

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"gorm.io/gorm"
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
	reflectionVersion = "rf2"
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
	ctx = jobSubmissionContext(ctx)
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	if !setting.LLMReflectionShadow() {
		return 0, nil
	}
	// 门槛和候选共享同一读取时点；强平估价不充当完整结算样本。
	var matured int64
	var cands []reflectionCandidate
	asOf := time.Now()
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		if err := reflectionMaturedQuery(tx, asOf).Count(&matured).Error; err != nil {
			return err
		}
		if matured < reflectionMinMatured {
			return nil
		}
		var err error
		cands, err = loadReflectionCandidatesDB(tx, asOf, reflectionBatchMax)
		return err
	})
	if err != nil || len(cands) == 0 {
		return 0, err
	}

	// 系统默认 LLM（resolveNewsLLM：管理后台指定回退配置优先，否则首个管理员默认配置）。
	// 反思与新闻情绪同为系统后台任务：token 记配置所有者审计、不扣次数配额。
	cfg, apiKey, adminID, err := resolveNewsLLM(ctx)
	if err != nil {
		common.SysWarn("反思生成跳过：%v", err)
		return 0, nil
	}

	items, usage, err := callReflectionLLM(ctx, adminID, cfg, apiKey, cands)
	if usage.TotalTokens > 0 {
		consumeQuota(adminID, usage.TotalTokens)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil {
		common.SysWarn("反思生成 LLM 失败（下轮再试）：%v", err)
		return 0, nil
	}

	saved, err := saveRecommendationReflections(ctx, cands, items)
	if err != nil {
		return 0, err
	}
	if saved > 0 {
		common.SysLog("推荐反思生成完成：本轮 %d 条（累计成熟标签 %d）", saved, matured)
	}
	return saved, nil
}

// 网络请求结束后锁定并重验实际使用过的推荐和成熟事实，来源改变则下轮重算。
func saveRecommendationReflections(ctx context.Context, cands []reflectionCandidate, items map[int]string) (int, error) {
	indices := make([]int, 0, len(items))
	for idx, lesson := range items {
		if idx >= 0 && idx < len(cands) && strings.TrimSpace(lesson) != "" {
			indices = append(indices, idx)
		}
	}
	// 多执行器统一按推荐主键取锁，避免相反候选顺序造成锁冲突。
	sort.Slice(indices, func(i, j int) bool { return cands[indices[i]].rec.ID < cands[indices[j]].rec.ID })
	saved := 0
	err := withJobResultTransaction(ctx, func(tx *gorm.DB) error {
		for _, idx := range indices {
			c := cands[idx]
			var rec model.Recommendation
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&rec, c.rec.ID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			rec.CreatedAt, c.rec.CreatedAt = rec.CreatedAt.UTC(), c.rec.CreatedAt.UTC()
			if rec != c.rec {
				continue
			}
			var label model.RecommendationLabel
			if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&label, c.label.ID).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					continue
				}
				return err
			}
			if !sameReflectionLabel(label, c.label) {
				continue
			}
			now := time.Now()
			digest, err := json.Marshal(map[string]any{
				"strategy": c.label.Strategy, "source": c.label.Source, "regime": c.label.Regime,
				"industry": c.label.Industry, "entry_chg_5d_pct": c.label.EntryChg5dPct,
				"entry_turnover": c.label.EntryTurnover, "entry_score": c.label.EntryScore,
				"has_bench": c.label.HasBench,
			})
			if err != nil {
				return err
			}
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
				Lesson:           truncateRunes(strings.TrimSpace(items[idx]), reflectionLessonMax),
				FactorDigest:     string(digest),
				// LabelMaturedAt=标签结算时刻；AvailableFrom=教训生成时刻（防回放泄漏的
				// 注入下界——教训在生成之前不存在，回放注入早于此即未来泄漏）。
				LabelMaturedAt:    c.label.UpdatedAt,
				AvailableFrom:     now,
				ReflectionVersion: reflectionVersion,
			}
			if !c.label.HasBench {
				row.AlphaPct = 0
			}
			// 唯一键 (recommendation_id, horizon_days) 冲突忽略：候选查询已排除已反思行，
			// 冲突只可能来自并发重入，DoNothing 保幂等。
			res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected > 0 {
				saved++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return saved, nil
}

func sameReflectionLabel(a, b model.RecommendationLabel) bool {
	a.SignalAsOf, b.SignalAsOf = a.SignalAsOf.UTC(), b.SignalAsOf.UTC()
	a.CreatedAt, b.CreatedAt = a.CreatedAt.UTC(), b.CreatedAt.UTC()
	a.UpdatedAt, b.UpdatedAt = a.UpdatedAt.UTC(), b.UpdatedAt.UTC()
	return a == b
}

// callReflectionLLM 批量一次 LLM 反思调用。返回 idx→lesson（程序校验 idx，越界丢弃在
// 调用方）；错误=本轮放弃（候选行未消耗，下轮重挑）。
func callReflectionLLM(ctx context.Context, userID int64, cfg *model.LLMConfig, apiKey string, cands []reflectionCandidate) (map[int]string, chatUsage, error) {
	var usage chatUsage
	rows := make([]map[string]any, 0, len(cands))
	for i, c := range cands {
		var alpha any
		if c.label.HasBench {
			alpha = c.label.AlphaPct
		}
		rows = append(rows, map[string]any{
			"idx": i, "symbol": c.label.Symbol, "name": c.rec.Name,
			"action": c.label.Action, "strategy": c.label.Strategy, "rec_type": c.label.Type,
			"reason_then": c.rec.Summary, // 推荐当时的首条理由（生成时点固化）
			"outcome": map[string]any{
				"horizon_days": c.label.HorizonDays, "net_return_pct": c.label.NetReturnPct,
				"alpha_pct": alpha, "has_bench": c.label.HasBench, "mfe_pct": c.label.MfePct, "mae_pct": c.label.MaePct,
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
			Idx    *int   `json:"idx"`
			Lesson string `json:"lesson"`
		} `json:"reflections"`
	}
	var lastErr error
	repairLimit := moduleRepairAttempts("reflection")
	requestMax := moduleTokenCap("reflection", cfg.MaxTokens)
	for attempt := 0; attempt <= repairLimit; attempt++ {
		res, err := chatCompletion(ctx, chatParams{
			BaseURL: cfg.BaseURL, APIKey: apiKey, Model: cfg.Model, EndpointType: cfg.EndpointType,
			ReasoningEffort: cfg.ReasoningEffort,
			Temperature:     cfg.Temperature, MaxTokens: requestMax,
			Messages: convo, JSONMode: true, AllowPrivate: llmAllowPrivate(false, cfg),
			Repair: attempt > 0,
			Meta:   run.chatMeta(userID, cfg, attempt+1),
		})
		run.record(res, err)
		if res != nil {
			addChatUsage(&usage, res.Usage)
		}
		if err != nil {
			if attempt < repairLimit && isTokenLimitFinishState(run.FinishState) {
				requestMax = moduleRepairTokenCap("reflection", requestMax)
				convo = appendModuleRepairMessages(convo, "reflection", chatResultContent(res), run.FinishState,
					`上一条输出因 token 上限被截断。请从头完整输出 JSON：{"reflections":[{"idx":0,"lesson":"..."}]}。`)
				continue
			}
			return nil, usage, err
		}

		var out reflOut
		if jerr := json.Unmarshal([]byte(extractJSONObject(res.Content)), &out); jerr == nil && len(out.Reflections) > 0 {
			items := make(map[int]string, len(out.Reflections))
			seen := make(map[int]bool, len(out.Reflections))
			for _, r := range out.Reflections {
				if r.Idx == nil || *r.Idx < 0 || *r.Idx >= len(cands) {
					continue
				}
				idx := *r.Idx
				if seen[idx] {
					delete(items, idx) // 重复关联有歧义，该序号整组丢弃。
					continue
				}
				seen[idx] = true
				if strings.TrimSpace(r.Lesson) != "" {
					items[idx] = r.Lesson
				}
			}
			if len(items) > 0 {
				return items, usage, nil
			}
		}
		lastErr = errors.New("反思输出解析失败")
		convo = appendModuleRepairMessages(convo, "reflection", res.Content, run.FinishState,
			`上一条输出不合格。请只输出 JSON：{"reflections":[{"idx":0,"lesson":"..."}]}，idx 为输入序号。`)
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
	// Layers P2-3 分层可观测（rf2）：各层命中/候选计数、被名额裁剪数与按需历史统计——
	// 「检索到什么、裁掉多少」可核查。llm_layered_context 关闭时缺席（回退 rf1 形态）。
	Layers *reflectionLayers `json:"layers,omitempty"`
}

// reflectionShadowVersion 影子检索快照版本（与生成版本 reflectionVersion 自 P2-3 起
// 拆分：检索快照 rf2=分层可观测；改检索/分层结构递增此常量）。
const reflectionShadowVersion = "rf2"

// reflectionLayers 影子检索分层元数据（Tier1=同标的教训、Tier2=同策略教训、
// Tier3=同策略全历史结局聚合统计——逐条注入有名额上限，聚合概览无名额压力）。
type reflectionLayers struct {
	Tier1Count int `json:"tier1_count"` // matched 中 symbol 命中数
	Tier2Count int `json:"tier2_count"` // matched 中 strategy 命中数
	// CandidatesTotal 符合条件（本人+available_from+symbol∪strategy 池）的候选总数；
	// TrimmedCount=候选超出名额被裁数（被裁剪可见——「还有多少教训没进快照」）。
	CandidatesTotal int                   `json:"candidates_total"`
	TrimmedCount    int                   `json:"trimmed_count"`
	Tier3Stats      *reflectionTier3Stats `json:"tier3_stats,omitempty"`
	ApproxChars     int                   `json:"approx_chars"` // matched 教训文本总字符（快照体积观察）
}

// reflectionTier3Stats 按需历史层：同策略全历史反思的结局分布（聚合统计非逐条）。
type reflectionTier3Stats struct {
	Total        int     `json:"total"`
	Wins         int     `json:"wins"`
	Losses       int     `json:"losses"`
	TakeProfit   int     `json:"take_profit"`
	StopLoss     int     `json:"stop_loss"`
	AvgReturnPct float64 `json:"avg_return_pct"`
}

// reflectionLayerStats 计算分层元数据（纯查询；恒带 userID 与 available_from 过滤——
// 用户隔离与防回放泄漏铁律对分层统计同样生效，严禁移除）。
func reflectionLayerStats(db *gorm.DB, asOf time.Time, userID int64, recType, strategy string, symbols []string, matched []reflectionMatch) (*reflectionLayers, error) {
	l := &reflectionLayers{}
	for _, m := range matched {
		switch m.MatchedBy {
		case "symbol":
			l.Tier1Count++
		case "strategy":
			l.Tier2Count++
		}
		l.ApproxChars += len([]rune(m.Lesson))
	}
	var total int64
	if err := db.Model(&model.RecommendationReflection{}).
		Where("user_id = ? AND available_from <= ? AND (symbol IN ? OR (rec_type = ? AND strategy = ?))",
			userID, asOf, symbols, recType, strategy).
		Count(&total).Error; err != nil {
		return nil, err
	}
	l.CandidatesTotal = int(total)
	if trimmed := l.CandidatesTotal - len(matched); trimmed > 0 {
		l.TrimmedCount = trimmed
	}
	var stats reflectionTier3Stats
	if err := db.Model(&model.RecommendationReflection{}).
		Select(`COUNT(*) AS total,
			COALESCE(SUM(CASE WHEN outcome = 'win' THEN 1 ELSE 0 END), 0) AS wins,
			COALESCE(SUM(CASE WHEN outcome = 'loss' THEN 1 ELSE 0 END), 0) AS losses,
			COALESCE(SUM(CASE WHEN outcome = 'take_profit' THEN 1 ELSE 0 END), 0) AS take_profit,
			COALESCE(SUM(CASE WHEN outcome = 'stop_loss' THEN 1 ELSE 0 END), 0) AS stop_loss,
			COALESCE(AVG(return_pct), 0) AS avg_return_pct`).
		Where("user_id = ? AND available_from <= ? AND rec_type = ? AND strategy = ?", userID, asOf, recType, strategy).
		Scan(&stats).Error; err != nil {
		return nil, err
	}
	if stats.Total > 0 {
		stats.AvgReturnPct = round2(stats.AvgReturnPct)
		l.Tier3Stats = &stats
	}
	return l, nil
}

// reflectionShadowJSON 推荐生成时的影子检索快照（runGeneration 调用，best-effort）：
// flag 关/无匹配返回空串（批次列保持空，前端零噪声）。**只读不写、不进 prompt、
// 不碰 picks**——影子纪律的代码形态就是「返回值只赋给 batch.ReflectionJSON」。
// P2-3：llm_layered_context 开时快照升 rf2（补分层元数据与 Tier3 聚合统计），关时
// 保持 rf1 形态——两 flag 正交（reflection_shadow 控检索与否、layered_context 控分层）。
func reflectionShadowJSON(userID int64, recType, strategy string, llmCands []candidate, contexts ...context.Context) string {
	if common.DB == nil || !setting.LLMReflectionShadow() {
		return ""
	}
	symbols := make([]string, 0, len(llmCands))
	for _, c := range llmCands {
		symbols = append(symbols, c.Symbol)
	}
	snap := reflectionShadowSnapshot{
		Version: "rf1", CheckedAt: time.Now(),
		Note: "影子层：历史教训仅记录未注入 prompt，不影响本批推荐结果；注入转正需影子配对评审",
	}
	layered := setting.LLMLayeredContext()
	err := readSnapshotTx(jobSubmissionContext(contexts...), func(tx *gorm.DB) error {
		var err error
		snap.Matched, err = lookupReflectionsDB(tx, snap.CheckedAt, userID, recType, strategy, symbols)
		if err != nil || len(snap.Matched) == 0 {
			return err
		}
		if layered {
			snap.Version = reflectionShadowVersion
			snap.Layers, err = reflectionLayerStats(tx, snap.CheckedAt, userID, recType, strategy, symbols, snap.Matched)
		}
		return err
	})
	if err != nil {
		common.SysWarn("反思影子读取失败 user=%d: %v", userID, err)
		return ""
	}
	if len(snap.Matched) == 0 {
		return ""
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return ""
	}
	return string(b)
}

// lookupReflections 检索适用教训（纯查询，可测）：available_from <= asOf 是防回放泄漏
// 铁律；同标的教训优先，名额未满再补同策略教训（symbol 命中语义更强）。
// userID 过滤是用户隔离铁律（审查修复批）：反思行携带来源用户的标的/收益结局/教训文本，
// 跨用户返回=把 A 的持仓线索泄漏进 B 的批次快照并在前端展示——严禁移除该条件。
func lookupReflections(asOf time.Time, userID int64, recType, strategy string, symbols []string, contexts ...context.Context) []reflectionMatch {
	if common.DB == nil || len(symbols) == 0 {
		return nil
	}
	var out []reflectionMatch
	err := readSnapshotTx(jobSubmissionContext(contexts...), func(tx *gorm.DB) error {
		var err error
		out, err = lookupReflectionsDB(tx, asOf, userID, recType, strategy, symbols)
		return err
	})
	if err != nil {
		common.SysWarn("反思教训读取失败 user=%d: %v", userID, err)
		return nil
	}
	return out
}

func lookupReflectionsDB(db *gorm.DB, asOf time.Time, userID int64, recType, strategy string, symbols []string) ([]reflectionMatch, error) {
	if len(symbols) == 0 {
		return nil, nil
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
	// id DESC tiebreaker 不许删：同一轮生成的反思共用同一个 now 作 available_from
	//（reflectionBatchMax=5 条一次落库），时间戳完全相同；无 tiebreaker 时候选超过名额
	// 选中哪几条退化为 SQLite 物理页序，影子快照不可复现。同 loadReflectionCandidates
	// 的 id ASC 先例（那处曾导致测试 flaky）。
	if err := db.Where("user_id = ? AND available_from <= ? AND symbol IN ?", userID, asOf, symbols).
		Order("available_from DESC, id DESC").Limit(reflectionShadowMax).Find(&bySymbol).Error; err != nil {
		return nil, err
	}
	appendRows(bySymbol, "symbol")
	if len(out) < reflectionShadowMax {
		var byStrategy []model.RecommendationReflection
		if err := db.Where("user_id = ? AND available_from <= ? AND rec_type = ? AND strategy = ?", userID, asOf, recType, strategy).
			Order("available_from DESC, id DESC").Limit(reflectionShadowMax).Find(&byStrategy).Error; err != nil {
			return nil, err
		}
		appendRows(byStrategy, "strategy")
	}
	return out, nil
}
