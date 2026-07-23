package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// P1-7/P1-8 管理端只读报表（LLM_ACCURACY_OPTIMIZATION_PLAN §7.2）：
// 校准与后验标签报表 + 角色/提示词资产 registry。两者均纯测量/纯声明，零门控零改写。

// LLMCalibration GET /api/admin/llm-calibration?refresh=1 —— P1-7 校准报表。
// 默认返回进程内缓存；无缓存或 refresh=1 时重算（只读标签/记录/日线表，秒级，
// 全局互斥）。样本不足时 evaluated=false 如实声明「未评估」。
func (ac *AdminController) LLMCalibration(c *gin.Context) {
	if c.Query("refresh") != "1" {
		if rep := service.CachedLLMCalibrationReport(); rep != nil {
			common.ApiSuccess(c, rep)
			return
		}
	}
	rep, err := service.RunLLMCalibration(c.Request.Context())
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}

// LLMRoles GET /api/admin/llm-roles —— P1-8 角色资产 registry（代码内声明表的
// 只读透出：版本锚/schema/触发条件/白名单/禁止动作/预算/反例坐标+全局纪律）。
func (ac *AdminController) LLMRoles(c *gin.Context) {
	common.ApiSuccess(c, gin.H{
		"roles":       service.LLMRoleRegistry(),
		"disciplines": service.LLMRoleDisciplines(),
	})
}
