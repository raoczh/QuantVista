package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"quantvista/common"
	"quantvista/model"

	"gorm.io/gorm/clause"
)

// D16 利空事件触发卖出复核。
//
// 与 B9 盘后守护轮的分工（改动前先读）：
//   - **guard 负责「推没推过」**（GuardEvent 台账 + 推送通道），事件一过就没了；
//   - **SellReview 负责「我处理没处理」**（open/resolved/dismissed 状态机 + 今日待办）。
//
// 两者的去重维度也不同：guard 按 (user, symbol, kind, 事件日) —— 同一标的多笔持仓
// 只推一次；SellReview 按 (user, **position**, trigger, 事件日) —— 每一笔持仓各自
// 需要复核（成本不同，同一个利空对不同成本的仓位含义完全不同）。
//
// 五类触发中：解禁 / 除权除息 / 业绩预告已有 B9 的 guard 推送，本轮**只落待办不重复推**；
// 跌破关键均线与龙虎榜净卖出是新增的两类，走 guard 台账去重后推送
// （复用既有 NotifyService 通道与 EnableNotify 总闸，**不新建推送通道**）。
//
// 每条待办的 Detail 必须回答「这件事对**我这笔持仓**意味着什么」——带上我的成本、
// 我的浮盈亏、我的数量。这正是用户说的「现在的提醒也没管我是否买入了」。

const (
	// sellReviewHour/Min 卖出复核轮 19:40（错峰：19:25 公司行动同步、19:35 盘后守护轮）。
	// 必须晚于守护轮——两者消费同一批本地数据，让守护轮先落台账，
	// 本轮的 ma_break/lhb_sell 推送才不会与它交叉。
	sellReviewHour = 19
	sellReviewMin  = 40

	// sellReviewWindowDays 事件回看窗口（自然日）：服务缺勤后补生成，靠唯一键幂等。
	sellReviewWindowDays = 2
	// sellReviewLiftAheadDays 解禁提前量（与 guardLiftAheadDays 同口径，10 天）。
	sellReviewLiftAheadDays = guardLiftAheadDays
	// sellReviewExDivAheadDays 除权除息提前量（与 guardExDivAheadDays 同口径，3 天）。
	sellReviewExDivAheadDays = guardExDivAheadDays

	// sellReviewMaShort/Long 关键均线周期（跌破即复核）。
	sellReviewMaShort = 20
	sellReviewMaLong  = 60
	// sellReviewBarLimit 判均线需要的日线根数（60 日均线 + 1 根前值 + 余量）。
	sellReviewBarLimit = 70
	// sellReviewLhbNetSellYi 龙虎榜净卖出的最小规模（亿元）——小额净卖出是噪声，
	// 不值得打断用户。0.05 亿 = 500 万，对小盘股已是显著出货。
	sellReviewLhbNetSellYi = 0.05
	// sellReviewListMax 列表接口单次返回上限。
	sellReviewListMax = 200
)

// SellReviewService 卖出复核评估与调度。
type SellReviewService struct {
	market *MarketService
	notify *NotifyService
}

func NewSellReviewService(market *MarketService) *SellReviewService {
	return &SellReviewService{market: market, notify: NewNotifyService()}
}

// ---------- 纯函数判定（单测锚点） ----------

// sellReviewHit 一条命中的利空事件（尚未附上持仓账面）。
type sellReviewHit struct {
	Trigger   string
	TradeDate string // 事件自身日期（去重键）
	Severity  string
	Title     string
	Detail    string // 事件本身的描述；账面影响由 composeSellReviewDetail 追加
	// Push=true 表示这一类**没有**既有 guard 推送覆盖，需要本轮推（ma_break/lhb_sell）。
	Push bool
}

