package controller

import (
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

const dataImportMaxBodyBytes = 2 << 20

type DataImportController struct{ svc *service.DataImportService }

func NewDataImportController(svc *service.DataImportService) *DataImportController {
	return &DataImportController{svc: svc}
}

func publicImportError(err error) string {
	return publicWorkflowError(err, "导入处理失败，请刷新批次后重试")
}

func publicWorkflowError(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	for _, internal := range []string{"sqlite", "mysql", "sqlstate", "constraint failed", "duplicate entry", "database is locked", "record not found", "no such table", "syntax error", "gorm", "driver:"} {
		if strings.Contains(lower, internal) {
			return fallback
		}
	}
	if len([]rune(msg)) > 250 {
		return fallback
	}
	hasUserText := false
	for _, r := range msg {
		if unicode.Is(unicode.Han, r) {
			hasUserText = true
			break
		}
	}
	if !hasUserText {
		return fallback
	}
	return msg
}

// Upload POST /api/imports，multipart: kind + file。
func (ctl *DataImportController) Upload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, dataImportMaxBodyBytes)
	fh, err := c.FormFile("file")
	if err != nil {
		common.ApiErrorMsg(c, "请选择 UTF-8 CSV 文件（文件不能超过 1 MiB）")
		return
	}
	f, err := fh.Open()
	if err != nil {
		common.ApiErrorMsg(c, "文件读取失败")
		return
	}
	defer f.Close()
	view, err := ctl.svc.UploadByAccount(currentUserID(c), optionalAccountID(c), c.PostForm("kind"), fh.Filename, f)
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	common.ApiSuccess(c, view)
}

func (ctl *DataImportController) Get(c *gin.Context) {
	view, err := ctl.svc.Get(currentUserID(c), c.Param("id"))
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	common.ApiSuccess(c, view)
}

func (ctl *DataImportController) List(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	rows, err := ctl.svc.List(currentUserID(c), limit)
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	common.ApiSuccess(c, rows)
}

func (ctl *DataImportController) Preview(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
	var in service.ImportMappingInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "列映射请求格式错误")
		return
	}
	view, err := ctl.svc.Preview(currentUserID(c), c.Param("id"), in)
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	common.ApiSuccess(c, view)
}

func (ctl *DataImportController) Confirm(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4<<10)
	var in service.ImportConfirmInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "确认请求格式错误")
		return
	}
	view, err := ctl.svc.Confirm(c.Request.Context(), currentUserID(c), c.Param("id"), in)
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	common.ApiSuccess(c, view)
}

func (ctl *DataImportController) Rollback(c *gin.Context) {
	result, err := ctl.svc.Rollback(currentUserID(c), c.Param("id"))
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	common.ApiSuccess(c, result)
}

func (ctl *DataImportController) Template(c *gin.Context) {
	data, filename, err := ctl.svc.Template(c.Param("kind"))
	if err != nil {
		common.ApiErrorMsg(c, publicImportError(err))
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", data)
}
