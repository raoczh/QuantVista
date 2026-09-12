package service

import (
	"testing"
	"time"

	"quantvista/model"
)

func TestMySQLJointAuditConcurrentIncrements(t *testing.T) {
	setupMySQLReviewDB(t, &model.Option{})
	const workers = 12
	type result struct {
		audit *JointLockedAudit
		err   error
	}
	start := make(chan struct{})
	done := make(chan result, workers)
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			audit, err := jointLockedAuditBump(time.Now())
			done <- result{audit, err}
		}()
	}
	close(start)
	counts := map[int]bool{}
	for i := 0; i < workers; i++ {
		select {
		case r := <-done:
			if r.err != nil || r.audit == nil {
				t.Errorf("并发登记失败：%v", r.err)
				continue
			}
			if counts[r.audit.Count] {
				t.Errorf("不能有两次读取取得相同累计次数：%d", r.audit.Count)
			}
			counts[r.audit.Count] = true
		case <-time.After(10 * time.Second):
			t.Fatal("并发登记未结束")
		}
	}
	audit, err := jointLockedAuditLoad()
	if err != nil || audit == nil || audit.Count != workers || len(audit.Log) != jointLockedLogMax {
		t.Fatalf("每次读取都必须累计，日志只保留最近十条：audit=%+v err=%v", audit, err)
	}
}
