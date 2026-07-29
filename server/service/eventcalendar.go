package service

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"quantvista/common"
	"quantvista/model"
)

// B9 事件日历：未来 N 天「与我相关」的事件（持仓/自选的解禁、除权除息、财报披露）
// + 全市场打新（新股/可转债申购，不依赖持仓）。
//
// 纪律：
//   - **纯读时聚合，零落库**（数据都在本地缓存表，读一次拼一次）；
//   - 用户隔离：持仓/自选集合恒带 user_id；打新是全市场公开信息，对所有用户一致；
//   - **数据不可用如实声明**：某一类读取失败时置 Complete=false 并记 Errors，
//     绝不吞错后返回空清单让用户以为「未来没事发生」（同 TodoService 的 fail-closed 纪律）。

// 事件类型（前端按此分组与配色）。
const (
	EventKindLift  = "lift"   // 限售解禁
	EventKindExDiv = "ex_div" // 除权除息
	EventKindEarn  = "earn"   // 财报预约披露
	EventKindIpo   = "ipo"    // 新股申购
	EventKindCb    = "cb"     // 可转债申购
)

// 事件与我的关系。
const (
	EventRelPosition = "position" // 我的持仓
	EventRelWatch    = "watch"    // 我的自选
	EventRelMarket   = "market"   // 全市场（打新）
)

// CalendarEvent 一条日历事件。
type CalendarEvent struct {
	Date     string `json:"date"`      // 事件日 YYYY-MM-DD
	DaysLeft int    `json:"days_left"` // 距今自然日（0=今天）
	Kind     string `json:"kind"`
	Relation string `json:"relation"`
	Symbol   string `json:"symbol"`
	Market   string `json:"market"`
	Name     string `json:"name"`
	Title    string `json:"title"`  // 一句话标题
	Detail   string `json:"detail"` // 细节（比例/金额/申购代码）

	// 结构化数值（前端排序与展示用；单位与落库一致：股/元/%）。
	Shares    float64 `json:"shares,omitempty"`     // 解禁股数
	MarketCap float64 `json:"market_cap,omitempty"` // 解禁市值
	Ratio     float64 `json:"ratio,omitempty"`      // 占流通股 %
	ApplyCode string  `json:"apply_code,omitempty"` // 申购代码
	Route     string  `json:"route,omitempty"`      // 前端跳转
}

// CalendarResult 事件日历聚合结果。
type CalendarResult struct {
	From     string          `json:"from"`
	To       string          `json:"to"`
	Days     int             `json:"days"`
	Events   []CalendarEvent `json:"events"`
	Total    int             `json:"total"`
	Complete bool            `json:"complete"` // false = 至少一类读取失败，清单可能不全
	Errors   []string        `json:"errors"`
}

// eventCalendarMaxDays 前瞻窗口上限。上游解禁/申购只同步未来 60 天，
// 超过这个数就是在承诺我们没有的数据。
const eventCalendarMaxDays = 60

