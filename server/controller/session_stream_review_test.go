package controller

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/middleware"
	"quantvista/model"
	"quantvista/service"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"gorm.io/gorm"
)

func reviewControllerDB(t *testing.T, models ...any) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	previous, previousSQLite := common.DB, common.UsingSQLite
	common.DB, common.UsingSQLite = db, true
	t.Cleanup(func() { common.DB, common.UsingSQLite = previous, previousSQLite; _ = conn.Close() })
	if err := db.AutoMigrate(models...); err != nil {
		t.Fatal(err)
	}
	return db
}

func reviewStreamToken(t *testing.T, db *gorm.DB) string {
	t.Helper()
	u := model.User{ID: 7, Username: "stream-review", Status: model.StatusEnabled, Role: model.RoleUser}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	token, _, err := common.IssueAccessToken(u.ID, u.Role, u.TokenVersion)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

type sessionStreamJobs struct {
	taskCenterJobsStub
	readyAt time.Time
}

func (s *sessionStreamJobs) Events(userID, afterID, limit int64, contexts ...context.Context) ([]service.JobEventView, error) {
	if afterID == 0 {
		return []service.JobEventView{{ID: 9, JobRunID: 1, Type: "status", Status: model.JobStatusRunning}}, nil
	}
	if time.Now().Before(s.readyAt) {
		return nil, nil
	}
	return []service.JobEventView{{ID: 10, JobRunID: 1, Type: "status", Status: model.JobStatusSuccess}}, nil
}

type sessionStreamRecorder struct {
	*httptest.ResponseRecorder
	afterFirst func()
	cancel     context.CancelFunc
}

func (r *sessionStreamRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseRecorder.Write(p)
	if bytes.Contains(p, []byte("id: 9\n")) && r.afterFirst != nil {
		r.afterFirst()
	}
	if bytes.Contains(p, []byte("id: 10\n")) {
		r.cancel()
	}
	return n, err
}

func TestTaskEventStreamStopsWhenSessionInvalidates(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, mode := range []string{"disabled", "password_changed", "expired"} {
		t.Run(mode, func(t *testing.T) {
			db := reviewControllerDB(t, &model.User{})
			token := reviewStreamToken(t, db)
			jobs := &sessionStreamJobs{}
			if mode == "expired" {
				previous := common.SessionSecret
				common.SessionSecret = "isolated-stream-regression-secret"
				t.Cleanup(func() { common.SessionSecret = previous })
				exp := jwt.NewNumericDate(time.Now().Add(2 * time.Second))
				claims := common.Claims{UserID: 7, Role: model.RoleUser,
					RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: exp}}
				var err error
				token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(common.SessionSecret))
				if err != nil {
					t.Fatal(err)
				}
				jobs.readyAt = exp.Time
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			recorder := &sessionStreamRecorder{ResponseRecorder: httptest.NewRecorder(), cancel: cancel}
			recorder.afterFirst = func() {
				var err error
				if mode == "disabled" {
					err = db.Model(&model.User{}).Where("id = ?", 7).Update("status", model.StatusDisabled).Error
				} else if mode == "password_changed" {
					err = db.Model(&model.User{}).Where("id = ?", 7).Update("token_version", 1).Error
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			oldPoll := taskEventPollInterval
			taskEventPollInterval = 5 * time.Millisecond
			t.Cleanup(func() { taskEventPollInterval = oldPoll })
			router := gin.New()
			router.GET("/events", middleware.JWTAuth(), NewTaskCenterController(jobs).Events)
			req := httptest.NewRequest(http.MethodGet, "/events", nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer "+token)
			router.ServeHTTP(recorder, req)
			body := recorder.Body.String()
			if !strings.Contains(body, "id: 9\n") {
				t.Fatalf("有效会话应先收到事件：%s", body)
			}
			if strings.Contains(body, "id: 10\n") {
				t.Fatalf("会话失效后仍收到新任务状态：%s", body)
			}
			if ctx.Err() != nil {
				t.Fatalf("连接应主动关闭，而不是等请求超时：%v", ctx.Err())
			}
		})
	}
}
