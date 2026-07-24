package service

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"quantvista/common"
	"quantvista/model"
	"quantvista/setting"
)

// P2-4 模型路由与成本优化（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.3 P2-4、§10.2）。
//
// 定位与边界（不可漂移）：
//   - **路由只换调用目标**（BaseURL/APIKey/Model/EndpointType/Temperature）：业务
//     prompt/schema/模块预算/repair 纪律/配额记账主体（发起用户）零改动——路由是
//     基建选择不是业务变更；审计 llm_call_logs 逐请求记路由后的真实目标，业务
//     manifest 增 routed 声明（原配置 vs 路由目标双向可查）。
//   - **挂点=中央客户端两公开出口**（chatCompletion/chatCompletionStream，P0-5
//     applyCapabilityRouting 同款收口）：凭 chatMeta.Module 命中路由表；探针
//     （module=test）与未接 run 三件套的路径（Module 空）恒不路由。
//   - **消费 P0-5 能力矩阵（按任务选择结构化能力）**：结构化任务（JSONMode）不路由到
//     已知不支持 json_object 的目标（capabilitiesFor 观察优先）——champion 能结构化
//     的任务不因路由退化成 free_text；观察带 TTL，目标恢复后自动重新路由。
//   - **自动回退（§10.2 阈值）持久化且不自动恢复**：健康检查在路由缓存重载时评估
//     （天然节流），任一触发即写 AutoFallbackAt/AutoFallbackReason 停用该路由并
//     SysWarn——恶化原因必须有人看过，管理端显式恢复（防抖动）。三类信号：
//       1) 失败率：近窗口该 (module, 路由配置) 的 llm_call_logs error 占比（结构化
//          拒收/完整性门禁/网络失败都算——「准确率下降」的调用层代理指标）；
//       2) 成本：路由目标平均 token / 同模块其他配置平均 token > MaxCostRatio
//          （默认 1.35，§8.1「token 成本增加不超过 35%」）；
//       3) 校准（仅 recommendation）：校准分层 provider·model 维度里路由目标的 Brier
//          比最优已评估层恶化超 10%（§10.2；只读 CachedLLMCalibrationReport 缓存，
//          不在调用路径触发重算；两层都「已评估」才可比——样本不足不下结论）。
//   - **experiment 模块恒跟随 recommendation 路由**（llmRouteModuleAlias）：champion
//     主调与 challenger 影子必须打同一目标模型，否则 P2-1 单变量对照（§9.3）失效；
//     experiment 不允许单独建路由。
//   - flag `llm_model_routing` **缺省关**（行为开关非回滚开关）；路由表空/未启用/已
//     自动回退时逐调用零改写。锁定测试段隔离、影子纪律、challenger 缺省关均不受本层影响。

const (
	// llmRouteCacheTTL 路由缓存有效期：也是健康检查的最小间隔（重载时评估）。
	llmRouteCacheTTL = 30 * time.Second
	// llmRouteHealthWindow 健康统计回看窗口。
	llmRouteHealthWindow = 24 * time.Hour
	// llmRouteHealthMinCalls 失败率/成本判定的最小样本（小样本不下结论）。
	llmRouteHealthMinCalls = 5
	// llmRouteMaxErrorRate 失败率自动回退阈值。
	llmRouteMaxErrorRate = 0.30
	// llmRouteDefaultMaxCostRatio 成本比默认阈值（§8.1：token 成本增加不超过 35%）。
	llmRouteDefaultMaxCostRatio = 1.35
	// llmRouteBrierDegradeRatio 校准 Brier 恶化阈值（§10.2：相对 champion 恶化超 10%）。
	llmRouteBrierDegradeRatio = 1.10
)

// llmRouteModuleAlias 跟随路由的模块：challenger 影子必须与推荐主调同目标（单变量）。
var llmRouteModuleAlias = map[string]string{
	"experiment": "recommendation",
}

