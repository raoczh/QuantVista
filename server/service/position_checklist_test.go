package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestPositionUpdateChecklist checklist 用 *string 区分「未传」(nil，保留原值) 与「清空全部
// 勾选」(空串，显式写入)——旧的 string 空值判断把清空当作不更新（#13）。
func TestPositionUpdateChecklist(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM positions")
	svc := &PositionService{}

	p := model.Position{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		PositionType: model.PositionTypeShortTerm, Status: model.PositionStatusHolding,
		BuyPrice: 8.5, BuyDate: "2026-06-01", Quantity: 1000,
		ChecklistJSON: `{"a":true}`}
	if err := common.DB.Create(&p).Error; err != nil {
		t.Fatalf("建持仓失败: %v", err)
	}

	base := func(cl *string) PositionInput {
		return PositionInput{Market: "cn", PositionType: model.PositionTypeShortTerm,
			BuyPrice: 8.5, BuyDate: "2026-06-01", Quantity: 1000, ChecklistJSON: cl}
	}

	// 未传 checklist（nil）→ 保留原值。
	if _, err := svc.Update(1, p.ID, base(nil)); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	var got model.Position
	common.DB.First(&got, p.ID)
	if got.ChecklistJSON != `{"a":true}` {
		t.Fatalf("未传 checklist 应保留原值，得到 %q", got.ChecklistJSON)
	}

	// 传空串（用户取消全部勾选）→ 写入空串。
	empty := ""
	if _, err := svc.Update(1, p.ID, base(&empty)); err != nil {
		t.Fatalf("清空更新失败: %v", err)
	}
	common.DB.First(&got, p.ID)
	if got.ChecklistJSON != "" {
		t.Fatalf("空串应清空 checklist，得到 %q", got.ChecklistJSON)
	}

	// 传新值 → 更新。
	nv := `{"b":false}`
	if _, err := svc.Update(1, p.ID, base(&nv)); err != nil {
		t.Fatalf("更新新值失败: %v", err)
	}
	common.DB.First(&got, p.ID)
	if got.ChecklistJSON != nv {
		t.Fatalf("应更新为新值，得到 %q", got.ChecklistJSON)
	}
}
