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

// B7 资产曲线：每交易日盘后为每个用户落一条真实持仓/模拟盘的资产快照。
//
// **fail-closed 铁律**：市值一律走 FreshQuotesFor；stale/取不到的标的**不用旧价冒充**，
// 该用户当日快照标 Partial 并记缺口数与说明（沿用 Overview 的 ValuationNote 纪律）。
// 曲线是用来回答「这几个月赚没赚」的，用一根旧价撑起来的净值点比没有这个点更糟。

const (
	// portfolioSnapshotHour/Min 交易日盘后落快照的时点：16:20。
	// 错峰依据：16:10 全市场日线、16:35 涨停池+人气榜、18:45 龙虎榜。
	portfolioSnapshotHour = 16
	portfolioSnapshotMin  = 20

	// curveMaxDays 曲线查询的最大回看自然日（防一次拉穿全表）。
	curveMaxDays = 730
	// curveDefaultDays 默认回看窗口。
	curveDefaultDays = 90
)

// PortfolioCurvePoint 曲线单点。
type PortfolioCurvePoint struct {
	TradeDate     string  `json:"trade_date"`
	MarketValue   float64 `json:"market_value"`
	Cost          float64 `json:"cost"`
	UnrealizedPnl float64 `json:"unrealized_pnl"`
	RealizedCum   float64 `json:"realized_cum"`
	Cash          float64 `json:"cash"`
	TotalAssets   float64 `json:"total_assets"` // paper=现金+市值；real=市值（无现金概念）
	PositionCount int     `json:"position_count"`
	Partial       bool    `json:"partial"`
	MissingCount  int     `json:"missing_count"`
	Note          string  `json:"note,omitempty"`
}

// PortfolioCurveView 曲线响应。
type PortfolioCurveView struct {
	Kind         string                `json:"kind"`
	Days         int                   `json:"days"`
	Points       []PortfolioCurvePoint `json:"points"` // 日期升序
	PartialCount int                   `json:"partial_count"`
	Notes        []string              `json:"notes"`
}

// PortfolioCurve 读资产曲线快照（严格按 user_id 隔离、日期升序、days 有界）。
// 快照由每交易日 16:20 的 job 落库；**查询侧不即时计算**——曲线要的是历史各日的
// 当时价，读时算只能拿到今天的价格，那是另一回事。
func PortfolioCurve(userID int64, kind string, days int) (*PortfolioCurveView, error) {
	account, err := ResolvePortfolioAccount(userID, 0, kind)
	if err != nil {
		return nil, err
	}
	return PortfolioCurveByAccount(userID, account.ID, kind, days)
}

func PortfolioCurveByAccount(userID, accountID int64, kind string, days int) (*PortfolioCurveView, error) {
	return PortfolioCurveByAccountContext(context.Background(), userID, accountID, kind, days)
}

func PortfolioCurveByAccountContext(ctx context.Context, userID, accountID int64, kind string, days int) (*PortfolioCurveView, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if kind != model.SnapshotKindReal && kind != model.SnapshotKindPaper {
		return nil, errors.New("账户类型须为 real 或 paper")
	}
	if days <= 0 {
		days = curveDefaultDays
	}
	if days > curveMaxDays {
		days = curveMaxDays
	}
	now := time.Now()
	from, through := now.AddDate(0, 0, -days).Format("2006-01-02"), now.Format("2006-01-02")
	var rows []model.PortfolioSnapshot
	var currencyGap portfolioCurrencyGap
	err := readSnapshotTx(ctx, func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ? AND account_id = ? AND kind = ? AND trade_date >= ? AND trade_date <= ?", userID, accountID, kind, from, through).
			Order("trade_date ASC").Find(&rows).Error; err != nil {
			return err
		}
		var err error
		currencyGap, err = portfolioCurrencyGapFor(tx, userID, accountID, kind)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := &PortfolioCurveView{Kind: kind, Days: days, Points: []PortfolioCurvePoint{}, Notes: []string{}}
	for _, r := range rows {
		if currencyGap.affects(r.TradeDate) {
			r.Partial = true
			if r.Note != "" {
				r.Note += "；"
			}
			r.Note += currencyGap.Reason
		}
		total := r.MarketValue
		if kind == model.SnapshotKindPaper {
			total = round2(r.Cash + r.MarketValue)
		}
		if r.Partial {
			out.PartialCount++
		}
		out.Points = append(out.Points, PortfolioCurvePoint{
			TradeDate: r.TradeDate, MarketValue: r.MarketValue, Cost: r.Cost,
			UnrealizedPnl: r.UnrealizedPnl, RealizedCum: r.RealizedCum, Cash: r.Cash,
			TotalAssets: total, PositionCount: r.PositionCount,
			Partial: r.Partial, MissingCount: r.MissingCount, Note: r.Note,
		})
	}
	out.Notes = append(out.Notes,
		"快照由每交易日 16:20 盘后任务落库；非交易日与服务未运行的日期如实缺席，不做插值补造")
	if out.PartialCount > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf(
			"%d 个交易日的行情、账本或币种口径不完整（标记 partial），这些点不可当作完整净值",
			out.PartialCount))
	}
	if len(out.Points) == 0 {
		out.Notes = append(out.Notes, "暂无快照——资产曲线自启用之日起按交易日积累，不回溯历史")
	}
	return out, nil
}

