package service

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

// 推荐策略目录：内置推荐策略（momentum/pullback/active/value/growth/leader）之外，
// 选股页的全部策略（内置选股策略、新手模板、用户自建策略）同样可作为推荐策略——
// 用户在推荐工作台选中后，该选股策略的全市场命中成为候选池的主供给（strategy_signal
// 来源），prompt 以策略白话讲解作为选股导向，量化规则使用策略明确声明的评分配置。
//
// key 命名空间（落库 recommendation_batches.strategy，≤64 字符）：
//   - 内置推荐策略：momentum / value …（不变）
//   - 内置选股策略：screen:<builtin key>
//   - 新手模板：tpl:<template key>（默认参数）
//   - 用户自建策略：screen:u<id>（按当前 revision 解析；归档后历史批次仍可回显）
const (
	recStrategyScreenPrefix   = "screen:"
	recStrategyTemplatePrefix = "tpl:"
	recStrategyCustomPrefix   = "screen:u"

	// 选股类推荐策略的策略信号进池上限：该策略是用户明确指定的主供给，给足名额
	//（榜单来源仍并入，名额分配由 assignScanQuota 轮转）。
	screenStrategySignalPoolLimit = 60
)

// strategyTemplate 策略模板（推荐工作台下拉项 + 生成期编排参数）。
type strategyTemplate struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Desc string `json:"desc"`
	// Group 下拉分组：rec（推荐内置）/ screen（选股内置）/ template（新手模板）/ custom（我的策略）。
	Group string `json:"group,omitempty"`
	// Period/Risk 选股类策略的适用周期与风险等级（前端分组展示；推荐内置策略留空）。
	Period       string `json:"period,omitempty"`
	Risk         string `json:"risk,omitempty"`
	ScoreProfile string `json:"score_profile,omitempty"`
	Intent       string `json:"intent,omitempty"`
	// 自建策略目录携带不可变版本，提交、排队、扫描和历史展示共用此身份。
	StrategyRevisionID int64 `json:"strategy_revision_id,omitempty"`

	guide string // 注入 prompt 的选股导向（不外泄给前端）
	// baseKey 量化加分/榜单来源沿用的基础推荐策略 key（内置推荐策略 = 自身 Key；
	// 选股类策略使用注册表或不可变 revision 中的配置）。
	baseKey    string
	scoring    recScoringRuntime
	frozenScan *recommendationFrozenScreen
	// screen 非 nil = 选股类策略：strategy_signal 来源直接扫描该策略；tree 为其
	// 规范化条件树（候选逐只评估条件命中度，见 evaluateStrategyHit）。
	screen *recScreenBinding
	tree   *CondNode
}

// recScreenBinding 选股类推荐策略到选股引擎的绑定（ScanRequest 四选一）。
type recScreenBinding struct {
	builtinKey         string
	templateKey        string
	strategyID         int64
	strategyRevisionID int64
}

func (t *strategyTemplate) scanRequest(limit int) ScanRequest {
	req := ScanRequest{Limit: limit, preselectionProfile: t.baseKey}
	if t.screen == nil {
		return req
	}
	if t.frozenScan != nil {
		req.frozenRecommendation = t.frozenScan
		return req
	}
	req.StrategyKey = t.screen.builtinKey
	req.TemplateKey = t.screen.templateKey
	req.StrategyID = t.screen.strategyID
	req.StrategyRevisionID = t.screen.strategyRevisionID
	return req
}

var shortStrategies = []strategyTemplate{
	{Key: "momentum", Name: "动量突破", Desc: "顺势追强，关注突破与量价配合", Group: "rec", baseKey: "momentum",
		guide: "优先选择处于上升趋势、价格站上均线、近日放量突破关键位、动能强的标的；回避明显滞涨或量价背离者。"},
	{Key: "pullback", Name: "强势回踩", Desc: "强势股回调至支撑的低吸机会", Group: "rec", baseKey: "pullback",
		guide: "优先选择整体强势、近期健康回调至均线/前高支撑附近、缩量企稳的标的；给出更靠近支撑的买入观察区间。"},
	{Key: "active", Name: "热点活跃", Desc: "资金聚焦的高活跃标的", Group: "rec", baseKey: "active",
		guide: "优先选择成交额显著放大、市场关注度高、处于热点板块的活跃标的；严格设置止损以控回撤。"},
}

