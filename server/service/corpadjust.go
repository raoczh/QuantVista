package service

import (
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
//	新数量 = 原数量 × (1 + (送股 + 转增)/10)
//	新成本 = (原成本 × 原数量 − 每10股派息 × 原数量/10) / 新数量
//
// 现金分红是真金到账，计入 RealizedPnl（与卖出兑现同属「已实现」）——不计入会让
// 分红股的累计收益长期少算。新成本因此下降，与「除权后股价下调」在账面上对齐。

// corpAdjustResult 一次折算的算术结果（纯函数产物，便于手工验算与单测）。
type corpAdjustResult struct {
	QtyAfter     float64 // 折算后数量（股）
	CostAfter    float64 // 折算后每股加权成本（元）
	CashDividend float64 // 本次到手税前现金分红（元）
}

// computeCorpAdjust 除权除息折算（纯函数）。qty/cost 为折算前的数量与每股加权成本；
// bonus/transfer 为每 10 股送股/转增（股）；dividend 为每 10 股派息税前（元）。
//
// 边界：数量或成本非正、方案三项全为零 → 返回 ok=false（无需调整，不生成建议）。
// 全部卖出后送转会把成本除以一个更大的数——数量为 0 时无从折算，直接拒绝。
func computeCorpAdjust(qty, cost, bonus, transfer, dividend float64) (corpAdjustResult, bool) {
	if qty <= positionQtyEps || cost <= 0 {
		return corpAdjustResult{}, false
	}
	if bonus <= 0 && transfer <= 0 && dividend <= 0 {
		return corpAdjustResult{}, false
	}
	factor := 1 + (bonus+transfer)/10
	qtyAfter := round4(qty * factor)
	cash := round4(dividend * qty / 10)
	costAfter := round4((cost*qty - cash) / qtyAfter)
	if costAfter < 0 {
		// 极端情形：单次派息超过总成本（长期持有的高分红股，成本已被历次分红摊到接近 0）。
		// 成本不允许为负——钉在 0 并如实保留现金分红，账面语义是「成本已全部收回」。
		costAfter = 0
	}
	return corpAdjustResult{QtyAfter: qtyAfter, CostAfter: costAfter, CashDividend: cash}, true
}

// ---------- 建议生成 ----------

// GenerateCorpAdjusts 为「命中除权除息日」的持仓生成待确认调整建议。
// asOf 为判定日（YYYY-MM-DD，通常是今天）。幂等：同一 (user, position, action) 只生成一条，
// 已确认/已撤销/已忽略的行不被重新拉回 pending。返回新生成的建议数。
//
// 只处理 status=holding 且 market=cn 的持仓（除权除息数据只有 A 股）。
func GenerateCorpAdjusts(userID int64, asOf string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	var positions []model.Position
	if err := common.DB.Where("user_id = ? AND status = ? AND market = ?",
		userID, model.PositionStatusHolding, "cn").Find(&positions).Error; err != nil {
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
	// 命中今日除权除息日的方案（一次批量查，绝不逐 symbol 循环查）。
	var actions []model.CorporateAction
	if err := common.DB.Where("symbol IN ? AND market = ? AND ex_date = ?", syms, "cn", asOf).
		Find(&actions).Error; err != nil {
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
	for _, p := range positions {
		for _, a := range actionsBySym[p.Symbol] {
			res, ok := computeCorpAdjust(p.Quantity, p.BuyPrice, a.BonusRatio, a.TransferRatio, a.DividendPretax)
			if !ok {
				continue
			}
			adj := model.PositionCorpAdjust{
				UserID: userID, PositionID: p.ID, CorporateActionID: a.ID,
				Symbol: p.Symbol, Market: p.Market, Name: orSymbol(p.Name, a.Name), ExDate: a.ExDate,
				BonusRatio: a.BonusRatio, TransferRatio: a.TransferRatio,
				DividendPretax: a.DividendPretax, PlanProfile: a.PlanProfile,
				QtyBefore: p.Quantity, QtyAfter: res.QtyAfter,
				CostBefore: p.BuyPrice, CostAfter: res.CostAfter,
				CashDividend: res.CashDividend,
				Status:       model.CorpAdjustPending,
			}
			// 幂等：唯一键冲突即跳过（**DoNothing 而非 DoUpdates**——已确认/撤销/忽略的
			// 行绝不能被重新覆盖回 pending，否则重复扫描会让用户被反复要求确认同一次除权）。
			ins := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(&adj)
			if ins.Error != nil {
				common.SysWarn("生成除权调整建议失败 user=%d pos=%d action=%d: %v",
					userID, p.ID, a.ID, ins.Error)
				continue
			}
			if ins.RowsAffected > 0 {
				created++
			}
		}
	}
	return created, nil
}

// ListCorpAdjusts 列出用户的调整建议（status 空=全部 pending）。仅本人。
func ListCorpAdjusts(userID int64, status string) ([]model.PositionCorpAdjust, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	q := common.DB.Where("user_id = ?", userID)
	switch status {
	case "", model.CorpAdjustPending:
		q = q.Where("status = ?", model.CorpAdjustPending)
	case "all":
	case model.CorpAdjustConfirmed, model.CorpAdjustReverted, model.CorpAdjustDismissed:
		q = q.Where("status = ?", status)
	default:
		return nil, errors.New("非法的状态筛选")
	}
	var rows []model.PositionCorpAdjust
	if err := q.Order("ex_date DESC, id DESC").Find(&rows).Error; err != nil {
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
//   - 过期建议：确认时用**行锁内的实时持仓**二次校验——持仓已平仓、或当前数量/成本
//     与建议生成时（QtyBefore/CostBefore）已不一致，说明期间用户加减过仓，
//     建议的折算基数失效，拒绝并要求重新生成，**绝不按过期基数改写账本**。
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

		var p model.Position
		if err := lockedPosition(tx, userID, adj.PositionID, &p); err != nil {
			return errors.New("持仓不存在")
		}
		if p.Status != model.PositionStatusHolding {
			return errors.New("该持仓已平仓，无法执行除权除息折算")
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
		res, ok := computeCorpAdjust(p.Quantity, p.BuyPrice, adj.BonusRatio, adj.TransferRatio, adj.DividendPretax)
		if !ok {
			return errors.New("当前持仓无法折算（数量或成本为零）")
		}

		trade := model.PositionTrade{
			UserID: userID, PositionID: p.ID, Side: model.PositionTradeAdjust,
			Price: 0, Quantity: round4(res.QtyAfter - p.Quantity), Fee: 0, Tax: 0,
			TradeDate: adj.ExDate,
			Note:      truncateRunes(corpAdjustNote(adj), 200),
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
		adj.QtyAfter, adj.CostAfter, adj.CashDividend = res.QtyAfter, res.CostAfter, res.CashDividend
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
		if p.Status != model.PositionStatusHolding {
			return errors.New("该持仓已平仓，无法撤销除权除息折算（平仓已按折算后账面结算）")
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
// 又不会在首次部署时把历史上所有除权一次性补进模拟账户（那会凭空造出大量分红现金）。
func RunPaperCorpAdjust() int {
	if common.DB == nil {
		return 0
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	since := now.AddDate(0, 0, -paperCorpAdjustWindowDays).Format("2006-01-02")

	var holdings []model.PaperHolding
	if err := common.DB.Where("market = ? AND quantity > 0", "cn").Find(&holdings).Error; err != nil {
		common.SysWarn("模拟盘除权调整读取持仓失败: %v", err)
		return 0
	}
	if len(holdings) == 0 {
		return 0
	}
	syms := make([]string, 0, len(holdings))
	seen := map[string]bool{}
	for _, h := range holdings {
		if !seen[h.Symbol] {
			seen[h.Symbol] = true
			syms = append(syms, h.Symbol)
		}
	}
	var actions []model.CorporateAction
	if err := common.DB.Where("symbol IN ? AND market = ? AND ex_date >= ? AND ex_date <= ?",
		syms, "cn", since, today).Find(&actions).Error; err != nil {
		common.SysWarn("模拟盘除权调整读取方案失败: %v", err)
		return 0
	}
	actionsBySym := map[string][]model.CorporateAction{}
	for i := range actions {
		if actions[i].HasAdjustment() {
			actionsBySym[actions[i].Symbol] = append(actionsBySym[actions[i].Symbol], actions[i])
		}
	}
	if len(actionsBySym) == 0 {
		return 0
	}

	done := 0
	for _, h := range holdings {
		for _, a := range actionsBySym[h.Symbol] {
			if applyPaperCorpAdjust(h, a) {
				done++
			}
		}
	}
	return done
}

// paperCorpAdjustWindowDays 模拟盘自动折算的回看窗口（自然日）。
const paperCorpAdjustWindowDays = 7

// applyPaperCorpAdjust 对单个模拟持仓执行一次折算。整个操作在一个事务里：
// 审计行先插（唯一键冲突=已调过，直接返回不重复），再改持仓与账户现金。
// 返回是否确有执行。
func applyPaperCorpAdjust(h model.PaperHolding, a model.CorporateAction) bool {
	var executed bool
	err := common.DB.Transaction(func(tx *gorm.DB) error {
		// 幂等闸门：唯一键冲突即本 action 已调过（DoNothing 不覆盖历史审计）。
		audit := model.PaperCorpAdjust{
			UserID: h.UserID, CorporateActionID: a.ID,
			Symbol: h.Symbol, Market: h.Market, Name: orSymbol(h.Name, a.Name), ExDate: a.ExDate,
			PlanProfile: a.PlanProfile,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&audit)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil // 已调过
		}
		// 行锁重读持仓（并发交易可能刚改过数量）。
		var cur model.PaperHolding
		q := tx.Where("id = ? AND user_id = ?", h.ID, h.UserID)
		if !common.UsingSQLite {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := q.First(&cur).Error; err != nil {
			return err // 持仓已清空：回滚审计行，下轮若仍持有再调
		}
		calc, ok := computeCorpAdjust(cur.Quantity, cur.AvgCost, a.BonusRatio, a.TransferRatio, a.DividendPretax)
		if !ok {
			return errors.New("模拟持仓无法折算")
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
			var acc model.PaperAccount
			if err := lockedAccount(tx, h.UserID, &acc); err != nil {
				return err
			}
			acc.Cash = round2(acc.Cash + calc.CashDividend)
			if err := tx.Save(&acc).Error; err != nil {
				return err
			}
		}
		trade := model.PaperTrade{
			UserID: h.UserID, Symbol: h.Symbol, Market: h.Market, Name: audit.Name,
			Side: model.PaperSideAdjust, Price: 0, Quantity: round4(calc.QtyAfter - audit.QtyBefore),
			Amount: 0, Fee: 0, Tax: 0, RealizedPnl: round2(calc.CashDividend),
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
