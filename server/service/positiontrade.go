package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// B5 分批加仓 / 减仓：把持仓从一条静态记录升级为一本带流水的账本。
//
// **口径铁律**（下游兼容的关键，改动前先读）：
//   - `Position.BuyPrice` 恒为**当前持仓**的加权平均成本；
//   - `Position.Quantity` 恒为**当前持仓**数量（全部卖出后为 0）；
//   - `Position.BuyFee/BuyTax` 恒为**当前持仓**尚未结转的买入费税；
//   - 累计口径（一共买了多少、卖回多少、赚了多少）走 TotalBuyQty/TotalBuyCost/
//     TotalSellNet/RealizedPnl 四个汇总列，绝不从当前持仓字段反推。
//
// 于是 tracking 的 actual_position 标签、todo 止损、guard 事件、组合总览等既有消费方
// 读法零改动，而流水表是唯一的明细来源。

// positionQtyEps 数量比较容差（浮点数量；A 股最小 1 股，1e-6 足够严）。
const positionQtyEps = 1e-6

// positionLedger 持仓账本的可计算态（与 Position 的成本口径逐字段对应，抽出成纯函数
// 便于手工验算：加权成本重算、部分卖出结转、卖超拒绝全部在此收口）。
type positionLedger struct {
	AvgCost  float64 // 当前加权平均成本（元/股）
	Quantity float64 // 当前持仓数量（股）
	BuyFee   float64 // 当前持仓尚未结转的买入费用（元）
	BuyTax   float64 // 当前持仓尚未结转的买入税费（元）

	RealizedPnl   float64 // 累计已实现盈亏（元）
	TotalBuyCost  float64 // 累计买入总成本（元，含买入费税）
	TotalSellNet  float64 // 累计卖出净收入（元，已扣卖出费税）
	TotalBuyQty   float64 // 累计买入数量（股）
	RemainingCost float64 // 当前剩余仓位尚未结转的精确成本余额（含买入费税）
}

func ledgerFromPosition(p *model.Position) positionLedger {
	return positionLedger{
		AvgCost: p.BuyPrice, Quantity: p.Quantity, BuyFee: p.BuyFee, BuyTax: p.BuyTax,
		RealizedPnl: p.RealizedPnl, TotalBuyCost: p.TotalBuyCost,
		TotalSellNet: p.TotalSellNet, TotalBuyQty: p.TotalBuyQty,
		RemainingCost: p.RemainingCost,
	}
}

func (l positionLedger) applyTo(p *model.Position) {
	p.BuyPrice, p.Quantity, p.BuyFee, p.BuyTax = l.AvgCost, l.Quantity, l.BuyFee, l.BuyTax
	p.RealizedPnl, p.TotalBuyCost = l.RealizedPnl, l.TotalBuyCost
	p.TotalSellNet, p.TotalBuyQty = l.TotalSellNet, l.TotalBuyQty
	p.RemainingCost = l.RemainingCost
}

// currentCost 返回当前仓位的金额权威。旧数据尚无 RemainingCost 时回退原口径，
// 下一次惰性补建或交易会把余额固化，避免加权均价的 4 位舍入误差继续放大。
func (l positionLedger) currentCost() float64 {
	if l.TotalBuyCost > 0 || l.RemainingCost > 0 || l.Quantity <= positionQtyEps {
		return round4(l.RemainingCost)
	}
	return round4(l.AvgCost*l.Quantity + l.BuyFee + l.BuyTax)
}

// inferredRemainingCost 从累计账与卖出流水还原精确剩余成本。RealizedPnl 同时包含
// adjust 现金分红，不能直接全量代入恒等式；只有 sell 流水满足
// 「本笔已实现 = 卖出净额 - 本笔结转成本」。
func inferredRemainingCost(db *gorm.DB, p model.Position) (float64, error) {
	var row struct {
		SellRealized float64 `gorm:"column:sell_realized"`
	}
	if err := db.Model(&model.PositionTrade{}).
		Select("COALESCE(SUM(realized_pnl), 0) AS sell_realized").
		Where("position_id = ? AND user_id = ? AND side = ?", p.ID, p.UserID, model.PositionTradeSell).
		Scan(&row).Error; err != nil {
		return 0, err
	}
	return round4(p.TotalBuyCost - p.TotalSellNet + row.SellRealized), nil
}

