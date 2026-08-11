package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// B8 除权除息调整持仓：修掉「10 转 10 后盈亏显示 -50%」这个真实错误数字。
//
// **最高纪律：程序绝不静默改写用户的真实账本。**
// 检测到持仓命中除权除息日只生成**待确认建议**（PositionCorpAdjust，持久化+幂等），
// 用户一键确认才在事务+行锁内写 PositionTrade{side:adjust} 并改写 Position，且可撤销。
// 模拟盘是虚拟账户无真实后果，例外——自动执行并落审计流水。
//
// 折算公式（严格按 A 股「每 10 股」口径，**不要预先除以 10**）：
//
//	新数量 = 当前数量 + 登记日有权数量 × (送股 + 转增)/10
//	新成本 = 当前成本 × 当前数量 / 新数量
//	现金分红 = 登记日有权数量 × 每10股派息 / 10
//
// 现金分红是真金到账，单独计入 RealizedPnl（与卖出兑现同属「已实现」）。它不能再
// 同时冲减成本，否则最终总收益会被重复计算一次。

// corpAdjustResult 一次折算的算术结果（纯函数产物，便于手工验算与单测）。
type corpAdjustResult struct {
	QtyAfter     float64 // 折算后数量（股）
	CostAfter    float64 // 折算后每股加权成本（元）
	CashDividend float64 // 本次到手税前现金分红（元）
}

// computeCorpAdjust 除权除息折算（纯函数）。qty/cost 为折算前的当前数量与每股成本；
// entitledQty 为登记日有权数量；bonus/transfer/dividend 均为每 10 股口径。
//
// 边界：数量或成本非正、方案三项全为零 → 返回 ok=false（无需调整，不生成建议）。
// 全部卖出后送转会把成本除以一个更大的数——数量为 0 时无从折算，直接拒绝。
func computeCorpAdjust(qty, cost, entitledQty, bonus, transfer, dividend float64) (corpAdjustResult, bool) {
	if qty <= positionQtyEps || cost <= 0 || entitledQty <= positionQtyEps {
		return corpAdjustResult{}, false
	}
	if bonus <= 0 && transfer <= 0 && dividend <= 0 {
		return corpAdjustResult{}, false
	}
	qtyAfter := round4(qty + entitledQty*(bonus+transfer)/10)
	if qtyAfter <= positionQtyEps {
		return corpAdjustResult{}, false
	}
	cash := round4(dividend * entitledQty / 10)
	costAfter := round4(cost * qty / qtyAfter)
	return corpAdjustResult{QtyAfter: qtyAfter, CostAfter: costAfter, CashDividend: cash}, true
}

// computePositionCorpAdjust 在通用送转算式之上使用账本的精确剩余成本余额修正每股成本。
// BuyPrice 只有四位，直接乘大数量会把舍入误差放大；RemainingCost 含尚未结转费税，
// 因此先减去独立展示的 BuyFee/BuyTax，再按送转后数量摊薄价格成本基。
func computePositionCorpAdjust(p model.Position, entitledQty, bonus, transfer, dividend float64) (corpAdjustResult, bool) {
	res, ok := computeCorpAdjust(p.Quantity, p.BuyPrice, entitledQty, bonus, transfer, dividend)
	if !ok || res.QtyAfter <= positionQtyEps || res.QtyAfter == p.Quantity {
		return res, ok
	}
	priceCost := round4(p.BuyPrice * p.Quantity)
	if p.RemainingCost > 0 {
		precise := round4(p.RemainingCost - p.BuyFee - p.BuyTax)
		if precise >= 0 {
			priceCost = precise
		}
	}
	res.CostAfter = round4(priceCost / res.QtyAfter)
	return res, true
}

// ---------- 建议生成 ----------

const corpAdjustLookbackDays = 30

func effectiveTradeDate(explicit string, createdAt time.Time) string {
	if explicit != "" {
		return explicit
	}
	if createdAt.IsZero() {
		return ""
	}
	return createdAt.In(time.Local).Format("2006-01-02")
}

func entitlementIncludes(tradeDate string, action model.CorporateAction) bool {
	if tradeDate == "" {
		return false
	}
	if action.RecordDate != "" {
		return tradeDate <= action.RecordDate
	}
	// 登记日缺失时保守使用除权日前一日口径；除权日新买入已按除权价成交，无本次权益。
	return action.ExDate != "" && tradeDate < action.ExDate
}

// positionEntitledQty 从流水重建登记日收盘数量。流水为空的升级前持仓按原始 BuyDate
// 兜底；有流水时 buy 加、sell 减、此前 adjust 的送转数量加回。
func positionEntitledQty(p model.Position, trades []model.PositionTrade, action model.CorporateAction) float64 {
	if len(trades) == 0 {
		// 升级前 closed 持仓可能还没有等价 buy/sell 流水，Quantity 保留的是原始
		// 建仓数量。卖出日在登记日或之前时收盘已无持仓，不能按原始数量发权益。
		if entitlementIncludes(p.BuyDate, action) &&
			!(p.Status == model.PositionStatusClosed && entitlementIncludes(p.SellDate, action)) {
			return round4(p.Quantity)
		}
		return 0
	}
	qty := 0.0
	for _, trade := range trades {
		if !entitlementIncludes(effectiveTradeDate(trade.TradeDate, trade.CreatedAt), action) {
			continue
		}
		switch trade.Side {
		case model.PositionTradeBuy, model.PositionTradeAdjust:
			qty += trade.Quantity
		case model.PositionTradeSell:
			qty -= trade.Quantity
		}
	}
	if qty < positionQtyEps {
		return 0
	}
	return round4(qty)
}

func hasShareAdjustment(action model.CorporateAction) bool {
	return action.BonusRatio > 0 || action.TransferRatio > 0
}

func positionTradeOnOrAfterAction(trades []model.PositionTrade, action model.CorporateAction) bool {
	if action.ExDate == "" {
		return false
	}
	for _, trade := range trades {
		if trade.Side != model.PositionTradeBuy && trade.Side != model.PositionTradeSell {
			continue
		}
		date := effectiveTradeDate(trade.TradeDate, trade.CreatedAt)
		if date != "" && date >= action.ExDate {
			return true
		}
	}
	return false
}

func positionSellOnOrAfterAction(trades []model.PositionTrade, action model.CorporateAction) bool {
	if action.ExDate == "" {
		return false
	}
	for _, trade := range trades {
		if trade.Side != model.PositionTradeSell {
			continue
		}
		date := effectiveTradeDate(trade.TradeDate, trade.CreatedAt)
		if date != "" && date >= action.ExDate {
			return true
		}
	}
	return false
}