var longStrategies = []strategyTemplate{
	{Key: "value", Name: "价值低估", Desc: "偏防御，关注估值与稳健", Group: "rec", baseKey: "value",
		guide: "优先选择商业模式稳健、估值相对合理或偏低的标的，弱化短期涨幅；以中长期持有视角评估。"},
	{Key: "growth", Name: "成长趋势", Desc: "关注景气与成长持续性", Group: "rec", baseKey: "growth",
		guide: "优先选择处于景气赛道、成长趋势明确、中长期逻辑清晰的标的；说明关键的成长驱动与验证指标。"},
	{Key: "leader", Name: "龙头优选", Desc: "行业龙头与确定性", Group: "rec", baseKey: "leader",
		guide: "优先选择行业地位领先、确定性较高的龙头标的；强调竞争壁垒与长期跟踪要点。"},
}

// recBuiltinStrategies 某推荐类型的内置推荐策略。
func recBuiltinStrategies(recType string) []strategyTemplate {
	if recType == model.RecTypeShortTerm {
		return shortStrategies
	}
	return longStrategies
}

// screenBaseKey 使用内置策略显式声明的配置；周期仅保留调用兼容，不再猜测评分风格。
func screenBaseKey(recType, _ string, builtinKey string) string {
	if profile, ok := builtinRecommendationProfiles[builtinKey]; ok {
		return profile.scoreProfile(recType)
	}
	return "balanced"
}

// screenStrategyGuide 选股类策略注入 prompt 的导向：白话讲解 + 命中标记说明。
func screenStrategyGuide(name, desc string, conditions []string) string {
	var b strings.Builder
	b.WriteString("本次采用选股策略「" + name + "」。")
	if strings.TrimSpace(desc) != "" {
		b.WriteString("策略讲解：" + strings.TrimSpace(desc))
		if !strings.HasSuffix(desc, "。") {
			b.WriteString("。")
		}
	}
	if len(conditions) > 0 {
		b.WriteString("策略条件：" + strings.Join(conditions, "；") + "。")
	}
	b.WriteString("名单已按所选策略的完整收盘条件与当前价格约束复核；sources 含 strategy_signal 仅说明来自全市场策略扫描，不能代替具体条件证据。发现矛盾、缺口或不合理入场距离时，应在 rejected 或风险说明中明确指出。")
	return b.String()
}

func screenPeriodLabel(p string) string {
	switch p {
	case "short":
		return "短线"
	case "swing":
		return "波段"
	case "mid":
		return "中线"
	}
	return ""
}

func screenRiskLabel(r string) string {
	switch r {
	case "low":
		return "低风险"
	case "mid":
		return "中风险"
	case "high":
		return "高风险"
	}
	return ""
}

// screenStrategyDesc 下拉描述：周期·风险 + 白话讲解首句（完整讲解进 prompt）。
func screenStrategyDesc(period, risk, desc string) string {
	parts := make([]string, 0, 3)
	if l := screenPeriodLabel(period); l != "" {
		parts = append(parts, l)
	}
	if l := screenRiskLabel(risk); l != "" {
		parts = append(parts, l)
	}
	head := strings.TrimSpace(desc)
	if i := strings.IndexAny(head, "。；;"); i >= 0 {
		head = head[:i]
	}
	if head != "" {
		parts = append(parts, truncateRunes(head, 40))
	}
	return strings.Join(parts, " · ")
}

