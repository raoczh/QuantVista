package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/datasource"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// round4 保留 4 位小数（均价列为 decimal(20,4)，用 round2 会丢精度导致盈亏漂移）。
func round4(v float64) float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0
	}
	return math.Round(v*1e4) / 1e4
}

// PaperService 模拟交易：每用户一个虚拟账户，用真实行情成交与估值。
// 成本基含买入手续费，卖出算真实净已实现盈亏；含手续费与（A 股卖出）印花税。
type PaperService struct {
	market *MarketService
}

func NewPaperService(market *MarketService) *PaperService {
	return &PaperService{market: market}
}

// tradeFee 简化券商费用模型：佣金万 2.5（最低 5 元）+ A 股股票卖出印花税万 5。
// ETF/场内基金（isCNFund）卖出免征印花税，故 cn 卖出仅对非基金标的计税。
func tradeFee(market, side, symbol string, amount float64) (fee, tax float64) {
	comm := amount * 0.00025
	if comm < 5 {
		comm = 5
	}
	fee = round2(comm)
	if market == "cn" && side == model.PaperSideSell && !isCNFund(symbol) {
		tax = round2(amount * 0.0005)
	}
	return fee, tax
}

// lockedAccount 事务内按 user_id 重读账户；MySQL 加 FOR UPDATE 行锁串行化并发交易，
// SQLite 不支持该子句（单写者天然串行），跳过。
func lockedAccount(tx *gorm.DB, userID int64, acc *model.PaperAccount) error {
	if acc.AccountID > 0 {
		if err := lockActivePortfolioAccount(tx, userID, acc.AccountID, model.PortfolioKindPaper); err != nil {
			return err
		}
	}
	q := tx.Where("user_id = ? AND account_id = ?", userID, acc.AccountID)
	if !common.UsingSQLite {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q.First(acc).Error
}

// GetOrCreateAccount 取用户模拟账户，无则以默认初始资金创建。
func (s *PaperService) GetOrCreateAccount(userID int64) (*model.PaperAccount, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	return s.GetOrCreateAccountByID(userID, account.ID)
}

func (s *PaperService) GetOrCreateAccountByID(userID, accountID int64) (*model.PaperAccount, error) {
	return s.paperAccountByID(userID, accountID, false)
}

// 归档账户允许读取已存在的资金记录，不能经读取接口新建资金账户。
func (s *PaperService) paperAccountByID(userID, accountID int64, allowArchived bool) (*model.PaperAccount, error) {
	account, err := PortfolioAccountByID(userID, accountID, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	if !allowArchived && account.Status != model.PortfolioStatusActive {
		return nil, errors.New("模拟账户已归档")
	}
	var acc model.PaperAccount
	err = common.DB.Where("user_id = ? AND account_id = ?", userID, accountID).First(&acc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) && account.Status == model.PortfolioStatusActive {
		err = common.DB.Transaction(func(tx *gorm.DB) error {
			if err := lockActivePortfolioAccount(tx, userID, accountID, model.PortfolioKindPaper); err != nil {
				return err
			}
			acc = model.PaperAccount{UserID: userID, AccountID: accountID, InitialCash: model.PaperDefaultCash, Cash: model.PaperDefaultCash}
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "account_id"}}, DoNothing: true}).Create(&acc).Error; err != nil {
				return err
			}
			acc = model.PaperAccount{}
			return tx.Where("user_id = ? AND account_id = ?", userID, accountID).First(&acc).Error
		})
	}
	return &acc, err
}

// TradeInput 下单入参。Price<=0 时用实时行情价成交。
type TradeInput struct {
	Symbol   string  `json:"symbol"`
	Market   string  `json:"market"`
	Name     string  `json:"name"` // 可选，展示名（给定价成交时用作兜底）
	Side     string  `json:"side"`
	Price    float64 `json:"price"`
	Quantity float64 `json:"quantity"`
}