// positionsWithUnconfirmedShareAction 返回给定持仓中「除权日已到、含送转、用户尚未
// 确认折算」的持仓 ID 集合。B8 纪律是程序绝不静默改账；确认之前持仓成本/峰值仍是
// 除权前口径，而行情已是除权后价格——任何基于成本/峰值/计划价的提醒与建议此时都是
// 假信号（10 转 10 会凭空显示 -50%），评估链路必须先跳过并引导用户确认折算。
// 查询失败时返回错误，由调用方决定按数据缺口降级还是保持原行为。
func positionsWithUnconfirmedShareAction(ctx context.Context, userID int64, positionIDs []int64, tradeDate string) (map[int64]bool, error) {
	out := map[int64]bool{}
	if common.DB == nil || len(positionIDs) == 0 || tradeDate == "" {
		return out, nil
	}
	var rows []model.PositionCorpAdjust
	if err := common.DB.WithContext(ctx).Select("position_id").Where(
		"user_id = ? AND position_id IN ? AND status IN ? AND ex_date <= ? AND (bonus_ratio > 0 OR transfer_ratio > 0)",
		userID, positionIDs, []string{model.CorpAdjustPending, model.CorpAdjustReverted}, tradeDate,
	).Find(&rows).Error; err != nil {
		return out, err
	}
	for _, row := range rows {
		out[row.PositionID] = true
	}
	return out, nil
}

// ensurePositionShareActionsProcessedTx 是加减仓的前向时序闸门。送转必须发生在除权日
// 交易之前；一旦先按旧数量/成本卖出，事后只改当前聚合态无法修复已实现盈亏。
func ensurePositionShareActionsProcessedTx(tx *gorm.DB, p model.Position, tradeDate string) error {
	if p.Market != "cn" || tradeDate == "" {
		return nil
	}
	// 已经生成的待处理记录不能因为 30 天扫描窗口滑过就自动失效；否则用户只要
	// 等到第 31 天，未入账送转便会绕过卖出闸门。
	var pending model.PositionCorpAdjust
	err := tx.Where(
		"user_id = ? AND position_id = ? AND status IN ? AND ex_date <= ? "+
			"AND (bonus_ratio > 0 OR transfer_ratio > 0)",
		p.UserID, p.ID, []string{model.CorpAdjustPending, model.CorpAdjustReverted}, tradeDate,
	).Order("ex_date ASC, corporate_action_id ASC").First(&pending).Error
	if err == nil {
		if pending.ManualReview {
			return fmt.Errorf("%s 存在 %s 送转需要人工核对，请先在持仓页明确处理后再交易",
				p.Symbol, pending.ExDate)
		}
		return fmt.Errorf("%s 存在 %s 到期送转尚未处理，请先在持仓页确认或忽略除权调整后再交易",
			p.Symbol, pending.ExDate)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	date, err := time.ParseInLocation("2006-01-02", tradeDate, time.Local)
	if err != nil {
		return err
	}
	since := date.AddDate(0, 0, -corpAdjustLookbackDays).Format("2006-01-02")
	var actions []model.CorporateAction
	if err := tx.Where(
		"symbol = ? AND market = ? AND ex_date BETWEEN ? AND ? AND (bonus_ratio > 0 OR transfer_ratio > 0)",
		p.Symbol, p.Market, since, tradeDate,
	).Order("ex_date ASC, id ASC").Find(&actions).Error; err != nil {
		return err
	}
	if len(actions) == 0 {
		return nil
	}
	var trades []model.PositionTrade
	if err := tx.Where("position_id = ? AND user_id = ?", p.ID, p.UserID).
		Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
		return err
	}
	for _, action := range actions {
		if positionEntitledQty(p, trades, action) <= positionQtyEps {
			continue
		}
		var adj model.PositionCorpAdjust
		err := tx.Where("user_id = ? AND position_id = ? AND corporate_action_id = ?",
			p.UserID, p.ID, action.ID).First(&adj).Error
		if err == nil && (adj.Status == model.CorpAdjustConfirmed || adj.Status == model.CorpAdjustDismissed) {
			continue
		}
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		return fmt.Errorf("%s 存在 %s 到期送转尚未处理，请先在持仓页确认或忽略除权调整后再交易",
			p.Symbol, action.ExDate)
	}
	return nil
}

