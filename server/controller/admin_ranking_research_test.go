package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
)

func TestRankingResearchHTTPReadsEmptySchemaWithoutMigration(t *testing.T) {
	db := reviewControllerDB(t, &model.TradingCalendar{})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/ranking-research", nil)
	(&MarketController{}).RankingResearch(c)
	var response struct {
		Success bool                          `json:"success"`
		Data    service.RankingResearchReport `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Success || response.Data.PromotionReady || response.Data.Reason == "" {
		t.Fatalf("空样本应准确报告不足: %s", w.Body.String())
	}
	if db.Migrator().HasTable(&model.BenchmarkDaily{}) || db.Migrator().HasTable(&model.RankingModelArtifact{}) {
		t.Fatal("读取研究不能迁移或建立表")
	}
}

func TestRankingPolicyHTTPRejectsBadPayloadWithoutWriting(t *testing.T) {
	db := reviewControllerDB(t, &model.RankingScoringPolicy{}, &model.RankingPolicyAudit{}, &model.RankingModelArtifact{})
	for _, body := range []string{`{"rec_type":"short_term","profile":"momentum","algorithm":"future"}`, `{"rec_type":"short_term","profile":"momentum","algorithm":"qr1","base_revision":"wrong"}`, `{"rec_type":"short_term","profile":"momentum","algorithm":"ridge1","artifact_id":888}`} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("uid", int64(1))
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/ranking-policy", strings.NewReader(body))
		(&MarketController{}).UpdateRankingPolicy(c)
		var response struct {
			Success bool `json:"success"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Success {
			t.Fatalf("无效版本或模型请求不能成功: %s", w.Body.String())
		}
	}
	for _, m := range []any{&model.RankingScoringPolicy{}, &model.RankingPolicyAudit{}, &model.RankingModelArtifact{}} {
		var n int64
		if err := db.Model(m).Count(&n).Error; err != nil || n != 0 {
			t.Fatal("失败请求不得创建版本或审计")
		}
	}
}

func TestRankingArtifactHTTPCannotUploadClaimedModel(t *testing.T) {
	db := reviewControllerDB(t, &model.TradingCalendar{}, &model.RankingModelArtifact{})
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("uid", int64(1))
	body := `{"request":{"source":"recommendations"},"dataset_hash":"` + strings.Repeat("a", 64) + `","eligible":true,"model":{"weights":[999]}}`
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/ranking-artifacts", strings.NewReader(body))
	(&MarketController{}).CaptureRankingArtifact(c)
	var response struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.Model(&model.RankingModelArtifact{}).Count(&n).Error; err != nil {
		t.Fatal(err)
	}
	if response.Success || n != 0 {
		t.Fatal("客户端声明通过或自行上传系数不能替代服务端重新验证")
	}
}
