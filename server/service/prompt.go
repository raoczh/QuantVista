package service

import (
	"errors"
	"regexp"
	"strings"
	"sync"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PromptService 用户自定义分析提示词模板管理。启用后覆盖对应模块的默认分析维度指引。
//
// P0-6 分层语义（docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §5.4）：自定义内容一律是 L3
// 任务段（关注角度/语气/排序偏好），L0 准确性契约（ac1，llm_contract.go 出口注入）、
// L1 模块契约（纪律/schema/输出纪律，本文件 composeCustomTaskPrompt 强制追加）、
// L2 冻结数据（各模块程序构造的快照）均不可被模板覆盖。存量 recommend/daily/qa/review
// 「整段替换」模板已降级为任务段注入（界面有迁移提示，Prompts.vue）。
type PromptService struct{}

func NewPromptService() *PromptService { return &PromptService{} }

const maxPromptContentRunes = 4000

// PromptModuleInfo 模块信息 + 默认指引（供前端展示与「重置为默认」参照）。
// Default 是可自定义的 L3 任务段默认值；Contract 是该模块不可覆盖的 L1 契约段
// （自定义时由系统自动追加在任务段之后；分析 5 模块的身份总则/输出规范由
// analysisSystemPrompt 组装保证，不在此展示故为空）。
type PromptModuleInfo struct {
	Module       string   `json:"module"`
	Label        string   `json:"label"`
	Default      string   `json:"default"`
	Contract     string   `json:"contract,omitempty"`
	Placeholders []string `json:"placeholders,omitempty"` // 模板可用占位符（{{name}} 形式）
}

var promptModuleLabels = map[string]string{
	model.AnalysisModuleStock:     "个股分析",
	model.AnalysisModuleMarket:    "全市场分析",
	model.AnalysisModuleSector:    "板块分析",
	model.AnalysisModuleWatchlist: "自选股分析",
	model.AnalysisModulePosition:  "持仓分析",
	model.PromptModuleRecommend:   "推荐（角色与铁律）",
	model.PromptModuleDaily:       "收盘日报（复盘）",
	model.PromptModuleQa:          "个股问答（角色）",
	model.PromptModuleReview:      "AI 复核（分析复核员）",
}

// validPromptModule 可自定义模板的模块合法性 = promptModuleLabels 的键集
// （单一权威，别再另建平行清单——加新模块只需 labels/Modules order/placeholders 三处）。
func validPromptModule(module string) bool {
	_, ok := promptModuleLabels[module]
	return ok
}

// promptModulePlaceholders 各模块模板可用的占位符（渲染宽容：未提供值的占位符保留原样）。
var promptModulePlaceholders = map[string][]string{
	model.AnalysisModuleStock:   {"market", "symbol", "target"},
	model.AnalysisModuleMarket:  {"market"},
	model.AnalysisModuleSector:  {"market", "target"},
	model.PromptModuleRecommend: {"type", "strategy", "market", "count"},
	model.PromptModuleDaily:     {"date"},
	model.PromptModuleQa:        {"symbol", "name", "market"},
	model.PromptModuleReview:    {"module"},
}

// promptModuleContracts 四个扩展模块的 L1 契约段（P0-6 不可覆盖边界的单一权威）：
// 自定义模板启用时由 composeCustomTaskPrompt 强制追加；默认路径的整段常量
// （recRoleIntro 等）由「任务段+契约段」编译期拼接而成，与拆分前逐字节一致。
// 分析 5 模块不在此表——它们的不可覆盖段（analysisRoleIntro/analysisOutputSpec）
// 由 analysisSystemPrompt 组装结构保证，自定义历来只替换中段 guidance。
var promptModuleContracts = map[string]string{
	model.PromptModuleRecommend: recPromptContract,
	model.PromptModuleDaily:     dailyReviewContract,
	model.PromptModuleQa:        qaPromptContract,
	model.PromptModuleReview:    analysisReviewContract,
}

// promptContractHeader L3 自定义任务段与 L1 模块契约的分界声明。
const promptContractHeader = "【系统契约】以下为本模块固定的纪律与输出要求，由系统自动追加，不可被上文自定义任务段覆盖；上文与以下内容冲突时，以下要求优先："

// composeCustomTaskPrompt 组装「用户自定义任务段（L3）+ 模块契约（L1）」。
// P0-6：自定义模板不再整段替换系统提示——纪律/schema/输出纪律恒由系统追加。
func composeCustomTaskPrompt(custom, contract string) string {
	if strings.TrimSpace(contract) == "" {
		return custom
	}
	return custom + "\n\n" + promptContractHeader + "\n" + contract
}

// promptModuleDefaultTaskSegs 四个扩展模块的默认 L3 任务段（单一权威）：Modules()
// 的「以默认为模板」展示与实验 champion 基线固化（llm_experiment.go）共用——默认
// champion 的「实际内容」就是这段任务段，发布审计不得用占位说明冒充（P2-6 审查修复批）。
var promptModuleDefaultTaskSegs = map[string]string{
	model.PromptModuleRecommend: recRoleTaskSeg,
	model.PromptModuleDaily:     dailyReviewTaskSeg,
	model.PromptModuleQa:        qaRoleTaskSeg,
	model.PromptModuleReview:    analysisReviewTaskSeg,
}

// Modules 返回所有可自定义的模块及其默认指引。扩展模块的默认值取自各消费方的
// 系统提示【任务段】（P0-6：不再返回整段——整段含契约，「以默认为模板」拷进
// 编辑框再保存会与系统追加的契约段重复）。
func (s *PromptService) Modules() []PromptModuleInfo {
	order := []string{
		model.AnalysisModuleStock, model.AnalysisModuleMarket, model.AnalysisModuleSector,
		model.AnalysisModuleWatchlist, model.AnalysisModulePosition,
		model.PromptModuleRecommend, model.PromptModuleDaily,
		model.PromptModuleQa, model.PromptModuleReview,
	}
	out := make([]PromptModuleInfo, 0, len(order))
	for _, m := range order {
		def, ok := promptModuleDefaultTaskSegs[m]
		if !ok {
			def = moduleGuidance[m]
		}
		out = append(out, PromptModuleInfo{
			Module: m, Label: promptModuleLabels[m], Default: def,
			Contract:     promptModuleContracts[m],
			Placeholders: promptModulePlaceholders[m],
		})
	}
	return out
}

// PromptInput 增改入参。
type PromptInput struct {
	Module  string `json:"module"`
	Content string `json:"content"`
	Enabled bool   `json:"enabled"`
}

// List 列出用户的模板。
func (s *PromptService) List(userID int64) ([]model.PromptTemplate, error) {
	var rows []model.PromptTemplate
	err := common.DB.Where("user_id = ?", userID).Order("module").Find(&rows).Error
	return rows, err
}

// promptContentHash 模板内容 hash：实现在 model.PromptContentHash（启动基线迁移与
// service 层必须共用同一实现，两份实现会造成归因 hash 漂移）。
func promptContentHash(content string) string {
	return model.PromptContentHash(content)
}

// Upsert 新建或更新某模块的模板（每用户每模块唯一）。P0-6：内容变化时 Revision 递增并落
// PromptTemplateRevision 不可变快照（同内容重复保存只切 enabled，revision/hash 不动）；
// 返回的 warnings 为占位符/内容 lint 诊断（不阻断保存，前端展示）。
func (s *PromptService) Upsert(userID int64, in PromptInput) (*model.PromptTemplate, []string, error) {
	module, content, hash, warnings, err := normalizePromptInput(in)
	if err != nil {
		return nil, warnings, err
	}
	promptExperimentStateMu.Lock()
	defer promptExperimentStateMu.Unlock()
	var tpl *model.PromptTemplate
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var txErr error
		tpl, txErr = upsertPromptTemplateTx(tx, userID, module, content, hash, in.Enabled, nil, false)
		return txErr
	})
	if err != nil {
		return nil, warnings, err
	}
	return tpl, warnings, nil
}