// GenerateCorpAdjusts 为近 30 天已除权的持仓生成待确认调整建议，覆盖服务缺勤或用户当天
// 未打开应用的情况。幂等键仍是 (user, position, action)；pending/reverted 会按当前账面刷新，
// confirmed/dismissed 保持终态不覆盖。返回新生成的建议数与全部写入错误。
//
// A 股 holding 全部处理；closed 只处理可独立入账的纯现金分红。已平仓且含送转的
// 历史账无法在不重放流水的前提下安全修复，显式报错而不是静默漏权或倒序改成本。
func GenerateCorpAdjusts(userID int64, asOf string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	asOfDate, err := time.ParseInLocation("2006-01-02", asOf, time.Local)
	if err != nil {
		return 0, errors.New("判定日期格式应为 YYYY-MM-DD")
	}
	var positions []model.Position
	if err := common.DB.Where("user_id = ? AND status IN ? AND market = ?",
		userID, []string{model.PositionStatusHolding, model.PositionStatusClosed}, "cn").
		Order("id ASC").Find(&positions).Error; err != nil {
		return 0, err
	}
	if len(positions) == 0 {
		return 0, nil
	}
	syms := make([]string, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		if !seen[p.Symbol] {
			seen[p.Symbol] = true
			syms = append(syms, p.Symbol)
		}
	}
	positionIDs := make([]int64, 0, len(positions))
	for _, p := range positions {
		positionIDs = append(positionIDs, p.ID)
	}
	var allTrades []model.PositionTrade
	if err := common.DB.Where("user_id = ? AND position_id IN ?", userID, positionIDs).
		Order("trade_date ASC, id ASC").Find(&allTrades).Error; err != nil {
		return 0, err
	}
	tradesByPosition := make(map[int64][]model.PositionTrade, len(positions))
	for _, trade := range allTrades {
		tradesByPosition[trade.PositionID] = append(tradesByPosition[trade.PositionID], trade)
	}

	// 命中回补窗口内的方案（一次批量查，绝不逐 symbol 循环查）。
	var actions []model.CorporateAction
	since := asOfDate.AddDate(0, 0, -corpAdjustLookbackDays).Format("2006-01-02")
	if err := common.DB.Where("symbol IN ? AND market = ? AND ex_date BETWEEN ? AND ?", syms, "cn", since, asOf).
		Order("ex_date ASC, id ASC").Find(&actions).Error; err != nil {
		return 0, err
	}
	if len(actions) == 0 {
		return 0, nil
	}
	actionsBySym := map[string][]model.CorporateAction{}
	for i := range actions {
		a := actions[i]
		if !a.HasAdjustment() {
			continue // 方案三项全零（如「不分配」）：无需调整
		}
		actionsBySym[a.Symbol] = append(actionsBySym[a.Symbol], a)
	}

	created := 0
	var writeErrs []error
	for _, p := range positions {
		for _, a := range actionsBySym[p.Symbol] {
			entitledQty := positionEntitledQty(p, tradesByPosition[p.ID], a)
			if entitledQty <= positionQtyEps {
				continue
			}
			var existing model.PositionCorpAdjust
			err := common.DB.Where("user_id = ? AND position_id = ? AND corporate_action_id = ?",
				userID, p.ID, a.ID).First(&existing).Error
			hasExisting := err == nil
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				writeErrs = append(writeErrs, fmt.Errorf("读取 pos=%d action=%d 建议: %w", p.ID, a.ID, err))
				continue
			}
			if hasExisting && existing.Status != model.CorpAdjustPending && existing.Status != model.CorpAdjustReverted {
				continue
			}
			manualReview := false
			reviewReason := ""
			if p.Status == model.PositionStatusHolding && hasShareAdjustment(a) &&
				positionSellOnOrAfterAction(tradesByPosition[p.ID], a) {
				manualReview = true
				reviewReason = "除权日或之后已有卖出流水，无法自动倒序重放送转；账本可能仍按除权前数量与成本结转，请人工核对"
			}
			var res corpAdjustResult
			if manualReview {
				// 不伪造自动折算结果；现金分红金额仍可按登记日权益如实展示。
				res = corpAdjustResult{
					QtyAfter: p.Quantity, CostAfter: p.BuyPrice,
					CashDividend: round4(a.DividendPretax * entitledQty / 10),
				}
			} else if p.Status == model.PositionStatusClosed {
				if hasShareAdjustment(a) {
					manualReview = true
					reviewReason = "登记日有权但持仓已平仓且方案含送转，无法自动倒序重放历史流水；账本数量、成本和已实现盈亏可能需要人工核对"
					res = corpAdjustResult{
						QtyAfter: p.Quantity, CostAfter: p.BuyPrice,
						CashDividend: round4(a.DividendPretax * entitledQty / 10),
					}
				} else {
					if a.DividendPretax <= 0 {
						continue
					}
					res = corpAdjustResult{
						QtyAfter: p.Quantity, CostAfter: p.BuyPrice,
						CashDividend: round4(a.DividendPretax * entitledQty / 10),
					}
				}
			} else {
				var ok bool
				res, ok = computePositionCorpAdjust(p, entitledQty,
					a.BonusRatio, a.TransferRatio, a.DividendPretax)
				if !ok {
					continue
				}
			}
			adj := model.PositionCorpAdjust{
				UserID: userID, PositionID: p.ID, CorporateActionID: a.ID,
				Symbol: p.Symbol, Market: p.Market, Name: orSymbol(p.Name, a.Name),
				ExDate: a.ExDate, RecordDate: a.RecordDate,
				BonusRatio: a.BonusRatio, TransferRatio: a.TransferRatio,
				DividendPretax: a.DividendPretax, PlanProfile: a.PlanProfile,
				QtyBefore: p.Quantity, EntitledQty: entitledQty, QtyAfter: res.QtyAfter,
				CostBefore: p.BuyPrice, CostAfter: res.CostAfter,
				CashDividend: res.CashDividend,
				ManualReview: manualReview, ReviewReason: reviewReason,
				Status: model.CorpAdjustPending,
			}
			if !hasExisting {
				if e := common.DB.Create(&adj).Error; e != nil {
					writeErrs = append(writeErrs, fmt.Errorf("pos=%d action=%d: %w", p.ID, a.ID, e))
				} else {
					created++
				}
			} else {
				updates := map[string]any{
					"symbol": adj.Symbol, "market": adj.Market, "name": adj.Name,
					"ex_date": adj.ExDate, "record_date": adj.RecordDate,
					"bonus_ratio": adj.BonusRatio, "transfer_ratio": adj.TransferRatio,
					"dividend_pretax": adj.DividendPretax, "plan_profile": adj.PlanProfile,
					"qty_before": adj.QtyBefore, "entitled_qty": adj.EntitledQty, "qty_after": adj.QtyAfter,
					"cost_before": adj.CostBefore, "cost_after": adj.CostAfter,
					"cash_dividend": adj.CashDividend,
					"manual_review": adj.ManualReview, "review_reason": adj.ReviewReason,
				}
				if e := common.DB.Model(&model.PositionCorpAdjust{}).
					Where("id = ? AND user_id = ? AND status IN ?", existing.ID, userID,
						[]string{model.CorpAdjustPending, model.CorpAdjustReverted}).
					Updates(updates).Error; e != nil {
					writeErrs = append(writeErrs, fmt.Errorf("刷新 pos=%d action=%d 建议: %w", p.ID, a.ID, e))
				}
			}
			// manualReview 是正常业务态（已平仓含送转、除权后已卖出等，可长期存在），
			// 事实已落在行上的 manual_review/review_reason 供持仓页展示。不能进
			// writeErrs：那会让今日待办每天误报「除权除息调整读取失败、数据不完整」，
			// 并把真实写入错误淹没在告警噪音里。
		}
	}
	return created, errors.Join(writeErrs...)
}

// ListCorpAdjusts 列出用户的调整建议（status 空=全部 pending）。仅本人。
func ListCorpAdjusts(userID int64, status string) ([]model.PositionCorpAdjust, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	q := common.DB.Where("user_id = ?", userID)
	pendingOnly := false
	switch status {
	case "", model.CorpAdjustPending:
		q = q.Where("status = ?", model.CorpAdjustPending)
		pendingOnly = true
	case "all":
	case model.CorpAdjustConfirmed, model.CorpAdjustReverted, model.CorpAdjustDismissed:
		q = q.Where("status = ?", status)
	default:
		return nil, errors.New("非法的状态筛选")
	}
	var rows []model.PositionCorpAdjust
	order := "ex_date DESC, id DESC"
	if pendingOnly {
		// 多次送转必须按发生顺序入账；待确认列表也按最早方案优先呈现。
		order = "ex_date ASC, corporate_action_id ASC, id ASC"
	}
	if err := q.Order(order).Find(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []model.PositionCorpAdjust{}
	}
	return rows, nil
}

// ---------- 确认 / 撤销 / 忽略 ----------

