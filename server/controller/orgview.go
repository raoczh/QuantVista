package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// OrgViewController P3a 机构观点（研报评级/机构调研）。
type OrgViewController struct {
	svc *service.OrgViewService
}

func NewOrgViewController(svc *service.OrgViewService) *OrgViewController {
	return &OrgViewController{svc: svc}
}

// StockOrgView GET /api/markets/:market/stocks/:symbol/orgview?price=
// 个股详情「机构观点」块：评级分布/评级变动/目标价统计/调研密度汇总 + 明细列表。
// price 为现价（可选，前端从行情带过来，用于目标价偏离计算；缺省不算偏离）。
// 首次访问触发按需同步（研报 1~2 请求 + 调研 1 请求，冷却 1h），非 A 股口径返回空集。
func (oc *OrgViewController) StockOrgView(c *gin.Context) {
	price := 0.0
	if s := c.Query("price"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil && v > 0 {
			price = v
		}
	}
	common.ApiSuccess(c, oc.svc.Overview(c.Request.Context(), c.Param("market"), c.Param("symbol"), price))
}
