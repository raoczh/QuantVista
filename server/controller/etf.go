package controller

import (
	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// EtfController 指数 ETF 清单（公开清单 + 实时行情富化）。
type EtfController struct {
	svc *service.EtfService
}

func NewEtfController(svc *service.EtfService) *EtfController {
	return &EtfController{svc: svc}
}

// List GET /api/etf/list —— 精选指数 ETF 清单 + 实时现价/涨跌幅。
func (ec *EtfController) List(c *gin.Context) {
	common.ApiSuccess(c, ec.svc.List(c.Request.Context()))
}
