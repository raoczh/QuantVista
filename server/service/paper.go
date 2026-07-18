package service

import (
	"context"
	"errors"
	"math"
	"strings"

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
	q := tx.Where("user_id = ?", userID)
	if !common.UsingSQLite {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q.First(acc).Error
}

// GetOrCreateAccount 取用户模拟账户，无则以默认初始资金创建。
func (s *PaperService) GetOrCreateAccount(userID int64) (*model.PaperAccount, error) {
	var acc model.PaperAccount
	err := common.DB.Where("user_id = ?", userID).First(&acc).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		acc = model.PaperAccount{UserID: userID, InitialCash: model.PaperDefaultCash, Cash: model.PaperDefaultCash}
		if err := common.DB.Create(&acc).Error; err != nil {
			return nil, err
		}
		return &acc, nil
	}
	if err != nil {
		return nil, err
	}
	return &acc, nil
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
	symbol, market, err := normalizeSymbolMarket(in.Symbol, in.Market)
	if err != nil {
		return nil, err
	}
	side := strings.ToLower(strings.TrimSpace(in.Side))
	if side != model.PaperSideBuy && side != model.PaperSideSell {
		return nil, errors.New("交易方向须为 buy 或 sell")
	}
	if in.Quantity <= 0 {
		return nil, errors.New("数量必须大于 0")
	}

	// 成交价：给定则用给定（跳过行情，便于离线/指定价成交）；否则取实时行情价。
	// fail-closed：市价成交走新鲜行情链路，全源 stale（停牌/数据源延迟）时拒绝按旧价
	// 成交——旧价成交会让模拟盘账面凭空盈亏，用户可手动指定价格明确担责。
	// 精度用 round4（列为 decimal(20,4)）：ETF/基金最小变动价位 0.001 元，round2 会抹掉限价第三位小数。
	price := round4(in.Price)
	name := ""
	if price <= 0 {
		q, fi, e := s.market.GetFreshQuote(ctx, market, symbol)
		if e != nil {
			if errors.Is(e, datasource.ErrSymbolInvalid) {
				return nil, errors.New("无法识别的股票代码")
			}
			return nil, errors.New("无法获取成交价，请手动指定价格")
		}
		if fi.Status == freshStatusStale {
			return nil, errors.New("实时行情已过期（可能停牌或数据源延迟），拒绝按旧价成交；请手动指定价格或稍后重试")
		}
		price = round4(q.Price)
		name = q.Name
	}
	if price <= 0 {
		return nil, errors.New("成交价必须大于 0")
	}
	if name == "" {
		name = strings.TrimSpace(in.Name)
	}

	acc, err := s.GetOrCreateAccount(userID)
	if err != nil {
		return nil, err
	}

	trade := &model.PaperTrade{
		UserID: userID, Symbol: symbol, Market: market, Name: name,
		Side: side, Price: price, Quantity: in.Quantity,
	}

	err = common.DB.Transaction(func(tx *gorm.DB) error {
		// 事务内重读账户（MySQL 加行锁；SQLite 单写者天然串行），
		// 避免并发交易基于事务外的过期余额计算、互相覆盖丢失更新。
		if err := lockedAccount(tx, userID, acc); err != nil {
			return err
		}
		amount := round2(price * in.Quantity)
		fee, tax := tradeFee(market, side, symbol, amount)
		trade.Amount, trade.Fee, trade.Tax = amount, fee, tax

		var holding model.PaperHolding
		hErr := tx.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).First(&holding).Error
		hasHolding := hErr == nil

		if side == model.PaperSideBuy {
			costBasis := amount + fee
			if acc.Cash < costBasis {
				return errors.New("现金不足，无法买入")
			}
			acc.Cash = round2(acc.Cash - costBasis)
			if hasHolding {
				newQty := holding.Quantity + in.Quantity
				newAvg := (holding.Quantity*holding.AvgCost + costBasis) / newQty
				holding.Quantity = newQty
				holding.AvgCost = round4(newAvg)
				if name != "" {
					holding.Name = name
				}
				if err := tx.Save(&holding).Error; err != nil {
					return err
				}
			} else {
				holding = model.PaperHolding{
					UserID: userID, Symbol: symbol, Market: market, Name: name,
					Quantity: in.Quantity, AvgCost: round4(costBasis / in.Quantity),
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
			trade.RealizedPnl = round2(proceeds - holding.AvgCost*in.Quantity)
			// 数量按列精度 round4（round2 会把碎股残余清零/失真）；清仓判断用 epsilon。
			holding.Quantity = round4(holding.Quantity - in.Quantity)
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
		return tx.Create(trade).Error
	})
	if err != nil {
		return nil, err
	}
	return trade, nil
}

// PaperHoldingView 持仓 + 实时估值。
type PaperHoldingView struct {
	model.PaperHolding
	Price        float64 `json:"price"`
	QuoteOK      bool    `json:"quote_ok"`
	Cost         float64 `json:"cost"`          // 成本 = 均价*数量
	MarketValue  float64 `json:"market_value"`  // 市值 = 现价*数量
	ProfitAmount float64 `json:"profit_amount"` // 浮动盈亏
	ProfitPct    float64 `json:"profit_pct"`
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
}

// Overview 账户总览：持仓按实时行情估值，汇总总资产与盈亏。
func (s *PaperService) Overview(ctx context.Context, userID int64) (*PaperOverview, error) {
	acc, err := s.GetOrCreateAccount(userID)
	if err != nil {
		return nil, err
	}
	var holdings []model.PaperHolding
	if err := common.DB.Where("user_id = ?", userID).Order("id").Find(&holdings).Error; err != nil {
		return nil, err
	}

	refs := make([]QuoteRef, 0, len(holdings))
	for _, h := range holdings {
		refs = append(refs, QuoteRef{Market: h.Market, Symbol: h.Symbol})
	}
	quotes := s.market.QuotesFor(ctx, refs)

	ov := &PaperOverview{Account: acc, Holdings: make([]PaperHoldingView, 0, len(holdings))}
	for _, h := range holdings {
		v := PaperHoldingView{PaperHolding: h, Cost: round2(h.AvgCost * h.Quantity)}
		if q := quotes[QuoteKey(h.Market, h.Symbol)]; q != nil {
			v.Price = round4(q.Price) // ETF 持仓现价保留 0.001 最小变动价位（个股两位小数不受影响）
			v.QuoteOK = true
			v.MarketValue = round2(q.Price * h.Quantity)
			v.ProfitAmount = round2(v.MarketValue - v.Cost)
			if v.Cost > 0 {
				v.ProfitPct = round2(v.ProfitAmount / v.Cost * 100)
			}
			ov.MarketValue = round2(ov.MarketValue + v.MarketValue)
		} else {
			// 无行情：用成本估值，盈亏记 0。
			v.MarketValue = v.Cost
			ov.MarketValue = round2(ov.MarketValue + v.Cost)
		}
		ov.Holdings = append(ov.Holdings, v)
	}
	ov.TotalAssets = round2(acc.Cash + ov.MarketValue)
	ov.TotalProfit = round2(ov.TotalAssets - acc.InitialCash)
	if acc.InitialCash > 0 {
		ov.TotalProfitPct = round2(ov.TotalProfit / acc.InitialCash * 100)
	}

	// 累计已实现盈亏（卖出流水求和）。
	var realized float64
	common.DB.Model(&model.PaperTrade{}).Where("user_id = ? AND side = ?", userID, model.PaperSideSell).
		Select("COALESCE(SUM(realized_pnl),0)").Scan(&realized)
	ov.RealizedPnl = round2(realized)
	return ov, nil
}

// Trades 成交流水（倒序）。
func (s *PaperService) Trades(userID int64, limit int) ([]model.PaperTrade, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	var rows []model.PaperTrade
	err := common.DB.Where("user_id = ?", userID).Order("id DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// Reset 重置账户：清空持仓与流水，现金恢复到指定初始资金（<=0 用默认）。
func (s *PaperService) Reset(userID int64, initialCash float64) (*model.PaperAccount, error) {
	if initialCash <= 0 {
		initialCash = model.PaperDefaultCash
	}
	if initialCash > 1e12 {
		return nil, errors.New("初始资金过大")
	}
	initialCash = round2(initialCash)
	acc, err := s.GetOrCreateAccount(userID)
	if err != nil {
		return nil, err
	}
	err = common.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockedAccount(tx, userID, acc); err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", userID).Delete(&model.PaperHolding{}).Error; err != nil {
			return err
		}
		if err := tx.Where("user_id = ?", userID).Delete(&model.PaperTrade{}).Error; err != nil {
			return err
		}
		acc.InitialCash = initialCash
		acc.Cash = initialCash
		return tx.Save(acc).Error
	})
	if err != nil {
		return nil, err
	}
	return acc, nil
}
