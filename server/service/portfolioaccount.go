package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const portfolioNameMaxRunes = 64

type PortfolioAccountInput struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Currency string `json:"currency"`
}

type PortfolioAccountUpdate struct {
	Name string `json:"name"`
}

type PortfolioAccountService struct{}

func NewPortfolioAccountService() *PortfolioAccountService { return &PortfolioAccountService{} }

func validatePortfolioKind(kind string) error {
	if kind != model.PortfolioKindReal && kind != model.PortfolioKindPaper {
		return errors.New("组合类型须为 real 或 paper")
	}
	return nil
}

func cleanPortfolioName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("组合名称不能为空")
	}
	if len([]rune(name)) > portfolioNameMaxRunes {
		return "", errors.New("组合名称不能超过 64 个字符")
	}
	return name, nil
}

func defaultPortfolioName(kind string) string {
	if kind == model.PortfolioKindPaper {
		return "默认模拟账户"
	}
	return "默认真实账户"
}

// EnsureDefaultPortfolioAccount 首次访问和迁移共用的幂等默认账户入口。
func EnsureDefaultPortfolioAccount(userID int64, kind string) (*model.PortfolioAccount, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if userID <= 0 {
		return nil, errors.New("组合账户必须属于有效用户")
	}
	if err := validatePortfolioKind(kind); err != nil {
		return nil, err
	}
	var out model.PortfolioAccount
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var row model.PortfolioAccount
		err := tx.Where("user_id = ? AND kind = ? AND is_default = ? AND status = ?", userID, kind, true, model.PortfolioStatusActive).First(&row).Error
		if err == nil {
			out = row
			return ensurePaperCashAccount(tx, row)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row = model.PortfolioAccount{UserID: userID, Name: defaultPortfolioName(kind), Kind: kind,
			Currency: "CNY", Status: model.PortfolioStatusActive, IsDefault: true}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
		if row.ID == 0 {
			if err := tx.Where("user_id = ? AND kind = ? AND is_default = ? AND status = ?", userID, kind, true, model.PortfolioStatusActive).First(&row).Error; err != nil {
				return err
			}
		}
		out = row
		return ensurePaperCashAccount(tx, row)
	})
	return &out, err
}

func ensurePaperCashAccount(tx *gorm.DB, account model.PortfolioAccount) error {
	if account.Kind != model.PortfolioKindPaper {
		return nil
	}
	var legacy model.PaperAccount
	if err := tx.Where("user_id = ? AND (account_id = 0 OR account_id IS NULL)", account.UserID).First(&legacy).Error; err == nil {
		return tx.Model(&model.PaperAccount{}).Where("id = ? AND user_id = ? AND (account_id = 0 OR account_id IS NULL)", legacy.ID, account.UserID).
			Update("account_id", account.ID).Error
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	row := model.PaperAccount{UserID: account.UserID, AccountID: account.ID,
		InitialCash: model.PaperDefaultCash, Cash: model.PaperDefaultCash}
	return tx.Where("user_id = ? AND account_id = ?", account.UserID, account.ID).FirstOrCreate(&row).Error
}

func PortfolioAccountByID(userID, accountID int64, kind string) (*model.PortfolioAccount, error) {
	if userID <= 0 || accountID <= 0 {
		return nil, gorm.ErrRecordNotFound
	}
	q := common.DB.Where("id = ? AND user_id = ?", accountID, userID)
	if kind != "" {
		q = q.Where("kind = ?", kind)
	}
	var row model.PortfolioAccount
	if err := q.First(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, gorm.ErrRecordNotFound
		}
		return nil, err
	}
	return &row, nil
}

func ActivePortfolioAccountByID(userID, accountID int64, kind string) (*model.PortfolioAccount, error) {
	row, err := PortfolioAccountByID(userID, accountID, kind)
	if err != nil {
		return nil, err
	}
	if row.Status != model.PortfolioStatusActive {
		return nil, errors.New("组合已归档，仅允许读取历史数据")
	}
	return row, nil
}

func ValidatePositionAccount(userID, accountID, positionID int64) error {
	return positionInAccount(userID, accountID, positionID)
}