// evalSellReviewLift 解禁临近（提前 sellReviewLiftAheadDays 天）。
// 同日多批合并成一条（同 evalPosLift 口径：唯一键不含类型，分开落只会互相覆盖语义）。
// 占流通股比例是散户最关心的量级信号，≥10% 判 high。
func evalSellReviewLift(rows []model.RestrictedRelease, today string) *sellReviewHit {
	td, err := time.ParseInLocation("2006-01-02", today, time.Local)
	if err != nil {
		return nil
	}
	target := ""
	for _, r := range rows {
		fd, perr := time.ParseInLocation("2006-01-02", r.FreeDate, time.Local)
		if perr != nil {
			continue
		}
		days := int(fd.Sub(td).Hours() / 24)
		if days < 0 || days > sellReviewLiftAheadDays {
			continue
		}
		if target == "" || r.FreeDate < target {
			target = r.FreeDate
		}
	}
	if target == "" {
		return nil
	}
	var shares, capital, ratio float64
	var types []string
	seenType := map[string]bool{}
	for _, r := range rows {
		if r.FreeDate != target {
			continue
		}
		shares += r.FreeShares
		capital += r.LiftMarketCap
		ratio += r.FreeRatio
		if r.FreeType != "" && !seenType[r.FreeType] {
			seenType[r.FreeType] = true
			types = append(types, r.FreeType)
		}
	}
	if shares <= 0 {
		return nil
	}
	fd, _ := time.ParseInLocation("2006-01-02", target, time.Local)
	days := int(fd.Sub(td).Hours() / 24)
	sev := model.SellReviewSeverityMed
	if ratio >= 10 {
		sev = model.SellReviewSeverityHigh
	}
	detail := fmt.Sprintf("%s（%s）解禁 %.0f 万股、市值约 %.2f 亿元",
		target, daysAheadText(days), shares/1e4, capital/1e8)
	if ratio > 0 {
		detail += fmt.Sprintf("，占流通股 %.2f%%", ratio)
	}
	if len(types) > 0 {
		detail += "（" + strings.Join(types, "；") + "）"
	}
	return &sellReviewHit{
		Trigger: model.SellReviewLift, TradeDate: target, Severity: sev,
		Title: "限售解禁临近", Detail: detail,
	}
}

// earnFcstBadTypes 业绩预告的「变脸」类型（含这些词即判利空）。
var earnFcstBadTypes = []string{"预减", "预亏", "首亏", "续亏", "略减", "减亏", "转亏"}

// isEarningsForecastBad 预告是否为变脸（纯函数）。
// 判定优先看**类型词**，再看变动幅度上限为负（上限都是负的，说明最好的情况也是下滑）。
// 「减亏」列入是有意的：仍然亏损，只是亏得少些——对持仓者不是利好。
func isEarningsForecastBad(fc model.EarningsForecast) bool {
	for _, w := range earnFcstBadTypes {
		if strings.Contains(fc.PredictType, w) {
			return true
		}
	}
	return fc.AmpUpper < 0
}

// evalSellReviewEarnFcst 业绩预告变脸（发布日落在回看窗口内）。
// 预计降幅 ≥50% 判 high——那是基本面级别的变化，不是波动。
func evalSellReviewEarnFcst(fc *model.EarningsForecast, since string) *sellReviewHit {
	if fc == nil || fc.NoticeDate == "" || fc.NoticeDate < since || !isEarningsForecastBad(*fc) {
		return nil
	}
	typ := fc.PredictType
	if typ == "" {
		typ = "业绩预告"
	}
	detail := fmt.Sprintf("%s 发布【%s】", fc.NoticeDate, typ)
	if fc.AmpLower != 0 || fc.AmpUpper != 0 {
		metric := fc.PredictFinance
		if metric == "" {
			metric = "业绩"
		}
		detail += fmt.Sprintf("，%s变动 %.2f%%~%.2f%%", metric, fc.AmpLower, fc.AmpUpper)
	} else if fc.Content != "" {
		detail += "：" + truncateRunes(fc.Content, 80)
	}
	sev := model.SellReviewSeverityMed
	if fc.AmpUpper <= -50 || strings.Contains(typ, "首亏") || strings.Contains(typ, "预亏") {
		sev = model.SellReviewSeverityHigh
	}
	return &sellReviewHit{
		Trigger: model.SellReviewEarnFcst, TradeDate: fc.NoticeDate, Severity: sev,
		Title: "业绩预告变脸", Detail: detail,
	}
}