var errPromptTemplateConcurrent = errors.New("提示词模板已被并发修改，请重试")

// promptExperimentStateMu 只覆盖短数据库临界区。MySQL 的跨实例正确性由 epoch/实验行
// FOR UPDATE 保证；本锁补足 SQLite 没有行锁的方言差异，并让同进程模板变更与实验转移
// 使用同一把锁。外部 LLM 调用不得持有它。
var promptExperimentStateMu sync.Mutex

// withPromptExperimentState 持锁执行 fn 并 defer 释放。
//
// 手写 Lock ... Unlock 之间夹一个 GORM Transaction 是危险的：Transaction 对 panic
// 会回滚后**重新 panic**，而 gin 的 Recovery 中间件会吃掉 handler panic 让进程存活
// —— promptExperimentStateMu 就此永久不释放，之后全部 prompt 模板 Upsert/Delete
// 与所有实验/发布审计操作永久阻塞。统一走本函数，锁的释放不依赖控制流。
func withPromptExperimentState(fn func() error) error {
	promptExperimentStateMu.Lock()
	defer promptExperimentStateMu.Unlock()
	return fn()
}

func lockPromptChampionState(tx *gorm.DB, userID int64, module string) (*model.PromptChampionState, error) {
	var state model.PromptChampionState
	q := tx.Where("user_id = ? AND module = ?", userID, module)
	if tx.Dialector.Name() != "sqlite" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	err := q.First(&state).Error
	if err == nil {
		return &state, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.PromptChampionState{
		UserID: userID, Module: module,
	}).Error; err != nil {
		return nil, err
	}
	q = tx.Where("user_id = ? AND module = ?", userID, module)
	if tx.Dialector.Name() != "sqlite" {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	if err := q.First(&state).Error; err != nil {
		return nil, err
	}
	return &state, nil
}

func advancePromptChampionState(tx *gorm.DB, state *model.PromptChampionState) error {
	res := tx.Model(&model.PromptChampionState{}).
		Where("id = ? AND generation = ?", state.ID, state.Generation).
		UpdateColumn("generation", gorm.Expr("generation + 1"))
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return errPromptTemplateConcurrent
	}
	state.Generation++
	return nil
}

