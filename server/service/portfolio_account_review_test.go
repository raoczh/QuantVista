package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

func TestLegacyPortfolioAttachmentRechecksAccount(t *testing.T) {
	setupTestDB(t)
	for i, action := range []string{"archive", "delete"} {
		t.Run(action, func(t *testing.T) {
			userID := int64(1080 + i)
			svc := NewPortfolioAccountService()
			account, err := svc.Create(userID, PortfolioAccountInput{Name: "原默认账户", Kind: model.PortfolioKindReal})
			if err != nil {
				t.Fatal(err)
			}
			other, err := svc.Create(userID, PortfolioAccountInput{Name: "新默认账户", Kind: model.PortfolioKindReal})
			if err != nil {
				t.Fatal(err)
			}
			p := seedHoldingWithLedger(t, userID, "600118", 10, 100, 0, 0, "2026-08-01")
			if _, err := svc.SetDefault(userID, other.ID); err != nil {
				t.Fatal(err)
			}
			if action == "archive" {
				_, err = svc.Archive(userID, account.ID)
			} else {
				err = svc.Delete(userID, account.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			// 等价于 Resolve 在拿到默认账户之后、挂接旧记录之前被生命周期操作穿过。
			attachErr := attachLegacyPortfolioRows(userID, *account)
			var stored model.Position
			if err := common.DB.First(&stored, p.ID).Error; err != nil {
				t.Fatal(err)
			}
			if attachErr == nil || stored.AccountID != 0 {
				t.Fatalf("旧记录不能被挂到已归档或删除的账户：error=%v accountID=%d", attachErr, stored.AccountID)
			}
		})
	}
}

func TestNewPaperAccountDoesNotClaimDefaultLegacyCash(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 1082
	svc := NewPortfolioAccountService()
	if _, err := svc.Create(userID, PortfolioAccountInput{Name: "默认模拟账户", Kind: model.PortfolioKindPaper}); err != nil {
		t.Fatal(err)
	}
	legacy := model.PaperAccount{UserID: userID, Cash: 76543, InitialCash: 100000}
	if err := common.DB.Create(&legacy).Error; err != nil {
		t.Fatal(err)
	}
	account, err := svc.Create(userID, PortfolioAccountInput{Name: "另一个模拟账户", Kind: model.PortfolioKindPaper})
	if err != nil {
		t.Fatal(err)
	}
	var cash model.PaperAccount
	if err := common.DB.Where("account_id = ?", account.ID).First(&cash).Error; err != nil {
		t.Fatal(err)
	}
	if err := common.DB.First(&legacy, legacy.ID).Error; err != nil {
		t.Fatal(err)
	}
	if cash.Cash != model.PaperDefaultCash || legacy.AccountID != 0 {
		t.Fatalf("非默认新账户不能接收未归属的旧余额：new=%+v legacy=%+v", cash, legacy)
	}
}
