package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/model"
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

// LLMJointEval GET /api/admin/llm-joint-eval?refresh=1&include_locked=1 —— P2-5
// 组合/回测联合评估。默认返回进程内缓存（常规视图，不含锁定段指标）；refresh=1 重算；
// include_locked=1 显式请求锁定段指标——恒重算不走缓存，且每次读取登记审计
// （§9.1「发布前只读一次」以可见审计落地）。
func (ac *AdminController) LLMJointEval(c *gin.Context) {
	includeLocked := c.Query("include_locked") == "1"
	if !includeLocked && c.Query("refresh") != "1" {
		if rep := service.CachedJointEvalReport(); rep != nil {
			common.ApiSuccess(c, rep)
			return
		}
	}
	rep, err := service.RunJointEval(includeLocked)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rep)
}

// ---------- P2-1/P2-2 champion/challenger 实验（llm_experiment.go） ----------

// LLMExperiments GET /api/admin/llm-experiments —— 实验列表（含 P2-2 谱系字段）。
func (ac *AdminController) LLMExperiments(c *gin.Context) {
	rows, err := service.ListLLMExperiments()
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// GetLLMExperiment GET /api/admin/llm-experiments/:id —— 实验详情 + 影子样本明细。
func (ac *AdminController) GetLLMExperiment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "非法的实验 id")
		return
	}
	exp, runs, err := service.LLMExperimentDetail(id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"experiment": exp, "runs": runs})
}

// CreateLLMExperiment POST /api/admin/llm-experiments —— 创建（draft，challenger 快照固化）。
func (ac *AdminController) CreateLLMExperiment(c *gin.Context) {
	var in service.LLMExperimentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	exp, warns, err := service.CreateLLMExperiment(currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"experiment": exp, "warnings": warns})
}

// LLMExperimentAction POST /api/admin/llm-experiments/:id/:action —— 状态机动作：
// start（单变量校验）/complete（P2-2 结论与失败原因）/promote（P1-9 发布质量门硬检）/
// abandon。
func (ac *AdminController) LLMExperimentAction(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		common.ApiErrorMsg(c, "非法的实验 id")
		return
	}
	var body struct {
		Conclusion    string `json:"conclusion"`
		FailureReason string `json:"failure_reason"`
	}
	_ = c.ShouldBindJSON(&body) // start/promote 无 body 合法
	var exp *model.LLMExperiment
	switch c.Param("action") {
	case "start":
		exp, err = service.StartLLMExperiment(id)
	case "complete":
		exp, err = service.CompleteLLMExperiment(id, body.Conclusion, body.FailureReason)
	case "promote":
		exp, err = service.PromoteLLMExperiment(id)
	case "abandon":
		exp, err = service.AbandonLLMExperiment(id, body.FailureReason)
	default:
		common.ApiErrorMsg(c, "未知动作")
		return
	}
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, exp)
}