// Trade 执行一次模拟买/卖（事务：更新现金、持仓、流水）。
func (s *PaperService) Trade(ctx context.Context, userID int64, in TradeInput) (*model.PaperTrade, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	return s.TradeByAccount(ctx, userID, account.ID, in)
}
func (s *PaperService) TradeByAccount(ctx context.Context, userID, accountID int64, in TradeInput) (*model.PaperTrade, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := ActivePortfolioAccountByID(userID, accountID, model.PortfolioKindPaper); err != nil {
		return nil, err
	}
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	if reason := positionCurrencyIssue(model.Position{Market: market}, "CNY"); reason != "" {
		return nil, fmt.Errorf("模拟账户仅支持 CNY 交易：%s", reason)
	}
	side := strings.ToLower(strings.TrimSpace(in.Side))
	if side != model.PaperSideBuy && side != model.PaperSideSell {
		return nil, errors.New("交易方向须为 buy 或 sell")
	}
	if math.IsNaN(in.Quantity) || math.IsInf(in.Quantity, 0) || in.Quantity <= 0 {
		return nil, errors.New("数量必须大于 0")
	}
	// 与持仓、流水的 decimal(20,4) 精度一致，金额和均价也必须使用最终保存数量。
	in.Quantity = round4(in.Quantity)
	if in.Quantity <= 0 {
		return nil, errors.New("数量精度最多为 4 位小数，不能小于 0.0001")
	}
	if math.IsNaN(in.Price) || math.IsInf(in.Price, 0) {
		return nil, errors.New("成交价无效")
	}
	tradeDate := time.Now().In(time.Local).Format("2006-01-02")
	if err := ensurePaperCorpAdjustBeforeTradeForAccount(userID, accountID, symbol, market, tradeDate); err != nil {
		return nil, err
	}

	// 成交价：给定则用给定（跳过行情，便于离线/指定价成交）；否则取实时行情价。
	// fail-closed：市价成交走新鲜行情链路，全源 stale（停牌/数据源延迟）时拒绝按旧价
	// 成交——旧价成交会让模拟盘账面凭空盈亏，用户可手动指定价格明确担责。
	// 精度用 round4（列为 decimal(20,4)）：ETF/基金最小变动价位 0.001 元，round2 会抹掉限价第三位小数。
	price := round4(in.Price)
	if in.Price > 0 && price <= 0 {
		return nil, errors.New("指定成交价不能小于 0.0001 元")
	}
	name := ""
	if price <= 0 {
		q, fi, e := s.market.GetFreshQuote(ctx, market, symbol)
		if e != nil {
			if errors.Is(e, datasource.ErrSymbolInvalid) {
				return nil, errors.New("无法识别的股票代码")
			}
			return nil, errors.New("无法获取成交价，请手动指定价格")
		}
		if fi.Status != freshStatusFresh {
			return nil, errors.New("实时行情已过期或时效无法核验（可能停牌或数据源延迟），拒绝按旧价成交；请手动指定价格或稍后重试")
		}
		price = round4(q.Price)
		name = q.Name
	}
	if price <= 0 {
		return nil, errors.New("成交价必须大于 0")
	}
	if amount := price * in.Quantity; math.IsNaN(amount) || math.IsInf(amount, 0) || round2(amount) <= 0 {
		return nil, errors.New("成交金额无效或小于 0.01 元")
	}
	if name == "" {
		name = strings.TrimSpace(in.Name)
	}

	acc, err := s.GetOrCreateAccountByID(userID, accountID)
	if err != nil {
		return nil, err
	}

	trade := &model.PaperTrade{
		UserID: userID, AccountID: accountID, Symbol: symbol, Market: market, Name: name,
		Side: side, Price: price, Quantity: in.Quantity,
		TradeDate: tradeDate,
	}

	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// 事务内重读账户（MySQL 加行锁；SQLite 单写者天然串行），
		// 避免并发交易基于事务外的过期余额计算、互相覆盖丢失更新。
		if err := lockedAccount(tx, userID, acc); err != nil {
			return err
		}
		currencyGap, err := portfolioCurrencyGapFor(tx, userID, accountID, model.PortfolioKindPaper)
		if err != nil {
			return err
		}
		if currencyGap.affects(tradeDate) {
			return fmt.Errorf("模拟账户余额口径不可用：%s", currencyGap.Reason)
		}
		if err := verifyPaperCorpAdjustBeforeTradeTx(tx, userID, accountID, symbol, market, tradeDate); err != nil {
			return err
		}
		amount := round2(price * in.Quantity)
		fee, tax := tradeFee(market, side, symbol, amount)
		trade.Amount, trade.Fee, trade.Tax = amount, fee, tax

		var holding model.PaperHolding
		hErr := tx.Where("user_id = ? AND account_id = ? AND symbol = ? AND market = ?", userID, accountID, symbol, market).First(&holding).Error
		hasHolding := hErr == nil
		if hErr != nil && !errors.Is(hErr, gorm.ErrRecordNotFound) {
			return hErr
		}
		if hasHolding {
			if _, err := restorePaperHoldingCost(tx, &holding); err != nil {
				return err
			}
		}

		if side == model.PaperSideBuy {
			costBasis := round2(amount + fee + tax)
			if acc.Cash < costBasis {
				return errors.New("现金不足，无法买入")
			}
			acc.Cash = round2(acc.Cash - costBasis)
			if hasHolding {
				newQty := round4(holding.Quantity + in.Quantity)
				newCost := round2(paperHoldingCost(holding) + costBasis)
				newAvg := newCost / newQty
				holding.Quantity = newQty
				holding.AvgCost = round4(newAvg)
				holding.RemainingCost = &newCost
				if name != "" {
					holding.Name = name
				}
				if err := tx.Save(&holding).Error; err != nil {
					return err
				}
			} else {
				holding = model.PaperHolding{
					UserID: userID, AccountID: accountID, Symbol: symbol, Market: market, Name: name,
					Quantity: in.Quantity, AvgCost: round4(costBasis / in.Quantity), RemainingCost: &costBasis,
				}
				if err := tx.Create(&holding).Error; err != nil {
					return err
				}
			}
		} else { // sell
			if !hasHolding || holding.Quantity < in.Quantity {
				return errors.New("持仓数量不足，无法卖出")
			}
			proceeds := round2(amount - fee - tax)
			acc.Cash = round2(acc.Cash + proceeds)
			cost := paperHoldingCost(holding)
			consumed := paperSoldCost(cost, holding.Quantity, in.Quantity)
			trade.RealizedPnl = round2(proceeds - consumed)
			remainingCost := round2(cost - consumed)
			holding.RemainingCost = &remainingCost
			// 数量按列精度 round4（round2 会把碎股残余清零/失真）；清仓判断用 epsilon。
			holding.Quantity = round4(holding.Quantity - in.Quantity)
			if holding.Quantity > positionQtyEps {
				holding.AvgCost = round4(remainingCost / holding.Quantity)
			}
			if holding.Quantity <= 1e-6 {
				if err := tx.Delete(&holding).Error; err != nil {
					return err
				}
			} else if err := tx.Save(&holding).Error; err != nil {
				return err
			}
		}

		if err := tx.Save(acc).Error; err != nil {
			return err
		}
		if err := invalidatePortfolioSnapshotsTx(tx, userID, accountID, tradeDate); err != nil {
			return err
		}
		return tx.Create(trade).Error
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return trade, nil
}