// lockedCorpAdjust 事务内按 (id, user_id) 重读建议并加行锁。
// **user_id 条件不可省**——全链路隔离铁律，锁的同时完成归属校验（越权直接查不到）。
func lockedCorpAdjust(tx *gorm.DB, userID, id int64, adj *model.PositionCorpAdjust) error {
	q := tx.Where("id = ? AND user_id = ?", id, userID)
	if !common.UsingSQLite {
		q = q.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	return q.First(adj).Error
}

// ConfirmCorpAdjust 用户确认除权除息折算：事务 + 行锁内改写持仓并落 adjust 流水。
//
// 安全边界（每一条都有对应反例测试）：
//   - 越权：lockedCorpAdjust 带 user_id，他人的建议直接「不存在」；
//   - 重复确认：状态非 pending 一律拒绝；
//   - 过期建议：确认时用**行锁内的实时持仓**二次校验当前数量/成本与登记日权益；
//   - 已平仓：纯现金分红可落零数量审计并增加已实现收益，含送转则拒绝倒序改账；
//   - 时序：送转前若已有除权日后卖出，拒绝并要求人工核对，绝不按当前聚合态硬套。
func (s *PositionService) ConfirmCorpAdjust(userID, adjustID int64) (*model.PositionCorpAdjust, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var out model.PositionCorpAdjust
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var adj model.PositionCorpAdjust
		if err := lockedCorpAdjust(tx, userID, adjustID, &adj); err != nil {
			return errors.New("调整建议不存在")
		}
		switch adj.Status {
		case model.CorpAdjustPending, model.CorpAdjustReverted:
			// reverted 允许再次确认（撤销是「先不调」，不是「永久拒绝」）。
		case model.CorpAdjustConfirmed:
			return errors.New("该调整已确认，无需重复确认")
		case model.CorpAdjustDismissed:
			return errors.New("该调整已忽略，如需调整请重新生成建议")
		default:
			return errors.New("调整建议状态异常")
		}
		if adj.ManualReview {
			return errors.New("该记录需要人工核对历史账本，不能自动确认折算；核对后可选择忽略")
		}

		var p model.Position
		if err := lockedPosition(tx, userID, adj.PositionID, &p); err != nil {
			return errors.New("持仓不存在")
		}
		shareAdjustment := adj.BonusRatio > 0 || adj.TransferRatio > 0
		// 更早送转会改变本次登记日的有权数量，即使当前建议只是纯现金分红也必须
		// 先处理前序。相同除权日通常共享此前的登记日基数，不强制互相复利。
		earlierQ := tx.Where(
			"user_id = ? AND position_id = ? AND id <> ? AND status IN ? "+
				"AND (bonus_ratio > 0 OR transfer_ratio > 0)",
			userID, p.ID, adj.ID, []string{model.CorpAdjustPending, model.CorpAdjustReverted},
		)
		if adj.RecordDate != "" {
			earlierQ = earlierQ.Where("ex_date <> '' AND ex_date <= ?", adj.RecordDate)
		} else {
			earlierQ = earlierQ.Where("ex_date <> '' AND ex_date < ?", adj.ExDate)
		}
		var earlier model.PositionCorpAdjust
		err := earlierQ.Order("ex_date ASC, corporate_action_id ASC").First(&earlier).Error
		if err == nil {
			return fmt.Errorf("请先处理会影响本次登记日权益的送转方案（除权日 %s），再刷新并确认本条", earlier.ExDate)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if p.Status != model.PositionStatusHolding && shareAdjustment {
			return errors.New("该持仓已平仓且方案含送转，无法倒序重放历史账本，请人工核对")
		}
		if p.Status != model.PositionStatusHolding && p.Status != model.PositionStatusClosed {
			return errors.New("持仓状态异常，无法执行除权除息调整")
		}
		// 先把旧持仓补成有流水的账本，再叠加折算（否则 adjust 流水挂在一本空账上）。
		if err := ensurePositionTradesTx(tx, &p); err != nil {
			return err
		}
		// 过期校验：建议生成后持仓变过（加减仓/编辑），折算基数已失效。
		if !nearlyEqual(p.Quantity, adj.QtyBefore) || !nearlyEqual(p.BuyPrice, adj.CostBefore) {
			return fmt.Errorf("持仓在建议生成后已变动（当前 %g 股/成本 %g，建议基于 %g 股/成本 %g），请重新生成调整建议",
				p.Quantity, p.BuyPrice, adj.QtyBefore, adj.CostBefore)
		}
		var trades []model.PositionTrade
		if err := tx.Where("position_id = ? AND user_id = ?", p.ID, userID).
			Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
			return err
		}
		action := model.CorporateAction{
			ExDate: adj.ExDate, RecordDate: adj.RecordDate,
			BonusRatio: adj.BonusRatio, TransferRatio: adj.TransferRatio,
			DividendPretax: adj.DividendPretax,
		}
		entitledQty := positionEntitledQty(p, trades, action)
		if !nearlyEqual(entitledQty, adj.EntitledQty) {
			return fmt.Errorf("登记日权益数量已变化（当前 %g 股，建议基于 %g 股），请重新生成调整建议",
				entitledQty, adj.EntitledQty)
		}
		if shareAdjustment && positionSellOnOrAfterAction(trades, action) {
			return errors.New("除权日或之后已有加减仓流水，送转必须先于这些交易入账；当前版本不做倒序账本重放，请人工核对")
		}
		var res corpAdjustResult
		if shareAdjustment {
			var ok bool
			res, ok = computePositionCorpAdjust(p, entitledQty,
				adj.BonusRatio, adj.TransferRatio, adj.DividendPretax)
			if !ok {
				return errors.New("当前持仓无法折算（数量或成本为零）")
			}
		} else {
			if adj.DividendPretax <= 0 || entitledQty <= positionQtyEps {
				return errors.New("当前持仓没有可入账的现金分红")
			}
			res = corpAdjustResult{
				QtyAfter: p.Quantity, CostAfter: p.BuyPrice,
				CashDividend: round4(adj.DividendPretax * entitledQty / 10),
			}
		}
		// D15：持仓期最高价按市场价格侧公式同步折算——不折算的话，10 转 10 的
		// 除权当天会凭空出现一个 50% 的假回撤，移动止盈提醒当场误报。
		// 峰值未初始化（旧持仓尚未惰性补齐）时跳过，不猜一个值。
		peakBefore, peakAfter := p.PeakPrice, 0.0
		peakChanged := false
		if p.Status == model.PositionStatusHolding && !positionTradeOnOrAfterAction(trades, action) {
			v, perr := peakAfterCorpAdjust(p, adj.BonusRatio, adj.TransferRatio, adj.DividendPretax)
			if perr == nil {
				peakAfter, peakChanged = v, true
			} else {
				peakBefore = 0
			}
		} else {
			// 已平仓无需维护峰值；纯派息若之后已有交易，当前峰值可能已被加仓重置，
			// 再按历史除权日折算会污染新的峰值口径。
			peakBefore = 0
		}

		note := corpAdjustNote(adj)
		if peakChanged {
			note += peakAdjustNote(peakBefore, peakAfter)
		}
		trade := model.PositionTrade{
			UserID: userID, PositionID: p.ID, Side: model.PositionTradeAdjust,
			Price: 0, Quantity: round4(res.QtyAfter - p.Quantity), Fee: 0, Tax: 0,
			TradeDate: adj.ExDate,
			Note:      truncateRunes(note, 200),
			// 现金分红是真金到账，计入已实现盈亏。
			RealizedPnl:    res.CashDividend,
			AvgCostBefore:  p.BuyPrice,
			QuantityBefore: p.Quantity,
			AvgCostAfter:   res.CostAfter,
			QuantityAfter:  res.QtyAfter,
			// 显式外键：审计与撤销都靠这两列，不靠解析 note。
			CorporateActionID: adj.CorporateActionID,
			AdjustID:          adj.ID,
		}
		if err := tx.Create(&trade).Error; err != nil {
			return err
		}

		p.Quantity = res.QtyAfter
		p.BuyPrice = res.CostAfter
		if peakChanged {
			// 峰值起算日不变——除权不打断持有期，改变的只是价格刻度。
			p.PeakPrice = peakAfter
		}
		// 现金分红进累计已实现盈亏；TotalBuyCost/TotalBuyQty 保持不变——
		// 送转不是「又买了一次」，用户没有再投入一分钱，改动它们会让
		// 「一共投入多少 / 已平仓收益率」全部失真。
		p.RealizedPnl = round4(p.RealizedPnl + res.CashDividend)
		if err := tx.Save(&p).Error; err != nil {
			return err
		}

		now := time.Now()
		adj.Status = model.CorpAdjustConfirmed
		adj.TradeID = trade.ID
		adj.ConfirmedAt = &now
		adj.RevertedAt = nil
		// 落库实际执行值（生成时的预估与执行时若有差异，以执行值为准）。
		adj.EntitledQty = entitledQty
		adj.QtyAfter, adj.CostAfter, adj.CashDividend = res.QtyAfter, res.CostAfter, res.CashDividend
		// 峰值折算前后值成列：撤销时按 PeakBefore **原值**还原，不反算公式（避免舍入漂移）。
		adj.PeakBefore, adj.PeakAfter = peakBefore, peakAfter
		if err := tx.Save(&adj).Error; err != nil {
			return err
		}
		out = adj
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// RevertCorpAdjust 撤销已确认的除权除息折算：事务 + 行锁内回滚账本并删除 adjust 流水。
//
// **只有在「当前状态仍精确等于该次调整的结果、且其后没有任何其它交易/调整」时才允许撤销**——
// 否则回滚会把后续交易的账面一并改坏。拒绝时明确告知原因，不做尽力而为的部分回滚。
func (s *PositionService) RevertCorpAdjust(userID, adjustID int64) (*model.PositionCorpAdjust, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var out model.PositionCorpAdjust
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var adj model.PositionCorpAdjust
		if err := lockedCorpAdjust(tx, userID, adjustID, &adj); err != nil {
			return errors.New("调整建议不存在")
		}
		if adj.Status != model.CorpAdjustConfirmed {
			return errors.New("只有已确认的调整才能撤销")
		}
		var p model.Position
		if err := lockedPosition(tx, userID, adj.PositionID, &p); err != nil {
			return errors.New("持仓不存在")
		}
		shareAdjustment := adj.BonusRatio > 0 || adj.TransferRatio > 0
		if p.Status != model.PositionStatusHolding && shareAdjustment {
			return errors.New("该持仓已平仓，无法撤销除权除息折算（平仓已按折算后账面结算）")
		}
		if p.Status != model.PositionStatusHolding && p.Status != model.PositionStatusClosed {
			return errors.New("持仓状态异常，无法撤销除权除息调整")
		}
		// 该 adjust 流水必须是持仓上的**最后一笔**：之后有任何流水（加仓/减仓/再次除权）
		// 都意味着后续账面建立在折算结果之上，回滚会连带改坏它们。
		var laterCount int64
		if err := tx.Model(&model.PositionTrade{}).
			Where("position_id = ? AND user_id = ? AND id > ?", p.ID, userID, adj.TradeID).
			Count(&laterCount).Error; err != nil {
			return err
		}
		if laterCount > 0 {
			return errors.New("折算之后已有新的交易或调整，无法撤销（请用加仓/减仓修正账本）")
		}
		// 当前账面必须仍精确等于折算结果（防手工编辑后再撤销把账改坏）。
		if !nearlyEqual(p.Quantity, adj.QtyAfter) || !nearlyEqual(p.BuyPrice, adj.CostAfter) {
			return fmt.Errorf("当前持仓（%g 股/成本 %g）与折算结果（%g 股/成本 %g）不一致，无法撤销",
				p.Quantity, p.BuyPrice, adj.QtyAfter, adj.CostAfter)
		}
		// 删流水（按 id + 归属双条件，绝不按 adjust_id 批量删）。
		if adj.TradeID > 0 {
			if err := tx.Where("id = ? AND user_id = ? AND position_id = ?",
				adj.TradeID, userID, p.ID).Delete(&model.PositionTrade{}).Error; err != nil {
				return err
			}
		}
		p.Quantity = adj.QtyBefore
		p.BuyPrice = adj.CostBefore
		// D15：峰值按落库的**原值**还原（不反算折算公式——反算会留下几厘的舍入漂移，
		// 让「撤销后应与折算前逐字节一致」这条不变式失守）。PeakBefore=0 表示折算时
		// 该持仓尚无峰值记录，此时不动峰值。
		if adj.PeakBefore > 0 {
			p.PeakPrice = adj.PeakBefore
		}
		p.RealizedPnl = round4(p.RealizedPnl - adj.CashDividend)
		if err := tx.Save(&p).Error; err != nil {
			return err
		}
		now := time.Now()
		adj.Status = model.CorpAdjustReverted
		adj.RevertedAt = &now
		adj.TradeID = 0
		if err := tx.Save(&adj).Error; err != nil {
			return err
		}
		out = adj
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DismissCorpAdjust 忽略一条待确认建议（终态，不再提示）。已确认的不能直接忽略——
// 先撤销再忽略，避免「账本已改但建议显示已忽略」的错位。
func (s *PositionService) DismissCorpAdjust(userID, adjustID int64) (*model.PositionCorpAdjust, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	var out model.PositionCorpAdjust
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		var adj model.PositionCorpAdjust
		if err := lockedCorpAdjust(tx, userID, adjustID, &adj); err != nil {
			return errors.New("调整建议不存在")
		}
		switch adj.Status {
		case model.CorpAdjustPending, model.CorpAdjustReverted:
		case model.CorpAdjustDismissed:
			return errors.New("该建议已忽略")
		case model.CorpAdjustConfirmed:
			return errors.New("该调整已确认并写入账本，请先撤销再忽略")
		default:
			return errors.New("调整建议状态异常")
		}
		adj.Status = model.CorpAdjustDismissed
		if err := tx.Save(&adj).Error; err != nil {
			return err
		}
		out = adj
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// corpAdjustNote 折算流水的人话说明（note 只作展示，**不承担任何数据语义**——
// 数量/成本/来源全部有独立列，解析 note 是反模式）。
func corpAdjustNote(adj model.PositionCorpAdjust) string {
	if adj.PlanProfile != "" {
		return "除权除息折算：" + adj.PlanProfile
	}
	return fmt.Sprintf("除权除息折算：每10股送%.4g转%.4g派%.4g元",
		adj.BonusRatio, adj.TransferRatio, adj.DividendPretax)
}

// nearlyEqual 金额/数量的浮点相等判定（沿用 positionQtyEps 口径）。
func nearlyEqual(a, b float64) bool {
	d := a - b
	return d < positionQtyEps && d > -positionQtyEps
}

// ---------- 模拟盘自动调整 ----------

// RunPaperCorpAdjust 模拟盘除权除息**自动**折算（虚拟账户无真实后果，不需用户确认）。
// 按 (user_id, corporate_action_id) 唯一键保证重跑不重复。返回调整笔数。
//
// 只处理「今天及以前 7 天内」除权的方案：窗口给足服务缺勤的补跑余量，
// 又不会在首次部署时把历史上所有除权一次性补进模拟账户。候选来自 holding 与流水
// 的并集，已清仓纯现金分红仍补发；已发生除权后卖出的送转显式阻断。
func RunPaperCorpAdjust() int {
	if common.DB == nil {
		return 0
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	since := now.AddDate(0, 0, -paperCorpAdjustWindowDays).Format("2006-01-02")

	// 先取窗口内全部方案，再反查相关模拟流水。不能只从当前 holding 出发：用户在
	// 登记日有权、除权后清仓时 holding 已删除，纯现金分红仍必须补到账户。
	var actions []model.CorporateAction
	if err := common.DB.Where("market = ? AND ex_date BETWEEN ? AND ?", "cn", since, today).
		Order("ex_date ASC, id ASC").Find(&actions).Error; err != nil {
		common.SysWarn("模拟盘除权调整读取方案失败: %v", err)
		return 0
	}
	filtered := actions[:0]
	syms := make([]string, 0, len(actions))
	seenSym := map[string]bool{}
	for _, action := range actions {
		if !action.HasAdjustment() {
			continue
		}
		filtered = append(filtered, action)
		if !seenSym[action.Symbol] {
			seenSym[action.Symbol] = true
			syms = append(syms, action.Symbol)
		}
	}
	actions = filtered
	if len(actions) == 0 {
		return 0
	}

	var holdings []model.PaperHolding
	if err := common.DB.Where("market = ? AND symbol IN ? AND quantity > 0", "cn", syms).
		Find(&holdings).Error; err != nil {
		common.SysWarn("模拟盘除权调整读取持仓失败: %v", err)
		return 0
	}
	var trades []model.PaperTrade
	if err := common.DB.Where("market = ? AND symbol IN ?", "cn", syms).
		Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
		common.SysWarn("模拟盘除权调整读取流水失败: %v", err)
		return 0
	}
	type paperCandidate struct {
		UserID         int64
		Symbol, Market string
		Name           string
	}
	keyFor := func(userID int64, symbol, market string) string {
		return fmt.Sprintf("%d\x00%s\x00%s", userID, market, symbol)
	}
	candidates := map[string]paperCandidate{}
	holdingByKey := map[string]model.PaperHolding{}
	tradesByKey := map[string][]model.PaperTrade{}
	for _, h := range holdings {
		key := keyFor(h.UserID, h.Symbol, h.Market)
		candidates[key] = paperCandidate{UserID: h.UserID, Symbol: h.Symbol, Market: h.Market, Name: h.Name}
		holdingByKey[key] = h
	}
	for _, trade := range trades {
		key := keyFor(trade.UserID, trade.Symbol, trade.Market)
		candidate := candidates[key]
		candidate.UserID, candidate.Symbol, candidate.Market = trade.UserID, trade.Symbol, trade.Market
		if candidate.Name == "" {
			candidate.Name = trade.Name
		}
		candidates[key] = candidate
		tradesByKey[key] = append(tradesByKey[key], trade)
	}

	done := 0
	for _, action := range actions {
		for key, candidate := range candidates {
			if candidate.Symbol != action.Symbol || candidate.Market != action.Market {
				continue
			}
			if h, ok := holdingByKey[key]; ok {
				if applyPaperCorpAdjust(h, action) {
					done++
				}
				continue
			}
			entitledQty := paperEntitledQty(tradesByKey[key], action)
			if entitledQty <= positionQtyEps {
				continue
			}
			if hasShareAdjustment(action) {
				exists, err := paperCorpAdjustExists(common.DB, candidate.UserID, action.ID)
				if err != nil {
					common.SysWarn("模拟盘读取除权审计失败 user=%d symbol=%s action=%d: %v",
						candidate.UserID, candidate.Symbol, action.ID, err)
					continue
				}
				if exists {
					continue // 已成功折算后才清仓，不是历史错序。
				}
				common.SysWarn("模拟盘送转补跑需人工核对 user=%d symbol=%s action=%d：除权后已清仓，无法倒序重放流水",
					candidate.UserID, candidate.Symbol, action.ID)
				continue
			}
			if applyPaperCashDividend(candidate.UserID, candidate.Symbol, candidate.Market,
				orSymbol(candidate.Name, action.Name), action) {
				done++
			}
		}
	}
	return done
}

// paperCorpAdjustWindowDays 模拟盘自动折算的回看窗口（自然日）。
const paperCorpAdjustWindowDays = 7

func paperEntitledQty(trades []model.PaperTrade, action model.CorporateAction) float64 {
	qty := 0.0
	for _, trade := range trades {
		if !entitlementIncludes(effectiveTradeDate(trade.TradeDate, trade.CreatedAt), action) {
			continue
		}
		switch trade.Side {
		case model.PaperSideBuy, model.PaperSideAdjust:
			qty += trade.Quantity
		case model.PaperSideSell:
			qty -= trade.Quantity
		}
	}
	if qty <= positionQtyEps {
		return 0
	}
	return round4(qty)
}

func paperEntitledQtyForHolding(h model.PaperHolding, trades []model.PaperTrade, action model.CorporateAction) float64 {
	if len(trades) == 0 && entitlementIncludes(effectiveTradeDate("", h.CreatedAt), action) {
		return round4(h.Quantity)
	}
	return paperEntitledQty(trades, action)
}

func paperSellOnOrAfterAction(trades []model.PaperTrade, action model.CorporateAction) bool {
	for _, trade := range trades {
		if trade.Side != model.PaperSideSell {
			continue
		}
		date := effectiveTradeDate(trade.TradeDate, trade.CreatedAt)
		if action.ExDate != "" && date >= action.ExDate {
			return true
		}
	}
	return false
}

func paperCorpAdjustExists(db *gorm.DB, userID, actionID int64) (bool, error) {
	var count int64
	err := db.Model(&model.PaperCorpAdjust{}).
		Where("user_id = ? AND corporate_action_id = ?", userID, actionID).Count(&count).Error
	return count > 0, err
}

// applyPaperCorpAdjust 对单个模拟持仓执行一次折算。整个操作在一个事务里：
// 审计行先插（唯一键冲突=已调过，直接返回不重复），再改持仓与账户现金。
// 返回是否确有执行。
func applyPaperCorpAdjust(h model.PaperHolding, a model.CorporateAction) bool {
	var executed bool
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		// 全部模拟盘写事务统一 account -> holding 的加锁顺序，避免与交易/Reset 互锁。
		var acc model.PaperAccount
		if err := lockedAccount(tx, h.UserID, &acc); err != nil {
			return err
		}
		if exists, err := paperCorpAdjustExists(tx, h.UserID, a.ID); err != nil {
			return err
		} else if exists {
			return nil
		}
		// 行锁重读持仓（并发交易可能刚改过数量）。
		var cur model.PaperHolding
		q := tx.Where("id = ? AND user_id = ?", h.ID, h.UserID)
		if !common.UsingSQLite {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&cur).Error; err != nil {
			return err
		}
		var trades []model.PaperTrade
		if err := tx.Where("user_id = ? AND symbol = ? AND market = ?", h.UserID, h.Symbol, h.Market).
			Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
			return err
		}
		// 极老模拟持仓可能没有流水；只在创建日在登记日口径内时按当前数量兜底。
		entitledQty := paperEntitledQtyForHolding(cur, trades, a)
		entitledQty = round4(entitledQty)
		if entitledQty <= positionQtyEps {
			return nil // 除权日或之后才买入：没有本次权益，不落空审计行。
		}
		if hasShareAdjustment(a) && paperSellOnOrAfterAction(trades, a) {
			return errors.New("除权日或之后已有卖出流水，无法在不重放账本的情况下补记送转")
		}
		calc, ok := computeCorpAdjust(cur.Quantity, cur.AvgCost, entitledQty,
			a.BonusRatio, a.TransferRatio, a.DividendPretax)
		if !ok {
			return errors.New("模拟持仓无法折算")
		}
		// 幂等闸门：唯一键冲突即本 action 已调过（DoNothing 不覆盖历史审计）。
		audit := model.PaperCorpAdjust{
			UserID: h.UserID, CorporateActionID: a.ID,
			Symbol: h.Symbol, Market: h.Market, Name: orSymbol(h.Name, a.Name),
			ExDate: a.ExDate, RecordDate: a.RecordDate, EntitledQty: entitledQty,
			PlanProfile: a.PlanProfile,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&audit)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil // 已调过
		}
		audit.QtyBefore, audit.CostBefore = cur.Quantity, cur.AvgCost
		audit.QtyAfter, audit.CostAfter = calc.QtyAfter, calc.CostAfter
		audit.CashDividend = calc.CashDividend
		if err := tx.Save(&audit).Error; err != nil {
			return err
		}
		cur.Quantity, cur.AvgCost = calc.QtyAfter, calc.CostAfter
		if err := tx.Save(&cur).Error; err != nil {
			return err
		}
		// 现金分红入账户余额 + 一笔可见的交易流水（模拟盘的账要能自圆其说）。
		if calc.CashDividend > 0 {
			acc.Cash = round2(acc.Cash + calc.CashDividend)
			if err := tx.Save(&acc).Error; err != nil {
				return err
			}
		}
		trade := model.PaperTrade{
			UserID: h.UserID, Symbol: h.Symbol, Market: h.Market, Name: audit.Name,
			Side: model.PaperSideAdjust, Price: 0, Quantity: round4(calc.QtyAfter - audit.QtyBefore),
			Amount: 0, Fee: 0, Tax: 0, RealizedPnl: round2(calc.CashDividend), TradeDate: a.ExDate,
		}
		if err := tx.Create(&trade).Error; err != nil {
			return err
		}
		executed = true
		return nil
	})
	if err != nil {
		common.SysWarn("模拟盘除权调整失败 user=%d symbol=%s action=%d: %v",
			h.UserID, h.Symbol, a.ID, err)
		return false
	}
	return executed
}

