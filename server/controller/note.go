package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// NoteController 投资笔记/决策日志（均限当前登录用户）。
type NoteController struct {
	svc *service.NoteService
}

func NewNoteController(svc *service.NoteService) *NoteController {
	return &NoteController{svc: svc}
}

// List GET /api/notes?symbol=&market=&keyword=&limit=
func (nc *NoteController) List(c *gin.Context) {
	limit := 50
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	rows, err := nc.svc.List(currentUserID(c), c.Query("symbol"), c.Query("market"), c.Query("keyword"), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Create POST /api/notes
func (nc *NoteController) Create(c *gin.Context) {
	var in service.NoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	note, err := nc.svc.Create(c.Request.Context(), currentUserID(c), in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, note)
}

// Update PUT /api/notes/:id
func (nc *NoteController) Update(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	var in service.NoteInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	note, err := nc.svc.Update(c.Request.Context(), currentUserID(c), id, in)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, note)
}

// Delete DELETE /api/notes/:id
func (nc *NoteController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := nc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"deleted": true})
}
