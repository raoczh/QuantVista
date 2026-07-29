package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// D14 基于持仓成本的止盈/止损 + D15 持仓期最高点回撤（移动止盈）。
//
// 用户的原话：「现在的提醒都是什么历史推荐的止亏点止盈点，**也没管我是否买入了**，
// 现在提醒的是没什么用的。」——本文件是这句话的直接答案：
//
//   - 评估单元是**一笔持仓**（不是一个 symbol）：一条规则可绑「我的全部持仓」，
//     评估时按当前 holding 逐仓展开，同一标的的多笔仓位各自判定各自的成本；
//   - 成本恒取 `positions.buy_price`（B5 口径铁律：当前持仓加权平均成本），
//     **用户不需要手工填任何价格**——旧「持仓止损」待办要求手工填 plan_stop_loss，
//     这正是它形同虚设的原因；
//   - fail-closed：无 fresh 行情的持仓本轮跳过（旧价触发止损提醒是误报）；
//   - 去重键 (rule_id, position_id, trade_date)：一条规则一天里逐仓落事件，同一标的的
//     多笔不同成本仓位也不会互相吞掉（旧的 symbol 去重会把它们压成一笔）。

// positionAlertEval 单笔持仓的观测值（纯函数入参，单测锚点）。
// 金额单位元/股，比例单位 %。
type positionAlertEval struct {
	AvgCost  float64 // 当前加权平均成本（positions.buy_price）
	Price    float64 // 当前 fresh 现价
	DayHigh  float64 // 当日最高（0=缺失，回退现价）
	DayLow   float64 // 当日最低（0=缺失，回退现价）
	Peak     float64 // 持仓期最高价（已含盘中抬升；0=未初始化，peak_drawdown 不判定）
	PeakDate string  // 峰值所在交易日（文案用）
}

// dayHighOrPrice 当日触达上界：缺当日最高时回退现价；上游 high 小于现价时取现价
// （盘中快照偶有不一致，取两者较大者才是真实触达过的上界）。
func dayHighOrPrice(in positionAlertEval) float64 {
	if in.DayHigh > 0 {
		return math.Max(in.DayHigh, in.Price)
	}
	return in.Price
}

// dayLowOrPrice 当日触达下界（同上，取较小者）。
func dayLowOrPrice(in positionAlertEval) float64 {
	if in.DayLow > 0 {
		return math.Min(in.DayLow, in.Price)
	}
	return in.Price
}

// costPctOf 相对成本的涨跌幅 %（正=盈利）。成本非正返回 0（调用方须先判 AvgCost>0）。
func costPctOf(cost, price float64) float64 {
	if cost <= 0 {
		return 0
	}
	return round2((price - cost) / cost * 100)
}

// evaluatePositionAlert 纯函数判定一条持仓类规则对一笔持仓是否命中。
// 返回（命中、观测值、命中说明）。观测值语义按 kind 各不相同，均为**正数百分比**：
//   - cost_gain：当日最高相对成本的涨幅
//   - cost_drawdown：当日最低相对成本的跌幅
//   - peak_drawdown：当日最低自峰值的回撤
//
// 数据不足（成本/现价非正、peak_drawdown 无峰值）一律不命中——**不可判定不是「未触发」**，
// 调用方对这种情况不写 last_value（见 evaluatePositionRules）。
func evaluatePositionAlert(rule model.AlertRule, name string, in positionAlertEval) (bool, float64, string) {
	if in.AvgCost <= 0 || in.Price <= 0 {
		return false, 0, ""
	}
	label := orSymbol(name, rule.Symbol)
	now := costPctOf(in.AvgCost, in.Price)
	// 当前浮盈亏后缀：每条消息都带上，用户看到提醒时不必再去翻持仓页。
	tail := fmt.Sprintf("；我的成本 %.2f，现价 %.2f，当前浮动%s%.2f%%",
		in.AvgCost, in.Price, pnlWord(now), math.Abs(now))

	switch rule.Kind {
	case model.AlertKindCostGain:
		hi := dayHighOrPrice(in)
		gain := costPctOf(in.AvgCost, hi)
		if gain >= rule.Threshold {
			return true, gain, fmt.Sprintf("%s 相对我的成本已涨 %.2f%%（≥%.4g%%，当日最高 %.2f）%s",
				label, gain, rule.Threshold, hi, tail)
		}
		return false, gain, ""

	case model.AlertKindCostDrawdown:
		lo := dayLowOrPrice(in)
		loss := -costPctOf(in.AvgCost, lo)
		if loss >= rule.Threshold {
			return true, loss, fmt.Sprintf("%s 相对我的成本已跌 %.2f%%（≥%.4g%%，当日最低 %.2f）%s",
				label, loss, rule.Threshold, lo, tail)
		}
		return false, loss, ""

	case model.AlertKindPeakDrawdown:
		if in.Peak <= 0 {
			return false, 0, "" // 峰值未建立：不可判定（不是「没回撤」）
		}
		lo := in.Price
		peak := in.Peak
		peakDate := in.PeakDate
		// 日线 High/Low 没有先后顺序。若本轮 DayHigh 首次抬高持仓峰值，不能再拿
		// 同一天更早可能出现的 DayLow 与它拼成“先高后低”的虚假回撤；此时只用
		// 当前现价判断从新峰值到现在确实已经发生的回撤。
		if in.DayHigh > peak {
			peak = in.DayHigh
			if in.PeakDate != "" {
				peakDate = in.PeakDate
			}
		} else {
			lo = dayLowOrPrice(in)
		}
		dd := peakDrawdownPct(peak, lo)
		if dd >= rule.Threshold {
			peakTxt := fmt.Sprintf("持仓期最高 %.2f", peak)
			if peakDate != "" {
				peakTxt += "（" + peakDate + "）"
			}
			return true, dd, fmt.Sprintf("%s 自%s回撤 %.2f%%（≥%.4g%%，当日最低 %.2f）%s",
				label, peakTxt, dd, rule.Threshold, lo, tail)
		}
		return false, dd, ""
	}
	return false, 0, ""
}