func promptChampionGeneration(db *gorm.DB, userID int64, module string) (int64, bool) {
	var state model.PromptChampionState
	if err := db.Where("user_id = ? AND module = ?", userID, module).First(&state).Error; err != nil {
		return 0, false
	}
	return state.Generation, true
}

func normalizePromptInput(in PromptInput) (module, content, hash string, warnings []string, err error) {
	module = strings.ToLower(strings.TrimSpace(in.Module))
	if !validPromptModule(module) {
		return "", "", "", nil, errors.New("不支持的提示词模块")
	}
	content = strings.TrimSpace(in.Content)
	if content == "" {
		return "", "", "", nil, errors.New("模板内容不能为空")
	}
	if len([]rune(content)) > maxPromptContentRunes {
		return "", "", "", nil, errors.New("模板内容过长")
	}
	warnings = lintPromptContent(module, content)
	return module, content, promptContentHash(content), warnings, nil
}

// sameStoredPromptRow 比较 CAS 所需的持久字段。UpdatedAt 不参与：内容、revision 与 enabled
// 已足以识别所有会改变运行时 prompt 的写入，避免 MySQL 时间精度差异造成误判。
func sameStoredPromptRow(a, b *model.PromptTemplate) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.ID == b.ID && a.UserID == b.UserID && a.Module == b.Module &&
		a.Content == b.Content && a.ContentHash == b.ContentHash &&
		a.Revision == b.Revision && a.Enabled == b.Enabled
}

func promptTemplateCAS(tx *gorm.DB, tpl *model.PromptTemplate) *gorm.DB {
	return tx.Model(&model.PromptTemplate{}).
		Where("id = ? AND user_id = ? AND module = ? AND content = ? AND content_hash = ? AND revision = ? AND enabled = ?",
			tpl.ID, tpl.UserID, tpl.Module, tpl.Content, tpl.ContentHash, tpl.Revision, tpl.Enabled)
}

