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

// ImportPositions POST /api/positions/import —— multipart 文件字段 file。
func (ec *ExportController) ImportPositions(c *gin.Context) {
	fh, err := c.FormFile("file")
	if err != nil {
		common.ApiErrorMsg(c, "请选择要导入的 CSV 文件")
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
