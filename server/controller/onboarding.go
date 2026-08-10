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
	view, err := service.GetOnboardingProgress(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Skip(c *gin.Context) {
	view, err := service.SkipOnboardingStep(currentUserID(c), strings.TrimSpace(c.Param("step")))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Finish(c *gin.Context) {
	view, err := service.FinishOnboarding(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Restart(c *gin.Context) {
	view, err := service.RestartOnboarding(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}

func (oc *OnboardingController) Defer(c *gin.Context) {
	view, err := service.DeferOnboarding(currentUserID(c))
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "引导进度处理失败，请稍后重试"))
		return
	}
	common.ApiSuccess(c, view)
}