// upsertPromptTemplateTx 是模板 revision 写入的事务内内核。requireExpected=true 时，
// expected（nil 表示期望仍无模板行）是调用方先前校验过的 CAS 锚；任何并发编辑都会令
// 条件更新/唯一键创建失败，外层事务随之整体回滚。SQL 只使用条件 UPDATE/INSERT，兼容
// SQLite 与 MySQL，不依赖 FOR UPDATE 方言。
func upsertPromptTemplateTx(tx *gorm.DB, userID int64, module, content, hash string, enabled bool,
	expected *model.PromptTemplate, requireExpected bool) (*model.PromptTemplate, error) {
	state, err := lockPromptChampionState(tx, userID, module)
	if err != nil {
		return nil, err
	}
	var tpl model.PromptTemplate
	findErr := tx.Where("user_id = ? AND module = ?", userID, module).First(&tpl).Error
	var current *model.PromptTemplate
	if findErr == nil {
		current = &tpl
	} else if !errors.Is(findErr, gorm.ErrRecordNotFound) {
		return nil, findErr
	}
	if requireExpected && !sameStoredPromptRow(current, expected) {
		return nil, errPromptTemplateConcurrent
	}
	if current == nil {
		tpl = model.PromptTemplate{
			UserID: userID, Module: module, Content: content, Enabled: enabled,
			ContentHash: hash, Revision: 1,
		}
		// 并发双建时唯一键保证只有一个提交；外层实验事务会连状态变更一起回滚。
		if err := tx.Create(&tpl).Error; err != nil {
			if requireExpected {
				return nil, errPromptTemplateConcurrent
			}
			return nil, err
		}
		if err := tx.Create(&model.PromptTemplateRevision{
			TemplateID: tpl.ID, UserID: userID, Module: module,
			Revision: 1, ContentHash: hash, Content: content,
		}).Error; err != nil {
			return nil, err
		}
		if err := advancePromptChampionState(tx, state); err != nil {
			return nil, err
		}
		return &tpl, nil
	}

	oldHash := tpl.ContentHash
	if oldHash == "" {
		// 升级前旧行：hash 列为空，按现存内容补算再比较。
		oldHash = promptContentHash(tpl.Content)
	}
	if oldHash == hash && tpl.Revision > 0 {
		// 内容未变：只切 enabled，revision/快照不动。完全相同的重复保存无需写库。
		if tpl.Enabled == enabled && tpl.ContentHash == hash {
			return &tpl, nil
		}
		res := promptTemplateCAS(tx, &tpl).
			Updates(map[string]any{"enabled": enabled, "content_hash": hash})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected != 1 {
			return nil, errPromptTemplateConcurrent
		}
		tpl.ContentHash = hash
		tpl.Enabled = enabled
		if err := advancePromptChampionState(tx, state); err != nil {
			return nil, err
		}
		return &tpl, nil
	}

	// 内容变化（或旧行首次触碰补建归因）：revision 递增 + 落不可变快照。
	var maxRev int
	if err := tx.Model(&model.PromptTemplateRevision{}).Where("template_id = ?", tpl.ID).
		Select("COALESCE(MAX(revision),0)").Scan(&maxRev).Error; err != nil {
		return nil, err
	}
	nextRev := tpl.Revision
	if maxRev > nextRev {
		nextRev = maxRev
	}
	nextRev++
	res := promptTemplateCAS(tx, &tpl).Updates(map[string]any{
		"content": content, "content_hash": hash, "revision": nextRev, "enabled": enabled,
	})
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, errPromptTemplateConcurrent
	}
	if err := tx.Create(&model.PromptTemplateRevision{
		TemplateID: tpl.ID, UserID: userID, Module: module,
		Revision: nextRev, ContentHash: hash, Content: content,
	}).Error; err != nil {
		return nil, err
	}
	if err := advancePromptChampionState(tx, state); err != nil {
		return nil, err
	}
	tpl.Content, tpl.ContentHash, tpl.Revision, tpl.Enabled = content, hash, nextRev, enabled
	return &tpl, nil
}

// Delete 删除模板（恢复默认）。历史 revision 快照不级联删——已落库调用的
// prompt_version hash8 归因链不能随模板删除断掉。
func (s *PromptService) Delete(userID, id int64) error {
	var probe model.PromptTemplate
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&probe).Error; err != nil {
		return errors.New("模板不存在")
	}
	promptExperimentStateMu.Lock()
	defer promptExperimentStateMu.Unlock()
	return common.DB.Transaction(func(tx *gorm.DB) error {
		state, err := lockPromptChampionState(tx, userID, probe.Module)
		if err != nil {
			return err
		}
		var current model.PromptTemplate
		if err := tx.Where("id = ? AND user_id = ? AND module = ?", id, userID, probe.Module).
			First(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("模板不存在")
			}
			return err
		}
		res := promptTemplateCAS(tx, &current).Delete(&model.PromptTemplate{})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errPromptTemplateConcurrent
		}
		return advancePromptChampionState(tx, state)
	})
}

