package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// M1 条件树选股：JSON DSL 的校验/递归求值/命中原因人话化 + 扫描服务。
//
// DSL 形态（组节点与叶子二选一）：
//   {"all":[...]} / {"any":[...]}                          —— 组合
//   {"factor":"rsi_14","op":"between","value":30,"value2":45} —— 数值区间
//   {"factor":"close","op":">","ref":"ma20"}                  —— 与另一因子比
//   {"factor":"bull_align","op":"is_true"}                    —— 布尔
//
// 求值语义：因子值 NaN（样本不足/筹码拒算）一律不命中——「数据不足」不冒充信号。
// 命中原因人话化（透明池同款风格）：`✓ 量比(5日) 介于 1.5~5（当前 2.13）`。

// ---------- DSL ----------

// CondNode 条件树节点：All/Any 非空为组节点，否则为叶子。
// Value/Value2 用指针区分「显式 0」与「未填」。
type CondNode struct {
	All    []CondNode `json:"all,omitempty"`
	Any    []CondNode `json:"any,omitempty"`
	Factor string     `json:"factor,omitempty"`
	Op     string     `json:"op,omitempty"` // > / >= / < / <= / between / is_true / is_false
	Value  *float64   `json:"value,omitempty"`
	Value2 *float64   `json:"value2,omitempty"` // between 上界
	Ref    string     `json:"ref,omitempty"`    // 数值比较的右侧因子（与 Value 互斥）
}

const (
	condMaxDepth  = 6  // 组嵌套深度上限
	condMaxLeaves = 48 // 叶子总数上限（防恶意巨树拖慢全市场扫描）
)

// validateCondTree 校验条件树结构与因子/操作符合法性。返回叶子总数。
func validateCondTree(n *CondNode, depth int) (int, error) {
	if n == nil {
		return 0, errors.New("条件树为空")
	}
	if depth > condMaxDepth {
		return 0, fmt.Errorf("条件嵌套超过 %d 层", condMaxDepth)
	}
	isGroup := len(n.All) > 0 || len(n.Any) > 0
	if isGroup {
		if n.Factor != "" || n.Op != "" {
			return 0, errors.New("组节点（all/any）不能同时是条件叶子")
		}
		if len(n.All) > 0 && len(n.Any) > 0 {
			return 0, errors.New("同一节点不能同时有 all 与 any（请拆成两层）")
		}
		children := n.All
		if len(children) == 0 {
			children = n.Any
		}
		if len(children) > 32 {
			return 0, errors.New("单组子条件超过 32 个")
		}
		total := 0
		for i := range children {
			c, err := validateCondTree(&children[i], depth+1)
			if err != nil {
				return 0, err
			}
			total += c
			if total > condMaxLeaves {
				return 0, fmt.Errorf("条件叶子总数超过 %d", condMaxLeaves)
			}
		}
		return total, nil
	}

	// 叶子
	def, ok := factorByKey(n.Factor)
	if !ok {
		return 0, fmt.Errorf("未知因子 %q", n.Factor)
	}
	switch n.Op {
	case "is_true", "is_false":
		if def.Kind != fkBool {
			return 0, fmt.Errorf("因子 %s 不是布尔型，不能用 %s", def.Name, n.Op)
		}
	case ">", ">=", "<", "<=":
		if def.Kind == fkBool {
			return 0, fmt.Errorf("布尔因子 %s 请用 is_true/is_false", def.Name)
		}
		if n.Ref != "" {
			refDef, ok := factorByKey(n.Ref)
			if !ok {
				return 0, fmt.Errorf("未知对比因子 %q", n.Ref)
			}
			if refDef.Kind == fkBool {
				return 0, fmt.Errorf("对比因子 %s 不能是布尔型", refDef.Name)
			}
			if n.Value != nil {
				return 0, fmt.Errorf("因子 %s：value 与 ref 只能二选一", def.Name)
			}
		} else if n.Value == nil {
			return 0, fmt.Errorf("因子 %s 缺少比较值", def.Name)
		}
	case "between":
		if def.Kind == fkBool {
			return 0, fmt.Errorf("布尔因子 %s 请用 is_true/is_false", def.Name)
		}
		if n.Value == nil || n.Value2 == nil {
			return 0, fmt.Errorf("因子 %s 的 between 需要 value 与 value2", def.Name)
		}
		if *n.Value > *n.Value2 {
			*n.Value, *n.Value2 = *n.Value2, *n.Value // 宽容交换
		}
	case "":
		return 0, errors.New("条件缺少操作符（op）")
	default:
		return 0, fmt.Errorf("不支持的操作符 %q", n.Op)
	}
	return 1, nil
}

