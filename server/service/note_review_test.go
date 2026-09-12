package service

import (
	"context"
	"strings"
	"testing"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"
)

func TestNoteRebindDoesNotKeepAnotherStockName(t *testing.T) {
	setupTestDB(t)
	note := model.ResearchNote{UserID: 980, Symbol: "600090", Market: "cn", Name: "原股票名称", Title: "笔记"}
	if err := common.DB.Create(&note).Error; err != nil {
		t.Fatal(err)
	}
	svc := NewNoteService(NewMarketService(datasource.NewManagerWithAdapters(&reviewDailyAdapter{})))
	updated, err := svc.Update(context.Background(), 980, note.ID, NoteInput{Symbol: "600091", Market: "cn", Title: "改绑股票"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Symbol != "600091" || updated.Name != "" {
		t.Fatalf("新股票名称查不到时不能沿用原股票名称：%+v", updated)
	}
}

func TestDeletedNoteIsNotRecreatedByDelayedUpdate(t *testing.T) {
	setupTestDB(t)
	note := model.ResearchNote{UserID: 981, Symbol: "600092", Market: "cn", Name: "笔记股票", Title: "稍后删除"}
	if err := common.DB.Create(&note).Error; err != nil {
		t.Fatal(err)
	}
	svc := &NoteService{}
	called := false
	svc.market = NewMarketService(datasource.NewManagerWithAdapters(reviewQuoteHookAdapter{hook: func() {
		called = true
		if err := svc.Delete(981, note.ID); err != nil {
			t.Error(err)
		}
	}}))
	_, err := svc.Update(context.Background(), 981, note.ID, NoteInput{Symbol: note.Symbol, Market: "cn", Title: "迟到的修改"})
	if err == nil {
		t.Error("取名称期间笔记已删除，迟到更新必须失败")
	}
	var count int64
	if err := common.DB.Model(&model.ResearchNote{}).Where("id = ?", note.ID).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if !called || count != 0 {
		t.Fatalf("迟到更新复活了已删除笔记：count=%d called=%v", count, called)
	}
}

func TestNoteRejectsContentBeyondMySQLStorageLimits(t *testing.T) {
	setupTestDB(t)
	for _, in := range []NoteInput{{Title: strings.Repeat("字", 129)}, {Title: "正文太长", Content: strings.Repeat("文", 22000)}} {
		if _, err := (&NoteService{}).Create(context.Background(), 982, in); err == nil {
			t.Error("超过 MySQL 字段容量的笔记应返回明确校验错误")
		}
	}
}
