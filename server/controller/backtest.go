package controller

import (
	"net/http"
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// BacktestController M2 回测时光机：条件树策略回测 + 历史推荐批次回验。
// 纯本地计算不走 LLM，不扣配额；回测有全局互斥（进行中返回错误）。
type BacktestController struct {
	svc *service.BacktestService
}

func NewBacktestController(svc *service.BacktestService) *BacktestController {
	return &BacktestController{svc: svc}
}

// Run POST /api/backtest/run —— 提交条件树策略回测作业（strategy_key / strategy_id / tree 三选一）。
func (bc *BacktestController) Run(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req service.BacktestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	res, err := bc.svc.StartBacktestJob(currentUserID(c), req)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, res)
}

// Results GET /api/backtest/results —— 本人的策略回测结果事实列表，不加载正文。
func (bc *BacktestController) Results(c *gin.Context) {
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	rows, err := service.ListStrategyRuns(currentUserID(c), service.JobKindStrategyBacktest, limit)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, rows)
}

// Result GET /api/backtest/results/:id —— 本人的单个策略回测结果稳定深链。
func (bc *BacktestController) Result(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	row, err := service.GetStrategyRun(currentUserID(c), service.JobKindStrategyBacktest, id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

// Recommendations POST /api/backtest/recommendations —— 历史推荐批次回验（alpha 分布）。
// batch_id>0 单批次；=0 近 90 天全部成功批次聚合。
func (bc *BacktestController) Recommendations(c *gin.Context) {
	var req service.BatchBacktestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	res, err := bc.svc.BatchBacktest(c.Request.Context(), currentUserID(c), req)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}