// builtinScreenStrategyTemplate 内置选股策略 → 推荐策略模板。
func builtinScreenStrategyTemplate(recType string, b builtinScreen) strategyTemplate {
	tree, _, _ := canonicalCondTree(&b.Tree)
	profile := builtinRecommendationProfiles[b.Key]
	base := screenBaseKey(recType, b.Period, b.Key)
	return strategyTemplate{
		Key: recStrategyScreenPrefix + b.Key, Name: b.Name,
		Desc: screenStrategyDesc(b.Period, b.Risk, b.Desc), Group: "screen",
		Period: b.Period, Risk: b.Risk,
		ScoreProfile: base, Intent: profile.intent,
		guide:   screenStrategyGuide(b.Name, b.Desc, describeCondTree(tree)),
		baseKey: base,
		screen:  &recScreenBinding{builtinKey: b.Key},
		tree:    tree,
	}
}

// retailTemplateStrategyTemplate 新手模板（默认参数）→ 推荐策略模板。
func retailTemplateStrategyTemplate(recType string, t retailTemplate) strategyTemplate {
	defaults := make(map[string]float64, len(t.Params))
	for _, p := range t.Params {
		defaults[p.Key] = p.Default
	}
	built := t.build(defaults)
	tree, _, _ := canonicalCondTree(&built)
	desc := t.Scenario
	profile := retailRecommendationProfiles[t.Key]
	base := profile.scoreProfile(recType)
	return strategyTemplate{
		Key: recStrategyTemplatePrefix + t.Key, Name: t.Name,
		Desc: screenStrategyDesc(t.Period, t.RiskLevel, desc), Group: "template",
		Period: t.Period, Risk: t.RiskLevel,
		ScoreProfile: base, Intent: profile.intent,
		guide:   screenStrategyGuide(t.Name, desc+" 风险提示："+t.Risk, describeCondTree(tree)),
		baseKey: base,
		screen:  &recScreenBinding{templateKey: t.Key},
		tree:    tree,
	}
}

// customScreenStrategyTemplate 用户自建策略（当前 revision）→ 推荐策略模板。
func customScreenStrategyTemplate(recType string, id int64, rev model.ScreenerStrategyRevision) strategyTemplate {
	profile := rev.ScoreProfile
	if profile == "" {
		profile = "balanced"
	}
	var conditions []string
	var tree *CondNode
	if parsed := condTreeFromJSON(rev.TreeJSON); parsed != nil {
		if canonical, _, err := canonicalCondTree(parsed); err == nil {
			tree = canonical
			conditions = describeCondTree(tree)
		}
	}
	return strategyTemplate{
		Key: recStrategyCustomPrefix + strconv.FormatInt(id, 10), Name: rev.Name,
		Desc: screenStrategyDesc(rev.Period, rev.Risk, rev.Desc), Group: "custom",
		Period: rev.Period, Risk: rev.Risk,
		ScoreProfile: profile, Intent: profileIntent(profile),
		StrategyRevisionID: rev.ID,
		guide:              screenStrategyGuide(rev.Name, rev.Desc, conditions),
		baseKey:            profile,
		screen:             &recScreenBinding{strategyID: id, strategyRevisionID: rev.ID},
		tree:               tree,
	}
}