// evalCondRow 对宽表第 i 行递归求值。NaN 因子值恒 false。
func evalCondRow(t *FactorTable, n *CondNode, i int) bool {
	if len(n.All) > 0 {
		for j := range n.All {
			if !evalCondRow(t, &n.All[j], i) {
				return false
			}
		}
		return true
	}
	if len(n.Any) > 0 {
		for j := range n.Any {
			if evalCondRow(t, &n.Any[j], i) {
				return true
			}
		}
		return false
	}
	col := t.Col(n.Factor)
	if col == nil {
		return false
	}
	v := col[i]
	if math.IsNaN(v) {
		return false
	}
	switch n.Op {
	case "is_true":
		return v == 1
	case "is_false":
		return v == 0
	case "between":
		return v >= *n.Value && v <= *n.Value2
	}
	var rhs float64
	if n.Ref != "" {
		refCol := t.Col(n.Ref)
		if refCol == nil {
			return false
		}
		rhs = refCol[i]
		if math.IsNaN(rhs) {
			return false
		}
	} else {
		rhs = *n.Value
	}
	switch n.Op {
	case ">":
		return v > rhs
	case ">=":
		return v >= rhs
	case "<":
		return v < rhs
	case "<=":
		return v <= rhs
	}
	return false
}

// explainRow 收集第 i 行命中的叶子人话描述（仅对已整体命中的行调用；
// any 组只列出实际命中的分支）。
func explainRow(t *FactorTable, n *CondNode, i int, out *[]string) {
	if len(n.All) > 0 {
		for j := range n.All {
			explainRow(t, &n.All[j], i, out)
		}
		return
	}
	if len(n.Any) > 0 {
		for j := range n.Any {
			if evalCondRow(t, &n.Any[j], i) {
				explainRow(t, &n.Any[j], i, out)
			}
		}
		return
	}
	if !evalCondRow(t, n, i) {
		return // all 分支下不会走到；防御 any 混嵌场景
	}
	def, _ := factorByKey(n.Factor)
	v := t.Col(n.Factor)[i]
	switch n.Op {
	case "is_true":
		*out = append(*out, "✓ "+def.Name)
	case "is_false":
		*out = append(*out, "✓ 非"+def.Name)
	case "between":
		*out = append(*out, fmt.Sprintf("✓ %s 介于 %s~%s（当前 %s）",
			def.Name, fmtFactorVal(def, *n.Value), fmtFactorVal(def, *n.Value2), fmtFactorVal(def, v)))
	default:
		if n.Ref != "" {
			refDef, _ := factorByKey(n.Ref)
			rv := t.Col(n.Ref)[i]
			*out = append(*out, fmt.Sprintf("✓ %s %s %s（%s %s %s）",
				def.Name, opText(n.Op), refDef.Name,
				fmtFactorVal(def, v), opText(n.Op), fmtFactorVal(refDef, rv)))
		} else {
			*out = append(*out, fmt.Sprintf("✓ %s %s %s（当前 %s）",
				def.Name, opText(n.Op), fmtFactorVal(def, *n.Value), fmtFactorVal(def, v)))
		}
	}
}

