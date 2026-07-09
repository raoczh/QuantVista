package service

import (
	"errors"
	"regexp"
	"strings"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// PromptService 用户自定义分析提示词模板管理。启用后覆盖对应模块的默认分析维度指引。
type PromptService struct{}

func NewPromptService() *PromptService { return &PromptService{} }

const maxPromptContentRunes = 4000

// PromptModuleInfo 模块信息 + 默认指引（供前端展示与「重置为默认」参照）。
type PromptModuleInfo struct {
	Module       string   `json:"module"`
	Label        string   `json:"label"`
	Default      string   `json:"default"`
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

// Modules 返回所有可自定义的模块及其默认指引。扩展模块的默认值取自各消费方的系统提示常量。
func (s *PromptService) Modules() []PromptModuleInfo {
	order := []string{
		model.AnalysisModuleStock, model.AnalysisModuleMarket, model.AnalysisModuleSector,
		model.AnalysisModuleWatchlist, model.AnalysisModulePosition,
		model.PromptModuleRecommend, model.PromptModuleDaily,
		model.PromptModuleQa, model.PromptModuleReview,
	}
	defaults := map[string]string{
		model.PromptModuleRecommend: recRoleIntro,
		model.PromptModuleDaily:     dailyReviewSystem,
		model.PromptModuleQa:        qaRoleIntro,
		model.PromptModuleReview:    analysisReviewSystem,
	}
	out := make([]PromptModuleInfo, 0, len(order))
	for _, m := range order {
		def, ok := defaults[m]
		if !ok {
			def = moduleGuidance[m]
		}
		out = append(out, PromptModuleInfo{
			Module: m, Label: promptModuleLabels[m], Default: def,
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

// Upsert 新建或更新某模块的模板（每用户每模块唯一）。
func (s *PromptService) Upsert(userID int64, in PromptInput) (*model.PromptTemplate, error) {
	module := strings.ToLower(strings.TrimSpace(in.Module))
	if !validPromptModule(module) {
		return nil, errors.New("不支持的提示词模块")
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil, errors.New("模板内容不能为空")
	}
	if len([]rune(content)) > maxPromptContentRunes {
		return nil, errors.New("模板内容过长")
	}

	// upsert 靠 (user_id, module) 唯一索引兜底并发：两请求同时未命中时，
	// 后到的 Create 冲突转为按键更新，而不是报「唯一约束冲突」。
	tpl := model.PromptTemplate{UserID: userID, Module: module, Content: content, Enabled: in.Enabled}
	err := common.DB.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "module"}},
		DoUpdates: clause.AssignmentColumns([]string{"content", "enabled", "updated_at"}),
	}).Create(&tpl).Error
	if err != nil {
		return nil, err
	}
	// 冲突更新路径不回填主键/时间戳，重查一次返回完整行。
	if err := common.DB.Where("user_id = ? AND module = ?", userID, module).First(&tpl).Error; err != nil {
		return nil, err
	}
	return &tpl, nil
}

// Delete 删除模板（恢复默认）。
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

// userPromptOverride 返回用户某模块启用的自定义指引；无则空串（调用方回退默认）。
func userPromptOverride(userID int64, module string) string {
	if common.DB == nil {
		return ""
	}
	var tpl model.PromptTemplate
	err := common.DB.Where("user_id = ? AND module = ? AND enabled = ?", userID, module, true).First(&tpl).Error
	if err != nil {
		return ""
	}
	return strings.TrimSpace(tpl.Content)
}

// promptPlaceholderRe 占位符形态：{{name}}（允许两侧空白，name 为小写字母/下划线）。
var promptPlaceholderRe = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

// renderPromptTemplate 占位符宽容渲染：vars 里有值的占位符替换为值；
// 未提供值/拼错的占位符保留原样（不报错不吞字），模板写错不至于让整段提示词失效。
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
// 各消费方（分析/推荐/日报/问答/复核）统一走它，custom=true 时版本号加 -custom 后缀归因。
func promptOverrideFor(userID int64, module string, vars map[string]string) (string, bool) {
	o := userPromptOverride(userID, module)
	if o == "" {
		return "", false
	}
	return renderPromptTemplate(o, vars), true
}

// promptVersionFor 统一的版本号归因后缀：该模块启用了自定义模板则 base+"-custom"。
// 推荐/日报/问答三处共用，别各自散写后缀拼接。
func promptVersionFor(userID int64, module, base string) string {
	if userPromptOverride(userID, module) != "" {
		return base + "-custom"
	}
	return base
}
