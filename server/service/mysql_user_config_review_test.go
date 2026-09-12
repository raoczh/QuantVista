package service

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

func TestMySQLUserLazyDefaultsConcurrentCreation(t *testing.T) {
	for _, table := range []string{"user_preferences", "user_quota"} {
		t.Run(table, func(t *testing.T) {
			db := setupMySQLReviewDB(t, &model.UserPreference{}, &model.UserQuota{})
			// 表名由模型解析，避免测试依赖单复数惯例。
			var schemaModel any = &model.UserPreference{}
			if table == "user_quota" {
				schemaModel = &model.UserQuota{}
			}
			stmt := &gorm.Statement{DB: db}
			if err := stmt.Parse(schemaModel); err != nil {
				t.Fatal(err)
			}
			ready, release := make(chan struct{}, 2), make(chan struct{})
			var count atomic.Int32
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			const callback = "review_user_default_creation"
			if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table != stmt.Schema.Table || count.Add(1) > 2 {
					return
				}
				ready <- struct{}{}
				select {
				case <-release:
				case <-tx.Statement.Context.Done():
					tx.AddError(tx.Statement.Context.Err())
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Query().Remove(callback) })
			results := make(chan error, 2)
			for i := 0; i < 2; i++ {
				go func() {
					var err error
					if table == "user_preferences" {
						_, err = NewUserService().GetPreference(1042)
					} else {
						_, err = NewUserService().GetQuota(1042)
					}
					results <- err
				}()
			}
			for i := 0; i < 2; i++ {
				select {
				case <-ready:
				case <-time.After(5 * time.Second):
					t.Fatal("并发请求未同时读到空行")
				}
			}
			unblock()
			for i := 0; i < 2; i++ {
				if err := <-results; err != nil {
					t.Errorf("并发首次加载必须均成功：%v", err)
				}
			}
			var rows int64
			if err := db.Model(schemaModel).Where("user_id = ?", 1042).Count(&rows).Error; err != nil || rows != 1 {
				t.Fatalf("默认行必须唯一：rows=%d err=%v", rows, err)
			}
		})
	}
}