// describeCondTree 静态条件清单（策略卡预览，不带当前值）。
func describeCondTree(n *CondNode) []string {
	var out []string
	var walk func(n *CondNode, inAny bool)
	walk = func(n *CondNode, inAny bool) {
		if len(n.All) > 0 {
			for j := range n.All {
				walk(&n.All[j], false)
			}
			return
		}
		if len(n.Any) > 0 {
			var parts []string
			for j := range n.Any {
				sub := describeCondTree(&n.Any[j])
				parts = append(parts, strings.Join(sub, " 且 "))
			}
			out = append(out, "满足其一："+strings.Join(parts, " / "))
			return
		}
		def, ok := factorByKey(n.Factor)
		if !ok {
			return
		}
		switch n.Op {
		case "is_true":
			out = append(out, def.Name)
		case "is_false":
			out = append(out, "非"+def.Name)
		case "between":
			out = append(out, fmt.Sprintf("%s 介于 %s~%s", def.Name, fmtFactorVal(def, *n.Value), fmtFactorVal(def, *n.Value2)))
		default:
			if n.Ref != "" {
				refDef, _ := factorByKey(n.Ref)
				out = append(out, fmt.Sprintf("%s %s %s", def.Name, opText(n.Op), refDef.Name))
			} else {
				out = append(out, fmt.Sprintf("%s %s %s", def.Name, opText(n.Op), fmtFactorVal(def, *n.Value)))
			}
		}
	}
	walk(n, false)
	return out
}

func opText(op string) string {
	switch op {
	case ">=":
		return "≥"
	case "<=":
		return "≤"
	}
	return op
}

// fmtFactorVal 按因子类型格式化数值（人话化与前端展示口径一致）。
func fmtFactorVal(def factorDef, v float64) string {
	if math.IsNaN(v) {
		return "无数据"
	}
	switch def.Kind {
	case fkPct:
		return trimFloat(v) + "%"
	case fkInt:
		return fmt.Sprintf("%.0f", v)
	case fkBool:
		if v == 1 {
			return "是"
		}
		return "否"
	default: // price / ratio
		return trimFloat(v)
	}
}

// ---------- 扫描服务 ----------

// ScreenerService 选股：内置/自定义策略管理 + 全市场扫描。
type ScreenerService struct{}

func NewScreenerService() *ScreenerService { return &ScreenerService{} }

const (
	scanDefaultLimit  = 100
	scanMaxLimit      = 200
	customStrategyMax = 50 // 每用户自定义策略上限
)

// ScanRequest 扫描入参：strategy_key（内置）/ strategy_id（自定义）/ tree（临时试跑）三选一。
type ScanRequest struct {
	StrategyKey  string    `json:"strategy_key"`
	StrategyID   int64     `json:"strategy_id"`
	Tree         *CondNode `json:"tree"`
	IncludeST    bool      `json:"include_st"`    // 默认排除 ST/退市警示
	IncludeStale bool      `json:"include_stale"` // 默认排除末根≠最新交易日的股（停牌/滞后，旧价因子会误导）
	Limit        int       `json:"limit"`
}

// ScanHit 单只命中：行情摘要 + 人话命中原因。
type ScanHit struct {
	Symbol       string   `json:"symbol"`
	Name         string   `json:"name"`
	Price        float64  `json:"price"` // 宽表收盘价（数据日期见 ScanResult.TradeDate）
	ChgPct       float64  `json:"chg_pct"`
	AmountYi     float64  `json:"amount_yi"`
	TurnoverRate float64  `json:"turnover_rate,omitempty"`
	Pos60        float64  `json:"pos_60,omitempty"`
	Reasons      []string `json:"reasons"`
}

// ScanResult 扫描结果：命中列表 + 全景计数（引擎排除了什么全透明）。
type ScanResult struct {
	Strategy     string    `json:"strategy"` // 展示名
	TradeDate    string    `json:"trade_date"`
	Universe     int       `json:"universe"`      // 宽表内标的总数
	Scanned      int       `json:"scanned"`       // 参与条件判定的行数
	StaleSkipped int       `json:"stale_skipped"` // 停牌/数据滞后跳过
	StSkipped    int       `json:"st_skipped"`    // ST/退市警示跳过
	Matched      int       `json:"matched"`       // 命中总数
	Truncated    bool      `json:"truncated"`     // 命中超出 limit 截断
	Items        []ScanHit `json:"items"`
	BuildMs      int64     `json:"build_ms"`   // 宽表构建耗时（缓存命中时为历史值）
	Conditions   []string  `json:"conditions"` // 本次策略的静态条件清单

	// P1 数据水位（对照交易日历，非库内自身 MAX）：ExpectedDate 应有交易日、
	// LagOpenDays 库数据落后开市日数、FreshCoverage fresh 行覆盖率（0~1）、
	// StaleNote 滞后时的用户提示（命中结果基于旧形态，非当日口径）。
	ExpectedDate  string  `json:"expected_date,omitempty"`
	LagOpenDays   int     `json:"lag_open_days,omitempty"`
	FreshCoverage float64 `json:"fresh_coverage,omitempty"`
	StaleNote     string  `json:"stale_note,omitempty"`
}

