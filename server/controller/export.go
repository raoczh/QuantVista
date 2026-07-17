package controller

import (
	"net/http"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// ExportController 用户数据 CSV 导出与持仓 CSV 导入（均限当前登录用户）。
type ExportController struct {
	svc *service.ExportService
}

func NewExportController(svc *service.ExportService) *ExportController {
	return &ExportController{svc: svc}
}

// Export GET /api/export/:kind —— kind=positions|watchlist|recommendations|analyses。
// 返回 text/csv（带 UTF-8 BOM），浏览器直接下载。
func (ec *ExportController) Export(c *gin.Context) {
	data, filename, err := ec.svc.Export(currentUserID(c), c.Param("kind"))
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Data(http.StatusOK, "text/csv; charset=utf-8", data)
}

// importMaxBodyBytes multipart 导入请求体硬上限：略大于 service 侧 importMaxSize（1MiB），
// 留出 multipart 边界/表单字段开销。防止 FormFile 触发的 ParseMultipartForm 把超大包吃进
// 内存+临时盘（gin 默认 32MiB）。
const importMaxBodyBytes = 2 << 20

// ImportPositions POST /api/positions/import —— multipart 文件字段 file。
func (ec *ExportController) ImportPositions(c *gin.Context) {
	// 前置限流：在 FormFile（会触发 ParseMultipartForm 解析）之前给整个请求体封顶，
	// 超限直接被 MaxBytesReader 截断报错，不落临时盘。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, importMaxBodyBytes)
	fh, err := c.FormFile("file")
	if err != nil {
		common.ApiErrorMsg(c, "请选择要导入的 CSV 文件（或文件过大）")
		return
	}
	f, err := fh.Open()
	if err != nil {
		common.ApiErrorMsg(c, "文件读取失败")
		return
	}
	defer f.Close()
	res, err := ec.svc.ImportPositions(currentUserID(c), f)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, res)
}
