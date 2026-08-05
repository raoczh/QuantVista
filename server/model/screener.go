package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"quantvista/common"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ScreenerStrategy 用户自定义选股策略（M1 条件树选股）。
// 内置策略在代码内（service/screener_builtin.go），不落库；本表只存用户自建，
// user_id 隔离。TreeJSON 为条件树 DSL（{all/any:[...]} 与 {factor,op,value/ref} 叶子），
// 读取时经 validateCondTree 校验——历史行里因子被下线时扫描报错而非静默漏配。
type ScreenerStrategy struct {
	ID     int64  `gorm:"primaryKey" json:"id"`
	UserID int64  `gorm:"index" json:"user_id"`
	Name   string `gorm:"size:64" json:"name"`
	Desc   string `gorm:"size:256" json:"desc"`
	// Period 适用周期：short（短线）/ swing（波段）/ mid（中线）。
	Period string `gorm:"size:16" json:"period"`
	// Risk 风险等级：low / mid / high。
	Risk     string `gorm:"size:8" json:"risk"`
	TreeJSON string `gorm:"type:text" json:"tree_json"`

	// Name/Desc/Period/Risk/TreeJSON 仅为旧代码和列表查询保留的当前版本投影；执行时
	// 必须以 CurrentRevisionID 指向的不可变快照为准。
	CurrentRevisionID int64      `gorm:"not null;default:0;index" json:"current_revision_id"`
	ArchivedAt        *time.Time `gorm:"index" json:"archived_at,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// ScreenerStrategyRevision 是用户选股策略的不可变完整快照。
// (strategy_id, revision) 唯一；历史行只允许创建，正常 GORM 更新和删除均由 hooks 拒绝。
// 主表归档不会级联删除 revision，确保既有扫描和回测可继续按旧版本复现。
type ScreenerStrategyRevision struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	UserID      int64     `gorm:"not null;index:idx_ssr_user_strategy" json:"user_id"`
	StrategyID  int64     `gorm:"not null;index:idx_ssr_strategy_revision,unique;index:idx_ssr_user_strategy" json:"strategy_id"`
	Revision    int       `gorm:"not null;index:idx_ssr_strategy_revision,unique" json:"revision"`
	ContentHash string    `gorm:"size:64;not null" json:"content_hash"`
	Name        string    `gorm:"size:64;not null" json:"name"`
	Desc        string    `gorm:"size:256" json:"desc"`
	Period      string    `gorm:"size:16;not null" json:"period"`
	Risk        string    `gorm:"size:8;not null" json:"risk"`
	TreeJSON    string    `gorm:"type:text;not null" json:"tree_json"`
	CreatedAt   time.Time `json:"created_at"`
}

// ErrImmutableScreenerStrategyRevision 表示调用方尝试改写或删除历史策略快照。
var ErrImmutableScreenerStrategyRevision = errors.New("选股策略历史 revision 不可修改或删除")

// BeforeUpdate 禁止通过 GORM 更新历史快照。
func (*ScreenerStrategyRevision) BeforeUpdate(*gorm.DB) error {
	return ErrImmutableScreenerStrategyRevision
}

// BeforeDelete 禁止通过 GORM 删除历史快照。
func (*ScreenerStrategyRevision) BeforeDelete(*gorm.DB) error {
	return ErrImmutableScreenerStrategyRevision
}

// CanonicalScreenerTreeJSON 将条件树解析后重新稳定编码。对象键序、空白、数字的等价
// JSON 表达不会影响结果；数组顺序保留，因为它是用户快照的一部分。
func CanonicalScreenerTreeJSON(treeJSON string) (string, error) {
	if strings.TrimSpace(treeJSON) == "" {
		return "", errors.New("策略条件树 JSON 为空")
	}
	dec := json.NewDecoder(strings.NewReader(treeJSON))
	var tree any
	if err := dec.Decode(&tree); err != nil {
		return "", fmt.Errorf("解析策略条件树 JSON: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", errors.New("策略条件树 JSON 包含多个值")
		}
		return "", fmt.Errorf("解析策略条件树 JSON 尾部: %w", err)
	}
	if _, ok := tree.(map[string]any); !ok {
		return "", errors.New("策略条件树 JSON 顶层必须是对象")
	}
	tree = normalizeScreenerJSONNumbers(tree)
	b, err := json.Marshal(tree)
	if err != nil {
		return "", fmt.Errorf("规范化策略条件树 JSON: %w", err)
	}
	return string(b), nil
}

// normalizeScreenerJSONNumbers 统一 JSON 的 -0 与 0；其他数字已经由 Decoder 解析为
// float64，重新编码时 1、1.0、1e0 等等价表示会自然收敛为同一文本。
func normalizeScreenerJSONNumbers(v any) any {
	switch x := v.(type) {
	case float64:
		if x == 0 {
			return float64(0)
		}
		return x
	case []any:
		for i := range x {
			x[i] = normalizeScreenerJSONNumbers(x[i])
		}
		return x
	case map[string]any:
		for k, item := range x {
			x[k] = normalizeScreenerJSONNumbers(item)
		}
		return x
	default:
		return x
	}
}

// ScreenerStrategyContentHash 计算规范化完整策略快照的 SHA-256（64 位 hex）。标量按
// 已落库值参与 hash，TreeJSON 则先结构化规范化，避免用户原始 JSON 的空白和键序造成漂移。
func ScreenerStrategyContentHash(name, desc, period, risk, treeJSON string) (string, error) {
	canonicalTree, err := CanonicalScreenerTreeJSON(treeJSON)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(struct {
		Name   string          `json:"name"`
		Desc   string          `json:"desc"`
		Period string          `json:"period"`
		Risk   string          `json:"risk"`
		Tree   json.RawMessage `json:"tree"`
	}{
		Name: name, Desc: desc, Period: period, Risk: risk,
		Tree: json.RawMessage(canonicalTree),
	})
	if err != nil {
		return "", fmt.Errorf("编码策略完整快照: %w", err)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

// migrateScreenerStrategyRevisions 为升级前的可变策略补建 revision 1，并回填当前指针。
// 迁移只创建缺失快照、绝不更新既有 revision；唯一索引和 ON CONFLICT 使多实例并发启动
// 仍保持幂等。部分迁移态（revision 已创建但指针未写入）会直接复用既有快照。
func migrateScreenerStrategyRevisions(db *gorm.DB) error {
	var strategies []ScreenerStrategy
	if err := db.Order("id ASC").Find(&strategies).Error; err != nil {
		return err
	}
	for _, strategy := range strategies {
		strategy := strategy
		if err := db.Transaction(func(tx *gorm.DB) error {
			if strategy.CurrentRevisionID > 0 {
				var currentCount int64
				if err := tx.Model(&ScreenerStrategyRevision{}).
					Where("id = ? AND strategy_id = ? AND user_id = ?", strategy.CurrentRevisionID, strategy.ID, strategy.UserID).
					Count(&currentCount).Error; err != nil {
					return err
				}
				if currentCount > 0 {
					return nil
				}
			}

			var baseline ScreenerStrategyRevision
			err := tx.Where("strategy_id = ? AND revision = ?", strategy.ID, 1).First(&baseline).Error
			switch {
			case err == nil:
				if baseline.UserID != strategy.UserID {
					return fmt.Errorf("revision 1 用户不匹配（strategy user=%d, revision user=%d）", strategy.UserID, baseline.UserID)
				}
			case errors.Is(err, gorm.ErrRecordNotFound):
				canonicalTree, canonicalErr := CanonicalScreenerTreeJSON(strategy.TreeJSON)
				if canonicalErr != nil {
					return canonicalErr
				}
				hash, hashErr := ScreenerStrategyContentHash(
					strategy.Name, strategy.Desc, strategy.Period, strategy.Risk, canonicalTree,
				)
				if hashErr != nil {
					return hashErr
				}
				candidate := ScreenerStrategyRevision{
					UserID: strategy.UserID, StrategyID: strategy.ID, Revision: 1,
					ContentHash: hash, Name: strategy.Name, Desc: strategy.Desc,
					Period: strategy.Period, Risk: strategy.Risk, TreeJSON: canonicalTree,
					CreatedAt: strategy.CreatedAt,
				}
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "strategy_id"}, {Name: "revision"}},
					DoNothing: true,
				}).Create(&candidate).Error; err != nil {
					return err
				}
				if err := tx.Where("strategy_id = ? AND revision = ?", strategy.ID, 1).First(&baseline).Error; err != nil {
					return err
				}
				if baseline.UserID != strategy.UserID {
					return fmt.Errorf("revision 1 用户不匹配（strategy user=%d, revision user=%d）", strategy.UserID, baseline.UserID)
				}
			default:
				return err
			}

			// 条件更新避免并发迁移覆盖另一个进程已经切换的新 revision。
			return tx.Model(&ScreenerStrategy{}).
				Where("id = ? AND current_revision_id = ?", strategy.ID, strategy.CurrentRevisionID).
				UpdateColumn("current_revision_id", baseline.ID).Error
		}); err != nil {
			return fmt.Errorf("迁移选股策略 revision（strategy_id=%d）: %w", strategy.ID, err)
		}
	}
	return nil
}

// MigrateScreenerStrategyRevisions 是启动迁移入口，也供迁移测试显式执行。
func MigrateScreenerStrategyRevisions() error {
	if common.DB == nil {
		return nil
	}
	return migrateScreenerStrategyRevisions(common.DB)
}
