package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestHistoricalShareActionCannotExpireBeforeMaterialization(t *testing.T) {
	setupTestDB(t)
	today := time.Now().Format("2006-01-02")
	oldDate := time.Now().AddDate(0, 0, -45).Format("2006-01-02")
	p, _ := seedAdjustCase(t, 1024, oldDate, 0, 10, 0)
	if err := ensurePositionShareActionsProcessedTx(common.DB, *p, today); !errors.Is(err, errPositionShareActionPending) {
		t.Errorf("45 天前未处理且未生成建议的送转仍应阻止旧成本交易：%v", err)
	}
	if pending, err := positionsWithUnconfirmedShareAction(context.Background(), p.UserID, []int64{p.ID}, today); err != nil || !pending[p.ID] {
		t.Errorf("历史送转不能退出风险评估检查：pending=%v err=%v", pending, err)
	}
	if n, err := GenerateCorpAdjusts(p.UserID, today); err != nil || n != 1 {
		t.Errorf("历史待确认建议必须能生成，不能只阻止交易却无处理入口：n=%d err=%v", n, err)
	}
	var stored model.Position
	if err := common.DB.First(&stored, p.ID).Error; err != nil || stored.Quantity != 1000 || stored.BuyPrice != 20 {
		t.Fatalf("生成建议不得自动修改真实持仓：stored=%+v err=%v", stored, err)
	}
}