// pnlWord 浮动盈亏的中文方向词（消息文案用）。
func pnlWord(pct float64) string {
	if pct < 0 {
		return "亏损 "
	}
	return "盈利 "
}

// positionAlertHit 一条规则在一笔持仓上的命中结果（待落库）。
type positionAlertHit struct {
	Position model.Position
	Value    float64
	Message  string
}

// evaluatePositionRules 评估用户的全部持仓类规则。
//
// 编排要点：
//   - 持仓与行情**一次性批量取**（绝不「每条规则 × 每笔持仓」各拉一次行情）；
//   - 峰值惰性初始化在评估前完成，否则新用户第一轮 peak_drawdown 恒不可判定；
//   - 一条规则的多笔命中在**同一事务**里落库，规则行的 last_value 取本轮最极端观测值。
func (s *AlertService) evaluatePositionRules(ctx context.Context, userID int64, rules []model.AlertRule) (int, error) {
	if len(rules) == 0 || common.DB == nil {
		return 0, nil
	}
	loadPositions := func() ([]model.Position, error) {
		var rows []model.Position
		err := common.DB.WithContext(ctx).Where("user_id = ? AND status = ? AND market = ?",
			userID, model.PositionStatusHolding, "cn").Order("id ASC").Find(&rows).Error
		return rows, err
	}
	positions, err := loadPositions()
	if err != nil {
		return 0, err
	}
	if len(positions) == 0 {
		return 0, nil // 没有持仓：这类规则天然无事可做（不是错误）
	}
	// D15 峰值惰性初始化（老持仓/首次启用），补齐后重读拿到落库值。
	if backfillPositionPeaks(userID, positions) {
		if positions, err = loadPositions(); err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	// 批量新鲜行情（fail-closed：仅 fresh 参与判定）。
	refs := make([]QuoteRef, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		k := QuoteKey(p.Market, p.Symbol)
		if !seen[k] {
			seen[k] = true
			refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
		}
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)

	notifyOn := false
	if s.notify != nil {
		var nerr error
		notifyOn, nerr = alertNotificationsEnabled(ctx, userID)
		if nerr != nil {
			return 0, nerr
		}
	}
	var pushLines []string
	if notifyOn {
		defer func() {
			flushAlertNotification(ctx, s.notify, userID, "QuantVista 持仓提醒", NotifyMsgKindAlert, pushLines)
		}()
	}

	today := time.Now().In(time.Local).Format("2006-01-02")
	hits := 0
	for _, rule := range rules {
		if err := ctx.Err(); err != nil {
			return hits, err
		}
		var ruleHits []positionAlertHit
		extreme, hasObs := 0.0, false
		for _, p := range positions {
			// 绑定了 symbol 的规则只评该标的；未绑定 = 我的全部持仓。
			if rule.Symbol != "" && rule.Symbol != p.Symbol {
				continue
			}
			fq, ok := quotes[QuoteKey(p.Market, p.Symbol)]
			if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
				continue // fail-closed：无当前有效行情，本轮不评这一笔
			}
			in := positionAlertEval{
				AvgCost: p.BuyPrice, Price: fq.Quote.Price,
				DayHigh: fq.Quote.High, DayLow: fq.Quote.Low,
				Peak: p.PeakPrice, PeakDate: p.PeakDate,
			}
			// 盘中用当日最高抬升峰值参与判定（**不落库**：盘中峰值随时在变，
			// 落库交给盘后 16:25 那一次，避免高频写与并发竞争）。
			if in.Peak > 0 && in.DayHigh > in.Peak {
				in.PeakDate = today
			}
			triggered, value, msg := evaluatePositionAlert(rule, p.Name, in)
			// 观测值只在「可判定」时参与 last_value——peak 未建立时 value 恒 0，
			// 写进去会让用户以为「回撤 0%」，那是把不知道说成没有。
			if in.AvgCost > 0 && (rule.Kind != model.AlertKindPeakDrawdown || in.Peak > 0) {
				if !hasObs || value > extreme {
					extreme, hasObs = value, true
				}
			}
			if triggered {
				ruleHits = append(ruleHits, positionAlertHit{Position: p, Value: value, Message: msg})
			}
		}
		created, active, perr := persistPositionAlertEvaluation(ctx, rule, ruleHits, extreme, hasObs, today, time.Now())
		if perr != nil {
			return hits, perr
		}
		if active {
			hits += len(ruleHits)
		}
		if notifyOn {
			for _, h := range created {
				pushLines = append(pushLines, h.Message)
			}
		}
	}
	return hits, nil
}

