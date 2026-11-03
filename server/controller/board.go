package controller

import (
	"errors"
	"strconv"
	"strings"

	"quantvista/common"
	"quantvista/datasource"
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
		common.SysWarn("板块热度读取失败 kind=%s: %v", kind, err)
		common.ApiErrorMsg(c, "板块主备数据源暂不可用，请在系统管理中检查 board_industry / board_concept 能力后重试")
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
	if _, valid := datasource.BoardCodeSource(code); !valid {
		common.ApiErrorMsg(c, "板块代码无效，请从热力图选择板块")
		return
	}
	common.ApiSuccess(c, bc.svc.Detail(c.Request.Context(), code))
}

// FundFlow GET /api/markets/:market/boards/:code/fundflow?days=90
// 板块资金流历史（P3b：上游透传+短缓存不落库）。
func (bc *BoardController) FundFlow(c *gin.Context) {
	if strings.ToLower(c.Param("market")) != "cn" {
		common.ApiErrorMsg(c, "板块资金流仅支持 A 股（cn）")
		return
	}
	code := strings.TrimSpace(c.Param("code"))
	if _, valid := datasource.BoardCodeSource(code); !valid {
		common.ApiErrorMsg(c, "板块代码无效，请从热力图选择板块")
		return
	}
	days, _ := strconv.Atoi(c.DefaultQuery("days", "90"))
	view, err := bc.svc.FundFlow(c.Request.Context(), code, days)
	if err != nil {
		if errors.Is(err, datasource.ErrNotSupported) {
			common.ApiErrorMsg(c, "当前分类源不提供对应板块资金流，不能用其他分类的资金流代替")
			return
		}
		common.SysWarn("板块资金流读取失败 code=%s: %v", code, err)
		common.ApiErrorMsg(c, "板块资金流数据源暂不可用，请稍后重试")
		return
	}
	common.ApiSuccess(c, view)
}