// evalSellReviewMaBreak 跌破关键均线（纯函数）。
//
// 判定的是「**刚**跌破」而非「在均线下方」：前一根收盘 ≥ 均线、最新一根收盘 < 均线。
// 长期在均线下方的票每天都提醒毫无意义；跌破那一天才是决策点。
// bars 必须按交易日升序，最后一根是最新交易日。
// 同时跌破 MA20 与 MA60 时只报 MA60（更重要的那条），severity 升为 high。
func evalSellReviewMaBreak(bars []model.DailyBar) *sellReviewHit {
	// 样本至少要够算一次短均线 + 一根前值。**样本不足返回 nil = 本轮不做判断**，
	// 不是「没跌破」——次新股/刚同步的标的属于这一类。
	if len(bars) < sellReviewMaShort+1 {
		return nil
	}
	closes := make([]float64, 0, len(bars))
	for _, b := range bars {
		if b.Close <= 0 {
			return nil // 有坏根就整段不判（宁可不报也不错报）
		}
		closes = append(closes, b.Close)
	}
	last := len(closes) - 1
	// check 判「**刚**跌破」：前一根收盘仍站在（截至前一根的）均线上，最新一根收盘掉到
	// （截至最新根的）均线下。长期在均线下方的票每天报一次毫无意义，跌破那天才是决策点。
	check := func(period int) (float64, bool) {
		maNow, ok := movingAverage(closes, period)
		if !ok {
			return 0, false
		}
		maBefore, ok := movingAverage(closes[:last], period)
		if !ok {
			return 0, false
		}
		if closes[last-1] >= maBefore && closes[last] < maNow {
			return maNow, true
		}
		return 0, false
	}
	date := bars[last].TradeDate
	// 同时跌破两条时只报更重要的 MA60（唯一键不含周期，报两条会互相覆盖）。
	if ma, ok := check(sellReviewMaLong); ok {
		return &sellReviewHit{
			Trigger: model.SellReviewMaBreak, TradeDate: date, Severity: model.SellReviewSeverityHigh,
			Title: fmt.Sprintf("跌破 MA%d", sellReviewMaLong),
			Detail: fmt.Sprintf("%s 收盘 %.2f 跌破 %d 日均线 %.2f（中期趋势转弱）",
				date, closes[last], sellReviewMaLong, ma),
			Push: true,
		}
	}
	if ma, ok := check(sellReviewMaShort); ok {
		return &sellReviewHit{
			Trigger: model.SellReviewMaBreak, TradeDate: date, Severity: model.SellReviewSeverityMed,
			Title: fmt.Sprintf("跌破 MA%d", sellReviewMaShort),
			Detail: fmt.Sprintf("%s 收盘 %.2f 跌破 %d 日均线 %.2f（短期趋势转弱）",
				date, closes[last], sellReviewMaShort, ma),
			Push: true,
		}
	}
	return nil
}

// evalSellReviewLhbSell 龙虎榜净卖出（游资出货）。窗口内按交易日分组，
// 同日多条上榜原因的净买额取**同一条记录**的值（LhbEntry 的 net_buy 是当日全榜口径，
// 多条只是原因不同，不能相加）。净卖出规模 ≥ sellReviewLhbNetSellYi 亿才报。
func evalSellReviewLhbSell(entries []model.LhbEntry) *sellReviewHit {
	// 取窗口内最近一个「净卖出达标」的交易日。
	var best *model.LhbEntry
	for i := range entries {
		e := entries[i]
		if e.NetBuy >= 0 {
			continue
		}
		if -e.NetBuy/1e8 < sellReviewLhbNetSellYi {
			continue
		}
		if best == nil || e.TradeDate > best.TradeDate {
			best = &entries[i]
		}
	}
	if best == nil {
		return nil
	}
	netSellYi := -best.NetBuy / 1e8
	sev := model.SellReviewSeverityMed
	if best.NetRatio <= -5 {
		sev = model.SellReviewSeverityHigh
	}
	detail := fmt.Sprintf("%s 登龙虎榜，席位净卖出 %.2f 亿元", best.TradeDate, netSellYi)
	if best.NetRatio != 0 {
		detail += fmt.Sprintf("（占当日成交 %.2f%%）", best.NetRatio)
	}
	if best.Reason != "" {
		detail += "，上榜原因：" + best.Reason
	}
	return &sellReviewHit{
		Trigger: model.SellReviewLhbSell, TradeDate: best.TradeDate, Severity: sev,
		Title: "龙虎榜净卖出", Detail: detail, Push: true,
	}
}