// realSnapshotFrom 由「该用户全部持仓 + 已取到的行情」算真实持仓快照（纯函数，
// 手工验算与 partial 反例的单测锚点）。定价 fail-closed：非 fresh 一律不计入。
func realSnapshotFrom(userID int64, tradeDate string, positions []model.Position, quotes map[string]FreshQuoteResult) *model.PortfolioSnapshot {
	return realSnapshotWithShareGaps(userID, tradeDate, positions, quotes, nil)
}

func realSnapshotWithShareGaps(userID int64, tradeDate string, positions []model.Position, quotes map[string]FreshQuoteResult, pendingShares map[int64]bool) *model.PortfolioSnapshot {
	accountID := int64(0)
	if len(positions) > 0 {
		accountID = positions[0].AccountID
	}
	snap := &model.PortfolioSnapshot{UserID: userID, AccountID: accountID, Kind: model.SnapshotKindReal, TradeDate: tradeDate}
	var realizedCum float64
	currencyGapCount, currencyHoldingCount := 0, 0
	for _, p := range positions {
		if positionCurrencyIssue(p, "CNY") != "" {
			currencyGapCount++
			continue
		}
		realizedCum += p.RealizedPnl
	}
	snap.RealizedCum = round2(realizedCum)
	shareGapCount := 0
	for _, p := range positions {
		if p.Status != model.PositionStatusHolding {
			continue
		}
		snap.PositionCount++
		if positionCurrencyIssue(p, "CNY") != "" {
			snap.MissingCount++
			currencyHoldingCount++
			continue
		}
		if pendingShares[p.ID] {
			snap.MissingCount++
			shareGapCount++
			continue
		}
		fq, ok := quotes[QuoteKey(p.Market, p.Symbol)]
		if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
			// fail-closed：非 fresh 一律不计入市值**与成本**（只剔市值会让浮亏 = −成本）。
			snap.MissingCount++
			continue
		}
		snap.MarketValue = round2(snap.MarketValue + fq.Quote.Price*p.Quantity)
		snap.Cost = round2(snap.Cost + positionCurrentCost(p))
	}
	snap.UnrealizedPnl = round2(snap.MarketValue - snap.Cost)
	if snap.MissingCount > 0 {
		snap.Partial = true
		if quoteGaps := snap.MissingCount - shareGapCount - currencyHoldingCount; quoteGaps > 0 {
			snap.Note = fmt.Sprintf("%d 笔持仓当日无当前有效行情，未计入市值与成本（不用旧价冒充）", quoteGaps)
		}
		if shareGapCount > 0 {
			if snap.Note != "" {
				snap.Note += "；"
			}
			snap.Note += fmt.Sprintf("%d 笔持仓到期送转尚未处理，未将除权前股数用于当前估值", shareGapCount)
		}
	}
	if currencyGapCount > 0 {
		snap.Partial = true
		if snap.Note != "" {
			snap.Note += "；"
		}
		snap.Note += fmt.Sprintf("%d 笔记录的币种无法与 CNY 合计，未计入市值、成本和已实现盈亏", currencyGapCount)
	}
	return snap
}