// loadCustomScreenStrategies 当前用户未归档自建策略（当前 revision 快照）。
// includeArchived 用于解析历史批次的策略（归档后仍需回显/重试）。
func loadCustomScreenStrategies(userID int64, onlyID int64, includeArchived bool) ([]model.ScreenerStrategy, map[int64]model.ScreenerStrategyRevision, error) {
	if common.DB == nil || userID <= 0 {
		return nil, nil, nil
	}
	q := common.DB.Where("user_id = ?", userID)
	if onlyID > 0 {
		q = q.Where("id = ?", onlyID)
	}
	if !includeArchived {
		q = q.Where("archived_at IS NULL")
	}
	var rows []model.ScreenerStrategy
	if err := q.Order("id DESC").Find(&rows).Error; err != nil {
		return nil, nil, err
	}
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		if r.CurrentRevisionID > 0 {
			ids = append(ids, r.CurrentRevisionID)
		}
	}
	revBy := map[int64]model.ScreenerStrategyRevision{}
	if len(ids) > 0 {
		var revs []model.ScreenerStrategyRevision
		if err := common.DB.Where("user_id = ? AND id IN ?", userID, ids).Find(&revs).Error; err != nil {
			return nil, nil, err
		}
		for _, rev := range revs {
			if rev.ScoreProfile != "" && !model.ValidStrategyScoreProfile(rev.ScoreProfile) {
				return nil, nil, fmt.Errorf("策略 %d 的评分方式无效，请修复后重试", rev.StrategyID)
			}
			revBy[rev.ID] = rev
		}
	}
	return rows, revBy, nil
}

// publicStrategy 去掉内部字段的下拉视图。
func publicStrategy(s strategyTemplate) strategyTemplate {
	profile := s.ScoreProfile
	if profile == "" {
		profile = s.baseKey
	}
	intent := s.Intent
	if intent == "" {
		intent = profileIntent(profile)
	}
	return strategyTemplate{Key: s.Key, Name: s.Name, Desc: s.Desc, Group: s.Group, Period: s.Period, Risk: s.Risk,
		ScoreProfile: profile, Intent: intent, StrategyRevisionID: s.StrategyRevisionID}
}

// StrategiesFor 返回某类型的内置推荐策略（供单测与无用户上下文的调用）。
func StrategiesFor(recType string) []strategyTemplate {
	src := recBuiltinStrategies(recType)
	out := make([]strategyTemplate, 0, len(src))
	for _, s := range src {
		out = append(out, publicStrategy(s))
	}
	return out
}

// StrategiesForUser 推荐工作台下拉的完整目录：内置推荐策略 + 选股页全部策略
// （内置选股策略、新手模板、当前用户未归档自建策略）。选股类策略按适用周期与
// 推荐类型的贴合度排序（短线：short→swing→mid；长线相反），同周期保持选股页展示序。
func StrategiesForUser(userID int64, recType string) ([]strategyTemplate, error) {
	recType = strings.ToLower(strings.TrimSpace(recType))
	if recType != model.RecTypeShortTerm && recType != model.RecTypeLongTerm {
		return nil, errors.New("推荐类型须为 short_term 或 long_term")
	}
	out := make([]strategyTemplate, 0, 32)
	for _, s := range recBuiltinStrategies(recType) {
		out = append(out, publicStrategy(s))
	}
	var screens []strategyTemplate
	for _, b := range builtinScreens {
		screens = append(screens, publicStrategy(builtinScreenStrategyTemplate(recType, b)))
	}
	for _, t := range retailTemplates {
		screens = append(screens, publicStrategy(retailTemplateStrategyTemplate(recType, t)))
	}
	rows, revBy, err := loadCustomScreenStrategies(userID, 0, false)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		rev, ok := revBy[r.CurrentRevisionID]
		if !ok || rev.StrategyID != r.ID {
			continue
		}
		t := customScreenStrategyTemplate(recType, r.ID, rev)
		if t.tree != nil {
			screens = append(screens, publicStrategy(t))
		}
	}
	periodOrder := map[string]int{"short": 0, "swing": 1, "mid": 2}
	if recType != model.RecTypeShortTerm {
		periodOrder = map[string]int{"mid": 0, "swing": 1, "short": 2}
	}
	groupOrder := map[string]int{"custom": 0, "screen": 1, "template": 2}
	sort.SliceStable(screens, func(i, j int) bool {
		if groupOrder[screens[i].Group] != groupOrder[screens[j].Group] {
			return groupOrder[screens[i].Group] < groupOrder[screens[j].Group]
		}
		return periodOrder[screens[i].Period] < periodOrder[screens[j].Period]
	})
	return append(out, screens...), nil
}