// PaperHoldingView 持仓 + 估值。行情时效契约（fail-closed）：QuoteOK 仅在取到
// fresh 行情时为 true；非 fresh 时按成本估值、浮盈记 0，并带 freshness 块说明。
type PaperHoldingView struct {
	model.PaperHolding
	Price                      float64 `json:"price"`
	QuoteOK                    bool    `json:"quote_ok"`
	Cost                       float64 `json:"cost"`          // 成本 = 均价*数量
	MarketValue                float64 `json:"market_value"`  // 市值 = 现价*数量
	ProfitAmount               float64 `json:"profit_amount"` // 浮动盈亏
	ProfitPct                  float64 `json:"profit_pct"`
	CostBasisNote              string  `json:"cost_basis_note,omitempty"` // 非空时浮动盈亏无法精确核验。
	ValuationUnavailableReason string  `json:"valuation_unavailable_reason,omitempty"`

	QuoteAsOf       string  `json:"quote_as_of,omitempty"`      // 行情数据源时刻（含 stale 的最近已知）
	FreshnessStatus string  `json:"freshness_status,omitempty"` // fresh | stale | unknown
	StaleReason     string  `json:"stale_reason,omitempty"`     // 非 fresh 的原因
	LastPrice       float64 `json:"last_price,omitempty"`       // 最近已知价（stale 展示用，不参与估值）
}

// PaperOverview 账户总览。
type PaperOverview struct {
	Account        *model.PaperAccount `json:"account"`
	Holdings       []PaperHoldingView  `json:"holdings"`
	MarketValue    float64             `json:"market_value"` // 持仓总市值
	TotalAssets    float64             `json:"total_assets"` // 现金 + 市值
	TotalProfit    float64             `json:"total_profit"` // 总资产 - 初始资金
	TotalProfitPct float64             `json:"total_profit_pct"`
	RealizedPnl    float64             `json:"realized_pnl"` // 累计已实现盈亏

	QuoteStaleCount            int    `json:"quote_stale_count"` // 无当前有效行情、按成本估值的持仓数
	ValuationNote              string `json:"valuation_note,omitempty"`
	CurrencyUnavailableReason  string `json:"currency_unavailable_reason,omitempty"` // 非空时现金、资产与盈亏汇总不可用。
	RealizedUnavailableReason  string `json:"realized_unavailable_reason,omitempty"`
	ValuationUnavailableReason string `json:"valuation_unavailable_reason,omitempty"`
}