// buildRealSnapshot 计算某用户当日真实持仓快照。定价 fail-closed。
func (s *PositionService) buildRealSnapshot(ctx context.Context, userID int64, tradeDate string) (*model.PortfolioSnapshot, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindReal)
	if err != nil {
		return nil, err
	}
	return s.buildRealSnapshotForAccount(ctx, userID, account.ID, tradeDate)
}
func (s *PositionService) buildRealSnapshotForAccount(ctx context.Context, userID, accountID int64, tradeDate string) (*model.PortfolioSnapshot, error) {
	return s.captureRealSnapshot(ctx, userID, accountID, tradeDate, false)
}

// 行情请求在事务外；请求结束后锁账户，重读账本并在同一事务内落快照。
func (s *PositionService) captureRealSnapshot(ctx context.Context, userID, accountID int64, tradeDate string, persist bool) (*model.PortfolioSnapshot, error) {
	var positions []model.Position
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND account_id = ?", userID, accountID).Find(&positions).Error; err != nil {
		return nil, err
	}
	// 先补齐账本再算：从未打开过持仓页的老用户，RealizedPnl 全为 0，
	// 曲线的「累计已实现」会一路是 0——不是他没赚过，是账本还没补建。
	wrote, err := ensurePositionLedgersStrict(userID, positions)
	if err != nil {
		return nil, err
	}
	if wrote {
		if err := common.DB.WithContext(ctx).Where("user_id = ? AND account_id = ?", userID, accountID).Find(&positions).Error; err != nil {
			return nil, err
		}
	}
	refs := make([]QuoteRef, 0, len(positions))
	seen := map[string]bool{}
	for _, p := range positions {
		if p.Status != model.PositionStatusHolding {
			continue
		}
		k := QuoteKey(p.Market, p.Symbol)
		if !seen[k] {
			seen[k] = true
			refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
		}
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)
	var snap *model.PortfolioSnapshot
	err = common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActivePortfolioAccount(tx, userID, accountID, model.PortfolioKindReal); err != nil {
			return err
		}
		var current []model.Position
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Find(&current).Error; err != nil {
			return err
		}
		pendingShares := map[int64]bool{}
		for _, p := range current {
			if p.TotalBuyCost <= 0 {
				return fmt.Errorf("持仓 %d 账本未就绪：累计买入成本为空", p.ID)
			}
			if p.Status != model.PositionStatusHolding {
				continue
			}
			if p.Quantity > positionQtyEps && p.RemainingCost <= 0 {
				return fmt.Errorf("持仓 %d 账本未就绪：剩余成本为空", p.ID)
			}
			if err := ensurePositionShareActionsProcessedTx(tx, p, tradeDate); err != nil {
				if !errors.Is(err, errPositionShareActionPending) {
					return err
				}
				pendingShares[p.ID] = true
			}
		}
		snap = realSnapshotWithShareGaps(userID, tradeDate, current, quotes, pendingShares)
		snap.AccountID = accountID
		if persist {
			return upsertPortfolioSnapshotTx(tx, snap)
		}
		return nil
	})
	return snap, err
}

// paperSnapshotFrom 由「模拟账户 + 持仓 + 行情 + 累计已实现」算模拟盘快照（纯函数）。
// 与 PaperService.Overview 的差异是**这里不用成本兜底估值**——曲线点必须能区分
// 「真实市值」与「按成本顶上的估值」，否则一段停牌期会画出一条平直的假净值。
func paperSnapshotFrom(userID int64, tradeDate string, acc model.PaperAccount, holdings []model.PaperHolding,
	quotes map[string]FreshQuoteResult, realizedCum float64) *model.PortfolioSnapshot {
	return paperSnapshotWithValuationGaps(userID, tradeDate, acc, holdings, quotes, realizedCum, nil)
}

