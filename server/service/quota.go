package service

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"quantvista/common"
	"quantvista/model"
)

// 配额共用逻辑（次数制）：分析/推荐/问答/对比四个入口统一走这里，
// 语义见 model.UserQuota 注释。之前四个 service 各自的 getQuota/addUsage 已收敛到此。

// errQuotaExhausted 次数用尽：机读码 quota_exhausted，文案保持既有中文（前端/运维文案锚点）。
// 指针常量便于 errors.Is 与 compare 等调用方分支。
var errQuotaExhausted = &RefusalError{
	Code: RefusalQuotaExhausted,
	Msg:  "AI 次数配额已用尽，请联系管理员调整额度",
}

func getUserQuota(userID int64) (*model.UserQuota, error) {
	if common.DB == nil || userID <= 0 {
		return nil, errors.New("配额数据库或用户无效")
	}
	var q model.UserQuota
	if err := common.DB.Where("user_id = ?", userID).First(&q).Error; err == nil {
		return &q, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if err := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.UserQuota{UserID: userID}).Error; err != nil {
		return nil, err
	}
	q = model.UserQuota{}
	if err := common.DB.Where("user_id = ?", userID).First(&q).Error; err != nil {
		return nil, err
	}
	return &q, nil
}

// checkQuota 熔断检查：次数额度用尽即拒绝（0 = 不限）。用尽时返回 *RefusalError（code=quota_exhausted）。
func checkQuota(userID int64, contexts ...context.Context) error {
	if len(contexts) > 0 {
		if _, ok := currentManualQuotaAction(contexts[0], userID); ok {
			return nil
		}
	}
	q, err := getUserQuota(userID)
	if err != nil {
		return refusalErr(RefusalQuotaUnavailable, "AI 配额信息读取失败，请稍后重试")
	}
	if q.ActionLimit > 0 && q.ActionUsed >= q.ActionLimit {
		return errQuotaExhausted
	}
	return nil
}

type manualQuotaKey struct{}
type manualQuotaAction struct {
	userID int64
	epoch  int64
	used   atomic.Bool
	closed atomic.Bool
}

func currentManualQuotaAction(ctx context.Context, userID int64) (*manualQuotaAction, bool) {
	if ctx == nil {
		return nil, false
	}
	action, ok := ctx.Value(manualQuotaKey{}).(*manualQuotaAction)
	return action, ok && action.userID == userID && !action.closed.Load()
}

// 模型调用前原子占用一次额度；repair、角色复核及日报子链路共享同一动作。
// 没有取得任何模型响应或用量时释放预留，释放不受请求取消影响；重置代次防止冲减新用量。
func beginManualQuotaAction(ctx context.Context, userID int64) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := currentManualQuotaAction(ctx, userID); ok {
		return ctx, func() {}, nil
	}
	if common.DB == nil || userID <= 0 {
		return ctx, nil, refusalErr(RefusalQuotaUnavailable, "AI 配额信息读取失败，请稍后重试")
	}
	action := &manualQuotaAction{userID: userID}
	err := withJobResultTransaction(ctx, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.UserQuota{UserID: userID}).Error; err != nil {
			return err
		}
		var quota model.UserQuota
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ?", userID).First(&quota).Error; err != nil {
			return err
		}
		if quota.ActionLimit > 0 && quota.ActionUsed >= quota.ActionLimit {
			return errQuotaExhausted
		}
		action.epoch = quota.ActionEpoch
		return tx.Model(&model.UserQuota{}).Where("user_id = ?", userID).UpdateColumn("action_used", gorm.Expr("action_used + 1")).Error
	})
	if err != nil {
		if errors.Is(err, errQuotaExhausted) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return ctx, nil, err
		}
		return ctx, nil, refusalErr(RefusalQuotaUnavailable, "AI 配额预留失败，请稍后重试")
	}
	finish := func() {
		if !action.closed.CompareAndSwap(false, true) || action.used.Load() {
			return
		}
		refundCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := common.DB.WithContext(refundCtx).Model(&model.UserQuota{}).
			Where("user_id = ? AND action_epoch = ? AND action_used > 0", userID, action.epoch).
			UpdateColumn("action_used", gorm.Expr("action_used - 1")).Error; err != nil {
			common.SysWarn("未使用的 AI 配额预留释放失败 user=%d: %v", userID, err)
		}
	}
	return context.WithValue(ctx, manualQuotaKey{}, action), finish, nil
}

func noteManualQuotaResponse(ctx context.Context, res *chatResult) {
	if ctx == nil || res == nil || (res.Usage.TotalTokens <= 0 && res.Content == "") {
		return
	}
	if action, ok := ctx.Value(manualQuotaKey{}).(*manualQuotaAction); ok && !action.closed.Load() {
		action.used.Store(true)
	}
}

// consumeQuota 仅累计 token/请求审计；动作次数由调用前预留统一管理。
func consumeQuota(userID int64, tokens int) {
	updates := map[string]any{
		"token_used":    gorm.Expr("token_used + ?", tokens),
		"request_count": gorm.Expr("request_count + 1"),
	}
	common.DB.Model(&model.UserQuota{}).Where("user_id = ?", userID).Updates(updates)
}
