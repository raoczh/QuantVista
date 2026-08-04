package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

type StockSearchController struct {
	svc *service.StockSearchService
}

func NewStockSearchController(svc *service.StockSearchService) *StockSearchController {
	return &StockSearchController{svc: svc}
}

// Search GET /api/stocks/search?q=...&limit=...
func (sc *StockSearchController) Search(c *gin.Context) {
	limit := service.StockSearchDefaultLimit
	if raw := c.Query("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			common.ApiErrorMsg(c, "limit 须为整数")
			return
		}
		limit = parsed
	}
	result, err := sc.svc.Search(c.Request.Context(), currentUserID(c), c.Query("q"), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, result)
}