func ValidateWritablePositionAccount(userID, accountID, positionID int64) error {
	if _, err := ActivePortfolioAccountByID(userID, accountID, model.PortfolioKindReal); err != nil {
		return err
	}
	return positionInAccount(userID, accountID, positionID)
}

func ResolvePortfolioAccount(userID, accountID int64, kind string) (*model.PortfolioAccount, error) {
	if accountID < 0 {
		return nil, gorm.ErrRecordNotFound
	}
	if accountID > 0 {
		return PortfolioAccountByID(userID, accountID, kind)
	}
	account, err := EnsureDefaultPortfolioAccount(userID, kind)
	if err != nil {
		return nil, err
	}
	if err := attachLegacyPortfolioRows(userID, *account); err != nil {
		return nil, err
	}
	return account, nil
}

func attachLegacyPortfolioRows(userID int64, account model.PortfolioAccount) error {
	return common.DB.Transaction(func(tx *gorm.DB) error {
		updates := []struct {
			model any
			where string
			args  []any
		}{
			{&model.PortfolioSnapshot{}, "user_id = ? AND kind = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID, account.Kind}},
		}
		if account.Kind == model.PortfolioKindReal {
			updates = append(updates,
				struct {
					model any
					where string
					args  []any
				}{&model.Position{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID}},
				struct {
					model any
					where string
					args  []any
				}{&model.PositionTrade{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID}},
				struct {
					model any
					where string
					args  []any
				}{&model.PositionCorpAdjust{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID}},
			)
		} else {
			updates = append(updates,
				struct {
					model any
					where string
					args  []any
				}{&model.PaperHolding{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID}},
				struct {
					model any
					where string
					args  []any
				}{&model.PaperTrade{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID}},
				struct {
					model any
					where string
					args  []any
				}{&model.PaperCorpAdjust{}, "user_id = ? AND (account_id = 0 OR account_id IS NULL)", []any{userID}},
			)
		}
		for _, update := range updates {
			if err := tx.Model(update.model).Where(update.where, update.args...).Update("account_id", account.ID).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *PortfolioAccountService) List(userID int64) ([]model.PortfolioAccount, error) {
	if _, err := EnsureDefaultPortfolioAccount(userID, model.PortfolioKindReal); err != nil {
		return nil, err
	}
	if _, err := EnsureDefaultPortfolioAccount(userID, model.PortfolioKindPaper); err != nil {
		return nil, err
	}
	var rows []model.PortfolioAccount
	err := common.DB.Where("user_id = ?", userID).
		Order("status ASC, kind ASC, is_default DESC, id ASC").Find(&rows).Error
	return rows, err
}

func (s *PortfolioAccountService) Create(userID int64, in PortfolioAccountInput) (*model.PortfolioAccount, error) {
	name, err := cleanPortfolioName(in.Name)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if err := validatePortfolioKind(kind); err != nil {
		return nil, err
	}
	currency := strings.ToUpper(strings.TrimSpace(in.Currency))
	if currency == "" {
		currency = "CNY"
	}
	if currency != "CNY" {
		return nil, errors.New("当前仅支持 CNY 组合")
	}
	row := model.PortfolioAccount{UserID: userID, Name: name, Kind: kind, Currency: currency,
		Status: model.PortfolioStatusActive}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Model(&model.PortfolioAccount{}).Where("user_id = ? AND kind = ? AND status = ?", userID, kind, model.PortfolioStatusActive).Count(&count).Error; err != nil {
			return err
		}
		row.IsDefault = count == 0
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return ensurePaperCashAccount(tx, row)
	})
	return &row, err
}

func (s *PortfolioAccountService) Update(userID, accountID int64, in PortfolioAccountUpdate) (*model.PortfolioAccount, error) {
	name, err := cleanPortfolioName(in.Name)
	if err != nil {
		return nil, err
	}
	row, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return nil, err
	}
	if row.Status == model.PortfolioStatusArchived {
		return nil, errors.New("已归档组合不能改名")
	}
	if err := common.DB.Model(&model.PortfolioAccount{}).Where("id = ? AND user_id = ?", accountID, userID).Update("name", name).Error; err != nil {
		return nil, err
	}
	row.Name = name
	return row, nil
}

func (s *PortfolioAccountService) SetDefault(userID, accountID int64) (*model.PortfolioAccount, error) {
	var out model.PortfolioAccount
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var target model.PortfolioAccount
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND status = ?", accountID, userID, model.PortfolioStatusActive).First(&target).Error; err != nil {
			return err
		}
		if err := tx.Model(&model.PortfolioAccount{}).Where("user_id = ? AND kind = ? AND id <> ?", userID, target.Kind, target.ID).
			Updates(map[string]any{"is_default": false, "default_key": nil}).Error; err != nil {
			return err
		}
		target.IsDefault = true
		if err := tx.Save(&target).Error; err != nil {
			return err
		}
		out = target
		return nil
	})
	return &out, err
}