// applyPaperCashDividend 为已清仓的模拟持仓补发纯现金分红。现金与送转可分离处理，
// 这里不创建/复活 holding，只落零数量 adjust 审计并增加账户现金。
func applyPaperCashDividend(userID int64, symbol, market, name string, a model.CorporateAction) bool {
	if a.DividendPretax <= 0 || hasShareAdjustment(a) {
		return false
	}
	var executed bool
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		// 与交易、Reset、持仓折算保持相同的账户优先锁序。
		var acc model.PaperAccount
		if err := lockedAccount(tx, userID, &acc); err != nil {
			return err
		}
		if exists, err := paperCorpAdjustExists(tx, userID, a.ID); err != nil {
			return err
		} else if exists {
			return nil
		}
		var trades []model.PaperTrade
		if err := tx.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).
			Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
			return err
		}
		entitledQty := paperEntitledQty(trades, a)
		if entitledQty <= positionQtyEps {
			return nil
		}
		cash := round4(a.DividendPretax * entitledQty / 10)
		audit := model.PaperCorpAdjust{
			UserID: userID, CorporateActionID: a.ID,
			Symbol: symbol, Market: market, Name: name,
			ExDate: a.ExDate, RecordDate: a.RecordDate,
			EntitledQty: entitledQty, CashDividend: cash, PlanProfile: a.PlanProfile,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&audit)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		acc.Cash = round2(acc.Cash + cash)
		if err := tx.Save(&acc).Error; err != nil {
			return err
		}
		trade := model.PaperTrade{
			UserID: userID, Symbol: symbol, Market: market, Name: name,
			Side: model.PaperSideAdjust, RealizedPnl: round2(cash), TradeDate: a.ExDate,
		}
		if err := tx.Create(&trade).Error; err != nil {
			return err
		}
		executed = true
		return nil
	})
	if err != nil {
		common.SysWarn("模拟盘现金分红补发失败 user=%d symbol=%s action=%d: %v", userID, symbol, a.ID, err)
		return false
	}
	return executed
}