// persistPositionAlertEvaluation 把一条持仓类规则本轮的全部命中落库（单事务）。
//
// 与 persistAlertEvaluation 的两点不同：
//  1. **一条规则可落多条事件**（逐持仓），去重键是 (rule_id, position_id, trade_date)；
//  2. **不置 status=triggered**——持仓类规则 Once 恒 false（validate 强制），
//     命中一笔就暂停整条规则会让其余持仓失联。
//
// 返回本轮**新建**的事件列表（供推送——已存在的当日事件不重复推）、规则是否仍生效。
func persistPositionAlertEvaluation(ctx context.Context, rule model.AlertRule, hits []positionAlertHit,
	extreme float64, hasObs bool, today string, now time.Time) ([]positionAlertHit, bool, error) {
	var created []positionAlertHit
	active := false
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current model.AlertRule
		q := tx.Where("id = ? AND user_id = ? AND status = ?", rule.ID, rule.UserID, model.AlertStatusActive)
		if tx.Dialector.Name() != "sqlite" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		err := q.First(&current).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return errAlertRuleChanged // 评估期间被用户暂停/删除：本轮结果作废
		}
		if err != nil {
			return err
		}
		active = true

		updates := map[string]any{"last_check_date": today}
		if hasObs {
			updates["last_value"] = round2(extreme)
		}
		if len(hits) > 0 {
			// trigger_msg 取本轮**最紧急**的那笔（观测值最大 = 涨得最多/跌得最多/回撤最深），
			// 规则行只有一个快照位，取首笔会让它随持仓 id 顺序而不是严重程度变化。
			worst := hits[0]
			for _, h := range hits[1:] {
				if h.Value > worst.Value {
					worst = h
				}
			}
			updates["triggered_at"] = now
			updates["trigger_msg"] = truncateRunes(worst.Message, 256)
		}
		for _, h := range hits {
			// 同一规则同一持仓当日只落一条。同一标的可能有多笔不同成本的仓位，
			// 按 symbol 去重会吞掉后面的决策提醒。
			var cnt int64
			if err := tx.Model(&model.AlertEvent{}).
				Where("rule_id = ? AND position_id = ? AND trade_date = ?", current.ID, h.Position.ID, today).
				Count(&cnt).Error; err != nil {
				return err
			}
			if cnt > 0 {
				continue
			}
			ev := model.AlertEvent{
				RuleID: current.ID, UserID: current.UserID,
				Symbol: h.Position.Symbol, Market: h.Position.Market,
				Name: orSymbol(h.Position.Name, h.Position.Symbol), Kind: current.Kind,
				Message: truncateRunes(h.Message, 256), TradeDate: today,
				PositionID: h.Position.ID, TriggeredAt: now, Status: model.AlertEventUnread,
			}
			if err := tx.Create(&ev).Error; err != nil {
				return err
			}
			created = append(created, h)
		}
		res := tx.Model(&model.AlertRule{}).
			Where("id = ? AND user_id = ? AND status = ?", current.ID, current.UserID, model.AlertStatusActive).
			Updates(updates)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			return errAlertRuleChanged
		}
		return nil
	})
	if errors.Is(err, errAlertRuleChanged) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return created, active, nil
}