// LLMRouteApplied 一次调用的路由声明（llmRun 级观测，manifest 透出；llm_call_logs
// 逐请求已记录路由后的真实 config/model，本结构补「从哪来」的归因）。
type LLMRouteApplied struct {
	Applied      bool   `json:"applied"`
	RouteID      int64  `json:"route_id,omitempty"`
	ConfigID     int64  `json:"config_id,omitempty"`
	Provider     string `json:"provider,omitempty"`
	Model        string `json:"model,omitempty"`
	FromConfigID int64  `json:"from_config_id,omitempty"`
	FromModel    string `json:"from_model,omitempty"`
}

// llmRouteEntry 缓存里的一条可用路由（已通过健康检查、配置可用、密钥已解密）。
type llmRouteEntry struct {
	route  model.LLMModuleRoute
	cfg    model.LLMConfig
	apiKey string
}

var (
	llmRouteCacheMu sync.Mutex
	llmRouteCacheAt time.Time
	llmRouteCache   map[string]*llmRouteEntry // module -> entry（仅健康可用路由）
)

// invalidateLLMRouteCache 路由 CRUD 后清缓存（下次调用重载+重新健康检查）。
func invalidateLLMRouteCache() {
	llmRouteCacheMu.Lock()
	llmRouteCache = nil
	llmRouteCacheAt = time.Time{}
	llmRouteCacheMu.Unlock()
}

// lookupLLMRoute 取某模块的可用路由（缓存 TTL 内直读；过期重载全表并逐条健康检查）。
func lookupLLMRoute(module string) *llmRouteEntry {
	llmRouteCacheMu.Lock()
	defer llmRouteCacheMu.Unlock()
	if llmRouteCache == nil || time.Since(llmRouteCacheAt) > llmRouteCacheTTL {
		llmRouteCache = loadLLMRoutes()
		llmRouteCacheAt = time.Now()
	}
	return llmRouteCache[module]
}

// loadLLMRoutes 重载路由表：启用且未自动回退的行 → 解密配置 → 健康检查。
// 配置不可用（已删/所有者非启用管理员/密钥解密失败）与健康检查触发都持久化自动回退。
func loadLLMRoutes() map[string]*llmRouteEntry {
	out := map[string]*llmRouteEntry{}
	var routes []model.LLMModuleRoute
	if err := common.DB.Where("enabled = ? AND auto_fallback_at IS NULL", true).Find(&routes).Error; err != nil {
		common.SysWarn("模型路由表加载失败（本轮不路由）: %v", err)
		return out
	}
	for _, rt := range routes {
		var cfg model.LLMConfig
		if err := common.DB.First(&cfg, rt.ConfigID).Error; err != nil || !isEnabledAdmin(cfg.UserID) {
			persistRouteFallback(rt.ID, "config_unavailable: 路由目标配置已删或所有者非启用管理员")
			continue
		}
		key, err := common.Decrypt(cfg.APIKeyCipher)
		if err != nil || strings.TrimSpace(key) == "" {
			persistRouteFallback(rt.ID, "config_unavailable: 路由目标配置 API Key 不可用")
			continue
		}
		if reason, ok := evaluateLLMRouteHealth(rt, cfg); !ok {
			persistRouteFallback(rt.ID, reason)
			continue
		}
		out[rt.Module] = &llmRouteEntry{route: rt, cfg: cfg, apiKey: key}
	}
	return out
}

// persistRouteFallback 自动回退持久化（只置一次；管理端显式恢复才清除）。
func persistRouteFallback(routeID int64, reason string) {
	now := time.Now()
	if err := common.DB.Model(&model.LLMModuleRoute{}).
		Where("id = ? AND auto_fallback_at IS NULL", routeID).
		Updates(map[string]any{"auto_fallback_at": now, "auto_fallback_reason": truncateRunes(reason, 250)}).Error; err != nil {
		common.SysWarn("模型路由自动回退落库失败 route=%d: %v", routeID, err)
		return
	}
	common.SysWarn("模型路由自动回退 route=%d：%s（管理端显式恢复后才重新生效）", routeID, reason)
}