func paperDueActions(db *gorm.DB, symbol, market, tradeDate string) ([]model.CorporateAction, error) {
	date, err := time.ParseInLocation("2006-01-02", tradeDate, time.Local)
	if err != nil {
		return nil, err
	}
	since := date.AddDate(0, 0, -paperCorpAdjustWindowDays).Format("2006-01-02")
	var actions []model.CorporateAction
	err = db.Where("symbol = ? AND market = ? AND ex_date BETWEEN ? AND ?",
		symbol, market, since, tradeDate).Order("ex_date ASC, id ASC").Find(&actions).Error
	return actions, err
}

// paperHistoricalUnprocessedShareAction 查找已滑出自动结算窗口、但仍未处理的送转。
// 这些记录不能自动倒序套当前聚合态，也不能靠等待第 8 天绕过交易闸门。
func paperHistoricalUnprocessedShareAction(db *gorm.DB, userID int64, symbol, market, tradeDate string,
	trades []model.PaperTrade, holding model.PaperHolding, hasHolding bool) (*model.CorporateAction, error) {
	date, err := time.ParseInLocation("2006-01-02", tradeDate, time.Local)
	if err != nil {
		return nil, err
	}
	before := date.AddDate(0, 0, -paperCorpAdjustWindowDays).Format("2006-01-02")
	var actions []model.CorporateAction
	if err := db.Where(
		"symbol = ? AND market = ? AND ex_date <> '' AND ex_date < ? AND (bonus_ratio > 0 OR transfer_ratio > 0)",
		symbol, market, before,
	).Order("ex_date ASC, id ASC").Find(&actions).Error; err != nil {
		return nil, err
	}
	for i := range actions {
		action := actions[i]
		entitledQty := paperEntitledQty(trades, action)
		if hasHolding {
			entitledQty = paperEntitledQtyForHolding(holding, trades, action)
		}
		if entitledQty <= positionQtyEps {
			continue
		}
		exists, err := paperCorpAdjustExists(db, userID, action.ID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return &action, nil
		}
	}
	return nil, nil
}