// strategyByKey 按类型查内置推荐策略：空 key 用第一个缺省；非空但查不到报错
// （旧版静默回退第一个，用户传跨类型 key 时会无感知地跑错策略）。
// 选股类 key（screen:/tpl:）需用户上下文解析自建策略，走 resolveRecStrategy。
func strategyByKey(recType, key string) (*strategyTemplate, error) {
	src := recBuiltinStrategies(recType)
	if key == "" {
		return &src[0], nil
	}
	for i := range src {
		if src[i].Key == key {
			return &src[i], nil
		}
	}
	if strings.HasPrefix(key, recStrategyScreenPrefix) || strings.HasPrefix(key, recStrategyTemplatePrefix) {
		return resolveScreenStrategy(0, recType, key, 0)
	}
	return nil, fmt.Errorf("策略 %s 与推荐类型不匹配，请重新选择", key)
}

// resolveRecStrategy 解析推荐请求中的策略 key（内置推荐策略或选股类策略）。
// 用户自建策略按 userID 隔离；已归档策略仍可解析（历史批次重试/回显），前端下拉不再列出。
func resolveRecStrategy(userID int64, recType, key string) (*strategyTemplate, error) {
	return resolveRecStrategyRevision(userID, recType, key, 0)
}

// revisionID=0 仅在首次提交时取当前版本；持久化作业始终传入解析后的版本。
func resolveRecStrategyRevision(userID int64, recType, key string, revisionID int64) (*strategyTemplate, error) {
	key = strings.TrimSpace(key)
	if revisionID < 0 || (revisionID > 0 && !strings.HasPrefix(key, recStrategyCustomPrefix)) {
		return nil, errors.New("策略版本必须属于指定的自建选股策略")
	}
	if strings.HasPrefix(key, recStrategyScreenPrefix) || strings.HasPrefix(key, recStrategyTemplatePrefix) {
		return resolveScreenStrategy(userID, recType, key, revisionID)
	}
	return strategyByKey(recType, key)
}

func resolveScreenStrategy(userID int64, recType, key string, revisionID int64) (*strategyTemplate, error) {
	switch {
	case strings.HasPrefix(key, recStrategyCustomPrefix):
		id, err := strconv.ParseInt(strings.TrimPrefix(key, recStrategyCustomPrefix), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("自建选股策略标识无效，请重新选择策略")
		}
		if userID <= 0 {
			return nil, errors.New("自建选股策略需要登录用户上下文")
		}
		if common.DB == nil {
			return nil, errors.New("数据库不可用")
		}
		var strategy model.ScreenerStrategy
		if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&strategy).Error; err != nil {
			return nil, errors.New("自建选股策略不存在或不属于当前用户，请重新选择策略")
		}
		if revisionID == 0 {
			revisionID = strategy.CurrentRevisionID
		}
		if revisionID <= 0 {
			return nil, errors.New("自建选股策略尚无可执行版本，请先在选股页保存")
		}
		var rev model.ScreenerStrategyRevision
		if err := common.DB.Where("id = ? AND user_id = ? AND strategy_id = ?", revisionID, userID, id).First(&rev).Error; err != nil {
			return nil, errors.New("策略版本不存在或不属于指定策略")
		}
		if rev.ScoreProfile != "" && !model.ValidStrategyScoreProfile(rev.ScoreProfile) {
			return nil, errors.New("策略版本的评分方式无效，请先在选股页修复")
		}
		t := customScreenStrategyTemplate(recType, id, rev)
		if t.tree == nil {
			return nil, errors.New("自建选股策略条件无效，请先在选股页修复")
		}
		return &t, nil
	case strings.HasPrefix(key, recStrategyScreenPrefix):
		b, ok := builtinScreenByKey(strings.TrimPrefix(key, recStrategyScreenPrefix))
		if !ok {
			return nil, fmt.Errorf("选股策略 %s 不存在，请重新选择", key)
		}
		t := builtinScreenStrategyTemplate(recType, b)
		return &t, nil
	case strings.HasPrefix(key, recStrategyTemplatePrefix):
		tpl, ok := retailTemplateByKey(strings.TrimPrefix(key, recStrategyTemplatePrefix))
		if !ok {
			return nil, fmt.Errorf("新手模板 %s 不存在，请重新选择", key)
		}
		t := retailTemplateStrategyTemplate(recType, tpl)
		return &t, nil
	}
	return nil, fmt.Errorf("策略 %s 无法识别", key)
}