func paperRealizedPnl(db *gorm.DB, userID int64) (float64, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return 0, err
	}
	return paperRealizedPnlByAccount(db, userID, account.ID)
}
func paperRealizedPnlByAccount(db *gorm.DB, userID, accountID int64) (float64, error) {
	var realized float64
	err := db.Model(&model.PaperTrade{}).Where("user_id = ? AND account_id = ? AND side IN ?", userID, accountID,
		[]string{model.PaperSideSell, model.PaperSideAdjust}).
		Where("market IN ?", []string{"", "cn"}).
		Select("COALESCE(SUM(realized_pnl),0)").Scan(&realized).Error
	return round2(realized), err
}

// Overview 账户总览：持仓按当前有效行情估值，汇总总资产与盈亏。
// fail-closed：非 fresh（stale/unknown/失败）的持仓按成本估值、浮盈记 0 并透明计数
// ——旧价市值冒充「实时资产」会让总资产与盈亏虚高/虚低。
func (s *PaperService) Overview(ctx context.Context, userID int64) (*PaperOverview, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	return s.OverviewByAccount(ctx, userID, account.ID)
}
func (s *PaperService) OverviewByAccount(ctx context.Context, userID, accountID int64) (*PaperOverview, error) {
	acc, err := s.paperAccountByID(userID, accountID, true)
	if err != nil {
		return nil, err
	}
	var holdings []model.PaperHolding
	var realized float64
	var realizedNote string
	var currencyGap portfolioCurrencyGap
	var valuationGaps map[int64]string
	valuationDate := time.Now().Format("2006-01-02")
	costNotes := map[int64]string{}
	// 现金、持仓、成本和已实现必须来自同一个账本状态。此事务不包含行情请求。
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("user_id = ? AND account_id = ?", userID, accountID).First(acc).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Order("id").Find(&holdings).Error; err != nil {
			return err
		}
		for i := range holdings {
			note, err := restorePaperHoldingCost(tx, &holdings[i])
			if err != nil {
				note = "模拟持仓成本无法由流水核验"
			}
			costNotes[holdings[i].ID] = note
		}
		var err error
		valuationGaps, err = paperPortfolioValuationGaps(tx, holdings, valuationDate)
		if err != nil {
			return err
		}
		currencyGap, err = portfolioCurrencyGapFor(tx, userID, accountID, model.PortfolioKindPaper)
		if err != nil {
			return err
		}
		realized, err = paperRealizedPnlByAccount(tx, userID, accountID)
		if err != nil {
			return err
		}
		realizedNote, err = paperRealizedAccountingNote(tx, userID, accountID)
		return err
	})
	if err != nil {
		return nil, err
	}

	refs := make([]QuoteRef, 0, len(holdings))
	for _, h := range holdings {
		refs = append(refs, QuoteRef{Market: h.Market, Symbol: h.Symbol})
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)

	ov := &PaperOverview{Account: acc, Holdings: make([]PaperHoldingView, 0, len(holdings))}
	if currencyGap.affects(valuationDate) {
		ov.CurrencyUnavailableReason = currencyGap.Reason
	}
	for _, h := range holdings {
		v := PaperHoldingView{PaperHolding: h, Cost: paperHoldingCost(h), CostBasisNote: costNotes[h.ID]}
		if reason := valuationGaps[h.ID]; reason != "" {
			v.ValuationUnavailableReason, ov.ValuationUnavailableReason = reason, reason
			v.StaleReason, v.FreshnessStatus = reason, freshStatusUnknown
			ov.Holdings = append(ov.Holdings, v)
			continue
		}
		if reason := positionCurrencyIssue(model.Position{Market: h.Market}, "CNY"); reason != "" {
			v.StaleReason, v.FreshnessStatus = reason, freshStatusUnknown
			ov.Holdings = append(ov.Holdings, v)
			continue
		}
		fq, ok := quotes[QuoteKey(h.Market, h.Symbol)]
		if ok && fq.Quote != nil && fq.Quote.Price > 0 && fq.Fresh.Status == freshStatusFresh {
			q := fq.Quote
			v.Price = round4(q.Price) // ETF 持仓现价保留 0.001 最小变动价位（个股两位小数不受影响）
			v.QuoteOK = true
			v.FreshnessStatus = freshStatusFresh
			if !q.DataTime.IsZero() {
				v.QuoteAsOf = q.DataTime.In(time.Local).Format("2006-01-02 15:04")
			}
			v.MarketValue = round2(q.Price * h.Quantity)
			if v.CostBasisNote == "" {
				v.ProfitAmount = round2(v.MarketValue - v.Cost)
				if v.Cost > 0 {
					v.ProfitPct = round2(v.ProfitAmount / v.Cost * 100)
				}
			}
			ov.MarketValue = round2(ov.MarketValue + v.MarketValue)
		} else {
			// 无当前有效行情：用成本估值，盈亏记 0，带过期标注（不冒充实时估值）。
			ov.QuoteStaleCount++
			v.MarketValue = v.Cost
			ov.MarketValue = round2(ov.MarketValue + v.Cost)
			if ok && fq.Quote != nil {
				v.FreshnessStatus = fq.Fresh.Status
				if fq.Quote.Price > 0 {
					v.LastPrice = round4(fq.Quote.Price)
				}
				if !fq.Quote.DataTime.IsZero() {
					v.QuoteAsOf = fq.Quote.DataTime.In(time.Local).Format("2006-01-02 15:04")
				}
				if note, _ := stockFreshnessNote(fq.Fresh, fq.Quote.DataTime); note != "" {
					v.StaleReason = note
				} else if fq.Fresh.Status == freshStatusUnknown {
					v.StaleReason = "该市场无交易日历，无法核验行情时效"
				}
			} else {
				v.FreshnessStatus = freshStatusStale
				v.StaleReason = "行情获取失败（可能停牌/数据源故障）"
			}
		}
		ov.Holdings = append(ov.Holdings, v)
	}
	ov.RealizedPnl, ov.RealizedUnavailableReason = realized, realizedNote
	valuationReason := ov.CurrencyUnavailableReason
	if valuationReason == "" {
		valuationReason = ov.ValuationUnavailableReason
	}
	if valuationReason != "" {
		ov.ValuationNote = valuationReason + "；原始持仓和流水继续保留，资产估值不可用"
		ov.MarketValue = 0
		return ov, nil
	}
	if ov.QuoteStaleCount > 0 {
		ov.ValuationNote = fmt.Sprintf("%d 笔持仓无当前有效行情，按成本估值计入总资产（非实时市值），浮动盈亏未知", ov.QuoteStaleCount)
	}
	ov.TotalAssets = round2(acc.Cash + ov.MarketValue)
	ov.TotalProfit = round2(ov.TotalAssets - acc.InitialCash)
	if acc.InitialCash > 0 {
		ov.TotalProfitPct = round2(ov.TotalProfit / acc.InitialCash * 100)
	}

	return ov, nil
}