// EventCalendar 聚合某用户未来 days 天的事件日历。
func EventCalendar(userID int64, days int) (*CalendarResult, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	if days <= 0 {
		days = 30
	}
	if days > eventCalendarMaxDays {
		days = eventCalendarMaxDays
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	until := now.AddDate(0, 0, days).Format("2006-01-02")
	res := &CalendarResult{From: today, To: until, Days: days, Events: []CalendarEvent{}, Complete: true}
	fail := func(block string, err error) {
		res.Complete = false
		res.Errors = append(res.Errors, block+"读取失败，该类事件可能缺失")
		common.SysWarn("事件日历读取%s失败 user=%d: %v", block, userID, err)
	}
	daysLeft := func(date string) int {
		d, err := time.ParseInLocation("2006-01-02", date, time.Local)
		if err != nil {
			return 0
		}
		t0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
		return int(d.Sub(t0).Hours() / 24)
	}

	// 我的标的集合：持仓（holding）与自选，各自记住关系（持仓优先于自选）。
	relBySym := map[string]string{}
	nameBySym := map[string]string{}
	var syms []string
	addSym := func(sym, name, rel string) {
		if sym == "" {
			return
		}
		if old, ok := relBySym[sym]; ok {
			if old == EventRelPosition {
				return // 持仓关系优先，不被自选覆盖
			}
		} else {
			syms = append(syms, sym)
		}
		relBySym[sym] = rel
		if name != "" {
			nameBySym[sym] = name
		}
	}
	var positions []model.Position
	if err := common.DB.Where("user_id = ? AND status = ? AND market = ?",
		userID, model.PositionStatusHolding, "cn").Find(&positions).Error; err != nil {
		fail("持仓", err)
	}
	for _, p := range positions {
		addSym(p.Symbol, p.Name, EventRelPosition)
	}
	var items []model.WatchlistItem
	if err := common.DB.Where("user_id = ? AND market = ?", userID, "cn").Find(&items).Error; err != nil {
		fail("自选", err)
	}
	for _, it := range items {
		addSym(it.Symbol, it.Name, EventRelWatch)
	}

	if len(syms) > 0 {
		// 解禁。
		var lifts []model.RestrictedRelease
		if err := common.DB.Where("symbol IN ? AND market = ? AND free_date BETWEEN ? AND ?",
			syms, "cn", today, until).Order("free_date, symbol").Find(&lifts).Error; err != nil {
			fail("限售解禁", err)
		}
		for _, l := range lifts {
			name := orSymbol(nameBySym[l.Symbol], l.Name)
			detail := fmt.Sprintf("解禁 %.0f 万股，市值约 %.2f 亿元", l.FreeShares/1e4, l.LiftMarketCap/1e8)
			if l.FreeRatio > 0 {
				detail += fmt.Sprintf("，占流通股 %.2f%%", l.FreeRatio)
			}
			if l.FreeType != "" {
				detail += "（" + l.FreeType + "）"
			}
			res.Events = append(res.Events, CalendarEvent{
				Date: l.FreeDate, DaysLeft: daysLeft(l.FreeDate), Kind: EventKindLift,
				Relation: relBySym[l.Symbol], Symbol: l.Symbol, Market: "cn",
				Name: orSymbol(name, l.Symbol), Title: "限售解禁", Detail: detail,
				Shares: l.FreeShares, MarketCap: l.LiftMarketCap, Ratio: l.FreeRatio,
				Route: stockDetailRoute("cn", l.Symbol),
			})
		}

		// 除权除息。
		var acts []model.CorporateAction
		if err := common.DB.Where("symbol IN ? AND market = ? AND ex_date BETWEEN ? AND ?",
			syms, "cn", today, until).Order("ex_date, symbol").Find(&acts).Error; err != nil {
			fail("除权除息", err)
		}
		for i := range acts {
			a := acts[i]
			if !a.HasAdjustment() {
				continue
			}
			plan := a.PlanProfile
			if plan == "" {
				plan = fmt.Sprintf("每10股送%.4g转%.4g派%.4g元", a.BonusRatio, a.TransferRatio, a.DividendPretax)
			}
			res.Events = append(res.Events, CalendarEvent{
				Date: a.ExDate, DaysLeft: daysLeft(a.ExDate), Kind: EventKindExDiv,
				Relation: relBySym[a.Symbol], Symbol: a.Symbol, Market: "cn",
				Name:  orSymbol(orSymbol(nameBySym[a.Symbol], a.Name), a.Symbol),
				Title: "除权除息", Detail: plan,
				Route: stockDetailRoute("cn", a.Symbol),
			})
		}

		// 财报预约披露（未实际披露的）。
		var scheds []model.DisclosureSchedule
		if err := common.DB.Where("symbol IN ? AND is_published = ? AND appoint_date BETWEEN ? AND ?",
			syms, false, today, until).Order("appoint_date, symbol").Find(&scheds).Error; err != nil {
			fail("财报披露", err)
		}
		for _, sc := range scheds {
			label := sc.ReportTypeName
			if label == "" {
				label = "财报"
			}
			res.Events = append(res.Events, CalendarEvent{
				Date: sc.AppointDate, DaysLeft: daysLeft(sc.AppointDate), Kind: EventKindEarn,
				Relation: relBySym[sc.Symbol], Symbol: sc.Symbol, Market: "cn",
				Name:  orSymbol(orSymbol(nameBySym[sc.Symbol], sc.Name), sc.Symbol),
				Title: "财报披露", Detail: "预约披露 " + label,
				Route: stockDetailRoute("cn", sc.Symbol),
			})
		}
	}

	// 打新（全市场，不依赖持仓/自选）。
	var subs []model.IpoSubscription
	if err := common.DB.Where("apply_date BETWEEN ? AND ?", today, until).
		Order("apply_date, kind, code").Find(&subs).Error; err != nil {
		fail("打新日历", err)
	}
	for _, s := range subs {
		kind, title := EventKindIpo, "新股申购"
		detail := ""
		if s.Kind == model.IpoKindCb {
			kind, title = EventKindCb, "可转债申购"
			if s.StockName != "" {
				detail = fmt.Sprintf("正股 %s(%s)", s.StockName, s.StockCode)
			}
			if s.Rating != "" {
				detail = appendSep(detail, "评级 "+s.Rating)
			}
			if s.IssueScaleYi > 0 {
				detail = appendSep(detail, fmt.Sprintf("规模 %.2f 亿元", s.IssueScaleYi))
			}
		} else {
			if s.Board != "" {
				detail = s.Board
			}
			if s.ApplyUpper > 0 {
				detail = appendSep(detail, fmt.Sprintf("申购上限 %.0f 股", s.ApplyUpper))
			}
		}
		if s.IssuePrice > 0 {
			detail = appendSep(detail, fmt.Sprintf("发行价 %.2f 元", s.IssuePrice))
		} else {
			detail = appendSep(detail, "发行价待定")
		}
		res.Events = append(res.Events, CalendarEvent{
			Date: s.ApplyDate, DaysLeft: daysLeft(s.ApplyDate), Kind: kind,
			Relation: EventRelMarket, Symbol: s.Code, Market: "cn", Name: s.Name,
			Title: title, Detail: detail, ApplyCode: s.ApplyCode,
		})
	}

	// 排序：日期升序 → 关系（持仓 > 自选 > 全市场）→ 代码，保证同日先看到自己的仓。
	relRank := map[string]int{EventRelPosition: 0, EventRelWatch: 1, EventRelMarket: 2}
	sort.SliceStable(res.Events, func(i, j int) bool {
		a, b := res.Events[i], res.Events[j]
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		if relRank[a.Relation] != relRank[b.Relation] {
			return relRank[a.Relation] < relRank[b.Relation]
		}
		return a.Symbol < b.Symbol
	})
	res.Total = len(res.Events)
	return res, nil
}

// liftSignal 候选池/bear 论据用的解禁信号（未来窗口内最近一批，同日多批已合并）。
type liftSignal struct {
	Date      string
	Days      int
	SharesWan float64 // 万股
	CapYi     float64 // 亿元
	RatioPct  float64 // 占流通股 %
}

// liftSignalsFor 批量取一组标的未来 riskLiftAheadDays 内最近一批解禁（一次查询）。
//
// 返回 (信号表, 数据是否可用)。**available=false 与「表里没有该 symbol」语义不同**：
// 前者是查询失败/数据未同步（不知道），后者是窗口内确无解禁（有依据）。
// 调用方必须区分——把「不知道」当成「没有」会让 AI 给出无依据的安全结论。
func liftSignalsFor(symbols []string) (map[string]liftSignal, bool) {
	out := map[string]liftSignal{}
	if common.DB == nil || len(symbols) == 0 {
		return out, false
	}
	now := time.Now()
	today := now.Format("2006-01-02")
	until := now.AddDate(0, 0, riskLiftAheadDays).Format("2006-01-02")
	var rows []model.RestrictedRelease
	if err := common.DB.Where("symbol IN ? AND market = ? AND free_date BETWEEN ? AND ?",
		symbols, "cn", today, until).Order("free_date ASC, id ASC").Find(&rows).Error; err != nil {
		common.SysWarn("解禁信号批量查询失败: %v", err)
		return out, false
	}
	t0 := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	for _, r := range rows {
		cur, seen := out[r.Symbol]
		if seen && cur.Date != r.FreeDate {
			continue // 已取到更早的一批（按 free_date 升序，首个即最近）
		}
		if !seen {
			d, err := time.ParseInLocation("2006-01-02", r.FreeDate, time.Local)
			if err != nil {
				continue
			}
			cur = liftSignal{Date: r.FreeDate, Days: int(d.Sub(t0).Hours() / 24)}
		}
		// 同一天多批：合并（口径与 guard/riskgate 一致）。
		cur.SharesWan = round2(cur.SharesWan + r.FreeShares/1e4)
		cur.CapYi = round2(cur.CapYi + r.LiftMarketCap/1e8)
		cur.RatioPct = round2(cur.RatioPct + r.FreeRatio)
		out[r.Symbol] = cur
	}
	return out, true
}

// appendSep 用「·」拼接非空片段。
func appendSep(base, add string) string {
	if add == "" {
		return base
	}
	if base == "" {
		return add
	}
	return base + " · " + add
}

// StockCorpEvents 个股维度的解禁 / 分红块（StockDetail 用）。
// 解禁取未来窗口 + 最近一次已过去的（用户要知道「刚解禁完」）；分红取最近几期方案。
type StockCorpEvents struct {
	Symbol string `json:"symbol"`
	Market string `json:"market"`

	Lifts   []model.RestrictedRelease `json:"lifts"`
	Actions []model.CorporateAction   `json:"actions"`

	// DividendYield 当前股息率与其报告期（C10；nil = 无数据，**不是 0%**）。
	// 由 pickLatestDividendYield 统一挑选，个股详情估值区与 AI 快照共用同一口径。
	DividendYield *DividendYieldView `json:"dividend_yield,omitempty"`

	// LiftUnavailable/ActionUnavailable：**该维度数据不可用**（同步未跑/读取失败），
	// 与「确实没有解禁/分红」是两回事——前端与 AI 都必须区分，不能把未知说成没有。
	LiftUnavailable   bool   `json:"lift_unavailable"`
	ActionUnavailable bool   `json:"action_unavailable"`
	Note              string `json:"note,omitempty"`
}

const (
	stockLiftAheadDays = 180 // 个股页解禁前瞻窗口（半年，覆盖上游 60 天同步窗口之外的已知行）
	stockLiftBackDays  = 90  // 回看窗口（刚解禁完同样是重要背景）
	stockActionLimit   = 8   // 分红方案取最近 N 期
)

// StockCorpEventsFor 查某只 A 股的解禁与分红（无用户隔离——公开市场信息）。
func StockCorpEventsFor(market, symbol string) (*StockCorpEvents, error) {
	if common.DB == nil {
		return nil, errors.New("数据库不可用")
	}
	out := &StockCorpEvents{Symbol: symbol, Market: market,
		Lifts: []model.RestrictedRelease{}, Actions: []model.CorporateAction{}}
	if market != "cn" {
		out.LiftUnavailable, out.ActionUnavailable = true, true
		out.Note = "解禁与分红数据仅覆盖 A 股"
		return out, nil
	}
	now := time.Now()
	from := now.AddDate(0, 0, -stockLiftBackDays).Format("2006-01-02")
	to := now.AddDate(0, 0, stockLiftAheadDays).Format("2006-01-02")
	if err := common.DB.Where("symbol = ? AND market = ? AND free_date BETWEEN ? AND ?",
		symbol, market, from, to).Order("free_date ASC").Find(&out.Lifts).Error; err != nil {
		out.LiftUnavailable = true
		common.SysWarn("个股解禁查询失败 %s: %v", symbol, err)
	}
	if err := common.DB.Where("symbol = ? AND market = ?", symbol, market).
		Order("report_date DESC, ex_date DESC").Limit(stockActionLimit).
		Find(&out.Actions).Error; err != nil {
		out.ActionUnavailable = true
		common.SysWarn("个股分红查询失败 %s: %v", symbol, err)
	}
	if out.Lifts == nil {
		out.Lifts = []model.RestrictedRelease{}
	}
	if out.Actions == nil {
		out.Actions = []model.CorporateAction{}
	}
	// C10 当前股息率：在已取到的最近 stockActionLimit 期方案里挑（它们已按报告期降序，
	// 覆盖约 2~4 年，足够回看窗口）。**读取失败时不挑**——失败已置 ActionUnavailable，
	// 此时给出股息率等于用空结果冒充「无分红」。
	if !out.ActionUnavailable {
		out.DividendYield = pickLatestDividendYield(out.Actions, now)
	}
	return out, nil
}