// ensurePaperCorpAdjustBeforeTrade 在模拟盘下单前主动结算到期公司行动。外层交易随后
// 还会在账户行锁内复验审计行，避免并发请求从预处理与正式下单之间穿过。
func ensurePaperCorpAdjustBeforeTrade(userID int64, symbol, market, tradeDate string) error {
	if common.DB == nil {
		return errors.New("数据库不可用")
	}
	if market != "cn" {
		return nil
	}
	actions, err := paperDueActions(common.DB, symbol, market, tradeDate)
	if err != nil {
		return err
	}
	var holding model.PaperHolding
	hErr := common.DB.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).
		First(&holding).Error
	hasHolding := hErr == nil
	if hErr != nil && !errors.Is(hErr, gorm.ErrRecordNotFound) {
		return hErr
	}
	var trades []model.PaperTrade
	if err := common.DB.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).
		Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
		return err
	}
	for _, action := range actions {
		entitledQty := paperEntitledQty(trades, action)
		if hasHolding {
			entitledQty = paperEntitledQtyForHolding(holding, trades, action)
		}
		if !action.HasAdjustment() || entitledQty <= positionQtyEps {
			continue
		}
		if exists, err := paperCorpAdjustExists(common.DB, userID, action.ID); err != nil {
			return err
		} else if exists {
			continue
		}
		executed := false
		if hasHolding {
			executed = applyPaperCorpAdjust(holding, action)
		} else if !hasShareAdjustment(action) {
			executed = applyPaperCashDividend(userID, symbol, market,
				orSymbol(holding.Name, action.Name), action)
		}
		if executed {
			continue
		}
		if exists, err := paperCorpAdjustExists(common.DB, userID, action.ID); err != nil {
			return err
		} else if exists {
			continue
		}
		return fmt.Errorf("%s 存在 %s 到期公司行动尚未安全入账；若已发生除权后卖出，请重置或人工核对模拟账本",
			symbol, action.ExDate)
	}
	if action, err := paperHistoricalUnprocessedShareAction(common.DB, userID, symbol, market, tradeDate,
		trades, holding, hasHolding); err != nil {
		return err
	} else if action != nil {
		return fmt.Errorf("%s 存在 %s 历史送转尚未安全入账，已超出自动补跑窗口；请重置或人工核对模拟账本",
			symbol, action.ExDate)
	}
	return nil
}

