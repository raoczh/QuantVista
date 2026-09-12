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
// 来源），prompt 以策略白话讲解作为选股导向，量化加分沿用按周期映射的基础推荐策略。
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
	Period string `json:"period,omitempty"`
	Risk   string `json:"risk,omitempty"`
	// 自建策略目录携带不可变版本，提交、排队、扫描和历史展示共用此身份。
	StrategyRevisionID int64 `json:"strategy_revision_id,omitempty"`

	guide string // 注入 prompt 的选股导向（不外泄给前端）
	// baseKey 量化加分/榜单来源沿用的基础推荐策略 key（内置推荐策略 = 自身 Key；
	// 选股类策略按周期映射）。
	baseKey string
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
	req := ScanRequest{Limit: limit}
	if t.screen == nil {
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

// screenBaseKey 选股类策略按（推荐类型, 选股周期）映射到基础推荐策略——决定量化
// 加分分支（strategyAdjust）与榜单来源组合（strategySources）。已有专属映射的内置
// 选股策略（recStrategySignalKey 的反向）优先沿用原对应关系。
func screenBaseKey(recType, period, builtinKey string) string {
	if builtinKey != "" {
		for _, base := range recBuiltinStrategies(recType) {
			if recStrategySignalKey(recType, base.Key) == builtinKey {
				return base.Key
			}
		}
	}
	if recType == model.RecTypeShortTerm {
		switch period {
		case "swing":
			return "pullback"
		case "mid":
			return "active"
		default:
			return "momentum"
		}
	}
	switch period {
	case "mid":
		return "value"
	default:
		return "growth"
	}
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
	b.WriteString("名单中 sources 含 strategy_signal 的标的为该策略在最近交易日的全市场命中，应优先在其中精选；其余标的须同样符合上述形态方可入选，不符合者应在 rejected 中说明。")
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
	return strategyTemplate{
		Key: recStrategyScreenPrefix + b.Key, Name: b.Name,
		Desc: screenStrategyDesc(b.Period, b.Risk, b.Desc), Group: "screen",
		Period: b.Period, Risk: b.Risk,
		guide:   screenStrategyGuide(b.Name, b.Desc, describeCondTree(tree)),
		baseKey: screenBaseKey(recType, b.Period, b.Key),
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
	return strategyTemplate{
		Key: recStrategyTemplatePrefix + t.Key, Name: t.Name,
		Desc: screenStrategyDesc(t.Period, t.RiskLevel, desc), Group: "template",
		Period: t.Period, Risk: t.RiskLevel,
		guide:   screenStrategyGuide(t.Name, desc+" 风险提示："+t.Risk, describeCondTree(tree)),
		baseKey: screenBaseKey(recType, t.Period, ""),
		screen:  &recScreenBinding{templateKey: t.Key},
		tree:    tree,
	}
}

// customScreenStrategyTemplate 用户自建策略（当前 revision）→ 推荐策略模板。
func customScreenStrategyTemplate(recType string, id int64, rev model.ScreenerStrategyRevision) strategyTemplate {
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
		StrategyRevisionID: rev.ID,
		guide:              screenStrategyGuide(rev.Name, rev.Desc, conditions),
		baseKey:            screenBaseKey(recType, rev.Period, ""),
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
			revBy[rev.ID] = rev
		}
	}
	return rows, revBy, nil
}

// publicStrategy 去掉内部字段的下拉视图。
func publicStrategy(s strategyTemplate) strategyTemplate {
	return strategyTemplate{Key: s.Key, Name: s.Name, Desc: s.Desc, Group: s.Group, Period: s.Period, Risk: s.Risk,
		StrategyRevisionID: s.StrategyRevisionID}
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
	Total     int      `json:"total"`                // 条件单元总数
	Hit       int      `json:"hit"`                  // 命中单元数
	Full      bool     `json:"full"`                 // 全部命中（= 选股页会扫出该股）
	Matched   []string `json:"matched,omitempty"`    // 命中条件（人话，含当前值）
	Missed    []string `json:"missed,omitempty"`     // 未命中条件（人话，含当前值）
	TradeDate string   `json:"trade_date,omitempty"` // 因子基准交易日（末根日线）
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
		hit.Missed = append(hit.Missed, "满足其一："+strings.Join(describeCondTree(n), " / "))
		return false
	}
	hit.Total++
	if evalCondRow(t, n, 0) {
		hit.Hit++
		explainRow(t, n, 0, &hit.Matched)
		return true
	}
	hit.Missed = append(hit.Missed, describeLeafWithValue(t, n))
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
	hit := &StrategyHit{TradeDate: bars[len(bars)-1].TradeDate}
	hit.Full = evalStrategyHitNode(t, strat.tree, hit)
	return hit
}

// screenStrategyBonus 选股类策略的加分：全部命中 +12（该股就是选股页会扫出的标的）；
// 部分命中按命中比例给 0~6 分（≥半数才给，避免「碰巧满足一条」得分）；命中率 <1/3 扣 4 分
// （与用户明确指定的形态明显不符，不该靠五维基础分混入名单前列）。逐条说明随候选落库。
func screenStrategyBonus(h *StrategyHit) (float64, []string) {
	if h == nil || h.Total == 0 {
		return 0, nil
	}
	ratio := float64(h.Hit) / float64(h.Total)
	switch {
	case h.Full:
		return 12, []string{fmt.Sprintf("策略条件全部命中 %d/%d（+12）", h.Hit, h.Total)}
	case ratio >= 0.5:
		d := float64(int(6*ratio + 0.5))
		return d, []string{fmt.Sprintf("策略条件命中 %d/%d，未命中：%s（+%.0f）", h.Hit, h.Total, strings.Join(h.Missed, "；"), d)}
	case ratio < 1.0/3:
		return -4, []string{fmt.Sprintf("策略条件仅命中 %d/%d，与所选形态不符（-4）", h.Hit, h.Total)}
	}
	return 0, []string{fmt.Sprintf("策略条件命中 %d/%d，未命中：%s（+0）", h.Hit, h.Total, strings.Join(h.Missed, "；"))}
}