func paperSnapshotWithValuationGaps(userID int64, tradeDate string, acc model.PaperAccount, holdings []model.PaperHolding,
	quotes map[string]FreshQuoteResult, realizedCum float64, gaps map[int64]string) *model.PortfolioSnapshot {
	snap := &model.PortfolioSnapshot{
		UserID: userID, AccountID: acc.AccountID, Kind: model.SnapshotKindPaper, TradeDate: tradeDate,
		Cash: round2(acc.Cash), PositionCount: len(holdings), RealizedCum: round2(realizedCum),
	}
	for _, h := range holdings {
		if reason := gaps[h.ID]; reason != "" {
			snap.MissingCount++
			snap.Note = reason
			continue
		}
		if positionCurrencyIssue(model.Position{Market: h.Market}, "CNY") != "" {
			snap.MissingCount++
			snap.Partial = true
			snap.Note = "模拟持仓含非 CNY 标的，缺少汇率事实，市值与现金口径待核验"
			continue
		}
		fq, ok := quotes[QuoteKey(h.Market, h.Symbol)]
		if !ok || fq.Quote == nil || fq.Quote.Price <= 0 || fq.Fresh.Status != freshStatusFresh {
			snap.MissingCount++
			continue
		}
		snap.MarketValue = round2(snap.MarketValue + fq.Quote.Price*h.Quantity)
		snap.Cost = round2(snap.Cost + paperHoldingCost(h))
	}
	snap.UnrealizedPnl = round2(snap.MarketValue - snap.Cost)
	if snap.MissingCount > 0 {
		snap.Partial = true
		if snap.Note == "" {
			snap.Note = fmt.Sprintf("%d 笔模拟持仓当日无当前有效行情，未计入市值与成本（不用旧价冒充）", snap.MissingCount)
		}
	}
	return snap
}

// buildPaperSnapshot 计算某用户当日模拟盘快照。
func (s *PaperService) buildPaperSnapshot(ctx context.Context, userID int64, tradeDate string) (*model.PortfolioSnapshot, error) {
	account, err := ResolvePortfolioAccount(userID, 0, model.PortfolioKindPaper)
	if err != nil {
		return nil, err
	}
	return s.buildPaperSnapshotForAccount(ctx, userID, account.ID, tradeDate)
}
func (s *PaperService) buildPaperSnapshotForAccount(ctx context.Context, userID, accountID int64, tradeDate string) (*model.PortfolioSnapshot, error) {
	return s.capturePaperSnapshot(ctx, userID, accountID, tradeDate, false)
}

func (s *PaperService) capturePaperSnapshot(ctx context.Context, userID, accountID int64, tradeDate string, persist bool) (*model.PortfolioSnapshot, error) {
	// 公司行动 job 在 19:25，而资产快照在 16:20。快照必须主动结算当日及近期
	// 已到期行动，否则会把除权后的价格配上除权前数量/成本，永久冻结一个假点。
	var symbols []struct {
		Symbol string
		Market string
	}
	if err := common.DB.WithContext(ctx).Raw(
		"SELECT symbol, market FROM paper_holdings WHERE user_id = ? "+
			"AND account_id = ? UNION SELECT symbol, market FROM paper_trades WHERE user_id = ? AND account_id = ?", userID, accountID, userID, accountID,
	).Scan(&symbols).Error; err != nil {
		return nil, err
	}
	for _, item := range symbols {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := ensurePaperCorpAdjustBeforeTradeForAccount(userID, accountID, item.Symbol, item.Market, tradeDate); err != nil {
			return nil, fmt.Errorf("模拟盘公司行动结算失败 %s/%s: %w", item.Market, item.Symbol, err)
		}
	}
	var holdings []model.PaperHolding
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND account_id = ?", userID, accountID).Find(&holdings).Error; err != nil {
		return nil, err
	}
	refs := make([]QuoteRef, 0, len(holdings))
	for _, h := range holdings {
		refs = append(refs, QuoteRef{Market: h.Market, Symbol: h.Symbol})
	}
	quotes := s.market.FreshQuotesFor(ctx, refs)
	var snap *model.PortfolioSnapshot
	err := common.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := lockActivePortfolioAccount(tx, userID, accountID, model.PortfolioKindPaper); err != nil {
			return err
		}
		var acc model.PaperAccount
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).First(&acc).Error; err != nil {
			return err
		}
		var current []model.PaperHolding
		if err := tx.Where("user_id = ? AND account_id = ?", userID, accountID).Find(&current).Error; err != nil {
			return err
		}
		costIncomplete := false
		for i := range current {
			note, err := restorePaperHoldingCost(tx, &current[i])
			if err != nil {
				return err
			}
			costIncomplete = costIncomplete || note != ""
		}
		realized, err := paperRealizedPnlByAccount(tx, userID, accountID)
		if err != nil {
			return err
		}
		valuationGaps, err := paperPortfolioValuationGaps(tx, current, tradeDate)
		if err != nil {
			return err
		}
		snap = paperSnapshotWithValuationGaps(userID, tradeDate, acc, current, quotes, realized, valuationGaps)
		accountingNote, err := paperRealizedAccountingNote(tx, userID, accountID)
		if err != nil {
			return err
		}
		if costIncomplete || accountingNote != "" {
			snap.Partial = true
			if snap.Note != "" {
				snap.Note += "；"
			}
			if accountingNote != "" {
				snap.Note += accountingNote
			} else {
				snap.Note += "存量模拟持仓成本缺少成交明细，盈亏待核验"
			}
		}
		currencyGap, err := portfolioCurrencyGapFor(tx, userID, accountID, model.PortfolioKindPaper)
		if err != nil {
			return err
		}
		if currencyGap.affects(tradeDate) {
			snap.Partial = true
			if snap.Note != "" {
				snap.Note += "；"
			}
			snap.Note += currencyGap.Reason
		}
		if persist {
			return upsertPortfolioSnapshotTx(tx, snap)
		}
		return nil
	})
	return snap, err
}