// Scan 全市场扫描。宽表构建有互斥防抖（并发请求等待同一次构建，见 ensureFactorTable），
// 扫描本身是内存只读遍历（5500 行 × ≤48 叶 <10ms），无需额外互斥。
func (s *ScreenerService) Scan(ctx context.Context, userID int64, req ScanRequest) (*ScanResult, error) {
	tree, name, err := s.resolveTree(userID, req)
	if err != nil {
		return nil, err
	}
	if _, err := validateCondTree(tree, 1); err != nil {
		return nil, err
	}
	t, err := ensureFactorTable(ctx)
	if err != nil {
		return nil, err
	}
	if t.Len() == 0 {
		return nil, errors.New("全市场日线数据为空：请先在管理端启动全市场同步与历史初始化")
	}

	limit := req.Limit
	if limit <= 0 {
		limit = scanDefaultLimit
	}
	if limit > scanMaxLimit {
		limit = scanMaxLimit
	}

	res := &ScanResult{
		Strategy:   name,
		TradeDate:  t.TradeDate,
		Universe:   t.Len(),
		BuildMs:    t.BuildMs,
		Conditions: describeCondTree(tree),

		ExpectedDate:  t.ExpectedDate,
		LagOpenDays:   t.LagOpenDays,
		FreshCoverage: t.FreshCoverage,
	}
	if t.LagOpenDays > 0 {
		res.StaleNote = fmt.Sprintf("全市场日线仅更新至 %s，落后应有交易日 %s 共 %d 个开市日——命中结果基于旧形态，请先在管理端补跑全市场同步", t.TradeDate, t.ExpectedDate, t.LagOpenDays)
	} else if t.LagOpenDays < 0 {
		res.StaleNote = fmt.Sprintf("交易日历不可用，无法核验全市场日线（截至 %s）是否落后应有交易日 %s——命中结果时效未知，请先在管理端回填日历", t.TradeDate, t.ExpectedDate)
	}
	stCol := t.Col("is_st")
	var matchedIdx []int
	for i := 0; i < t.Len(); i++ {
		if !req.IncludeStale && !t.Fresh(i) {
			res.StaleSkipped++
			continue
		}
		if !req.IncludeST && stCol[i] == 1 {
			res.StSkipped++
			continue
		}
		res.Scanned++
		if evalCondRow(t, tree, i) {
			matchedIdx = append(matchedIdx, i)
		}
	}
	res.Matched = len(matchedIdx)

	// 成交额降序（活跃优先），前 limit 只生成人话原因。
	amountCol := t.Col("amount_yi")
	sort.Slice(matchedIdx, func(a, b int) bool {
		av, bv := amountCol[matchedIdx[a]], amountCol[matchedIdx[b]]
		if math.IsNaN(av) {
			av = -1
		}
		if math.IsNaN(bv) {
			bv = -1
		}
		return av > bv
	})
	if len(matchedIdx) > limit {
		matchedIdx = matchedIdx[:limit]
		res.Truncated = true
	}
	closeCol, chgCol := t.Col("close"), t.Col("chg_pct")
	turnCol, posCol := t.Col("turnover_rate"), t.Col("pos_60")
	nz := func(v float64) float64 {
		if math.IsNaN(v) {
			return 0
		}
		return v
	}
	res.Items = make([]ScanHit, 0, len(matchedIdx))
	for _, i := range matchedIdx {
		hit := ScanHit{
			Symbol:       t.Symbols[i],
			Name:         t.Names[i],
			Price:        nz(closeCol[i]),
			ChgPct:       nz(chgCol[i]),
			AmountYi:     nz(amountCol[i]),
			TurnoverRate: nz(turnCol[i]),
			Pos60:        nz(posCol[i]),
		}
		explainRow(t, tree, i, &hit.Reasons)
		res.Items = append(res.Items, hit)
	}
	return res, nil
}