func verifyPaperCorpAdjustBeforeTradeTx(tx *gorm.DB, userID int64, symbol, market, tradeDate string) error {
	if market != "cn" {
		return nil
	}
	actions, err := paperDueActions(tx, symbol, market, tradeDate)
	if err != nil {
		return err
	}
	var trades []model.PaperTrade
	if err := tx.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).
		Order("trade_date ASC, id ASC").Find(&trades).Error; err != nil {
		return err
	}
	var holding model.PaperHolding
	hErr := tx.Where("user_id = ? AND symbol = ? AND market = ?", userID, symbol, market).First(&holding).Error
	hasHolding := hErr == nil
	if hErr != nil && !errors.Is(hErr, gorm.ErrRecordNotFound) {
		return hErr
	}
	for _, action := range actions {
		entitledQty := paperEntitledQty(trades, action)
		if hasHolding {
			entitledQty = paperEntitledQtyForHolding(holding, trades, action)
		}
		if !action.HasAdjustment() || entitledQty <= positionQtyEps {
			continue
		}
		exists, err := paperCorpAdjustExists(tx, userID, action.ID)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("%s 的 %s 公司行动尚未入账，请重试", symbol, action.ExDate)
		}
	}
	if action, err := paperHistoricalUnprocessedShareAction(tx, userID, symbol, market, tradeDate,
		trades, holding, hasHolding); err != nil {
		return err
	} else if action != nil {
		return fmt.Errorf("%s 的 %s 历史送转尚未入账，请重置或人工核对模拟账本", symbol, action.ExDate)
	}
	return nil
}