// evalSellReviewExDiv 除权除息日临近（提前 sellReviewExDivAheadDays 天）。
// 这一条的价值不在「利空」而在「账面数字会变」——不确认折算的话持仓页盈亏就是错的。
func evalSellReviewExDiv(rows []model.CorporateAction, today string) *sellReviewHit {
	td, err := time.ParseInLocation("2006-01-02", today, time.Local)
	if err != nil {
		return nil
	}
	var best *model.CorporateAction
	for i := range rows {
		r := rows[i]
		if r.ExDate == "" || !r.HasAdjustment() {
			continue
		}
		ed, perr := time.ParseInLocation("2006-01-02", r.ExDate, time.Local)
		if perr != nil {
			continue
		}
		days := int(ed.Sub(td).Hours() / 24)
		if days < 0 || days > sellReviewExDivAheadDays {
			continue
		}
		if best == nil || r.ExDate < best.ExDate {
			best = &rows[i]
		}
	}
	if best == nil {
		return nil
	}
	ed, _ := time.ParseInLocation("2006-01-02", best.ExDate, time.Local)
	days := int(ed.Sub(td).Hours() / 24)
	plan := best.PlanProfile
	if plan == "" {
		plan = fmt.Sprintf("每10股送%.4g转%.4g派%.4g元", best.BonusRatio, best.TransferRatio, best.DividendPretax)
	}
	return &sellReviewHit{
		Trigger: model.SellReviewExDiv, TradeDate: best.ExDate, Severity: model.SellReviewSeverityLow,
		Title: "除权除息临近",
		Detail: fmt.Sprintf("%s（%s）除权除息，方案 %s——除权后成本与数量需在持仓页确认折算，否则账面盈亏会显示错误",
			best.ExDate, daysAheadText(days), plan),
	}
}

// daysAheadText 「N 天后 / 今日」的统一措辞。
func daysAheadText(days int) string {
	if days <= 0 {
		return "今日"
	}
	return fmt.Sprintf("%d 天后", days)
}

// composeSellReviewDetail 把「事件本身」拼成「这件事对**我这笔持仓**意味着什么」。
//
// **fail-closed**：无当前有效行情时不写现价与浮盈亏，如实声明「行情不可用」——
// 绝不用旧价算一个浮盈亏摆在用户面前（那比不显示更糟）。
func composeSellReviewDetail(base string, p model.Position, price float64, quoteOK bool) (string, float64) {
	cost := p.BuyPrice
	if !quoteOK || price <= 0 || cost <= 0 {
		return truncateRunes(fmt.Sprintf("%s。我的持仓：成本 %.2f 元 × %g 股（当前行情不可用，浮动盈亏未知）",
			base, cost, p.Quantity), 500), 0
	}
	pnlPct := costPctOf(cost, price)
	amount := round2((price - cost) * p.Quantity)
	return truncateRunes(fmt.Sprintf("%s。我的持仓：成本 %.2f 元 × %g 股，现价 %.2f，浮动%s%.2f%%（%.2f 元）",
		base, cost, p.Quantity, price, pnlWord(pnlPct), absFloat(pnlPct), amount), 500), pnlPct
}

