package controller

import (
	"encoding/json"
	"net/http"
	"strconv"

	"quantvista/common"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// QaController 个股 AI 问答（限当前登录用户）。
type QaController struct {
	svc *service.QaService
}

func NewQaController(svc *service.QaService) *QaController {
	return &QaController{svc: svc}
}

// Ask POST /api/qa/ask —— 新建会话或在已有会话上追问。
func (qc *QaController) Ask(c *gin.Context) {
	var req service.QaAskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	allowPrivate := currentRole(c) == model.RoleAdmin
	v, err := qc.svc.Ask(c.Request.Context(), currentUserID(c), allowPrivate, req)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// qaStreamLine 流式问答 NDJSON 协议行（S1）。code 字段现在就进协议：单标的留空，
// 为横向对比/批量场景预留（届时按标的代码区分行归属）。
type qaStreamLine struct {
	Module  string                      `json:"module"`
	Code    string                      `json:"code"`
	Status  string                      `json:"status"` // streaming / done / error
	Chunk   string                      `json:"chunk,omitempty"`
	Message string                      `json:"message,omitempty"`
	Data    *service.QaConversationView `json:"data,omitempty"`
}

// AskStream POST /api/qa/ask-stream —— 流式问答：application/x-ndjson 逐行推
// {module,code,chunk,status}；X-Accel-Buffering:no 防反代整段缓冲。流结束后推
// status=done 行携带完整会话视图（含后置核验 CheckJSON），前端据此替换本地状态。
func (qc *QaController) AskStream(c *gin.Context) {
	var req service.QaAskRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	allowPrivate := currentRole(c) == model.RoleAdmin

	c.Header("Content-Type", "application/x-ndjson; charset=utf-8")
	c.Header("Cache-Control", "no-cache")
	c.Header("X-Accel-Buffering", "no") // nginx 反代下禁用响应缓冲，保证逐行到达
	flusher, _ := c.Writer.(http.Flusher)
	writeLine := func(line qaStreamLine) {
		line.Module = "qa"
		b, err := json.Marshal(line)
		if err != nil {
			return
		}
		_, _ = c.Writer.Write(append(b, '\n'))
		if flusher != nil {
			flusher.Flush()
		}
	}

	v, err := qc.svc.AskStream(c.Request.Context(), currentUserID(c), allowPrivate, req, func(chunk string) {
		writeLine(qaStreamLine{Status: "streaming", Chunk: chunk})
	})
	if err != nil {
		// 首字节前的失败（配额/配置/快照）与流中断都走同一 error 行；
		// 此时可能已写过 200 头，无法降级为标准包络，前端按行协议处理。
		writeLine(qaStreamLine{Status: "error", Message: err.Error()})
		return
	}
	writeLine(qaStreamLine{Status: "done", Data: v})
}

// List GET /api/qa?limit=
func (qc *QaController) List(c *gin.Context) {
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := qc.svc.List(currentUserID(c), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Get GET /api/qa/:id
func (qc *QaController) Get(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	v, err := qc.svc.Get(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, v)
}

// Snapshot GET /api/qa/:id/snapshot —— 会话固定数据快照原文（透明面板）。
func (qc *QaController) Snapshot(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	snap, err := qc.svc.Snapshot(currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"data_snapshot": snap})
}

// Delete DELETE /api/qa/:id
func (qc *QaController) Delete(c *gin.Context) {
	id, ok := parseIDParam(c, "id")
	if !ok {
		return
	}
	if err := qc.svc.Delete(currentUserID(c), id); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}