// upsertPortfolioSnapshot 按 (user_id, account_id, trade_date) 幂等 upsert。
// 同日重跑覆盖（盘后重启补跑取最后一次结果），绝不产生重复点。
func upsertPortfolioSnapshot(snap *model.PortfolioSnapshot) error {
	if snap.AccountID == 0 {
		account, err := ResolvePortfolioAccount(snap.UserID, 0, snap.Kind)
		if err != nil {
			return err
		}
		snap.AccountID = account.ID
	}
	return common.DB.Transaction(func(tx *gorm.DB) error {
		if err := lockActivePortfolioAccount(tx, snap.UserID, snap.AccountID, snap.Kind); err != nil {
			return err
		}
		return upsertPortfolioSnapshotTx(tx, snap)
	})
}

func upsertPortfolioSnapshotTx(tx *gorm.DB, snap *model.PortfolioSnapshot) error {
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "user_id"}, {Name: "account_id"}, {Name: "trade_date"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"market_value", "cost", "unrealized_pnl", "realized_cum", "cash",
			"position_count", "partial", "missing_count", "note", "updated_at",
		}),
	}).Create(snap).Error
}

// 补录或更正交易会改变历史股数与现金回放；已有市值快照没有逐标的明细，不能据
// 今天的价格重造历史点。与账本同事务标记受影响日期，后续盘后重跑可恢复当日点。
func invalidatePortfolioSnapshotsTx(tx *gorm.DB, userID, accountID int64, fromDate string) error {
	q := tx.Model(&model.PortfolioSnapshot{}).Where("user_id = ? AND account_id = ?", userID, accountID)
	if fromDate != "" {
		q = q.Where("trade_date >= ?", fromDate)
	}
	return q.Updates(map[string]any{
		"partial": true,
		"note":    "快照生成后补录或更正了交易，历史资产需重新核对，暂不参与完整收益统计",
	}).Error
}

// snapshotUserIDs 需要落快照的用户：有持仓（含已平仓，累计已实现盈亏要继续画）
// ∪ 有模拟账户。两类分别返回，避免给没有模拟盘的用户造空快照。
func snapshotUserIDs() (real, paper []int64, err error) {
	if common.DB == nil {
		return nil, nil, errors.New("数据库不可用")
	}
	if err := common.DB.Model(&model.Position{}).Distinct().Pluck("user_id", &real).Error; err != nil {
		return nil, nil, fmt.Errorf("读取真实持仓快照候选用户: %w", err)
	}
	if err := common.DB.Model(&model.PaperAccount{}).Distinct().Pluck("user_id", &paper).Error; err != nil {
		return nil, nil, fmt.Errorf("读取模拟盘快照候选用户: %w", err)
	}
	return real, paper, nil
}

