package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"
)

func TestNotifySlowChannelCannotStarveHealthyDestination(t *testing.T) {
	setupTestDB(t)
	const userID int64 = 129901
	seedNotificationReviewUser(t, userID)
	slowStarted, healthySent := make(chan struct{}, 1), make(chan struct{}, 1)
	svc := &NotifyService{channelSender: reviewNotifySender(func(ctx context.Context, _, target string, _ NotifyMessage) error {
		if strings.Contains(target, "SLOW") {
			slowStarted <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		healthySent <- struct{}{}
		return nil
	})}
	for _, target := range []string{"SCT_TEST_SLOW", "SCT_TEST_HEALTHY"} {
		if _, err := svc.Create(userID, NotifyChannelInput{Kind: model.NotifyKindServerChan, Target: target, Name: target, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	finished := make(chan struct{})
	go func() {
		svc.SendMsgContext(ctx, userID, NotifyMessage{Title: "持仓保护触发"})
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(4 * time.Second):
			t.Error("取消后通知工作未退出")
		}
	})
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("慢通道未启动")
	}
	select {
	case <-healthySent:
	case <-time.After(time.Second):
		t.Fatal("健康通道被前一个慢通道阻塞")
	}
	cancel()
	<-finished
	var channels []model.NotifyChannel
	if err := common.DB.Where("user_id = ?", userID).Find(&channels).Error; err != nil {
		t.Fatal(err)
	}
	for _, channel := range channels {
		if channel.LastSentAt == nil {
			t.Fatal("每个已尝试通道均应记录结果，包括超时或取消")
		}
		if strings.Contains(channel.Name, "SLOW") != (channel.LastError != "") {
			t.Fatalf("通道成败应分别记录：%s %s", channel.Name, channel.LastError)
		}
	}
}