// llmRouteCallStats 近窗口 (module, config) 的调用统计。
type llmRouteCallStats struct {
	Total     int64   `json:"total"`
	Errors    int64   `json:"errors"`
	AvgTokens float64 `json:"avg_tokens"` // 成功调用的平均 total_tokens
	SuccessN  int64   `json:"success_n"`
}

// routeCallStats 查 llm_call_logs：configID>0 限定路由目标，configID=0 且 excludeID>0
// 表示「同模块其他配置」的基线。
func routeCallStats(module string, configID, excludeID int64) llmRouteCallStats {
	var s llmRouteCallStats
	since := time.Now().Add(-llmRouteHealthWindow)
	q := common.DB.Model(&model.LLMCallLog{}).Where("module = ? AND created_at > ?", module, since)
	if configID > 0 {
		q = q.Where("llm_config_id = ?", configID)
	} else if excludeID > 0 {
		q = q.Where("llm_config_id <> ?", excludeID)
	}
	type agg struct {
		Total    int64
		Errors   int64
		SumTok   float64
		SuccessN int64
	}
	var a agg
	if err := q.Select(
		"COUNT(*) AS total, " +
			"SUM(CASE WHEN status = 'error' THEN 1 ELSE 0 END) AS errors, " +
			"SUM(CASE WHEN status = 'success' THEN total_tokens ELSE 0 END) AS sum_tok, " +
			"SUM(CASE WHEN status = 'success' THEN 1 ELSE 0 END) AS success_n").
		Scan(&a).Error; err != nil {
		return s
	}
	s.Total, s.Errors, s.SuccessN = a.Total, a.Errors, a.SuccessN
	if a.SuccessN > 0 {
		s.AvgTokens = round2(a.SumTok / float64(a.SuccessN))
	}
	return s
}

// routeMaxCostRatio 路由行成本阈值（0=默认）。
func routeMaxCostRatio(rt model.LLMModuleRoute) float64 {
	if rt.MaxCostRatio > 0 {
		return rt.MaxCostRatio
	}
	return llmRouteDefaultMaxCostRatio
}

// routeCalibBrierPair 从校准分层缓存取（路由目标 Brier, 最优其他层 Brier）：
// 仅 recommendation 模块有此信号；只读缓存（nil=无报表不下结论）；两层都已评估
// （≥calibEvalMinSample 才产出 Brier）才可比。返回 ok=false 表示无可比数据。
func routeCalibBrierPair(module string, cfg model.LLMConfig) (routed, bestOther float64, ok bool) {
	if module != "recommendation" {
		return 0, 0, false
	}
	rep := CachedLLMCalibrationReport()
	if rep == nil {
		return 0, 0, false
	}
	key := calibProviderModelKey(calibBatchMeta{Provider: cfg.Provider, Model: cfg.Model}, true)
	if key == "" {
		return 0, 0, false
	}
	var routedP, bestP *float64
	for _, rc := range rep.Recommendation {
		if rc == nil {
			continue
		}
		for _, g := range rc.Slices {
			if g.Dim != "provider_model" {
				continue
			}
			for _, row := range g.Rows {
				if row.Brier == nil {
					continue
				}
				if row.Key == key {
					if routedP == nil || *row.Brier > *routedP {
						v := *row.Brier
						routedP = &v // 双 horizon 取更差者（保守）
					}
					continue
				}
				if bestP == nil || *row.Brier < *bestP {
					v := *row.Brier
					bestP = &v
				}
			}
		}
	}
	if routedP == nil || bestP == nil {
		return 0, 0, false
	}
	return *routedP, *bestP, true
}

