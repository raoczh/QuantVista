package controller

import (
	"context"
	"net/http"
	"time"

	"quantvista/common"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

// RankingResearch 只读评估，不推进标签、不补拉行情，也不调用 AI。
func (mc *MarketController) RankingResearch(c *gin.Context) {
	var req service.RankingResearchRequest
	if err := c.ShouldBindQuery(&req); err != nil {
		common.ApiErrorMsg(c, "研究参数格式无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	rep, err := service.RunRankingResearch(ctx, common.DB, req)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "排序研究读取失败，请缩小范围后重试"))
		return
	}
	common.ApiSuccess(c, rep)
}

func (mc *MarketController) RankingPolicyState(c *gin.Context) {
	state, err := service.ReadRankingPolicyState(c.Request.Context(), common.DB)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "评分版本读取失败"))
		return
	}
	common.ApiSuccess(c, state)
}

func (mc *MarketController) CaptureRankingArtifact(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 16<<10)
	var req struct {
		Request     service.RankingResearchRequest `json:"request"`
		DatasetHash string                         `json:"dataset_hash"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "模型保存参数无效")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	artifact, err := service.CaptureRankingArtifact(ctx, common.DB, currentUserID(c), req.Request, req.DatasetHash)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "学习模型保存失败"))
		return
	}
	common.ApiSuccess(c, artifact)
}

func (mc *MarketController) UpdateRankingPolicy(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 8<<10)
	var req service.RankingPolicyUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		common.ApiErrorMsg(c, "评分政策参数无效")
		return
	}
	policy, err := service.UpdateRankingPolicy(c.Request.Context(), common.DB, currentUserID(c), req)
	if err != nil {
		common.ApiErrorMsg(c, publicWorkflowError(err, "评分版本更新失败"))
		return
	}
	common.ApiSuccess(c, policy)
}