// resolveTree 解析扫描目标：内置 key / 自定义 id（校验归属）/ 临时树。
func (s *ScreenerService) resolveTree(userID int64, req ScanRequest) (*CondNode, string, error) {
	switch {
	case req.StrategyKey != "":
		b, ok := builtinScreenByKey(req.StrategyKey)
		if !ok {
			return nil, "", fmt.Errorf("未知内置策略 %q", req.StrategyKey)
		}
		tree := b.Tree // 拷贝（between 校验会原地交换 value）
		return &tree, b.Name, nil
	case req.StrategyID > 0:
		if common.DB == nil {
			return nil, "", errors.New("数据库不可用")
		}
		var row model.ScreenerStrategy
		if err := common.DB.Where("id = ? AND user_id = ?", req.StrategyID, userID).First(&row).Error; err != nil {
			return nil, "", errors.New("自定义策略不存在")
		}
		var tree CondNode
		if err := json.Unmarshal([]byte(row.TreeJSON), &tree); err != nil {
			return nil, "", fmt.Errorf("策略条件解析失败: %v", err)
		}
		return &tree, row.Name, nil
	case req.Tree != nil:
		return req.Tree, "自定义条件", nil
	}
	return nil, "", errors.New("请指定策略（strategy_key / strategy_id / tree 三选一）")
}

// ---------- 策略管理 ----------

// BuiltinStrategyView 内置策略卡（含静态条件预览）。
type BuiltinStrategyView struct {
	Key        string   `json:"key"`
	Name       string   `json:"name"`
	Desc       string   `json:"desc"`
	Period     string   `json:"period"` // short / swing / mid
	Risk       string   `json:"risk"`   // low / mid / high
	Conditions []string `json:"conditions"`
}

// StrategiesView 策略广场：内置 + 当前用户自定义。
type StrategiesView struct {
	Builtin []BuiltinStrategyView    `json:"builtin"`
	Custom  []CustomStrategyView     `json:"custom"`
	Factors []factorDef              `json:"factors"` // 因子字典（自定义编辑器）
}

// CustomStrategyView 自定义策略行（树已展开供编辑器回填）。
type CustomStrategyView struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Desc       string    `json:"desc"`
	Period     string    `json:"period"`
	Risk       string    `json:"risk"`
	Tree       *CondNode `json:"tree"`
	Conditions []string  `json:"conditions"`
}

// Strategies 列出策略广场内容。
func (s *ScreenerService) Strategies(userID int64) (*StrategiesView, error) {
	v := &StrategiesView{Factors: factorDefs}
	for _, b := range builtinScreens {
		tree := b.Tree
		v.Builtin = append(v.Builtin, BuiltinStrategyView{
			Key: b.Key, Name: b.Name, Desc: b.Desc, Period: b.Period, Risk: b.Risk,
			Conditions: describeCondTree(&tree),
		})
	}
	if common.DB == nil {
		return v, nil
	}
	var rows []model.ScreenerStrategy
	if err := common.DB.Where("user_id = ?", userID).Order("id DESC").Find(&rows).Error; err != nil {
		return nil, err
	}
	for _, r := range rows {
		cv := CustomStrategyView{ID: r.ID, Name: r.Name, Desc: r.Desc, Period: r.Period, Risk: r.Risk}
		var tree CondNode
		if json.Unmarshal([]byte(r.TreeJSON), &tree) == nil {
			cv.Tree = &tree
			cv.Conditions = describeCondTree(&tree)
		}
		v.Custom = append(v.Custom, cv)
	}
	return v, nil
}

// SaveStrategyRequest 新建/更新自定义策略。
type SaveStrategyRequest struct {
	ID     int64     `json:"id"` // 0=新建
	Name   string    `json:"name"`
	Desc   string    `json:"desc"`
	Period string    `json:"period"`
	Risk   string    `json:"risk"`
	Tree   *CondNode `json:"tree"`
}

