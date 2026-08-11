package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// SelectionEval GET /api/admin/selection-eval?refresh=1 —— S3-6B 统一 fixed-hold
// selection outcome 与同批配对评估。默认返回进程内缓存；refresh=1 推进 pending
// outcome 并重算。纯测量路径不产生 LLM 调用，也不改写推荐结果。
func (mc *MarketController) SelectionEval(c *gin.Context) {
	if c.Query("refresh") != "1" {
		if rep := service.CachedSelectionEvalReport(); rep != nil {
			common.ApiSuccess(c, rep)
			return
		}
	}
	rep, err := service.RunSelectionEval(c.Request.Context(), mc.svc)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}

// PositionExitOutcomes GET /api/admin/position-exit-outcomes —— 卖出评估前向结果
// 台账的跨用户脱敏聚合（level 对照 + 单信号预测力）。纯读、零 LLM；样本达标前
// 只作观察，不据此调 pea1 阈值。
func (mc *MarketController) PositionExitOutcomes(c *gin.Context) {
	rep, err := service.PositionExitOutcomeReport()
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}