// evaluateLLMRouteHealth 健康检查三信号（任一触发返回 ok=false 与机读原因）。
// 全部只读：llm_call_logs 聚合 + 校准报表缓存；样本不足不下结论（继续路由）。
func evaluateLLMRouteHealth(rt model.LLMModuleRoute, cfg model.LLMConfig) (string, bool) {
	routedStats := routeCallStats(rt.Module, rt.ConfigID, 0)
	// 1) 失败率（准确率下降的调用层代理：结构化拒收/完整性门禁/网络失败都进 error）。
	if routedStats.Total >= llmRouteHealthMinCalls {
		if rate := float64(routedStats.Errors) / float64(routedStats.Total); rate >= llmRouteMaxErrorRate {
			return fmt.Sprintf("error_rate: 近 24h 失败率 %.0f%%（%d/%d）≥ %.0f%%",
				rate*100, routedStats.Errors, routedStats.Total, llmRouteMaxErrorRate*100), false
		}
	}
	// 2) 成本比（对照=同模块其他配置的成功调用均值；双方都够样本才可比）。
	base := routeCallStats(rt.Module, 0, rt.ConfigID)
	if routedStats.SuccessN >= llmRouteHealthMinCalls && base.SuccessN >= llmRouteHealthMinCalls &&
		base.AvgTokens > 0 {
		if ratio := routedStats.AvgTokens / base.AvgTokens; ratio > routeMaxCostRatio(rt) {
			return fmt.Sprintf("cost_exceeded: 平均 token %.0f 是同模块其他配置 %.0f 的 %.2f 倍（阈值 %.2f）",
				routedStats.AvgTokens, base.AvgTokens, ratio, routeMaxCostRatio(rt)), false
		}
	}
	// 3) 校准分层 Brier（仅 recommendation；P2-5 provider·model 数据的路由消费点）。
	if routed, best, ok := routeCalibBrierPair(rt.Module, cfg); ok && best > 0 {
		if routed > best*llmRouteBrierDegradeRatio {
			return fmt.Sprintf("brier_degraded: 路由目标 Brier %.4f 比最优层 %.4f 恶化超 %.0f%%",
				routed, best, (llmRouteBrierDegradeRatio-1)*100), false
		}
	}
	return "", true
}

// applyModelRouting 中央客户端出口的模块级模型路由（P2-4）。必须在
// applyAccuracyContract/initCallObservers/applyCapabilityRouting **之前**调用：
// 温度钳制与能力路由都要作用于路由后的最终目标。零命中时原样返回（逐字节不变）。
func applyModelRouting(p chatParams) chatParams {
	if !setting.LLMModelRouting() || common.DB == nil {
		return p
	}
	module := p.Meta.Module
	if module == "" || module == "test" {
		return p
	}
	if alias, ok := llmRouteModuleAlias[module]; ok {
		module = alias
	}
	rt := lookupLLMRoute(module)
	if rt == nil || rt.cfg.ID == p.Meta.ConfigID {
		return p // 无路由 / 目标即当前配置：零改写
	}
	// 按任务选择结构化能力（P0-5 矩阵消费）：结构化任务不路由到已知不支持 json_object
	// 的目标——观察带 TTL，恢复 unknown 后自动重新路由；free_text 任务不受此限。
	if p.JSONMode {
		target := llmCapabilityTarget(rt.cfg.ID, rt.cfg.Provider, rt.cfg.BaseURL, rt.cfg.Model, rt.cfg.EndpointType)
		if capabilitiesFor(rt.cfg.Provider, target).JSONObject == capUnsupported {
			return p
		}
	}
	if p.Meta.RouteApplied != nil {
		*p.Meta.RouteApplied = LLMRouteApplied{
			Applied: true, RouteID: rt.route.ID,
			ConfigID: rt.cfg.ID, Provider: rt.cfg.Provider, Model: rt.cfg.Model,
			FromConfigID: p.Meta.ConfigID, FromModel: p.Model,
		}
	}
	p.BaseURL, p.APIKey = rt.cfg.BaseURL, rt.apiKey
	p.Model, p.EndpointType = rt.cfg.Model, rt.cfg.EndpointType
	p.Temperature = rt.cfg.Temperature
	// 输出预算取严：模块预算/原用户上限已并入 p.MaxTokens，路由配置更小时以路由配置为准
	//（预算是防超时护栏，路由不得放大它）。
	if rt.cfg.MaxTokens > 0 && (p.MaxTokens == 0 || rt.cfg.MaxTokens < p.MaxTokens) {
		p.MaxTokens = rt.cfg.MaxTokens
	}
	// AllowPrivate 按路由目标配置所有者重判（路由目标恒属启用管理员，llm 回退同款语义）。
	p.AllowPrivate = llmAllowPrivate(p.AllowPrivate, &rt.cfg)
	// 审计随目标走：llm_call_logs 逐请求记路由后的真实 config/provider/model。
	p.Meta.ConfigID = rt.cfg.ID
	p.Meta.Provider = rt.cfg.Provider
	return p
}