// SaveStrategy 保存自定义策略（user_id 隔离；树先校验）。
func (s *ScreenerService) SaveStrategy(userID int64, req SaveStrategyRequest) (*CustomStrategyView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return nil, errors.New("策略名称不能为空")
	}
	if len([]rune(req.Name)) > 32 {
		return nil, errors.New("策略名称过长（≤32 字）")
	}
	if req.Tree == nil {
		return nil, errors.New("条件树不能为空")
	}
	if _, err := validateCondTree(req.Tree, 1); err != nil {
		return nil, err
	}
	if !validScreenPeriod(req.Period) {
		req.Period = "swing"
	}
	if !validScreenRisk(req.Risk) {
		req.Risk = "mid"
	}
	treeJSON, err := json.Marshal(req.Tree)
	if err != nil {
		return nil, err
	}
	row := model.ScreenerStrategy{
		UserID: userID, Name: req.Name, Desc: truncate(req.Desc, 256),
		Period: req.Period, Risk: req.Risk, TreeJSON: string(treeJSON),
	}
	if req.ID > 0 {
		var exist model.ScreenerStrategy
		if err := common.DB.Where("id = ? AND user_id = ?", req.ID, userID).First(&exist).Error; err != nil {
			return nil, errors.New("策略不存在")
		}
		row.ID = exist.ID
		row.CreatedAt = exist.CreatedAt
		if err := common.DB.Model(&exist).Updates(map[string]any{
			"name": row.Name, "desc": row.Desc, "period": row.Period, "risk": row.Risk, "tree_json": row.TreeJSON,
		}).Error; err != nil {
			return nil, err
		}
	} else {
		var n int64
		common.DB.Model(&model.ScreenerStrategy{}).Where("user_id = ?", userID).Count(&n)
		if n >= customStrategyMax {
			return nil, fmt.Errorf("自定义策略已达上限 %d 条", customStrategyMax)
		}
		if err := common.DB.Create(&row).Error; err != nil {
			return nil, err
		}
	}
	return &CustomStrategyView{
		ID: row.ID, Name: row.Name, Desc: row.Desc, Period: row.Period, Risk: row.Risk,
		Tree: req.Tree, Conditions: describeCondTree(req.Tree),
	}, nil
}

// DeleteStrategy 删除自定义策略（user_id 隔离）。
func (s *ScreenerService) DeleteStrategy(userID, id int64) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.ScreenerStrategy{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("策略不存在")
	}
	return nil
}

func validScreenPeriod(p string) bool { return p == "short" || p == "swing" || p == "mid" }
func validScreenRisk(r string) bool   { return r == "low" || r == "mid" || r == "high" }

// ---------- 推荐池接线（strategy_signal 来源） ----------

// strategySignalPoolLimit 策略信号路进池的命中上限（与其他榜单来源量级一致，
// 名额分配仍由 assignScanQuota 轮转控制）。
const strategySignalPoolLimit = 30

// strategySignalHits 推荐池的策略信号来源：按推荐策略映射的内置选股策略
//（recStrategySignalKey）做全市场扫描，返回成交额降序的前 n 只命中。
// 宽表未就绪/全市场日线未初始化时返回 nil——best-effort，不阻断建池
//（与榜单来源单路失败降级同款纪律）。
// P1 fail-closed：宽表数据落后应有交易日超过 1 个开市日时放弃本路来源——
// 旧形态命中的「策略信号」会把过期技术形态当最新供给喂进推荐池。
func strategySignalHits(ctx context.Context, recType, stratKey string, n int) []ScanHit {
	key := recStrategySignalKey(recType, stratKey)
	svc := ScreenerService{}
	res, err := svc.Scan(ctx, 0, ScanRequest{StrategyKey: key, Limit: n})
	if err != nil {
		common.SysDebug("策略信号来源跳过（%s）: %v", key, err)
		return nil
	}
	// fail-closed：落后超 1 个开市日或日历不可用（-1 无法判定）都不作推荐供给——
	// 「无法判定新鲜度」当 0 用会让旧形态在日历缺失时冒充最新信号。
	if res.LagOpenDays > 1 || res.LagOpenDays < 0 {
		common.SysWarn("策略信号来源跳过：全市场日线时效不达标（lag=%d，数据截至 %s，应有 %s；-1=日历不可用），旧形态不作推荐供给",
			res.LagOpenDays, res.TradeDate, res.ExpectedDate)
		return nil
	}
	return res.Items
}
