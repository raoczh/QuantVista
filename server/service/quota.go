package service

import (
	"errors"

	"gorm.io/gorm"

	"quantvista/common"
	"quantvista/model"
)

// 配额共用逻辑（次数制）：分析/推荐/问答/对比四个入口统一走这里，
// 语义见 model.UserQuota 注释。之前四个 service 各自的 getQuota/addUsage 已收敛到此。

var errQuotaExhausted = errors.New("AI 次数配额已用尽，请联系管理员调整额度")

func getUserQuota(userID int64) (*model.UserQuota, error) {
	var q model.UserQuota
	if err := common.DB.FirstOrCreate(&q, model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

// checkQuota 熔断检查：次数额度用尽即拒绝（0 = 不限）。
func checkQuota(userID int64) error {
	q, err := getUserQuota(userID)
	if err != nil {
		return err
	}
	if q.ActionLimit > 0 && q.ActionUsed >= q.ActionLimit {
		return errQuotaExhausted
	}
	return nil
}

// consumeQuota 记账：token/请求轮次始终累计（审计）；manualAction=true 时次数 +1。
// 一次用户动作只允许调用一次 manualAction=true（不论内部发了多少次 LLM 请求）；
// 后台自动任务传 false，只记 token 不扣次。
func consumeQuota(userID int64, tokens int, manualAction bool) {
	updates := map[string]any{
		"token_used":    gorm.Expr("token_used + ?", tokens),
		"request_count": gorm.Expr("request_count + 1"),
	}
	if manualAction {
		updates["action_used"] = gorm.Expr("action_used + 1")
	}
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(updates)
}

// chargeAction 仅计 1 次手动动作，不动 token/请求轮次审计。
// 用于「一次动作内部的 LLM 记账已由各环节以 manualAction=false 完成」的场景（如手动重生成日报）。
func chargeAction(userID int64) {
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).
		Update("action_used", gorm.Expr("action_used + 1"))
}
