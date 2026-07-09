package controller

import (
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// BoardController 行业/概念板块：热力图 + 板块详情（M3c）。
type BoardController struct {
	svc *service.BoardService
}

func NewBoardController(svc *service.BoardService) *BoardController {
	return &BoardController{svc: svc}
}

// Heatmap GET /api/markets/:market/boards?kind=industry|concept
func (bc *BoardController) Heatmap(c *gin.Context) {
	if strings.ToLower(c.Param("market")) != "cn" {
		common.ApiErrorMsg(c, "板块热力图仅支持 A 股（cn）")
		return
	}
	kind := strings.ToLower(strings.TrimSpace(c.DefaultQuery("kind", "industry")))
	if kind != "industry" && kind != "concept" {
		common.ApiErrorMsg(c, "kind 仅支持 industry / concept")
		return
	}
	rows, err := bc.svc.Heatmap(c.Request.Context(), kind)
	if err != nil {
		common.ApiErrorMsg(c, "获取板块热度失败: "+err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}

// Detail GET /api/markets/:market/boards/:code
func (bc *BoardController) Detail(c *gin.Context) {
	if strings.ToLower(c.Param("market")) != "cn" {
		common.ApiErrorMsg(c, "板块详情仅支持 A 股（cn）")
		return
	}
	code := strings.TrimSpace(c.Param("code"))
	if code == "" {
		common.ApiErrorMsg(c, "板块代码不能为空")
		return
	}
	common.ApiSuccess(c, bc.svc.Detail(c.Request.Context(), code))
}
