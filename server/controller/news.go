package controller

import (
	"strconv"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// NewsController 新闻/快讯查询。
type NewsController struct {
	svc *service.NewsService
}

func NewNewsController(svc *service.NewsService) *NewsController {
	return &NewsController{svc: svc}
}

// List GET /api/news?symbol=&source=&limit=
func (nc *NewsController) List(c *gin.Context) {
	limit := 0
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			limit = n
		}
	}
	rows, err := nc.svc.ListNews(c.Query("symbol"), c.Query("source"), limit)
	if err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	common.ApiSuccess(c, rows)
}