func portfolioAccountFactCounts(tx *gorm.DB, userID, accountID int64) (map[string]int64, error) {
	counts := map[string]int64{}
	for key, value := range map[string]any{
		"持仓": &model.Position{}, "持仓流水": &model.PositionTrade{}, "资产快照": &model.PortfolioSnapshot{},
		"模拟资金账户": &model.PaperAccount{}, "模拟持仓": &model.PaperHolding{}, "模拟流水": &model.PaperTrade{}, "现金流": &model.PortfolioCashFlow{},
		"除权调整": &model.PositionCorpAdjust{}, "导入批次": &model.ImportBatch{}, "目标配置": &model.TargetAllocationRevision{},
	} {
		var n int64
		if err := tx.Model(value).Where("user_id = ? AND account_id = ?", userID, accountID).Count(&n).Error; err != nil {
			return nil, err
		}
		counts[key] = n
	}
	return counts, nil
}

func (s *PortfolioAccountService) Archive(userID, accountID int64) (*model.PortfolioAccount, error) {
	row, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return nil, err
	}
	if row.Status == model.PortfolioStatusArchived {
		return row, nil
	}
	if row.IsDefault {
		return nil, errors.New("默认账户不能归档，请先切换同类型默认账户")
	}
	now := time.Now()
	err = common.DB.Model(&model.PortfolioAccount{}).Where("id = ? AND user_id = ?", accountID, userID).
		Updates(map[string]any{"status": model.PortfolioStatusArchived, "archived_at": now, "is_default": false, "default_key": nil}).Error
	if err == nil {
		row.Status, row.ArchivedAt, row.IsDefault = model.PortfolioStatusArchived, &now, false
	}
	return row, err
}

func (s *PortfolioAccountService) Delete(userID, accountID int64) error {
	row, err := PortfolioAccountByID(userID, accountID, "")
	if err != nil {
		return err
	}
	if row.IsDefault {
		return errors.New("默认账户不能删除")
	}
	counts, err := portfolioAccountFactCounts(common.DB, userID, accountID)
	if err != nil {
		return err
	}
	for label, n := range counts {
		if n > 0 {
			return fmt.Errorf("账户已有%s，不能删除，只能归档", label)
		}
	}
	return common.DB.Where("id = ? AND user_id = ?", accountID, userID).Delete(&model.PortfolioAccount{}).Error
}

type CashFlowInput struct {
	Type           string  `json:"type"`
	Amount         float64 `json:"amount"`
	TradeDate      string  `json:"trade_date"`
	Note           string  `json:"note"`
	IdempotencyKey string  `json:"idempotency_key"`
}

func validateCashFlowInput(in CashFlowInput) (CashFlowInput, error) {
	in.Type = strings.ToLower(strings.TrimSpace(in.Type))
	in.TradeDate = strings.TrimSpace(in.TradeDate)
	in.IdempotencyKey = strings.TrimSpace(in.IdempotencyKey)
	in.Note = strings.TrimSpace(in.Note)
	if in.Type != model.CashFlowDeposit && in.Type != model.CashFlowWithdrawal && in.Type != model.CashFlowFeeAdjustment {
		return in, errors.New("现金流类型不支持")
	}
	if math.IsNaN(in.Amount) || math.IsInf(in.Amount, 0) || in.Amount == 0 || math.Abs(in.Amount) > 1e12 {
		return in, errors.New("现金流金额无效")
	}
	if in.Type == model.CashFlowDeposit && in.Amount <= 0 {
		return in, errors.New("入金金额必须大于 0")
	}
	if in.Type == model.CashFlowWithdrawal && in.Amount >= 0 {
		return in, errors.New("出金金额必须小于 0")
	}
	d, err := time.ParseInLocation("2006-01-02", in.TradeDate, time.Local)
	today, _ := time.ParseInLocation("2006-01-02", time.Now().Format("2006-01-02"), time.Local)
	if err != nil || d.Format("2006-01-02") != in.TradeDate || d.After(today) {
		return in, errors.New("现金流日期无效")
	}
	if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 128 {
		return in, errors.New("idempotency_key 不能为空且不能超过 128 字符")
	}
	if len([]rune(in.Note)) > 255 {
		return in, errors.New("备注不能超过 255 个字符")
	}
	in.Amount = round4(in.Amount)
	return in, nil
}

