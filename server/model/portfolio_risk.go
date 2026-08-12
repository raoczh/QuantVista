package model

import (
	"errors"
	"strconv"
	"time"

	"gorm.io/gorm"
)

const (
	PortfolioKindReal  = "real"
	PortfolioKindPaper = "paper"

	PortfolioStatusActive   = "active"
	PortfolioStatusArchived = "archived"

	CashFlowDeposit       = "deposit"
	CashFlowWithdrawal    = "withdrawal"
	CashFlowFeeAdjustment = "fee_adjustment"
	CashFlowReversal      = "reversal"
)

// PortfolioAccount 是用户拥有的命名组合。DefaultKey 只在默认账户上有值，利用
// UNIQUE + NULL 可重复语义表达“每用户每种 kind 至多一个默认账户”。
type PortfolioAccount struct {
	ID         int64      `gorm:"primaryKey" json:"id"`
	UserID     int64      `gorm:"not null;index:idx_portfolio_owner_kind,priority:1" json:"user_id"`
	Name       string     `gorm:"size:64;not null" json:"name"`
	Kind       string     `gorm:"size:8;not null;index:idx_portfolio_owner_kind,priority:2" json:"kind"`
	Currency   string     `gorm:"size:8;not null;default:CNY" json:"currency"`
	Status     string     `gorm:"size:16;not null;default:active;index" json:"status"`
	IsDefault  bool       `gorm:"not null;default:false" json:"is_default"`
	DefaultKey *string    `gorm:"size:64;uniqueIndex" json:"-"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

func (a *PortfolioAccount) BeforeCreate(_ *gorm.DB) error {
	if a.UserID <= 0 {
		return errors.New("组合账户必须属于有效用户")
	}
	if a.Kind != PortfolioKindReal && a.Kind != PortfolioKindPaper {
		return errors.New("组合账户类型须为 real 或 paper")
	}
	if a.IsDefault {
		key := portfolioDefaultKey(a.UserID, a.Kind)
		a.DefaultKey = &key
	} else {
		a.DefaultKey = nil
	}
	return nil
}

func (a *PortfolioAccount) BeforeUpdate(_ *gorm.DB) error {
	// Model(&PortfolioAccount{}).Where(...).Updates(map) 的 hook 接收的是零值占位对象，
	// 归属已由 SQL 条件保护；只有 Save 完整对象时才需要同步默认唯一键。
	if a.UserID == 0 && a.Kind == "" {
		return nil
	}
	if a.UserID <= 0 || (a.Kind != PortfolioKindReal && a.Kind != PortfolioKindPaper) {
		return errors.New("组合账户归属无效")
	}
	if a.IsDefault {
		key := portfolioDefaultKey(a.UserID, a.Kind)
		a.DefaultKey = &key
	} else {
		a.DefaultKey = nil
	}
	return nil
}

func portfolioDefaultKey(userID int64, kind string) string {
	return strconv.FormatInt(userID, 10) + ":" + kind
}

// PortfolioCashFlow 是追加式外部资金流水。原流水不允许更新或删除，冲正通过
// ReversalOfID 指向原记录并写入金额相反的新事实。
type PortfolioCashFlow struct {
	ID             int64     `gorm:"primaryKey" json:"id"`
	UserID         int64     `gorm:"not null;index:idx_pcf_account_date,priority:1;uniqueIndex:idx_pcf_idempotency,priority:1" json:"user_id"`
	AccountID      int64     `gorm:"not null;index:idx_pcf_account_date,priority:2;uniqueIndex:idx_pcf_idempotency,priority:2" json:"account_id"`
	Type           string    `gorm:"size:24;not null" json:"type"`
	Amount         float64   `gorm:"type:decimal(20,4);not null" json:"amount"`
	TradeDate      string    `gorm:"size:10;not null;index:idx_pcf_account_date,priority:3" json:"trade_date"`
	Note           string    `gorm:"size:255" json:"note"`
	IdempotencyKey string    `gorm:"size:128;not null;uniqueIndex:idx_pcf_idempotency,priority:3" json:"idempotency_key"`
	ReversalOfID   *int64    `gorm:"uniqueIndex" json:"reversal_of_id,omitempty"`
	CreatedAt      time.Time `gorm:"not null" json:"created_at"`
}

func (f *PortfolioCashFlow) BeforeCreate(_ *gorm.DB) error {
	if f.UserID <= 0 || f.AccountID <= 0 || f.IdempotencyKey == "" || f.TradeDate == "" || f.Amount == 0 {
		return errors.New("现金流缺少必填事实")
	}
	return nil
}

func (*PortfolioCashFlow) BeforeUpdate(_ *gorm.DB) error {
	return errors.New("现金流不可修改，请新增冲正流水")
}
func (*PortfolioCashFlow) BeforeDelete(_ *gorm.DB) error {
	return errors.New("现金流不可删除，请新增冲正流水")
}

// TargetAllocationRevision 保存一次完整目标配置快照。ItemsJSON 是规范化排序后的配置，
// 历史再计算只读取指定 revision，绝不跟随当前配置漂移。
type TargetAllocationRevision struct {
	ID          int64     `gorm:"primaryKey" json:"id"`
	UserID      int64     `gorm:"not null;index:idx_target_revision,unique,priority:1" json:"user_id"`
	AccountID   int64     `gorm:"not null;index:idx_target_revision,unique,priority:2" json:"account_id"`
	Revision    int       `gorm:"not null;index:idx_target_revision,unique,priority:3" json:"revision"`
	ContentHash string    `gorm:"size:64;not null;index" json:"content_hash"`
	ItemsJSON   string    `gorm:"type:text;not null" json:"items_json"`
	CreatedAt   time.Time `gorm:"not null" json:"created_at"`
}

func (r *TargetAllocationRevision) BeforeCreate(_ *gorm.DB) error {
	if r.UserID <= 0 || r.AccountID <= 0 || r.Revision <= 0 || r.ContentHash == "" || r.ItemsJSON == "" {
		return errors.New("目标配置 revision 缺少必填事实")
	}
	return nil
}

func (*TargetAllocationRevision) BeforeUpdate(_ *gorm.DB) error {
	return errors.New("目标配置 revision 不可修改")
}
func (*TargetAllocationRevision) BeforeDelete(_ *gorm.DB) error {
	return errors.New("目标配置 revision 不可删除")
}