// round4 见 paper.go（落库精度 decimal(20,4)）：金额/单价统一按 4 位收敛，
// 避免读回后与内存值漂移。

// ledgerBuy 加仓：加权重算成本、数量累加、费税累加。
// 加权成本 = (原成本×原数量 + 本次价×本次数量) / 新数量——与 computeView 的
// `Cost = BuyPrice*Quantity + BuyFee + BuyTax` 口径自洽（费税单独累加不摊进单价）。
func ledgerBuy(l positionLedger, price, qty, fee, tax float64) (positionLedger, error) {
	if price <= 0 {
		return l, errors.New("买入价格必须大于 0")
	}
	if qty <= 0 {
		return l, errors.New("买入数量必须大于 0")
	}
	if fee < 0 || tax < 0 {
		return l, errors.New("费用/税费不能为负")
	}
	currentCost := l.currentCost()
	newQty := l.Quantity + qty
	l.AvgCost = round4((l.AvgCost*l.Quantity + price*qty) / newQty)
	l.Quantity = round4(newQty)
	l.BuyFee = round4(l.BuyFee + fee)
	l.BuyTax = round4(l.BuyTax + tax)
	l.TotalBuyCost = round4(l.TotalBuyCost + price*qty + fee + tax)
	l.TotalBuyQty = round4(l.TotalBuyQty + qty)
	l.RemainingCost = round4(currentCost + price*qty + fee + tax)
	return l, nil
}

// ledgerSell 减仓：按当前加权成本结转已实现盈亏，数量递减，买入费税按卖出比例结转。
// 已实现盈亏 = (卖出金额 − 卖出费税) − (加权成本×卖出数量 + 按比例分摊的买入费税)。
// 卖超（数量大于当前持仓）一律拒绝——账本不允许出现负持仓。
// 返回本笔结转的已实现盈亏。
func ledgerSell(l positionLedger, price, qty, fee, tax float64) (positionLedger, float64, error) {
	if price <= 0 {
		return l, 0, errors.New("卖出价格必须大于 0")
	}
	if qty <= 0 {
		return l, 0, errors.New("卖出数量必须大于 0")
	}
	if fee < 0 || tax < 0 {
		return l, 0, errors.New("费用/税费不能为负")
	}
	if l.Quantity <= positionQtyEps {
		return l, 0, errors.New("该持仓已无可卖数量")
	}
	if qty > l.Quantity+positionQtyEps {
		return l, 0, fmt.Errorf("卖出数量超过当前持仓（持有 %g，卖出 %g）", l.Quantity, qty)
	}
	sellAll := qty >= l.Quantity-positionQtyEps
	allocFee, allocTax := round4(l.BuyFee*qty/l.Quantity), round4(l.BuyTax*qty/l.Quantity)
	if sellAll {
		// 清仓时把剩余买入费税一次结清，避免按比例分摊的舍入残渣永久挂在账上。
		allocFee, allocTax = l.BuyFee, l.BuyTax
	}
	remainingCost := l.currentCost()
	costPart := remainingCost
	if !sellAll {
		costPart = round4(remainingCost * qty / l.Quantity)
	}
	netProceeds := price*qty - fee - tax
	realized := round4(netProceeds - costPart)

	l.RealizedPnl = round4(l.RealizedPnl + realized)
	l.TotalSellNet = round4(l.TotalSellNet + netProceeds)
	l.BuyFee = round4(l.BuyFee - allocFee)
	l.BuyTax = round4(l.BuyTax - allocTax)
	l.RemainingCost = round4(remainingCost - costPart)
	if sellAll {
		l.Quantity, l.BuyFee, l.BuyTax, l.RemainingCost = 0, 0, 0, 0
	} else {
		l.Quantity = round4(l.Quantity - qty)
	}
	// 加权成本不因卖出改变（卖出结转的是盈亏，不是成本）。清仓后保留最后的加权成本
	// 供复盘展示（Quantity=0 时它不参与任何金额计算）。
	return l, realized, nil
}