// userPromptTemplateRow 用户某模块「启用中」的模板行；无则 nil。
func userPromptTemplateRow(userID int64, module string) *model.PromptTemplate {
	if common.DB == nil {
		return nil
	}
	var tpl model.PromptTemplate
	err := common.DB.Where("user_id = ? AND module = ? AND enabled = ?", userID, module, true).First(&tpl).Error
	if err != nil {
		return nil
	}
	return &tpl
}

// promptTemplateRowHash 模板行内容 hash（nil 安全：无行返回空串；升级前旧行
// content_hash 为空时按正文现算兜底）。实验的 champion 锚一致性/回滚校验与
// loadPromptRuntime 共用此实现——两处各算会造成归因漂移。
func promptTemplateRowHash(row *model.PromptTemplate) string {
	if row == nil {
		return ""
	}
	if row.ContentHash != "" {
		return row.ContentHash
	}
	return promptContentHash(row.Content)
}

// promptRuntime 一次业务运行的 prompt 模板不可变快照（P0-6 修复批）：同一模板行只读一次，
// 正文渲染、版本归因、run/manifest、业务表与 llm_call_logs 全部消费同一份数据——
// 消除「promptOverrideFor 与 promptVersionFor 分别查库，模板在两次查询间被编辑导致
// 实际正文与记录版本/hash 不一致」的竞态。异步任务（推荐 recGenPlan）把本结构固化进
// 计划，后台不得重新查库读另一版模板。
type promptRuntime struct {
	Module          string
	Custom          bool   // 是否命中启用中的自定义模板
	Raw             string // 模板原始内容（渲染前；Custom=false 为空）
	Hash            string // content hash（sha256 前 16；Custom=false 为空）
	Revision        int
	Generation      int64 // 用户/模块 champion 单调代际
	GenerationKnown bool  // false 仅表示尚无 state 行；实验创建会先建立该行
}

// loadPromptRuntime 一次查询固化用户某模块的模板快照。无启用模板返回零值（Custom=false，
// Render 回退默认、Version 回退裸 base）。升级前旧行 content_hash 为空时读取侧现算
// （启动迁移 MigratePromptTemplateBaselines 已回填，这里是双保险）。
func loadPromptRuntime(userID int64, module string) promptRuntime {
	generation, generationKnown := promptChampionGeneration(common.DB, userID, module)
	tpl := userPromptTemplateRow(userID, module)
	if tpl == nil {
		return promptRuntime{Module: module, Generation: generation, GenerationKnown: generationKnown}
	}
	h := promptTemplateRowHash(tpl)
	return promptRuntime{
		Module: module, Custom: true,
		Raw: strings.TrimSpace(tpl.Content), Hash: h, Revision: tpl.Revision,
		Generation: generation, GenerationKnown: generationKnown,
	}
}

// Render 渲染快照正文（占位符宽容渲染）。Custom=false 返回 ("", false)，调用方回退默认。
func (pr promptRuntime) Render(vars map[string]string) (string, bool) {
	if !pr.Custom || pr.Raw == "" {
		return "", false
	}
	return renderPromptTemplate(pr.Raw, vars), true
}

// Version 版本归因串：Custom 时 base+"-custom."+hash8（同一快照的正文与版本必然一致），
// 否则裸 base。
func (pr promptRuntime) Version(base string) string {
	if !pr.Custom {
		return base
	}
	if len(pr.Hash) < 8 {
		return base + "-custom"
	}
	return base + "-custom." + pr.Hash[:8]
}

// userPromptOverride 返回用户某模块启用的自定义指引；无则空串（调用方回退默认）。
func userPromptOverride(userID int64, module string) string {
	return loadPromptRuntime(userID, module).Raw
}

// promptPlaceholderRe 占位符形态：{{name}}（允许两侧空白，name 为小写字母/下划线）。
var promptPlaceholderRe = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

// promptPlaceholderAnyCaseRe 宽形态（lint 用）：任意大小写字母/下划线的双花括号内容。
var promptPlaceholderAnyCaseRe = regexp.MustCompile(`\{\{\s*([A-Za-z_]+)\s*\}\}`)

