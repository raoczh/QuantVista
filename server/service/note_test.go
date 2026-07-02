package service

import (
	"testing"

	"quantvista/common"
	"quantvista/model"
)

// TestNoteListFilterAndIsolation 笔记筛选（标的/关键字）、排序与用户隔离、删除。
func TestNoteListFilterAndIsolation(t *testing.T) {
	setupTestDB(t)
	common.DB.Exec("DELETE FROM research_notes")

	mk := func(userID int64, symbol, title, content, kind string) *model.ResearchNote {
		n := &model.ResearchNote{UserID: userID, Symbol: symbol, Market: "cn", Kind: kind, Title: title, Content: content}
		common.DB.Create(n)
		return n
	}
	n1 := mk(1, "600000", "浦发决策", "低估买入的决策记录", model.NoteKindDecision)
	mk(1, "600519", "茅台复盘", "回调原因复盘", model.NoteKindReview)
	mk(1, "", "大盘随想", "市场情绪偏冷", model.NoteKindIdea)
	mk(2, "600000", "他人笔记", "不可见", "")

	svc := &NoteService{}

	// 全量：只看到自己的 3 条。
	all, err := svc.List(1, "", "", "", 0)
	if err != nil || len(all) != 3 {
		t.Fatalf("用户1 应有 3 条: %v %d", err, len(all))
	}

	// 按标的过滤。
	bySym, _ := svc.List(1, "600000", "cn", "", 0)
	if len(bySym) != 1 || bySym[0].ID != n1.ID {
		t.Fatalf("按标的应命中 1 条: %d", len(bySym))
	}

	// 关键字搜索（标题或内容）。
	byKw, _ := svc.List(1, "", "", "复盘", 0)
	if len(byKw) != 1 || byKw[0].Symbol != "600519" {
		t.Fatalf("关键字应命中茅台复盘: %d", len(byKw))
	}

	// 删除隔离。
	if err := svc.Delete(2, n1.ID); err == nil {
		t.Fatal("跨用户删除应失败")
	}
	if err := svc.Delete(1, n1.ID); err != nil {
		t.Fatalf("本人删除应成功: %v", err)
	}
}
