package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
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
//（单一权威，别再另建平行清单——加新模块只需 labels/Modules order/placeholders 三处）。
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
	defaults := map[string]string{
		model.PromptModuleRecommend: recRoleTaskSeg,
		model.PromptModuleDaily:     dailyReviewTaskSeg,
		model.PromptModuleQa:        qaRoleTaskSeg,
		model.PromptModuleReview:    analysisReviewTaskSeg,
	}
	out := make([]PromptModuleInfo, 0, len(order))
	for _, m := range order {
		def, ok := defaults[m]
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

// promptContentHash 模板内容 hash（sha256 hex 前 16 位）。对原始模板内容（渲染前）计算——
// 渲染后含运行时变量值，同一模板不同标的会得到不同 hash，失去「归因到模板」的意义。
func promptContentHash(content string) string {
	if content == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])[:16]
}

// Upsert 新建或更新某模块的模板（每用户每模块唯一）。P0-6：内容变化时 Revision 递增并落
// PromptTemplateRevision 不可变快照（同内容重复保存只切 enabled，revision/hash 不动）；
// 返回的 warnings 为占位符/内容 lint 诊断（不阻断保存，前端展示）。
func (s *PromptService) Upsert(userID int64, in PromptInput) (*model.PromptTemplate, []string, error) {
	module := strings.ToLower(strings.TrimSpace(in.Module))
	if !validPromptModule(module) {
		return nil, nil, errors.New("不支持的提示词模块")
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil, nil, errors.New("模板内容不能为空")
	}
	if len([]rune(content)) > maxPromptContentRunes {
		return nil, nil, errors.New("模板内容过长")
	}
	warnings := lintPromptContent(module, content)
	hash := promptContentHash(content)

	var tpl model.PromptTemplate
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		err := tx.Where("user_id = ? AND module = ?", userID, module).First(&tpl).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			tpl = model.PromptTemplate{
				UserID: userID, Module: module, Content: content, Enabled: in.Enabled,
				ContentHash: hash, Revision: 1,
			}
			// 并发双建时后到者吃 (user_id,module) 唯一索引冲突报错（概率趋零，重试即可；
			// 旧版 OnConflict 无脑覆盖 content 会丢失「旧内容」，无法判定 revision 递增，故弃用）。
			if err := tx.Create(&tpl).Error; err != nil {
				return err
			}
			return tx.Create(&model.PromptTemplateRevision{
				TemplateID: tpl.ID, UserID: userID, Module: module,
				Revision: 1, ContentHash: hash, Content: content,
			}).Error
		}
		if err != nil {
			return err
		}
		oldHash := tpl.ContentHash
		if oldHash == "" {
			// 升级前旧行：hash 列为空，按现存内容补算再比较。
			oldHash = promptContentHash(tpl.Content)
		}
		if oldHash == hash && tpl.Revision > 0 {
			// 内容未变：只切 enabled，revision/快照不动。
			tpl.Enabled = in.Enabled
			tpl.ContentHash = hash
			return tx.Model(&model.PromptTemplate{}).Where("id = ?", tpl.ID).
				Updates(map[string]any{"enabled": in.Enabled, "content_hash": hash}).Error
		}
		// 内容变化（或旧行首次触碰补建归因）：revision 递增 + 落不可变快照。
		// 递增基准取快照表 MAX(revision) 与模板行 revision 的较大者——模板行字段与
		// 快照表失步（升级前旧行、手工改库）时不会撞 (template_id,revision) 唯一索引。
		var maxRev int
		if err := tx.Model(&model.PromptTemplateRevision{}).Where("template_id = ?", tpl.ID).
			Select("COALESCE(MAX(revision),0)").Scan(&maxRev).Error; err != nil {
			return err
		}
		if maxRev > tpl.Revision {
			tpl.Revision = maxRev
		}
		tpl.Content = content
		tpl.ContentHash = hash
		tpl.Revision++
		tpl.Enabled = in.Enabled
		if err := tx.Model(&model.PromptTemplate{}).Where("id = ?", tpl.ID).Updates(map[string]any{
			"content": content, "content_hash": hash, "revision": tpl.Revision, "enabled": in.Enabled,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&model.PromptTemplateRevision{
			TemplateID: tpl.ID, UserID: userID, Module: module,
			Revision: tpl.Revision, ContentHash: hash, Content: content,
		}).Error
	})
	if err != nil {
		return nil, warnings, err
	}
	return &tpl, warnings, nil
}

// Delete 删除模板（恢复默认）。历史 revision 快照不级联删——已落库调用的
// prompt_version hash8 归因链不能随模板删除断掉。
func (s *PromptService) Delete(userID, id int64) error {
	res := common.DB.Where("id = ? AND user_id = ?", id, userID).Delete(&model.PromptTemplate{})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("模板不存在")
	}
	return nil
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

// userPromptOverride 返回用户某模块启用的自定义指引；无则空串（调用方回退默认）。
func userPromptOverride(userID int64, module string) string {
	tpl := userPromptTemplateRow(userID, module)
	if tpl == nil {
		return ""
	}
	return strings.TrimSpace(tpl.Content)
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
//（保存时 lintPromptContent 已给出诊断警告）。
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

// promptOverrideFor 组合读取链：取用户自定义模板并做占位符渲染；无自定义返回 ("", false)。
// 各消费方（分析/推荐/日报/问答/复核）统一走它，custom=true 时版本号经 promptVersionFor
// 加 -custom.<hash8> 后缀归因。注意：四个扩展模块拿到返回值后必须经 composeCustomTaskPrompt
// 追加模块契约（L1 不可覆盖），不得直接整段当系统提示。
func promptOverrideFor(userID int64, module string, vars map[string]string) (string, bool) {
	o := userPromptOverride(userID, module)
	if o == "" {
		return "", false
	}
	return renderPromptTemplate(o, vars), true
}

// promptVersionFor 统一的版本号归因后缀：该模块启用了自定义模板则 base+"-custom."+hash8。
// hash8=模板内容 sha256 前 8 位（P0-6）：同名版本必对应同一模板内容——内容被编辑后
// 版本串随之变化，落库记录可凭 hash 在 prompt_template_revisions 回查当时原文。
// 升级前旧行 content_hash 为空，读取侧按现存内容现算（下次保存时回填）。
// 推荐/日报/问答/分析（含复核）共用，别各自散写后缀拼接。
func promptVersionFor(userID int64, module, base string) string {
	tpl := userPromptTemplateRow(userID, module)
	if tpl == nil {
		return base
	}
	h := tpl.ContentHash
	if h == "" {
		h = promptContentHash(tpl.Content)
	}
	if len(h) < 8 {
		return base + "-custom"
	}
	return base + "-custom." + h[:8]
}
