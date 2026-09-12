package service

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
)

func TestRecommendationGetPropagatesRelatedReadFailures(t *testing.T) {
	setupTestDB(t)
	batch, _ := seedLinkFixture(t, 8818, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	for _, table := range []string{"recommendation_statuses", "positions"} {
		t.Run(table, func(t *testing.T) {
			failure := errors.New("推荐关联事实读取故障")
			const callback = "review_recommendation_related_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			view, err := (&RecommendationService{}).Get(8818, batch.ID)
			if !errors.Is(err, failure) || view != nil {
				t.Errorf("关联事实读取失败不得报告没有追踪或持仓: view=%+v err=%v", view, err)
			}
		})
	}
}

func TestRecommendationUnlinkedReadFailureIsNotEmpty(t *testing.T) {
	setupTestDB(t)
	_, rec := seedLinkFixture(t, 8819, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	failure := errors.New("同标的持仓读取故障")
	const callback = "review_recommendation_unlinked_read"
	if err := common.DB.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "positions" && strings.Contains(tx.Statement.SQL.String(), "symbol IN") {
			tx.AddError(failure)
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
	// 关联持仓查询成功，仅同标的软匹配查询失败，也不能回报未持仓。
	if view, err := (&RecommendationService{}).Get(8819, rec.BatchID); !errors.Is(err, failure) || view != nil {
		t.Errorf("持仓读取故障被包装成未持仓: view=%+v err=%v", view, err)
	}
}

func recommendationHoldingFromJSON(t *testing.T, view *RecommendationView) *RecPositionLink {
	t.Helper()
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Items []struct {
			Holding *RecPositionLink `json:"holding_position"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || len(decoded.Items) != 1 {
		t.Fatalf("详情结构异常: %s err=%v", raw, err)
	}
	return decoded.Items[0].Holding
}

func TestRecommendationHoldingDoesNotReplaceHistoricalLineage(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8820, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	first := seedHoldingWithLedger(t, 8820, rec.Symbol, 10, 100, 0, 0, "2026-08-20")
	second := seedHoldingWithLedger(t, 8820, rec.Symbol, 12, 200, 0, 0, "2026-08-21")
	if err := common.DB.Model(&first).Updates(map[string]any{"recommendation_id": rec.ID, "status": model.PositionStatusClosed}).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.Model(&second).Update("recommendation_id", rec.ID).Error; err != nil {
		t.Fatal(err)
	}
	view, err := (&RecommendationService{}).Get(8820, batch.ID)
	if err != nil || len(view.Items) != 1 {
		t.Fatalf("读取详情: view=%+v err=%v", view, err)
	}
	if historical := view.Items[0].Position; historical == nil || historical.PositionID != first.ID {
		t.Fatalf("不能改变历史实际收益所对应的最早持仓: %+v", historical)
	}
	if holding := recommendationHoldingFromJSON(t, view); holding == nil || holding.PositionID != second.ID {
		t.Errorf("历史已平仓记录遮住后来仍持有的关联持仓: %+v", holding)
	}
}

func TestRecommendationArchivedLineageIsHistoryOnly(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8821, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	account := model.PortfolioAccount{UserID: 8821, Name: "已归档真实账户", Kind: model.PortfolioKindReal, Status: model.PortfolioStatusArchived}
	if err := common.DB.Create(&account).Error; err != nil {
		t.Fatal(err)
	}
	position := seedHoldingWithLedger(t, 8821, rec.Symbol, 10, 100, 0, 0, "2026-08-20")
	if err := common.DB.Model(&position).Updates(map[string]any{"account_id": account.ID, "recommendation_id": rec.ID}).Error; err != nil {
		t.Fatal(err)
	}
	view, err := (&RecommendationService{}).Get(8821, batch.ID)
	if err != nil || len(view.Items) != 1 {
		t.Fatalf("读取详情: view=%+v err=%v", view, err)
	}
	if historical := view.Items[0].Position; historical == nil || historical.PositionID != position.ID {
		t.Fatal("归档账户的历史血缘必须保留")
	}
	if holding := recommendationHoldingFromJSON(t, view); holding != nil {
		t.Errorf("归档账户不能提供当前持仓决策入口: %+v", holding)
	}
}

func TestRecommendationSoftMatchDoesNotOfferOtherLineage(t *testing.T) {
	setupTestDB(t)
	batch, rec := seedLinkFixture(t, 8822, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	_, other := seedLinkFixture(t, 8822, rec.Symbol, model.RecTypeShortTerm, model.RecStatusSuccess)
	position := seedHoldingWithLedger(t, 8822, rec.Symbol, 10, 100, 0, 0, "2026-08-20")
	if err := common.DB.Model(&position).Update("recommendation_id", other.ID).Error; err != nil {
		t.Fatal(err)
	}
	view, err := (&RecommendationService{}).Get(8822, batch.ID)
	if err != nil || len(view.Items) != 1 {
		t.Fatalf("读取详情: view=%+v err=%v", view, err)
	}
	if got := view.Items[0].UnlinkedPosition; got != nil {
		t.Errorf("已关联另一推荐的持仓不能被称为未登记并提供无说明的补关联: %+v", got)
	}
}

func TestRecommendationLinkCandidatesPropagateRelatedFailures(t *testing.T) {
	setupTestDB(t)
	seedLinkFixture(t, 8831, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	for _, table := range []string{"recommendation_batches", "positions"} {
		t.Run(table, func(t *testing.T) {
			failure := errors.New("关联候选读取失败")
			const callback = "review_link_candidates_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			if rows, err := (&RecommendationService{}).RecommendationLinkCandidates(8831, "600519", "cn"); !errors.Is(err, failure) || rows != nil {
				t.Errorf("关联候选查询失败不能回报空候选或未关联: rows=%+v err=%v", rows, err)
			}
		})
	}
}

func TestPositionListPropagatesLineageFailure(t *testing.T) {
	setupTestDB(t)
	_, rec := seedLinkFixture(t, 8832, "600519", model.RecTypeShortTerm, model.RecStatusSuccess)
	p := seedHoldingWithLedger(t, 8832, rec.Symbol, 10, 100, 0, 0, "2026-08-20")
	if err := common.DB.Model(&p).Update("recommendation_id", rec.ID).Error; err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"recommendations", "recommendation_batches"} {
		t.Run(table, func(t *testing.T) {
			failure := errors.New("持仓来源读取失败")
			const callback = "review_position_lineage_read"
			if err := common.DB.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == table {
					tx.AddError(failure)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { common.DB.Callback().Query().Remove(callback) })
			market := NewMarketService(datasource.NewManagerWithAdapters(&recPreheatMarketAdapter{}))
			if rows, err := NewPositionService(market).ListByAccount(t.Context(), 8832, 0, model.PositionStatusHolding); !errors.Is(err, failure) || rows != nil {
				t.Errorf("来源推荐故障被当成无血缘: count=%d err=%v", len(rows), err)
			}
		})
	}
}
