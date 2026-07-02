package service

import (
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// TestThesisCRUDAndIsolation 逻辑卡 upsert 唯一性、状态机与用户隔离。
func TestThesisCRUDAndIsolation(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM thesis_cards")

	// 直接落库两张卡（绕过 Upsert 的行情校验，聚焦 DB 行为）。
	c1 := model.ThesisCard{UserID: 1, Symbol: "600000", Market: "cn", Name: "浦发银行",
		Thesis: "低估值银行修复", KillSwitches: "跌破净资产折价扩大", Status: model.ThesisStatusActive}
	if err := common.DB.Create(&c1).Error; err != nil {
		t.Fatalf("建卡失败: %v", err)
	}
	common.DB.Create(&model.ThesisCard{UserID: 2, Symbol: "600000", Market: "cn",
		Thesis: "他人的卡", Status: model.ThesisStatusActive})

	svc := &ThesisService{}

	// 同标的同用户唯一：重复创建应违反唯一索引。
	dup := model.ThesisCard{UserID: 1, Symbol: "600000", Market: "cn", Thesis: "重复", Status: model.ThesisStatusActive}
	if err := common.DB.Create(&dup).Error; err == nil {
		t.Fatal("同用户同标的重复建卡应失败（唯一索引）")
	}

	// 列表隔离。
	rows, err := svc.List(1, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("用户1 应只有 1 张卡: %v %d", err, len(rows))
	}

	// GetBySymbol 命中与未命中。
	card, _ := svc.GetBySymbol(1, "600000", "cn")
	if card == nil || card.ID != c1.ID {
		t.Fatal("GetBySymbol 应命中自己的卡")
	}
	none, _ := svc.GetBySymbol(1, "000001", "cn")
	if none != nil {
		t.Fatal("无卡标的应返回 nil")
	}

	// 状态机：失效需带原因；恢复 active 清空原因。
	got, err := svc.SetStatus(1, c1.ID, model.ThesisStatusInvalidated, "逻辑被证伪")
	if err != nil || got.InvalidReason != "逻辑被证伪" {
		t.Fatalf("置失效失败: %v", err)
	}
	got, err = svc.SetStatus(1, c1.ID, model.ThesisStatusActive, "")
	if err != nil || got.InvalidReason != "" {
		t.Fatalf("恢复 active 应清空原因: %v %q", err, got.InvalidReason)
	}
	if _, err := svc.SetStatus(2, c1.ID, model.ThesisStatusArchived, ""); err == nil {
		t.Fatal("跨用户改状态应失败")
	}

	// 删除隔离。
	if err := svc.Delete(2, c1.ID); err == nil {
		t.Fatal("跨用户删除应失败")
	}
	if err := svc.Delete(1, c1.ID); err != nil {
		t.Fatalf("本人删除应成功: %v", err)
	}
}

// TestThesisDueForUser 到期筛选：仅 active + 复盘日 <= 今天。
func TestThesisDueForUser(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM thesis_cards")

	today := time.Now().In(time.Local).Format("2006-01-02")
	yesterday := time.Now().AddDate(0, 0, -1).In(time.Local).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).In(time.Local).Format("2006-01-02")

	mk := func(sym, due, status string) {
		common.DB.Create(&model.ThesisCard{UserID: 1, Symbol: sym, Market: "cn",
			Thesis: "t", NextReviewDate: due, Status: status})
	}
	mk("600001", yesterday, model.ThesisStatusActive) // 过期 → 应出现
	mk("600002", today, model.ThesisStatusActive)     // 今天 → 应出现
	mk("600003", tomorrow, model.ThesisStatusActive)  // 未来 → 不出现
	mk("600004", "", model.ThesisStatusActive)        // 未设复盘日 → 不出现
	mk("600005", yesterday, model.ThesisStatusArchived)
	common.DB.Create(&model.ThesisCard{UserID: 2, Symbol: "600006", Market: "cn",
		Thesis: "t", NextReviewDate: yesterday, Status: model.ThesisStatusActive})

	svc := &ThesisService{}
	due, err := svc.DueForUser(1)
	if err != nil {
		t.Fatalf("DueForUser 失败: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("到期卡应为 2 张，得到 %d", len(due))
	}
	if due[0].Symbol != "600001" {
		t.Fatalf("应按复盘日升序，首张为 600001，得到 %s", due[0].Symbol)
	}
}
