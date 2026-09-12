package controller

import (
	"strings"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

type OnboardingController struct{}

func NewOnboardingController() *OnboardingController { return &OnboardingController{} }

func (oc *OnboardingController) Get(c *gin.Context) {
	view, err := service.GetOnboardingProgressContext(c.Request.Context(), currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func onboardingProgressID(c *gin.Context) (int64, bool) {
	var input struct {
		ProgressID int64 `json:"progress_id" binding:"required,gt=0"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		common.ApiErrorMsg(c, "请重新加载引导进度后再操作")
		return 0, false
	}
	return input.ProgressID, true
}

func (oc *OnboardingController) Skip(c *gin.Context) {
	id, ok := onboardingProgressID(c)
	if !ok {
		return
	}
	view, err := service.SkipOnboardingStepContext(c.Request.Context(), currentUserID(c), strings.TrimSpace(c.Param("step")), id)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Finish(c *gin.Context) {
	id, ok := onboardingProgressID(c)
	if !ok {
		return
	}
	view, err := service.FinishOnboardingContext(c.Request.Context(), currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Restart(c *gin.Context) {
	id, ok := onboardingProgressID(c)
	if !ok {
		return
	}
	view, err := service.RestartOnboardingContext(c.Request.Context(), currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Defer(c *gin.Context) {
	id, ok := onboardingProgressID(c)
	if !ok {
		return
	}
	view, err := service.DeferOnboardingContext(c.Request.Context(), currentUserID(c), id)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}