// lintPromptContent 保存时的占位符/内容诊断（P0-6「占位符错误可诊断」）：返回人话警告
// 列表，不阻断保存——运行时渲染保持宽容（未知占位符原样保留），但用户在保存时就能
// 看到「为什么我的占位符没生效」。纯函数可测。
func lintPromptContent(module, content string) []string {
	var warns []string
	allowed := promptModulePlaceholders[module]
	allowedSet := map[string]bool{}
	for _, p := range allowed {
		allowedSet[p] = true
	}
	allowedHint := "本模块无可用占位符"
	if len(allowed) > 0 {
		allowedHint = "本模块可用：{{" + strings.Join(allowed, "}}、{{") + "}}"
	}

	seen := map[string]bool{}
	for _, m := range promptPlaceholderAnyCaseRe.FindAllStringSubmatch(content, -1) {
		name := m[1]
		if seen[name] {
			continue
		}
		seen[name] = true
		lower := strings.ToLower(name)
		switch {
		case name != lower && allowedSet[lower]:
			warns = append(warns, "占位符 {{"+name+"}} 含大写字母，渲染只识别小写形态 {{"+lower+"}}，当前会原样保留")
		case !allowedSet[lower]:
			warns = append(warns, "未知占位符 {{"+name+"}}（"+allowedHint+"），渲染时会原样保留")
		}
	}
	// 单花括号疑似漏写：{name} 且 name 恰为本模块合法占位符词。
	singleRe := regexp.MustCompile(`([^{]|^)\{\s*([a-z_]+)\s*\}([^}]|$)`)
	for _, m := range singleRe.FindAllStringSubmatch(content, -1) {
		name := m[2]
		if allowedSet[name] && !seen["single:"+name] {
			seen["single:"+name] = true
			warns = append(warns, "检测到 {"+name+"}：占位符需要双层花括号 {{"+name+"}}，单层不会被渲染")
		}
	}
	// 四个分层模块：模板中疑似自带输出格式/schema 段（P0-6 起契约由系统追加，重复会
	// 让模型收到两份输出要求；若与系统契约矛盾则以系统契约为准，但重复本身浪费预算）。
	if _, layered := promptModuleContracts[module]; layered {
		for _, kw := range []string{"只输出 JSON", "只输出JSON", "输出严格 JSON", "输出严格JSON", "schema"} {
			if strings.Contains(content, kw) {
				warns = append(warns, "模板疑似包含输出格式/schema 要求（检测到「"+kw+"」）：本模块的输出契约由系统自动追加且不可覆盖，模板中无需再写，建议移除以免重复")
				break
			}
		}
	}
	return warns
}

// renderPromptTemplate 占位符宽容渲染：vars 里有值的占位符替换为值；
// 未提供值/拼错的占位符保留原样（不报错不吞字），模板写错不至于让整段提示词失效
// （保存时 lintPromptContent 已给出诊断警告）。
func renderPromptTemplate(content string, vars map[string]string) string {
	if content == "" || len(vars) == 0 {
		return content
	}
	return promptPlaceholderRe.ReplaceAllStringFunc(content, func(m string) string {
		key := promptPlaceholderRe.FindStringSubmatch(m)[1]
		if v, ok := vars[key]; ok {
			return v
		}
		return m
	})
}

// promptOverrideFor 兼容读取链（一次独立查询）：取用户自定义模板并做占位符渲染；无自定义
// 返回 ("", false)。⚠️ P0-6 修复批起，需要「正文与版本同源」的消费点（分析/推荐/日报/问答/
// 复核的业务链路）必须先 loadPromptRuntime 固化快照，再从同一快照 Render+Version——
// 本函数与 promptVersionFor 各自独立查库，成对使用存在模板中途更新的竞态，仅供
// 不落版本号的轻量场景与既有测试使用。
func promptOverrideFor(userID int64, module string, vars map[string]string) (string, bool) {
	return loadPromptRuntime(userID, module).Render(vars)
}

// promptVersionFor 兼容版本归因（一次独立查询）：base 或 base-custom.<hash8>。
// 同 promptOverrideFor 的警告：业务链路统一走 loadPromptRuntime 快照，勿与
// promptOverrideFor 成对分别查库。
func promptVersionFor(userID int64, module, base string) string {
	return loadPromptRuntime(userID, module).Version(base)
}