func snapshotAccounts() (real, paper []model.PortfolioAccount, err error) {
	realUsers, paperUsers, err := snapshotUserIDs()
	if err != nil {
		return nil, nil, err
	}
	for _, uid := range realUsers {
		if _, e := ResolvePortfolioAccount(uid, 0, model.PortfolioKindReal); e != nil {
			return nil, nil, e
		}
	}
	for _, uid := range paperUsers {
		if _, e := ResolvePortfolioAccount(uid, 0, model.PortfolioKindPaper); e != nil {
			return nil, nil, e
		}
	}
	positionAccounts := common.DB.Model(&model.Position{}).Select("DISTINCT account_id").Where("account_id > 0")
	cashFlowAccounts := common.DB.Model(&model.PortfolioCashFlow{}).Select("DISTINCT account_id").Where("account_id > 0")
	if err = common.DB.Where("kind = ? AND status = ? AND (id IN (?) OR id IN (?))", model.PortfolioKindReal, model.PortfolioStatusActive,
		positionAccounts, cashFlowAccounts).Find(&real).Error; err != nil {
		return
	}
	err = common.DB.Where("kind = ? AND status = ? AND id IN (?)", model.PortfolioKindPaper, model.PortfolioStatusActive,
		common.DB.Model(&model.PaperAccount{}).Select("DISTINCT account_id").Where("account_id > 0")).Find(&paper).Error
	return
}

// RunPortfolioSnapshots 为全部相关用户落当日快照（幂等；供 job 与测试调用）。
// 返回成功落库的快照条数。单个用户失败只告警不中断——一个用户的行情故障不该让
// 其他人当天的曲线整体缺点。
func RunPortfolioSnapshots(ctx context.Context, posSvc *PositionService, paperSvc *PaperService, tradeDate string) int {
	if common.DB == nil || tradeDate == "" {
		return 0
	}
	realAccounts, paperAccounts, err := snapshotAccounts()
	if err != nil {
		common.SysWarn("资产快照候选用户读取失败: %v", err)
		return 0
	}
	n := 0
	for _, account := range realAccounts {
		uid := account.UserID
		if err := ctx.Err(); err != nil {
			common.SysWarn("资产快照中止: %v", err)
			return n
		}
		if posSvc == nil {
			break
		}
		_, err := posSvc.captureRealSnapshot(ctx, uid, account.ID, tradeDate, true)
		if err != nil {
			common.SysWarn("用户 %d 持仓资产快照失败: %v", uid, err)
			continue
		}
		n++
	}
	for _, account := range paperAccounts {
		uid := account.UserID
		if err := ctx.Err(); err != nil {
			common.SysWarn("资产快照中止: %v", err)
			return n
		}
		if paperSvc == nil {
			break
		}
		_, err := paperSvc.capturePaperSnapshot(ctx, uid, account.ID, tradeDate, true)
		if err != nil {
			common.SysWarn("用户 %d 模拟盘资产快照失败: %v", uid, err)
			continue
		}
		n++
	}
	if n > 0 {
		common.SysLog("资产快照完成 %s：%d 条", tradeDate, n)
	}
	return n
}

// StartPortfolioSnapshotJob 每交易日 16:20 落资产快照。
// 非交易日跳过（不给休市日造点，曲线上如实断开）。
func StartPortfolioSnapshotJob(posSvc *PositionService, paperSvc *PaperService) {
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(4 * time.Minute) // 启动错峰，让行情源先就绪
		for {
			now := time.Now()
			if isTradingDayToday(now) &&
				now.Hour()*60+now.Minute() >= portfolioSnapshotHour*60+portfolioSnapshotMin {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				RunPortfolioSnapshots(ctx, posSvc, paperSvc, now.Format("2006-01-02"))
				cancel()
			}
			time.Sleep(time.Until(nextDailyAt(time.Now(), portfolioSnapshotHour, portfolioSnapshotMin)))
		}
	}()
}
