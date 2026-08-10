package controller

import (
	"net/http"
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

type WatchlistBatchController struct{}

func NewWatchlistBatchController() *WatchlistBatchController { return &WatchlistBatchController{} }

func (bc *WatchlistBatchController) Create(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	resultID, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var req service.WatchlistBatchRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	view, err := service.CreateWatchlistBatch(currentUserID(c), resultID, req)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "批量操作失败，请刷新后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (bc *WatchlistBatchController) Get(c *gin.Context) {
	view, err := service.GetWatchlistBatch(currentUserID(c), strings.TrimSpace(c.Param("id")))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "批量操作失败，请刷新后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (bc *WatchlistBatchController) Undo(c *gin.Context) {
	view, err := service.UndoWatchlistBatch(currentUserID(c), strings.TrimSpace(c.Param("id")))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "批量操作失败，请刷新后重试"))
		return
	}
	common.ApiSuccess(c, view)
}
