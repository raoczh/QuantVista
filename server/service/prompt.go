package service

import (
	"errors"
	"strings"

	"quantvista/common"
	"quantvista/model"
)

// PromptService 用户自定义分析提示词模板管理。启用后覆盖对应模块的默认分析维度指引。
type PromptService struct{}

func NewPromptService() *PromptService { return &PromptService{} }

const maxPromptContentRunes = 4000

// PromptModuleInfo 模块信息 + 默认指引（供前端展示与「重置为默认」参照）。
type PromptModuleInfo struct {
	Module  string `json:"module"`
	Label   string `json:"label"`
	Default string `json:"default"`
}

var promptModuleLabels = map[string]string{
	model.AnalysisModuleStock:     "个股分析",
	model.AnalysisModuleMarket:    "全市场分析",
	model.AnalysisModuleSector:    "板块分析",
	model.AnalysisModuleWatchlist: "自选股分析",
	model.AnalysisModulePosition:  "持仓分析",
}

// Modules 返回所有可自定义的模块及其默认指引。
func (s *PromptService) Modules() []PromptModuleInfo {
	order := []string{
		model.AnalysisModuleStock, model.AnalysisModuleMarket, model.AnalysisModuleSector,
		model.AnalysisModuleWatchlist, model.AnalysisModulePosition,
	}
	out := make([]PromptModuleInfo, 0, len(order))
	for _, m := range order {
		out = append(out, PromptModuleInfo{Module: m, Label: promptModuleLabels[m], Default: moduleGuidance[m]})
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
	if !validAnalysisModule[module] {
		return nil, errors.New("不支持的分析模块")
	}
	content := strings.TrimSpace(in.Content)
	if content == "" {
		return nil, errors.New("模板内容不能为空")
	}
	if len([]rune(content)) > maxPromptContentRunes {
		return nil, errors.New("模板内容过长")
	}

	var tpl model.PromptTemplate
	err := common.DB.Where("user_id = ? AND module = ?", userID, module).First(&tpl).Error
	if err != nil {
		tpl = model.PromptTemplate{UserID: userID, Module: module, Content: content, Enabled: in.Enabled}
		if err := common.DB.Create(&tpl).Error; err != nil {
			return nil, err
		}
		return &tpl, nil
	}
	tpl.Content = content
	tpl.Enabled = in.Enabled
	if err := common.DB.Save(&tpl).Error; err != nil {
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