func ListPortfolioCashFlows(userID, accountID int64) ([]model.PortfolioCashFlow, error) {
	if _, err := PortfolioAccountByID(userID, accountID, model.PortfolioKindReal); err != nil {
		return nil, err
	}
	var rows []model.PortfolioCashFlow
	err := common.DB.Where("user_id = ? AND account_id = ?", userID, accountID).
		Order("trade_date DESC, id DESC").Find(&rows).Error
	return rows, err
}

func CreatePortfolioCashFlow(userID, accountID int64, raw CashFlowInput) (*model.PortfolioCashFlow, error) {
	if _, err := ActivePortfolioAccountByID(userID, accountID, model.PortfolioKindReal); err != nil {
		return nil, err
	}
	in, err := validateCashFlowInput(raw)
	if err != nil {
		return nil, err
	}
	var existing model.PortfolioCashFlow
	err = common.DB.Where("user_id = ? AND account_id = ? AND idempotency_key = ?", userID, accountID, in.IdempotencyKey).First(&existing).Error
	if err == nil {
		if existing.Type != in.Type || existing.Amount != in.Amount || existing.TradeDate != in.TradeDate || existing.Note != in.Note {
			return nil, errors.New("idempotency_key 已用于不同现金流")
		}
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	row := model.PortfolioCashFlow{UserID: userID, AccountID: accountID, Type: in.Type, Amount: in.Amount,
		TradeDate: in.TradeDate, Note: in.Note, IdempotencyKey: in.IdempotencyKey}
	if err := common.DB.Create(&row).Error; err != nil {
		if findErr := common.DB.Where("user_id = ? AND account_id = ? AND idempotency_key = ?", userID, accountID, in.IdempotencyKey).First(&existing).Error; findErr == nil {
			if existing.Type != in.Type || existing.Amount != in.Amount || existing.TradeDate != in.TradeDate || existing.Note != in.Note {
				return nil, errors.New("idempotency_key 已用于不同现金流")
			}
			return &existing, nil
		}
		return nil, err
	}
	return &row, nil
}

func ReversePortfolioCashFlow(userID, accountID, cashFlowID int64, idempotencyKey, note string) (*model.PortfolioCashFlow, error) {
	if _, err := ActivePortfolioAccountByID(userID, accountID, model.PortfolioKindReal); err != nil {
		return nil, err
	}
	idempotencyKey, note = strings.TrimSpace(idempotencyKey), strings.TrimSpace(note)
	if idempotencyKey == "" || len(idempotencyKey) > 128 {
		return nil, errors.New("idempotency_key 不能为空且不能超过 128 字符")
	}
	var out model.PortfolioCashFlow
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var source model.PortfolioCashFlow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ? AND user_id = ? AND account_id = ?", cashFlowID, userID, accountID).First(&source).Error; err != nil {
			return err
		}
		if source.ReversalOfID != nil {
			return errors.New("冲正流水不能再次冲正")
		}
		var existing model.PortfolioCashFlow
		if err := tx.Where("reversal_of_id = ?", source.ID).First(&existing).Error; err == nil {
			out = existing
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		row := model.PortfolioCashFlow{UserID: userID, AccountID: accountID, Type: model.CashFlowReversal,
			Amount: -source.Amount, TradeDate: time.Now().Format("2006-01-02"), Note: note,
			IdempotencyKey: idempotencyKey, ReversalOfID: &source.ID}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		out = row
		return nil
	})
	return &out, err
}