// strategySignalPoolLimitFor 策略信号来源的进池上限：选股类策略是主供给，给更高名额。
func strategySignalPoolLimitFor(strat *strategyTemplate) int {
	if strat != nil && strat.screen != nil {
		return screenStrategySignalPoolLimit
	}
	return strategySignalPoolLimit
}

// ---------- 选股类策略的量化规则：条件命中度评估与加分 ----------

// StrategyHit 选股类推荐策略对单只候选的条件命中评估（随候选池快照落库、喂给 LLM、
// 前端可展开）。因子行用与选股引擎完全相同的 computeWideRow 由候选的 250 根日线
// 计算（收盘口径、同一求值函数），保证「推荐里的策略命中」与选股页扫描一致。
// any 组算作一个条件单元（满足其一即命中）。
type StrategyHit struct {
	Total     int                   `json:"total"`                // 条件单元总数
	Hit       int                   `json:"hit"`                  // 命中单元数
	Full      bool                  `json:"full"`                 // 全部命中（= 选股页会扫出该股）
	Matched   []string              `json:"matched,omitempty"`    // 命中条件（人话，含当前值）
	Missed    []string              `json:"missed,omitempty"`     // 未命中条件（人话，含当前值）
	TradeDate string                `json:"trade_date,omitempty"` // 因子基准交易日（末根日线）
	Status    string                `json:"status,omitempty"`     // matched / missed / unknown
	Missing   int                   `json:"missing,omitempty"`
	Unknown   []string              `json:"unknown,omitempty"`
	Values    map[string]float64    `json:"values,omitempty"` // 所用条件的可用原始因子，缺键即缺失
	Current   *strategyCurrentCheck `json:"current,omitempty"`
	row       []float64             // 本次评分冻结因子，仅供终选复核；不以现价改写历史序列
}

// singleRowFactorTable 把一行因子值包装成单行宽表，复用 evalCondRow/explainRow。
func singleRowFactorTable(vals []float64) *FactorTable {
	t := &FactorTable{Symbols: []string{""}, Names: []string{""}, LastDates: []string{""},
		cols: make(map[string][]float64, len(factorDefs))}
	for j, d := range factorDefs {
		t.cols[d.Key] = []float64{vals[j]}
	}
	return t
}

// describeLeafWithValue 未命中叶子的人话描述（附当前值，缺失显示「无数据」）。
func describeLeafWithValue(t *FactorTable, n *CondNode) string {
	def, ok := factorByKey(n.Factor)
	if !ok {
		return n.Factor
	}
	v := t.Col(n.Factor)[0]
	switch n.Op {
	case "is_true":
		return fmt.Sprintf("%s（当前 %s）", def.Name, fmtFactorVal(def, v))
	case "is_false":
		return fmt.Sprintf("非%s（当前 %s）", def.Name, fmtFactorVal(def, v))
	case "between":
		return fmt.Sprintf("%s 介于 %s~%s（当前 %s）", def.Name, fmtFactorVal(def, *n.Value), fmtFactorVal(def, *n.Value2), fmtFactorVal(def, v))
	}
	if n.Ref != "" {
		refDef, _ := factorByKey(n.Ref)
		rv := t.Col(n.Ref)[0]
		return fmt.Sprintf("%s %s %s（%s %s %s）", def.Name, opText(n.Op), refDef.Name,
			fmtFactorVal(def, v), opText(n.Op), fmtFactorVal(refDef, rv))
	}
	return fmt.Sprintf("%s %s %s（当前 %s）", def.Name, opText(n.Op), fmtFactorVal(def, *n.Value), fmtFactorVal(def, v))
}