// PositionTradeInput 加仓/减仓入参。
type PositionTradeInput struct {
	Side      string  `json:"side"` // buy / sell
	Price     float64 `json:"price"`
	Quantity  float64 `json:"quantity"`
	Fee       float64 `json:"fee"`
	Tax       float64 `json:"tax"`
	TradeDate string  `json:"trade_date"`
	Note      string  `json:"note"`

	// 减仓到 0 自动平仓时沿用既有 Close 的复盘字段（可选；非清仓笔忽略）。
	SellReason    string `json:"sell_reason"`
	ReviewNote    string `json:"review_note"`
	SellPlanned   string `json:"sell_planned"`
	AiVerdict     string `json:"ai_verdict"`
	LessonLearned string `json:"lesson_learned"`

	// closeAll 仅供 PositionService.Close 使用：在持仓行锁内读取实时剩余数量，避免
	// 事务外读数量后与并发加减仓竞态。HTTP 入参无法设置。
	closeAll bool
}

// lockedPosition 事务内按 (id, user_id) 重读持仓并加行锁；MySQL 走 FOR UPDATE 串行化
// 并发加减仓，SQLite 不支持该子句（单写者天然串行）跳过。**user_id 条件不可省**——
// 全链路隔离铁律，锁的同时就完成归属校验。
func lockedPosition(tx *gorm.DB, userID, id int64, p *model.Position) error {
	q := tx.Where("id = ? AND user_id = ?", id, userID)
	if !common.UsingSQLite {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q.First(p).Error
}

func positionInAccount(userID, accountID, positionID int64) error {
	if accountID <= 0 {
		return gorm.ErrRecordNotFound
	}
	var n int64
	if err := common.DB.Model(&model.Position{}).Where("id = ? AND user_id = ? AND account_id = ?", positionID, userID, accountID).Count(&n).Error; err != nil {
		return err
	}
	if n == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// buildInitialTrade 建仓时的首笔 buy 流水（Create 内联调用，保证账本从第一天就自洽）。
func buildInitialTrade(p *model.Position) model.PositionTrade {
	return model.PositionTrade{
		UserID: p.UserID, AccountID: p.AccountID, PositionID: p.ID, Side: model.PositionTradeBuy,
		Price: p.BuyPrice, Quantity: p.Quantity, Fee: p.BuyFee, Tax: p.BuyTax,
		TradeDate: p.BuyDate, Note: "建仓",
		AvgCostAfter: p.BuyPrice, QuantityAfter: p.Quantity,
	}
}

// ensurePositionTradesTx 旧持仓惰性补建等价流水。
//
// 三条硬要求：
//  1. **幂等**：已有任意流水即跳过（不按条数猜）。
//  2. **并发安全**：调用方必须已在事务内持有该持仓行锁（lockedPosition），
//     两个请求同时补建时后者会看到前者已提交的流水。
//  3. **绝不改变汇总值**：只补 RealizedPnl/TotalBuyCost/TotalSellNet/TotalBuyQty 四个
//     新列与流水行，**不动** BuyPrice/Quantity/BuyFee/BuyTax/SellPrice 等既有字段——
//     旧 closed 持仓的 Quantity 保持为「平仓时的数量」，展示与导出逐字不变。
//     回填出的 RealizedPnl 与旧 computeView 的 `proceeds − SellFee − SellTax − Cost`
//     完全同式，所以补建前后用户看到的盈亏一分不差。
func ensurePositionTradesTx(tx *gorm.DB, p *model.Position) error {
	var n int64
	if err := tx.Model(&model.PositionTrade{}).
		Where("position_id = ? AND user_id = ?", p.ID, p.UserID).Count(&n).Error; err != nil {
		return err
	}
	if n > 0 {
		if p.Status == model.PositionStatusHolding && p.Quantity > positionQtyEps && p.RemainingCost <= 0 {
			if p.TotalBuyCost > 0 {
				// 账本恒等式只使用 sell 流水的已实现；adjust 现金分红不冲减也不抬高成本。
				var err error
				p.RemainingCost, err = inferredRemainingCost(tx, *p)
				if err != nil {
					return err
				}
				if p.RemainingCost < -positionQtyEps {
					return fmt.Errorf("持仓账本剩余成本异常为负数：%g", p.RemainingCost)
				}
				if p.RemainingCost < 0 {
					p.RemainingCost = 0
				}
			} else {
				p.RemainingCost = round4(p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax)
			}
			return tx.Model(&model.Position{}).Where("id = ? AND user_id = ?", p.ID, p.UserID).
				Update("remaining_cost", p.RemainingCost).Error
		}
		return nil
	}
	buyCost := round4(p.BuyPrice*p.Quantity + p.BuyFee + p.BuyTax)
	trades := []model.PositionTrade{{
		UserID: p.UserID, AccountID: p.AccountID, PositionID: p.ID, Side: model.PositionTradeBuy,
		Price: p.BuyPrice, Quantity: p.Quantity, Fee: p.BuyFee, Tax: p.BuyTax,
		TradeDate: p.BuyDate, Note: "历史建仓（补建）", Backfilled: true,
		AvgCostAfter: p.BuyPrice, QuantityAfter: p.Quantity,
	}}
	p.TotalBuyCost, p.TotalBuyQty = buyCost, p.Quantity
	p.TotalSellNet, p.RealizedPnl = 0, 0
	p.RemainingCost = buyCost
	if p.Status == model.PositionStatusClosed && p.SellPrice > 0 {
		sellNet := round4(p.SellPrice*p.Quantity - p.SellFee - p.SellTax)
		trades = append(trades, model.PositionTrade{
			UserID: p.UserID, AccountID: p.AccountID, PositionID: p.ID, Side: model.PositionTradeSell,
			Price: p.SellPrice, Quantity: p.Quantity, Fee: p.SellFee, Tax: p.SellTax,
			TradeDate: p.SellDate, Note: "历史平仓（补建）", Backfilled: true,
			RealizedPnl: round4(sellNet - buyCost), AvgCostAfter: p.BuyPrice, QuantityAfter: 0,
		})
		p.TotalSellNet = sellNet
		p.RealizedPnl = round4(sellNet - buyCost)
		p.RemainingCost = 0
	}
	if err := tx.Create(&trades).Error; err != nil {
		return err
	}
	return tx.Model(&model.Position{}).Where("id = ? AND user_id = ?", p.ID, p.UserID).
		Updates(map[string]any{
			"realized_pnl":   p.RealizedPnl,
			"total_buy_cost": p.TotalBuyCost,
			"total_sell_net": p.TotalSellNet,
			"total_buy_qty":  p.TotalBuyQty,
			"remaining_cost": p.RemainingCost,
		}).Error
}

// backfillPositionLedgers 列表读取时的批量惰性补建。逐笔独立事务：单条失败不影响其它，
// 也不阻断列表返回（补建只是账本自洽的补丁，不是查询的前置条件）。
// 返回是否确有写入（调用方据此决定要不要重读，正常请求这里全命中、零写入零重读）。
func backfillPositionLedgers(userID int64, positions []model.Position) bool {
	if common.DB == nil || len(positions) == 0 {
		return false
	}
	ids := make([]int64, 0, len(positions))
	for _, p := range positions {
		if p.TotalBuyCost <= 0 ||
			(p.Status == model.PositionStatusHolding && p.Quantity > positionQtyEps && p.RemainingCost <= 0) {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		return false
	}
	wrote := false
	for _, id := range ids {
		err := common.DB.Transaction(func(tx *gorm.DB) error {
			var p model.Position
			if err := lockedPosition(tx, userID, id, &p); err != nil {
				return err
			}
			return ensurePositionTradesTx(tx, &p)
		})
		if err != nil {
			common.SysWarn("持仓 %d 补建流水失败: %v", id, err)
			continue
		}
		wrote = true
	}
	return wrote
}

// ensurePositionLedgersStrict 是资产快照使用的 fail-closed 版本。与列表读取的 best-effort
// 补建不同，任何一笔失败都会返回错误，调用方不得据不完整 RealizedPnl 落历史快照。
func ensurePositionLedgersStrict(userID int64, positions []model.Position) (bool, error) {
	if common.DB == nil {
		return false, errors.New("数据库不可用")
	}
	if len(positions) == 0 {
		return false, nil
	}
	ids := make([]int64, 0, len(positions))
	for _, p := range positions {
		// 汇总口径已经完整的 legacy closed 记录可能没有足够字段重建明细
		// （例如 Quantity/SellPrice 已归零），但它的 RealizedPnl/TotalBuyCost
		// 仍可直接用于快照；只有汇总本身缺失时才要求严格补建。
		if p.TotalBuyCost <= 0 ||
			(p.Status == model.PositionStatusHolding && p.Quantity > positionQtyEps && p.RemainingCost <= 0) {
			ids = append(ids, p.ID)
		}
	}
	wrote := false
	for _, id := range ids {
		if err := common.DB.Transaction(func(tx *gorm.DB) error {
			var p model.Position
			if err := lockedPosition(tx, userID, id, &p); err != nil {
				return err
			}
			return ensurePositionTradesTx(tx, &p)
		}); err != nil {
			return wrote, fmt.Errorf("持仓 %d 补建流水失败: %w", id, err)
		}
		wrote = true
	}
	return wrote, nil
}

// ListTrades 列出某持仓的流水明细（仅本人；按时间正序）。读取前惰性补建，
// 保证「老持仓点开流水也看得到等价首笔买入」。
func (s *PositionService) ListTrades(userID, positionID int64) ([]model.PositionTrade, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var p model.Position
	if err := common.DB.Where("id = ? AND user_id = ?", positionID, userID).First(&p).Error; err != nil {
		return nil, errors.New("持仓不存在")
	}
	if p.TotalBuyCost <= 0 ||
		(p.Status == model.PositionStatusHolding && p.Quantity > positionQtyEps && p.RemainingCost <= 0) {
		if err := common.DB.Transaction(func(tx *gorm.DB) error {
			var locked model.Position
			if err := lockedPosition(tx, userID, positionID, &locked); err != nil {
				return err
			}
			return ensurePositionTradesTx(tx, &locked)
		}); err != nil {
			common.SysWarn("持仓 %d 补建流水失败: %v", positionID, err)
		}
	}
	var rows []model.PositionTrade
	if err := common.DB.Where("position_id = ? AND user_id = ?", positionID, userID).
		Order("trade_date ASC, id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []model.PositionTrade{}
	}
	return rows, nil
}

// AddTrade 加仓 / 减仓。全流程在单事务内：行锁读持仓 → 惰性补流水 → 账本重算 →
// 落流水 → 回写汇总。减到 0 自动置 closed 并写入复盘字段（沿用既有 Close 的维度）。
func (s *PositionService) AddTrade(userID, positionID int64, in PositionTradeInput) (*model.Position, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	side := strings.ToLower(strings.TrimSpace(in.Side))
	if side != model.PositionTradeBuy && side != model.PositionTradeSell {
		return nil, errors.New("交易方向须为 buy 或 sell")
	}
	today := time.Now().In(time.Local).Format("2006-01-02")
	if strings.TrimSpace(in.TradeDate) == "" {
		in.TradeDate = today
	} else if _, err := time.Parse("2006-01-02", in.TradeDate); err != nil {
		return nil, errors.New("交易日期格式应为 YYYY-MM-DD")
	}
	if in.TradeDate > today {
		return nil, errors.New("交易日期不能晚于今天")
	}
	if side == model.PositionTradeSell {
		if !validSellPlanned[in.SellPlanned] {
			return nil, errors.New("是否按计划卖出取值须为 yes/no/partial")
		}
		if !validAiVerdict[in.AiVerdict] {
			return nil, errors.New("AI 判断对错取值须为 right/wrong/mixed/unused")
		}
	}

	var out model.Position
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var p model.Position
		if err := lockedPosition(tx, userID, positionID, &p); err != nil {
			return errors.New("持仓不存在")
		}
		if p.Status == model.PositionStatusClosed {
			return errors.New("该持仓已平仓，不能再加减仓")
		}
		// 先把旧持仓补成有流水的账本，再叠加本次操作——否则新流水会挂在一本空账上。
		if err := ensurePositionTradesTx(tx, &p); err != nil {
			return err
		}
		if in.TradeDate != "" && p.BuyDate != "" && in.TradeDate < p.BuyDate {
			return errors.New("交易日期不能早于建仓日期")
		}
		if side == model.PositionTradeSell && in.closeAll {
			in.Quantity = p.Quantity
		}
		var lastTrade model.PositionTrade
		if err := tx.Where("position_id = ? AND user_id = ?", p.ID, userID).
			Order("trade_date DESC, id DESC").First(&lastTrade).Error; err != nil {
			return err
		}
		if lastTrade.TradeDate != "" && in.TradeDate < lastTrade.TradeDate {
			return fmt.Errorf("交易日期不能早于最近一笔流水日期 %s（账本按录入顺序结转）", lastTrade.TradeDate)
		}
		if side == model.PositionTradeSell {
			if err := ensurePositionShareActionsProcessedTx(tx, p, in.TradeDate); err != nil {
				return err
			}
		}

		ledger := ledgerFromPosition(&p)
		// D15：先补齐峰值（老持仓可能尚未初始化），加仓分支随后按新成本重置它。
		if _, err := ensurePositionPeakTx(tx, &p, today); err != nil {
			return err
		}
		trade := model.PositionTrade{
			UserID: userID, AccountID: p.AccountID, PositionID: p.ID, Side: side,
			Price: in.Price, Quantity: in.Quantity, Fee: in.Fee, Tax: in.Tax,
			TradeDate: in.TradeDate, Note: truncateRunes(strings.TrimSpace(in.Note), 200),
		}
		if side == model.PositionTradeBuy {
			next, err := ledgerBuy(ledger, in.Price, in.Quantity, in.Fee, in.Tax)
			if err != nil {
				return err
			}
			ledger = next
		} else {
			next, realized, err := ledgerSell(ledger, in.Price, in.Quantity, in.Fee, in.Tax)
			if err != nil {
				return err
			}
			ledger, trade.RealizedPnl = next, realized
		}
		trade.AvgCostAfter, trade.QuantityAfter = ledger.AvgCost, ledger.Quantity
		if err := tx.Create(&trade).Error; err != nil {
			return err
		}
		ledger.applyTo(&p)
		if side == model.PositionTradeBuy {
			// D15 口径：**加仓重置持仓期峰值**（成本已变，加仓前的高点不再是这本账
			// 赚到过的利润）；减仓不重置（剩余仓位的持有期是连续的）。
			// 完整理由与反例见 model.Position.PeakPrice 注释与 resetPeakOnBuy。
			if err := rebuildPositionPeakOnBuyTx(tx, &p, in.Price, in.TradeDate, today); err != nil {
				return err
			}
		}
		if side == model.PositionTradeSell {
			// 卖出笔同时刷新「最近一次卖出」快照（既有字段语义：最后一笔卖出）。
			p.SellPrice, p.SellFee, p.SellTax = in.Price, in.Fee, in.Tax
			if in.TradeDate != "" {
				p.SellDate = in.TradeDate
			}
			if r := truncateRunes(strings.TrimSpace(in.SellReason), 500); r != "" {
				p.SellReason = r
			}
		}
		if ledger.Quantity <= positionQtyEps {
			// 减到 0 自动平仓，并沿用既有 Close 的结构化复盘字段。
			p.Status = model.PositionStatusClosed
			if r := truncateRunes(strings.TrimSpace(in.ReviewNote), 500); r != "" {
				p.ReviewNote = r
			}
			if in.SellPlanned != "" {
				p.SellPlanned = in.SellPlanned
			}
			if in.AiVerdict != "" {
				p.AiVerdict = in.AiVerdict
			}
			if l := truncateRunes(strings.TrimSpace(in.LessonLearned), 500); l != "" {
				p.LessonLearned = l
			}
		}
		if err := tx.Save(&p).Error; err != nil {
			return err
		}
		if p.Status == model.PositionStatusClosed {
			if err := finalizePositionSellSignalsTx(tx, userID, p.ID, false); err != nil {
				return err
			}
		}
		out = p
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