// ---------- 管理端 CRUD 与只读视图 ----------

// llmRoutableModules 可配路由的模块（预算表键集减 experiment——它恒跟随 recommendation）。
func llmRoutableModules() []string {
	out := make([]string, 0, len(llmModuleBudgets))
	for k := range llmModuleBudgets {
		if _, aliased := llmRouteModuleAlias[k]; aliased {
			continue
		}
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// LLMRouteModuleOption 路由模块下拉项（label 取角色 registry 中文名）。
type LLMRouteModuleOption struct {
	Module string `json:"module"`
	Label  string `json:"label"`
}

// LLMRouteHealthView 路由健康快照（管理端展示；与自动回退同一套统计口径）。
type LLMRouteHealthView struct {
	Routed        llmRouteCallStats `json:"routed"`
	Baseline      llmRouteCallStats `json:"baseline"`
	CostRatio     float64           `json:"cost_ratio"` // 0=样本不足未计算
	CalibBrier    *float64          `json:"calib_brier,omitempty"`
	CalibBestPeer *float64          `json:"calib_best_peer,omitempty"`
}

// LLMRouteView 一行路由的管理端视图。
type LLMRouteView struct {
	model.LLMModuleRoute
	ConfigName     string             `json:"config_name"`
	ConfigProvider string             `json:"config_provider"`
	ConfigModel    string             `json:"config_model"`
	ConfigMissing  bool               `json:"config_missing"`
	Health         LLMRouteHealthView `json:"health"`
}

// ListLLMRoutes 全部路由 + 可选模块 + 健康快照（只读；健康快照即时计算不写回退）。
func ListLLMRoutes() ([]LLMRouteView, []LLMRouteModuleOption, error) {
	var routes []model.LLMModuleRoute
	if err := common.DB.Order("module").Find(&routes).Error; err != nil {
		return nil, nil, err
	}
	views := make([]LLMRouteView, 0, len(routes))
	for _, rt := range routes {
		v := LLMRouteView{LLMModuleRoute: rt}
		var cfg model.LLMConfig
		if err := common.DB.First(&cfg, rt.ConfigID).Error; err != nil {
			v.ConfigMissing = true
		} else {
			v.ConfigName, v.ConfigProvider, v.ConfigModel = cfg.Name, cfg.Provider, cfg.Model
		}
		v.Health.Routed = routeCallStats(rt.Module, rt.ConfigID, 0)
		v.Health.Baseline = routeCallStats(rt.Module, 0, rt.ConfigID)
		if v.Health.Routed.SuccessN >= llmRouteHealthMinCalls && v.Health.Baseline.SuccessN >= llmRouteHealthMinCalls &&
			v.Health.Baseline.AvgTokens > 0 {
			v.Health.CostRatio = round2(v.Health.Routed.AvgTokens / v.Health.Baseline.AvgTokens)
		}
		if !v.ConfigMissing {
			if routed, best, ok := routeCalibBrierPair(rt.Module, cfg); ok {
				r, b := routed, best
				v.Health.CalibBrier, v.Health.CalibBestPeer = &r, &b
			}
		}
		views = append(views, v)
	}
	mods := make([]LLMRouteModuleOption, 0)
	for _, m := range llmRoutableModules() {
		label := m
		if a, ok := llmRoleAssets[m]; ok {
			label = a.Name
		}
		mods = append(mods, LLMRouteModuleOption{Module: m, Label: label})
	}
	return views, mods, nil
}

// LLMRouteInput 路由创建/更新入参（按 module upsert）。
type LLMRouteInput struct {
	Module       string  `json:"module"`
	ConfigID     int64   `json:"config_id"`
	Enabled      bool    `json:"enabled"`
	Note         string  `json:"note"`
	MaxCostRatio float64 `json:"max_cost_ratio"`
}

// UpsertLLMRoute 建/改路由：模块须在可路由集合内、目标配置须属启用管理员；
// 显式保存=清除自动回退状态（管理员已看过原因）。
func UpsertLLMRoute(in LLMRouteInput) (*model.LLMModuleRoute, error) {
	module := strings.ToLower(strings.TrimSpace(in.Module))
	if _, aliased := llmRouteModuleAlias[module]; aliased {
		return nil, errors.New("experiment 模块不可单独配路由（challenger 恒跟随 recommendation 路由，单变量纪律）")
	}
	if _, ok := llmModuleBudgets[module]; !ok {
		return nil, fmt.Errorf("未知模块 %q（须为预算表登记的业务模块）", in.Module)
	}
	var cfg model.LLMConfig
	if err := common.DB.First(&cfg, in.ConfigID).Error; err != nil {
		return nil, errors.New("路由目标 LLM 配置不存在")
	}
	if !isEnabledAdmin(cfg.UserID) {
		return nil, errors.New("路由目标配置须属启用状态的管理员（AllowPrivate 与稳定性语义）")
	}
	if in.MaxCostRatio < 0 {
		in.MaxCostRatio = 0
	}
	var rt model.LLMModuleRoute
	err := common.DB.Where("module = ?", module).First(&rt).Error
	if err != nil {
		rt = model.LLMModuleRoute{Module: module}
	}
	rt.ConfigID = in.ConfigID
	rt.Enabled = in.Enabled
	rt.Note = truncateRunes(strings.TrimSpace(in.Note), 250)
	rt.MaxCostRatio = in.MaxCostRatio
	rt.AutoFallbackAt = nil
	rt.AutoFallbackReason = ""
	if rt.ID > 0 {
		err = common.DB.Model(&model.LLMModuleRoute{}).Where("id = ?", rt.ID).Updates(map[string]any{
			"config_id": rt.ConfigID, "enabled": rt.Enabled, "note": rt.Note,
			"max_cost_ratio": rt.MaxCostRatio, "auto_fallback_at": nil, "auto_fallback_reason": "",
		}).Error
	} else {
		err = common.DB.Create(&rt).Error
	}
	if err != nil {
		return nil, err
	}
	invalidateLLMRouteCache()
	return &rt, nil
}

// DeleteLLMRoute 删除路由（回到默认配置链路）。
func DeleteLLMRoute(id int64) error {
	res := common.DB.Delete(&model.LLMModuleRoute{}, id)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("路由不存在")
	}
	invalidateLLMRouteCache()
	return nil
}

// ResetLLMRouteFallback 显式恢复自动回退的路由（管理员已阅原因）。
func ResetLLMRouteFallback(id int64) (*model.LLMModuleRoute, error) {
	var rt model.LLMModuleRoute
	if err := common.DB.First(&rt, id).Error; err != nil {
		return nil, errors.New("路由不存在")
	}
	if err := common.DB.Model(&model.LLMModuleRoute{}).Where("id = ?", id).
		Updates(map[string]any{"auto_fallback_at": nil, "auto_fallback_reason": ""}).Error; err != nil {
		return nil, err
	}
	rt.AutoFallbackAt = nil
	rt.AutoFallbackReason = ""
	invalidateLLMRouteCache()
	return &rt, nil
}