// Trades 成交流水（倒序）。
func (s *PaperService) Trades(userID int64, limit int) ([]model.PaperTrade, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	return s.TradesByAccount(userID, account.ID, limit)
}
func (s *PaperService) TradesByAccount(userID, accountID int64, limit int) ([]model.PaperTrade, error) {
	if _, err := PortfolioAccountByID(userID, accountID, model.PortfolioKindPaper); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []model.PaperTrade
	err := common.DB.Where("user_id = ? AND account_id = ?", userID, accountID).Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Reset 重置账户：清空持仓与流水，现金恢复到指定初始资金（<=0 用默认）。
func (s *PaperService) Reset(userID int64, initialCash float64) (*model.PaperAccount, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	return s.ResetByAccount(userID, account.ID, initialCash)
}
func (s *PaperService) ResetByAccount(userID, accountID int64, initialCash float64) (*model.PaperAccount, error) {
	return s.ResetByAccountContext(context.Background(), userID, accountID, initialCash)
}

func (s *PaperService) ResetByAccountContext(ctx context.Context, userID, accountID int64, initialCash float64) (*model.PaperAccount, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if math.IsNaN(initialCash) || math.IsInf(initialCash, 0) {
		return nil, errors.New("初始资金无效")
	}
	if initialCash <= 0 {
		initialCash = model.PaperDefaultCash
	}
	if initialCash > 1e12 {
		return nil, errors.New("初始资金过大")
	}
	initialCash = round2(initialCash)
	if initialCash <= 0 {
		return nil, errors.New("初始资金不能小于 0.01 元")
	}
	acc, err := s.GetOrCreateAccountByID(userID, accountID)
	if err != nil {
		return nil, err
	}
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockedAccount(tx, userID, acc); err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Delete(&model.PaperHolding{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Delete(&model.PaperTrade{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Delete(&model.PaperCorpAdjust{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ? AND account_id = ? AND kind = ?", userID, accountID, model.SnapshotKindPaper).
			Delete(&model.PortfolioSnapshot{}).Error; err != nil {
			return err
		}
		acc.InitialCash = initialCash
		acc.Cash = initialCash
		return tx.Save(acc).Error
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return acc, nil
}