// evalStrategyHitNode 递归评估：all 组展开逐条计数；any 组作一个单元。返回本节点是否命中。
func evalStrategyHitNode(t *FactorTable, n *CondNode, hit *StrategyHit) bool {
	if len(n.All) > 0 {
		all := true
		for j := range n.All {
			if !evalStrategyHitNode(t, &n.All[j], hit) {
				all = false
			}
		}
		return all
	}
	if len(n.Any) > 0 {
		hit.Total++
		matched := false
		for j := range n.Any {
			if evalCondRow(t, &n.Any[j], 0) {
				matched = true
				var parts []string
				explainRow(t, &n.Any[j], 0, &parts)
				hit.Matched = append(hit.Matched, strings.Join(parts, " 且 "))
				break
			}
		}
		if matched {
			hit.Hit++
			return true
		}
		if strategyNodeState(t, n) == strategyUnknown {
			hit.Missing++
			hit.Unknown = append(hit.Unknown, "满足其一的数据不足："+strings.Join(describeCondTree(n), " / "))
		} else {
			hit.Missed = append(hit.Missed, "满足其一："+strings.Join(describeCondTree(n), " / "))
		}
		return false
	}
	hit.Total++
	if evalCondRow(t, n, 0) {
		hit.Hit++
		explainRow(t, n, 0, &hit.Matched)
		return true
	}
	if strategyNodeState(t, n) == strategyUnknown {
		hit.Missing++
		hit.Unknown = append(hit.Unknown, describeLeafWithValue(t, n))
	} else {
		hit.Missed = append(hit.Missed, describeLeafWithValue(t, n))
	}
	return false
}

// evaluateStrategyHit 对一只候选评估选股类策略条件命中度。strat 非选股类或树缺失返回 nil。
// meta 的股息率由调用方按池批量装载（C10 因子仅 dividend 类模板引用）。
func evaluateStrategyHit(strat *strategyTemplate, symbol string, meta wideStockMeta, bars []datasource.Bar) *StrategyHit {
	if strat == nil || strat.screen == nil || strat.tree == nil || len(bars) == 0 {
		return nil
	}
	vals := computeWideRow(symbol, meta, bars)
	t := singleRowFactorTable(vals)
	hit := &StrategyHit{TradeDate: bars[len(bars)-1].TradeDate, Status: strategyNodeState(t, strat.tree), row: vals, Values: map[string]float64{}}
	var collect func(*CondNode)
	collect = func(n *CondNode) {
		for _, key := range []string{n.Factor, n.Ref} {
			if j, ok := factorIndex[key]; ok && finiteRecNumber(vals[j]) {
				hit.Values[key] = vals[j]
			}
		}
		for i := range n.All {
			collect(&n.All[i])
		}
		for i := range n.Any {
			collect(&n.Any[i])
		}
	}
	collect(strat.tree)
	hit.Full = evalStrategyHitNode(t, strat.tree, hit)
	return hit
}

// screenStrategyBonus 只确认完整命中；缺失或部分命中由必要条件硬门排除，不用加分补偿。
func screenStrategyBonus(h *StrategyHit) (float64, []string) {
	if h == nil || h.Total == 0 {
		return 0, nil
	}
	switch {
	case h.Full:
		return 12, []string{fmt.Sprintf("策略条件全部命中 %d/%d（+12）", h.Hit, h.Total)}
	}
	return 0, []string{fmt.Sprintf("策略条件命中 %d/%d，未命中：%s（+0）", h.Hit, h.Total, strings.Join(h.Missed, "；"))}
}
