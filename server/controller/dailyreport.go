package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// DailyReportController 收盘日报：列表 / 详情 / 最新 / 手动重生成。
type DailyReportController struct {
	svc *service.DailyReportService
}

func NewDailyReportController(svc *service.DailyReportService) *DailyReportController {
	return &DailyReportController{svc: svc}
}

// List GET /api/daily-reports?limit=
func (dc *DailyReportController) List(c *gin.Context) {
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := dc.svc.List(currentUserID(c), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Latest GET /api/daily-reports/latest —— 最新一份（无则 data=null）。
func (dc *DailyReportController) Latest(c *gin.Context) {
	v, err := dc.svc.Latest(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Get GET /api/daily-reports/:id
func (dc *DailyReportController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	v, err := dc.svc.Get(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Generate POST /api/daily-reports/generate —— 手动生成/重生成当日日报（计 1 次配额）。
func (dc *DailyReportController) Generate(c *gin.Context) {
	v, err := dc.svc.GenerateFor(c.Request.Context(), currentUserID(c), true)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Delete DELETE /api/daily-reports/:id —— 删除一份日报（生成中的任务拒删）。
func (dc *DailyReportController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := dc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
