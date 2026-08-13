package controller

import (
	"errors"
	"strconv"
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type PortfolioController struct {
	accounts *service.PortfolioAccountService
	risk     *service.PortfolioRiskService
}

func NewPortfolioController(accounts *service.PortfolioAccountService, risk *service.PortfolioRiskService) *PortfolioController {
	return &PortfolioController{accounts: accounts, risk: risk}
}

func portfolioID(c *gin.Context) (int64, bool) { return parseIDParam(c, "id") }

func portfolioError(c *gin.Context, err error) {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		common.ApiErrorMsg(c, "组合不存在")
		return
	}
	common.ApiErrorMsg(c, err.Error())
}

func (pc *PortfolioController) List(c *gin.Context) {
	rows, err := pc.accounts.List(currentUserID(c))
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, rows)
}

func (pc *PortfolioController) Create(c *gin.Context) {
	var in service.PortfolioAccountInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	row, err := pc.accounts.Create(currentUserID(c), in)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Update(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	var in service.PortfolioAccountUpdate
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	row, err := pc.accounts.Update(currentUserID(c), id, in)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Archive(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	row, err := pc.accounts.Archive(currentUserID(c), id)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Default(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	row, err := pc.accounts.SetDefault(currentUserID(c), id)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Delete(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	if err := pc.accounts.Delete(currentUserID(c), id); err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"ok": true})
}

func (pc *PortfolioController) Overview(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	row, err := pc.risk.Overview(c.Request.Context(), currentUserID(c), id)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Risk(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	window, _ := strconv.Atoi(c.DefaultQuery("window", "252"))
	annualization, _ := strconv.Atoi(c.DefaultQuery("annualization", "252"))
	riskFree, parseErr := strconv.ParseFloat(c.DefaultQuery("risk_free_rate_pct", "0"), 64)
	if parseErr != nil || riskFree != riskFree || riskFree > 100 || riskFree < -100 {
		common.ApiErrorMsg(c, "无风险利率参数无效")
		return
	}
	if window < 30 || window > 730 || annualization < 1 || annualization > 1000 {
		common.ApiErrorMsg(c, "风险窗口或年化参数无效")
		return
	}
	asOf := strings.TrimSpace(c.Query("as_of"))
	if err := service.ValidatePortfolioRiskAsOf(asOf); err != nil {
		common.ApiErrorMsg(c, err.Error())
		return
	}
	params := service.NewPortfolioRiskParameters(window, annualization, riskFree, c.Query("benchmark"), asOf)
	row, err := pc.risk.Risk(c.Request.Context(), currentUserID(c), id, params)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Holdings(c *gin.Context) { pc.Overview(c) }

func (pc *PortfolioController) CashFlows(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	rows, err := service.ListPortfolioCashFlows(currentUserID(c), id)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, rows)
}

func (pc *PortfolioController) CreateCashFlow(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	var in service.CashFlowInput
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	row, err := service.CreatePortfolioCashFlow(currentUserID(c), id, in)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) ReverseCashFlow(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	flowID, ok := parseIDParam(c, "flow_id")
	if !ok {
		return
	}
	var in struct {
		IdempotencyKey string `json:"idempotency_key"`
		Note           string `json:"note"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	row, err := service.ReversePortfolioCashFlow(currentUserID(c), id, flowID, in.IdempotencyKey, in.Note)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Stress(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	var in service.StressScenario
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	row, err := pc.risk.Stress(c.Request.Context(), currentUserID(c), id, in)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Targets(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	revision, _ := strconv.Atoi(c.Query("revision"))
	row, items, err := service.LoadTargetRevision(currentUserID(c), id, revision)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			if _, accountErr := service.PortfolioAccountByID(currentUserID(c), id, ""); accountErr == nil {
				common.ApiSuccess(c, gin.H{"revision": nil, "items": []service.TargetAllocationItem{}})
				return
			}
		}
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"revision": row, "items": items})
}

func (pc *PortfolioController) SaveTargets(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	var in struct {
		Items []service.TargetAllocationItem `json:"items"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		common.ApiErrorMsg(c, "请求格式错误")
		return
	}
	row, err := pc.risk.SaveTargets(currentUserID(c), id, in.Items)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}

func (pc *PortfolioController) Rebalance(c *gin.Context) {
	id, ok := portfolioID(c)
	if !ok {
		return
	}
	revision, _ := strconv.Atoi(c.Query("revision"))
	row, err := pc.risk.Rebalance(c.Request.Context(), currentUserID(c), id, revision)
	if err != nil {
		portfolioError(c, err)
		return
	}
	common.ApiSuccess(c, row)
}