func absFloat(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

// ---------- 落库 ----------

// upsertSellReview 落一条卖出复核（幂等）。**OnConflict{DoNothing}**——
// 已被用户处理（resolved）或忽略（dismissed）的行绝不能被下一轮扫描拉回 open，
// 否则用户会被反复要求处理同一件事（同 B8 除权建议的先例）。
// 返回是否为本轮新建。
func upsertSellReview(row *model.SellReview) (bool, error) {
	res := common.DB.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// ListSellReviews 列出用户的卖出复核（status 空=open；"all"=全部）。仅本人。
func ListSellReviews(userID int64, status string) ([]model.SellReview, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	q := common.DB.Where("user_id = ?", userID)
	switch status {
	case "", model.SellReviewStatusOpen:
		q = q.Where("status = ?", model.SellReviewStatusOpen)
	case "all":
	case model.SellReviewStatusResolved, model.SellReviewStatusDismissed:
		q = q.Where("status = ?", status)
	default:
		return nil, errors.New("非法的状态筛选")
	}
	var rows []model.SellReview
	if err := q.Order("trade_date DESC, id DESC").Limit(sellReviewListMax).Find(&rows).Error; err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []model.SellReview{}
	}
	return rows, nil
}

// SetSellReviewStatus 标记复核已处理/忽略（也允许恢复 open，仅本人）。
func SetSellReviewStatus(userID, id int64, status string) (*model.SellReview, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	switch status {
	case model.SellReviewStatusOpen, model.SellReviewStatusResolved, model.SellReviewStatusDismissed:
	default:
		return nil, errors.New("非法状态")
	}
	var row model.SellReview
	if err := common.DB.Where("id = ? AND user_id = ?", id, userID).First(&row).Error; err != nil {
		return nil, errors.New("卖出复核不存在")
	}
	row.Status = status
	if status == model.SellReviewStatusOpen {
		row.ResolvedAt = nil
	} else {
		now := time.Now()
		row.ResolvedAt = &now
	}
	if err := common.DB.Save(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

// ---------- 评估编排 ----------

// sellReviewUserIDs 有 A 股持仓的用户（本轮的候选集合）。
func sellReviewUserIDs(ctx context.Context) ([]int64, error) {
	var ids []int64
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if err := common.DB.WithContext(ctx).Model(&model.Position{}).
		Where("status = ? AND market = ?", model.PositionStatusHolding, "cn").
		Distinct().Pluck("user_id", &ids).Error; err != nil {
		return nil, fmt.Errorf("读取候选用户失败: %w", err)
	}
	return ids, nil
}

// EvaluateSellReviewsForUser 评估单个用户的持仓利空事件，落待办并（对新增两类）推送。
// 返回本轮新建的待办数。
//
// 数据全部来自本地缓存表（解禁/预告/龙虎榜/除权/日线），**零上游请求**；
// 现价走 FreshQuotesFor，仅用于给 Detail 补账面影响——**取不到不阻断待办生成**
// （利空事件本身与行情无关，行情只是让描述更有用；fail-closed 体现在
// 「取不到就如实说浮盈亏未知」而不是「拿旧价算一个」）。
func (s *SellReviewService) EvaluateSellReviewsForUser(ctx context.Context, userID int64, today, since string) int {
	created, err := s.evaluateSellReviewsForUser(ctx, userID, today, since)
	if err != nil {
		common.SysWarn("卖出复核评估未完成 user=%d: %v", userID, err)
	}
	return created
}

// evaluateSellReviewsForUser 是带错误结果的内部执行路径。公开方法为兼容既有调用仍
// 返回计数，但调度器走这里，避免把数据库故障误报成“本轮没有待办”。
func (s *SellReviewService) evaluateSellReviewsForUser(ctx context.Context, userID int64, today, since string) (int, error) {
	if common.DB == nil {
		return 0, errors.New("数据库不可用")
	}
	var positions []model.Position
	if err := common.DB.WithContext(ctx).Where("user_id = ? AND status = ? AND market = ?",
		userID, model.PositionStatusHolding, "cn").Order("id ASC").Find(&positions).Error; err != nil {
		return 0, fmt.Errorf("读取持仓失败: %w", err)
	}
	if len(positions) == 0 {
		return 0, nil
	}
	seen := map[string]bool{}
	var syms []string
	refs := make([]QuoteRef, 0, len(positions))
	for _, p := range positions {
		if seen[p.Symbol] {
			continue
		}
		seen[p.Symbol] = true
		syms = append(syms, p.Symbol)
		refs = append(refs, QuoteRef{Market: p.Market, Symbol: p.Symbol})
	}

	// 五类数据一次性批量查（symbol IN），绝不逐 symbol 循环查。
	var lifts []model.RestrictedRelease
	if err := common.DB.WithContext(ctx).Where("symbol IN ? AND market = ? AND free_date BETWEEN ? AND ?",
		syms, "cn", today, mustAddDays(today, sellReviewLiftAheadDays)).
		Order("free_date, id").Find(&lifts).Error; err != nil {
		return 0, fmt.Errorf("读取解禁数据失败: %w", err)
	}
	var exdivs []model.CorporateAction
	if err := common.DB.WithContext(ctx).Where("symbol IN ? AND market = ? AND ex_date BETWEEN ? AND ?",
		syms, "cn", today, mustAddDays(today, sellReviewExDivAheadDays)).
		Order("ex_date, id").Find(&exdivs).Error; err != nil {
		return 0, fmt.Errorf("读取除权除息数据失败: %w", err)
	}
	var fcs []model.EarningsForecast
	if err := common.DB.WithContext(ctx).Where("symbol IN ? AND notice_date >= ?", syms, since).
		Order("notice_date DESC, id DESC").Find(&fcs).Error; err != nil {
		return 0, fmt.Errorf("读取业绩预告数据失败: %w", err)
	}
	var lhbs []model.LhbEntry
	if err := common.DB.WithContext(ctx).Where("symbol IN ? AND trade_date >= ?", syms, since).
		Order("trade_date, id").Find(&lhbs).Error; err != nil {
		return 0, fmt.Errorf("读取龙虎榜数据失败: %w", err)
	}

	liftBySym := map[string][]model.RestrictedRelease{}
	for _, r := range lifts {
		liftBySym[r.Symbol] = append(liftBySym[r.Symbol], r)
	}
	exdivBySym := map[string][]model.CorporateAction{}
	for _, r := range exdivs {
		exdivBySym[r.Symbol] = append(exdivBySym[r.Symbol], r)
	}
	fcBySym := map[string]*model.EarningsForecast{} // 每 symbol 最新一份（已按 notice_date 降序）
	for i := range fcs {
		if _, ok := fcBySym[fcs[i].Symbol]; !ok {
			fcBySym[fcs[i].Symbol] = &fcs[i]
		}
	}
	lhbBySym := map[string][]model.LhbEntry{}
	for _, e := range lhbs {
		lhbBySym[e.Symbol] = append(lhbBySym[e.Symbol], e)
	}
	maBySym := map[string]*sellReviewHit{}
	for _, sym := range syms {
		bars, err := recentLocalBars(ctx, "cn", sym, sellReviewBarLimit)
		if err != nil {
			return 0, fmt.Errorf("读取 %s 日线失败: %w", sym, err)
		}
		if h := evalSellReviewMaBreak(bars); h != nil {
			maBySym[sym] = h
		}
	}

	// 现价（best-effort，只影响 Detail 的账面段）。market 未注入时按「行情不可用」处理——
	// 利空事件本身与行情无关，缺行情只是让描述少一段，不该阻断待办生成。
	var quotes map[string]FreshQuoteResult
	if s.market != nil {
		quotes = s.market.FreshQuotesFor(ctx, refs)
	}

	pushOn := userNotifyEnabled(userID) && s.notify != nil && s.notify.HasEnabledChannel(userID)
	if pushOn {
		cfg := loadGuardConfig(userID)
		// 卖出复核推送沿用守护总开关与「盘后事件」子开关——用户关掉盘后事件
		// 就是不想被盘后的事打扰，本轮不该绕过它另开一路。
		pushOn = cfg.Enabled && cfg.Evening
	}

	created := 0
	var writeErrs []error
	for _, p := range positions {
		var hits []sellReviewHit
		if h := evalSellReviewLift(liftBySym[p.Symbol], today); h != nil {
			hits = append(hits, *h)
		}
		if h := evalSellReviewEarnFcst(fcBySym[p.Symbol], since); h != nil {
			hits = append(hits, *h)
		}
		if h := maBySym[p.Symbol]; h != nil {
			hits = append(hits, *h)
		}
		if h := evalSellReviewLhbSell(lhbBySym[p.Symbol]); h != nil {
			hits = append(hits, *h)
		}
		if h := evalSellReviewExDiv(exdivBySym[p.Symbol], today); h != nil {
			hits = append(hits, *h)
		}
		if len(hits) == 0 {
			continue
		}
		price, quoteOK := 0.0, false
		if fq, ok := quotes[QuoteKey(p.Market, p.Symbol)]; ok && fq.Quote != nil &&
			fq.Quote.Price > 0 && fq.Fresh.Status == freshStatusFresh {
			price, quoteOK = fq.Quote.Price, true
		}
		for _, h := range hits {
			detail, pnlPct := composeSellReviewDetail(h.Detail, p, price, quoteOK)
			row := &model.SellReview{
				UserID: userID, PositionID: p.ID,
				Symbol: p.Symbol, Market: p.Market, Name: orSymbol(p.Name, p.Symbol),
				Trigger: h.Trigger, TradeDate: h.TradeDate, Severity: h.Severity,
				Title: h.Title, Detail: detail,
				BuyPrice: p.BuyPrice, Quantity: p.Quantity, Price: round4(price),
				ProfitPct: pnlPct, QuoteOK: quoteOK,
				Status: model.SellReviewStatusOpen,
			}
			isNew, err := upsertSellReview(row)
			if err != nil {
				common.SysWarn("卖出复核落库失败 user=%d pos=%d trigger=%s: %v",
					userID, p.ID, h.Trigger, err)
				writeErrs = append(writeErrs, fmt.Errorf(
					"写入 pos=%d trigger=%s 卖出复核失败: %w", p.ID, h.Trigger, err,
				))
				continue
			}
			if !isNew {
				continue
			}
			created++
			// 只有既有 guard 未覆盖的两类才推送（解禁/除权/预告已由 B9 盘后轮推过，
			// 再推一遍就是同一件事两条通知）。去重仍走 GuardEvent 台账。
			if h.Push && pushOn {
				s.pushSellReview(userID, p, h, detail)
			}
		}
	}
	return created, errors.Join(writeErrs...)
}

// pushSellReview 推送一条卖出复核（去重走 GuardEvent 台账，通道复用 NotifyService）。
func (s *SellReviewService) pushSellReview(userID int64, p model.Position, h sellReviewHit, detail string) {
	kind := model.GuardKindPosMaBreak
	if h.Trigger == model.SellReviewLhbSell {
		kind = model.GuardKindPosLhbSell
	}
	msg := truncateRunes(fmt.Sprintf("⚠️ 卖出复核：%s(%s) %s——%s",
		orSymbol(p.Name, p.Symbol), p.Symbol, h.Title, detail), 256)
	hit := guardHit{
		Symbol: p.Symbol, Market: p.Market, Name: orSymbol(p.Name, p.Symbol),
		Kind: kind, Message: msg, Route: "/todos", EventDate: h.TradeDate,
	}
	if !recordGuardEvent(userID, h.TradeDate, hit) {
		return // 已推过
	}
	s.notify.SendMsg(userID, NotifyMessage{
		Title: guardTitle(kind), Content: msg,
		Route: hit.Route, Kind: NotifyMsgKindGuard, Priority: 0,
	})
}

// recentLocalBars 读本地日线尾部 limit 根（升序）。**零上游请求**——
// 全市场日线 16:10 已同步，跌破均线判定不该再去打行情源。
// 无数据返回空切片；读取失败显式上浮，不能与“尚无日线”混为一谈。
func recentLocalBars(ctx context.Context, market, symbol string, limit int) ([]model.DailyBar, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if symbol == "" || limit <= 0 {
		return nil, errors.New("日线查询参数非法")
	}
	var rows []model.DailyBar
	if err := common.DB.WithContext(ctx).
		Where("symbol = ? AND market = ?", symbol, market).
		Order("trade_date DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	// 反转成升序（评估函数约定最后一根是最新交易日）。
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
	return rows, nil
}

// RunSellReviewRound 一轮卖出复核（供 job 与测试调用）。返回新建待办总数。
func (s *SellReviewService) RunSellReviewRound(ctx context.Context) int {
	if common.DB == nil {
		common.SysWarn("卖出复核轮未执行: 数据库不可用")
		return 0
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	since := now.AddDate(0, 0, -sellReviewWindowDays).Format("2006-01-02")
	total := 0
	userIDs, err := sellReviewUserIDs(ctx)
	if err != nil {
		common.SysWarn("卖出复核读取候选用户失败: %v", err)
		return 0
	}
	for _, uid := range userIDs {
		if err := ctx.Err(); err != nil {
			common.SysWarn("卖出复核轮中止: %v", err)
			return total
		}
		n, err := s.evaluateSellReviewsForUser(ctx, uid, today, since)
		if err != nil {
			common.SysWarn("卖出复核评估未完成 user=%d: %v", uid, err)
			continue
		}
		if n > 0 {
			common.SysLog("用户 %d 新增卖出复核 %d 条", uid, n)
			total += n
		}
	}
	return total
}

// StartSellReviewJob 每日 19:40 跑一轮卖出复核（含周末——预告/公告周末也发布，
// 龙虎榜与日线在非交易日自然无新数据，唯一键保证重跑不重复）。
// 启动 6 分钟后先补跑一轮（服务缺勤后补生成窗口内漏掉的复核）。
func StartSellReviewJob(market *MarketService) *SellReviewService {
	svc := NewSellReviewService(market)
	go func() {
		if common.DB == nil {
			return
		}
		time.Sleep(6 * time.Minute)
		runOnce := func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			svc.RunSellReviewRound(ctx)
		}
		runOnce()
		for {
			time.Sleep(time.Until(nextDailyAt(time.Now(), sellReviewHour, sellReviewMin)))
			runOnce()
		}
	}()
	return svc
}