func TestMySQLLLMConfigConcurrentMutation(t *testing.T) {
	for _, operation := range []string{"edit_deleted", "default_deleted", "keep_rotated_key"} {
		t.Run(operation, func(t *testing.T) {
			db := setupMySQLReviewDB(t, &model.User{}, &model.LLMConfig{}, &model.LLMModuleRoute{}, &model.LLMCallLog{})
			user := model.User{Username: "llm-concurrent-review"}
			if err := db.Create(&user).Error; err != nil {
				t.Fatal(err)
			}
			oldKey := common.EncryptionKey
			common.EncryptionKey = "llm-concurrent-review-key"
			t.Cleanup(func() { common.EncryptionKey = oldKey })
			svc := NewLLMService()
			input := validLLMConfigInput()
			input.APIKey, input.IsDefault = "old-review-key", true
			first, err := svc.Create(user.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			input.Name, input.IsDefault = "second", false
			target, err := svc.Create(user.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			writer := db.Begin()
			if writer.Error != nil {
				t.Fatal(writer.Error)
			}
			t.Cleanup(func() { writer.Rollback() })
			var locked model.User
			if err := writer.Clauses(clause.Locking{Strength: "UPDATE"}).First(&locked, user.ID).Error; err != nil {
				t.Fatal(err)
			}
			cipher, err := common.Encrypt("new-review-key")
			if err != nil {
				t.Fatal(err)
			}
			if operation == "keep_rotated_key" {
				err = writer.Model(&model.LLMConfig{}).Where("id = ?", target.ID).Update("api_key_cipher", cipher).Error
			} else {
				err = writer.Delete(&model.LLMConfig{}, target.ID).Error
			}
			if err != nil {
				t.Fatal(err)
			}
			waiting := make(chan struct{})
			var once sync.Once
			const callback = "review_llm_concurrent_mutation"
			beforeQuery := func(tx *gorm.DB) {
				if tx.Statement.Table == "users" {
					once.Do(func() { close(waiting) })
				}
			}
			beforeUpdate := func(tx *gorm.DB) {
				if tx.Statement.Table == "llm_configs" {
					once.Do(func() { close(waiting) })
				}
			}
			if err := db.Callback().Query().Before("gorm:query").Register(callback, beforeQuery); err != nil {
				t.Fatal(err)
			}
			if err := db.Callback().Update().Before("gorm:update").Register(callback, beforeUpdate); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { db.Callback().Query().Remove(callback); db.Callback().Update().Remove(callback) })
			result := make(chan error, 1)
			go func() {
				var err error
				if operation == "default_deleted" {
					_, err = svc.SetDefault(user.ID, target.ID)
				} else {
					input.Name, input.APIKey = "edited", ""
					_, err = svc.Update(user.ID, target.ID, input)
				}
				result <- err
			}()
			select {
			case <-waiting:
			case err := <-result:
				t.Fatalf("请求未参与并发写入：%v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("请求未等待已有事务")
			}
			if err := writer.Commit().Error; err != nil {
				t.Fatal(err)
			}
			err = <-result
			if operation == "keep_rotated_key" {
				var stored model.LLMConfig
				if err != nil {
					t.Fatal(err)
				}
				if err := db.First(&stored, target.ID).Error; err != nil {
					t.Fatal(err)
				}
				key, err := common.Decrypt(stored.APIKeyCipher)
				if err != nil || key != "new-review-key" {
					t.Fatalf("编辑时留空密钥必须保留并发轮换后的密钥：err=%v", err)
				}
				return
			}
			if err == nil {
				t.Error("目标已被删除，旧编辑或设默认请求必须失败")
			}
			var rows []model.LLMConfig
			if err := db.Where("user_id = ?", user.ID).Find(&rows).Error; err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].ID != first.ID || !rows[0].IsDefault {
				t.Fatalf("不得复活删除项或清除其他默认项：%s", fmt.Sprint(len(rows)))
			}
		})
	}
}

func TestMySQLPreferencePatchesPreserveConcurrentChanges(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.UserPreference{})
	if err := db.Create(&model.UserPreference{UserID: 1043, TotalCapital: 100000, EnableNotify: true}).Error; err != nil {
		t.Fatal(err)
	}
	locked, waiting, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	var waitOnce, releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	const callback = "review_preference_locked_merge"
	if err := db.Callback().Query().Before("gorm:query").Register(callback+"_before", func(tx *gorm.DB) {
		if tx.Statement.Table == "user_preferences" && first.Load() {
			if _, ok := tx.Statement.Clauses["FOR"]; ok {
				waitOnce.Do(func() { close(waiting) })
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Callback().Query().After("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table != "user_preferences" {
			return
		}
		if _, ok := tx.Statement.Clauses["FOR"]; !ok || !first.CompareAndSwap(false, true) {
			return
		}
		close(locked)
		select {
		case <-release:
		case <-tx.Statement.Context.Done():
			tx.AddError(tx.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Callback().Query().Remove(callback + "_before"); db.Callback().Query().Remove(callback) })
	results := make(chan error, 2)
	go func() {
		_, err := NewUserService().UpdatePreference(1043, PreferenceInput{TotalCapital: preferenceReviewValue(float64(200000))})
		results <- err
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("资金更新未进入锁内合并")
	}
	go func() {
		_, err := NewUserService().UpdatePreference(1043, PreferenceInput{EnableNotify: preferenceReviewValue(false)})
		results <- err
	}()
	select {
	case <-waiting:
	case <-time.After(5 * time.Second):
		t.Fatal("通知更新未等待资金更新")
	}
	unblock()
	for i := 0; i < 2; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var stored model.UserPreference
	if err := db.Where("user_id = ?", 1043).First(&stored).Error; err != nil || stored.TotalCapital != 200000 || stored.EnableNotify {
		t.Fatalf("并发保存的资金和开关必须同时保留：pref=%+v err=%v", stored, err)
	}
}

func TestMySQLLLMConcurrentDefaultsRemainUnique(t *testing.T) {
	db := setupMySQLReviewDB(t, &model.User{}, &model.LLMConfig{})
	user := model.User{Username: "llm-defaults-review"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	oldKey := common.EncryptionKey
	common.EncryptionKey = "llm-defaults-review-key"
	t.Cleanup(func() { common.EncryptionKey = oldKey })
	start, results := make(chan struct{}), make(chan error, 12)
	for i := 0; i < 12; i++ {
		go func(i int) {
			<-start
			in := validLLMConfigInput()
			in.Name, in.APIKey, in.IsDefault = fmt.Sprintf("default-%d", i), "review-key", true
			_, err := NewLLMService().Create(user.ID, in)
			results <- err
		}(i)
	}
	close(start)
	for i := 0; i < 12; i++ {
		if err := <-results; err != nil {
			t.Errorf("并发设置默认配置失败：%v", err)
		}
	}
	var total, defaults int64
	if err := db.Model(&model.LLMConfig{}).Where("user_id = ?", user.ID).Count(&total).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.LLMConfig{}).Where("user_id = ? AND is_default = ?", user.ID, true).Count(&defaults).Error; err != nil {
		t.Fatal(err)
	}
	if total != 12 || defaults != 1 {
		t.Fatalf("12 个配置应全部创建且仅一个默认：total=%d defaults=%d", total, defaults)
	}
}
