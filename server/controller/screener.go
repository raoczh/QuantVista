package controller

import (
	"net/http"
	"strconv"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// ScreenerController M1 条件树选股：策略广场 / 全市场扫描 / 自定义策略管理 / AI 白话建策略（P3c）。
type ScreenerController struct {
	svc *service.ScreenerService
	ai  *service.ScreenerAIService
}

func NewScreenerController(svc *service.ScreenerService, ai *service.ScreenerAIService) *ScreenerController {
	return &ScreenerController{svc: svc, ai: ai}
}

// Strategies GET /api/screener/strategies —— 内置策略 + 当前用户自定义 + 因子字典。
func (sc *ScreenerController) Strategies(c *gin.Context) {
	if c.Query("history") == "1" {
		strategyID, err := strconv.ParseInt(c.Query("strategy_id"), 10, 64)
		if err != nil || strategyID <= 0 {
			common.ApiErrorMsg(c, "strategy_id 无效")
			return
		}
		v, err := sc.svc.StrategyHistory(currentUserID(c), strategyID)
		if err != nil {
			common.ApiErrorMsg(c, err.Error())
			return
		}
		common.ApiSuccess(c, v)
		return
	}
	v, err := sc.svc.Strategies(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Scan POST /api/screener/scan —— 提交全市场扫描作业（strategy_key / strategy_id / tree 三选一）。
// 宽表过期时由后台作业同步重建（构建互斥，并发作业等待同一次构建）。
func (sc *ScreenerController) Scan(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req service.ScanRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	res, err := sc.svc.StartScanJob(currentUserID(c), req)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, res)
}

// Results GET /api/screener/results —— 本人的扫描结果事实列表，不加载请求和结果正文。
func (sc *ScreenerController) Results(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	rows, err := service.ListStrategyRuns(currentUserID(c), service.JobKindScreenerScan, limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, rows)
}

// Result GET /api/screener/results/:id —— 本人的单个扫描结果稳定深链。
func (sc *ScreenerController) Result(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	row, err := service.GetStrategyRun(currentUserID(c), service.JobKindScreenerScan, id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

// SaveStrategy POST /api/screener/strategies —— 新建/更新自定义策略。
func (sc *ScreenerController) SaveStrategy(c *gin.Context) {
	var req service.SaveStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	v, err := sc.svc.SaveStrategy(currentUserID(c), req)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, v)
}

// DeleteStrategy DELETE /api/screener/strategies/:id
func (sc *ScreenerController) DeleteStrategy(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := sc.svc.DeleteStrategy(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	// 归档是当前语义；保留 deleted=true，避免旧前端把成功响应误判为失败。
	common.ApiSuccess(c, gin.H{"archived": true, "deleted": true})
}

// Parse POST /api/screener/parse —— AI 白话建策略：自然语言解析为条件树（P3c）。
// 只生成不执行：树由用户在前端确认后才落编辑器/保存/扫描。
func (sc *ScreenerController) Parse(c *gin.Context) {
	var req service.ParseStrategyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	// allowPrivate：仅管理员可触达内网自建模型（与分析/问答一致，防 SSRF）。
	allowPrivate := currentRole(c) == model.RoleAdmin
	res, err := sc.ai.ParseStrategyAsync(currentUserID(c), allowPrivate, req)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, res)
}

// Status GET /api/screener/status —— 因子宽表状态（数据日期/覆盖数/是否构建中）。
func (sc *ScreenerController) Status(c *gin.Context) {
	common.ApiSuccess(c, service.FactorTableStatus())
}

// FactorRebuild POST /api/admin/market/factor-rebuild —— 手动异步重建因子宽表（管理员）。
func (sc *ScreenerController) FactorRebuild(c *gin.Context) {
	actor := currentUserID(c)
	job, started, err := service.StartSystemDataSyncJob(service.JobKindFactorRebuild, &actor, service.DataSyncJobRequest{
		Version: 1, Market: "cn", TriggerSource: "admin", ParameterSummary: "manual_rebuild=true", Reason: "管理端手动触发",
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"started": started, "task": service.JobKindFactorRebuild, "job_run_id": job.ID})
}
