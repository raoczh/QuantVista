package datasource

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
)

// 公司行动与打新日历（B8/B9）：东财 datacenter 四张 RPT_* 报表，全部走 DataCenterQuery
// 网关（包级令牌桶 QPS≤2 + 退避重试），**不得另起 HTTP 路径**。
//
//	RPT_SHAREBONUS_DET  分红送转（除权除息日/送转比例/每 10 股派息/股息率）
//	RPT_LIFT_STAGE      限售解禁（解禁日/本次解禁股数/解禁市值/占比）
//	RPTA_APP_IPOAPPLY   新股申购（申购日/申购代码/申购上限/中签缴款日）
//	RPT_BOND_CB_LIST    可转债申购（网上申购日/申购代码/正股/评级）
//
// **单位口径（2026-07-28 逐字段实测验算，与项目铁律「金额=元、比例=百分比数值」对齐）**：
//   - 分红 BONUS_RATIO/IT_RATIO/PRETAX_BONUS_RMB 均为「每 10 股」口径的原值，直接落库不换算；
//   - 分红 DIVIDENT_RATIO 上游是**小数**（0.002731=0.27%），落库前 ×100 转百分比数值；
//   - 解禁 CURRENT_FREE_SHARES/LIFT_MARKET_CAP 上游单位是**万股 / 万元**，落库前 ×1e4 转股/元；
//   - 解禁 FREE_RATIO/TOTAL_RATIO 上游是小数，落库前 ×100 转百分比数值。
//
// **解禁字段的坑（务必先读，计划文档 §2.2 原表述有误已按实测修正）**：
// `FREE_SHARES` 不是本次解禁量，而是**解禁后的已流通股数**；本次真正解禁的是
// `CURRENT_FREE_SHARES`（≈`ABLE_FREE_SHARES`）。两者可差数倍（实测 600675：
// CURRENT=21276.6 万股 vs FREE_SHARES=132074.5 万股），错用会把解禁规模严重高估。
// 验算锚点：CURRENT_FREE_SHARES × NEW(现价) == LIFT_MARKET_CAP；
// FREE_RATIO == CURRENT/(FREE_SHARES−CURRENT)（占解禁前流通股）；
// TOTAL_RATIO == CURRENT/(FREE_SHARES+NON_FREE_SHARES)（占总股本）。

const (
	corpActionReportBonus = "RPT_SHAREBONUS_DET"
	corpActionReportLift  = "RPT_LIFT_STAGE"
	corpActionReportIpo   = "RPTA_APP_IPOAPPLY"
	corpActionReportCb    = "RPT_BOND_CB_LIST"

	// corpActionMaxPages 单次同步的翻页护栏。窗口化查询（近 90 天分红 / 未来 60 天解禁申购）
	// 单页 500 行，实测各报表窗口内均 <2000 行；超出即截断并由调用方记日志，不静默丢数据。
	corpActionMaxPages = 8
)

// CorpActionRow 一条分红送转方案（RPT_SHAREBONUS_DET）。
// 比例口径：BonusRatio/TransferRatio/DividendPretax 均为「每 10 股」；DividendYield 为百分比数值。
type CorpActionRow struct {
	Symbol string // 6 位代码
	Name   string
	Market string // cn 内部市场标识（本报表恒 cn）

	ExDate     string // 除权除息日 YYYY-MM-DD（空=方案未到实施阶段）
	RecordDate string // 股权登记日 YYYY-MM-DD
	ReportDate string // 报告期 YYYY-MM-DD（与 ExDate 共同构成自然唯一键）
	NoticeDate string // 公告日 YYYY-MM-DD

	BonusRatio     float64 // 每 10 股送股（股）
	TransferRatio  float64 // 每 10 股转增（股）
	DividendPretax float64 // 每 10 股派息税前（元）
	DividendYield  float64 // 股息率 %（上游小数已 ×100）

	Progress    string // 方案进度（实施分配/预案/股东大会通过…）
	PlanProfile string // 方案描述（「10派0.059元(含税…)」）
}

// LiftRow 一条限售解禁（RPT_LIFT_STAGE）。
type LiftRow struct {
	Symbol string
	Name   string

	FreeDate      string  // 解禁日 YYYY-MM-DD
	FreeShares    float64 // **本次**解禁股数（股，上游万股已 ×1e4）
	LiftMarketCap float64 // 解禁市值（元，上游万元已 ×1e4）
	FreeType      string  // 解禁类型（首发原股东限售股份/定向增发机构配售股份…）
	FreeRatio     float64 // 占解禁前流通股比例 %（上游小数已 ×100）
	TotalRatio    float64 // 占总股本比例 %（上游小数已 ×100）
}

// IpoRow 一条新股申购（RPTA_APP_IPOAPPLY）。
type IpoRow struct {
	Symbol     string
	Name       string
	ApplyCode  string  // 申购代码（沪市与股票代码不同，须独立落库）
	ApplyDate  string  // 网上申购日 YYYY-MM-DD
	IssuePrice float64 // 发行价（元；未定价为 0）
	ApplyUpper float64 // 网上申购上限（股）
	PayDate    string  // 中签缴款日 YYYY-MM-DD
	BallotDate string  // 中签号公布日 YYYY-MM-DD
	ListDate   string  // 上市日 YYYY-MM-DD（未定为空）
	Board      string  // 板块（深交所创业板/上交所科创板…）
}

// CbRow 一条可转债申购（RPT_BOND_CB_LIST）。
type CbRow struct {
	Symbol       string  // 转债代码（113709）
	Name         string  // 转债简称（振26转债）
	ApplyCode    string  // 申购代码（CORRECODE，如 754067——与转债代码不同）
	ApplyDate    string  // 网上申购日 YYYY-MM-DD（PUBLIC_START_DATE）
	IssuePrice   float64 // 发行价（元，通常 100）
	ListDate     string  // 上市日 YYYY-MM-DD（未定为空）
	StockCode    string  // 正股代码
	StockName    string  // 正股简称
	Rating       string  // 债项评级（AA/AA-/A+…）
	IssueScaleYi float64 // 发行规模（亿元，上游即亿元口径不换算）
}

// corpActionStockRow 该行是否 A 股股票：SECURITY_TYPE_CODE 058 前缀 + 代码可映射 secid
// （沿用龙虎榜 lhbStockRow 先例；060=可转债等衍生品，6 位数字代码挡不住）。
// 报表缺该字段时（部分报表不返回）typeCode 传空，由调用方决定是否放行。
func corpActionStockRow(typeCode, symbol string) bool {
	if !strings.HasPrefix(typeCode, "058") {
		return false
	}
	return cnAShareCode(symbol)
}

// cnAShareCode 是否**沪深 A 股正股**代码（**不能用 cnSecid 代替**：cnSecid 对 '9' 开头
// 一律返回 "1."+code，沪 B 股 900xxx 会被放行；深 B 股 200xxx 同理落在 '2' 分支）。
// 分红报表不返回 SECURITY_TYPE_CODE，只能靠代码段白名单把关。
//
//	放行：沪主板 600/601/603/605、科创板 688/689、深主板 000/001/002/003、创业板 300/301
//	排除：B 股 900/200、基金 15x/16x/18x/50x/51x、可转债 11x/12x/13x
//
// **北交所（43x/83x/87x/88x/920）一并排除**：cnSecid 不识别、全项目行情源不覆盖，
// 放行会落入拿不到行情也关联不上持仓的死数据。口径与龙虎榜/解禁侧保持一致。
func cnAShareCode(symbol string) bool {
	s := strings.TrimSpace(symbol)
	if len(s) != 6 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	switch s[:3] {
	case "600", "601", "603", "605", "688", "689",
		"000", "001", "002", "003", "300", "301":
		return true
	}
	return false
}

// round4 单位换算后按落库精度 decimal(20,4) 收敛，避免 ×1e4 / ×100 的浮点尾巴
// （0.192030726836*100 = 19.203072683600001）读回后与内存值漂移。
func round4(v float64) float64 {
	return math.Round(v*1e4) / 1e4
}

// dcPages 通用翻页读取：把一次报表查询的全部页原始行收集起来，逐行交给 parse 回调。
// parse 返回 (ok=false, err=nil) 表示该行按业务规则过滤掉（非错误）。
// 首页无数据返回 ErrNoData（datacenter 9201 语义，调用方据此区分「窗口内确实没有」）。
func dcPages(ctx context.Context, e *EastMoneyAdapter, q DataCenterQuery, label string,
	consume func(DcRow) error) (int, error) {
	it := e.DataCenterQuery(q)
	rows := 0
	for page := 0; page < corpActionMaxPages; page++ {
		raws, err := it.Next(ctx)
		if err != nil {
			return rows, err
		}
		if raws == nil {
			return rows, nil
		}
		for _, raw := range raws {
			r, perr := ParseDcRow(raw)
			if perr != nil {
				return rows, fmt.Errorf("%w: %s 行解析失败: %v", ErrUpstream, label, perr)
			}
			if cerr := consume(r); cerr != nil {
				return rows, cerr
			}
			rows++
		}
	}
	// 走满护栏页数仍未结束：如实报错而非静默截断（窗口内数据量异常时须被发现）。
	return rows, fmt.Errorf("%w: %s 结果超过 %d 页护栏，窗口需收窄", ErrUpstream, label, corpActionMaxPages)
}

// GetCorpActions 拉取公告日 >= since 的分红送转方案（近 N 天公告窗口，增量同步用）。
// since 形如 2026-05-01。窗口内无数据返回 ErrNoData。
func (e *EastMoneyAdapter) GetCorpActions(ctx context.Context, since string) ([]CorpActionRow, error) {
	var out []CorpActionRow
	_, err := dcPages(ctx, e, DataCenterQuery{
		ReportName:  corpActionReportBonus,
		Filter:      fmt.Sprintf("(NOTICE_DATE>='%s')", since),
		SortColumns: "NOTICE_DATE",
		SortTypes:   "-1",
	}, "分红送转", func(r DcRow) error {
		row, ok, perr := parseCorpActionRow(r)
		if perr != nil {
			return fmt.Errorf("%w: 分红送转行结构无效: %v", ErrUpstream, perr)
		}
		if ok {
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseCorpActionRow 分红送转单行解析（抽出便于 fixture 单测，防上游字段漂移）。
// 必填：代码 + 报告期（唯一键的一半）；除权日可空（预案阶段尚未确定）。
func parseCorpActionRow(r DcRow) (CorpActionRow, bool, error) {
	sym := r.String("SECURITY_CODE")
	reportDate := r.Date("REPORT_DATE")
	if sym == "" || reportDate == "" {
		return CorpActionRow{}, false, errors.New("缺少代码/报告期必填字段")
	}
	// 本报表不返回 SECURITY_TYPE_CODE，用 A 股代码段白名单把关
	//（B 股 900xxx 会分红且会被 cnSecid 放行，必须在这里挡住）。
	if !cnAShareCode(sym) {
		return CorpActionRow{}, false, nil
	}
	return CorpActionRow{
		Symbol:     sym,
		Name:       r.String("SECURITY_NAME_ABBR"),
		Market:     "cn",
		ExDate:     r.Date("EX_DIVIDEND_DATE"),
		RecordDate: r.Date("EQUITY_RECORD_DATE"),
		ReportDate: reportDate,
		NoticeDate: r.Date("NOTICE_DATE"),

		BonusRatio:     r.Float("BONUS_RATIO"),
		TransferRatio:  r.Float("IT_RATIO"),
		DividendPretax: r.Float("PRETAX_BONUS_RMB"),
		// 上游 DIVIDENT_RATIO 是小数（0.002731=0.27%），统一转百分比数值。
		DividendYield: round4(r.Float("DIVIDENT_RATIO") * 100),

		Progress:    r.String("ASSIGN_PROGRESS"),
		PlanProfile: r.String("IMPL_PLAN_PROFILE"),
	}, true, nil
}

// GetLiftReleases 拉取解禁日落在 [from, to] 的限售解禁（未来窗口，增量同步用）。
// 仅保留 SECURITY_TYPE_CODE=058* 的股票行（排除 060 可转债等非股票）。
func (e *EastMoneyAdapter) GetLiftReleases(ctx context.Context, from, to string) ([]LiftRow, error) {
	var out []LiftRow
	_, err := dcPages(ctx, e, DataCenterQuery{
		ReportName:  corpActionReportLift,
		Filter:      fmt.Sprintf("(FREE_DATE>='%s')(FREE_DATE<='%s')", from, to),
		SortColumns: "FREE_DATE",
		SortTypes:   "1",
	}, "限售解禁", func(r DcRow) error {
		row, ok, perr := parseLiftRow(r)
		if perr != nil {
			return fmt.Errorf("%w: 限售解禁行结构无效: %v", ErrUpstream, perr)
		}
		if ok {
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseLiftRow 限售解禁单行解析。
//
// **CURRENT_FREE_SHARES 才是本次解禁量**（FREE_SHARES 是解禁后已流通股数），
// 万股→股、万元→元换算在此收口，落库后全链路统一为股/元。
func parseLiftRow(r DcRow) (LiftRow, bool, error) {
	sym := r.String("SECURITY_CODE")
	freeDate := r.Date("FREE_DATE")
	if sym == "" || freeDate == "" {
		return LiftRow{}, false, errors.New("缺少代码/解禁日必填字段")
	}
	if !corpActionStockRow(r.String("SECURITY_TYPE_CODE"), sym) {
		return LiftRow{}, false, nil
	}
	shares := r.Float("CURRENT_FREE_SHARES")
	if shares <= 0 {
		// 少数行 CURRENT 缺失但 ABLE 有值（口径同为「实际可上市流通量」），退一步取 ABLE；
		// 绝不退回 FREE_SHARES——那是解禁后流通总股数，会把规模放大数倍。
		shares = r.Float("ABLE_FREE_SHARES")
	}
	return LiftRow{
		Symbol:        sym,
		Name:          r.String("SECURITY_NAME_ABBR"),
		FreeDate:      freeDate,
		FreeShares:    round4(shares * 1e4),                     // 万股 → 股
		LiftMarketCap: round4(r.Float("LIFT_MARKET_CAP") * 1e4), // 万元 → 元
		FreeType:      r.String("FREE_SHARES_TYPE"),
		FreeRatio:     round4(r.Float("FREE_RATIO") * 100),  // 小数 → %
		TotalRatio:    round4(r.Float("TOTAL_RATIO") * 100), // 小数 → %
	}, true, nil
}

// GetIpoSubscriptions 拉取申购日落在 [from, to] 的新股申购。
func (e *EastMoneyAdapter) GetIpoSubscriptions(ctx context.Context, from, to string) ([]IpoRow, error) {
	var out []IpoRow
	_, err := dcPages(ctx, e, DataCenterQuery{
		ReportName:  corpActionReportIpo,
		Filter:      fmt.Sprintf("(APPLY_DATE>='%s')(APPLY_DATE<='%s')", from, to),
		SortColumns: "APPLY_DATE",
		SortTypes:   "1",
	}, "新股申购", func(r DcRow) error {
		row, ok, perr := parseIpoRow(r)
		if perr != nil {
			return fmt.Errorf("%w: 新股申购行结构无效: %v", ErrUpstream, perr)
		}
		if ok {
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseIpoRow 新股申购单行解析。ISSUE_PRICE 在询价完成前为 null（发行价未定），
// 此处保持 0 并由消费方如实展示「待定」——不用预估价 PREDICT_ISSUE_PRICE 冒充定价。
func parseIpoRow(r DcRow) (IpoRow, bool, error) {
	sym := r.String("SECURITY_CODE")
	applyDate := r.Date("APPLY_DATE")
	applyCode := r.String("APPLY_CODE")
	if sym == "" || applyDate == "" || applyCode == "" {
		return IpoRow{}, false, errors.New("缺少代码/申购日/申购代码必填字段")
	}
	if !corpActionStockRow(r.String("SECURITY_TYPE_CODE"), sym) {
		return IpoRow{}, false, nil
	}
	return IpoRow{
		Symbol:     sym,
		Name:       r.String("SECURITY_NAME"),
		ApplyCode:  applyCode,
		ApplyDate:  applyDate,
		IssuePrice: r.Float("ISSUE_PRICE"),
		ApplyUpper: r.Float("ONLINE_APPLY_UPPER"),
		PayDate:    r.Date("BALLOT_PAY_DATE"),
		BallotDate: r.Date("BALLOT_NUM_DATE"),
		ListDate:   r.Date("LISTING_DATE"),
		Board:      r.String("MARKET"),
	}, true, nil
}

// GetCbSubscriptions 拉取网上申购日落在 [from, to] 的可转债申购。
//
// 本报表是**可转债**清单（SECURITY_TYPE_CODE 若返回则为 060*），因此不套用 058 股票过滤；
// 把关方式是「必须有申购代码 + 正股代码可映射 A 股 secid」——正股不可识别的转债不进库
// （消费方要靠正股关联持仓/自选）。
func (e *EastMoneyAdapter) GetCbSubscriptions(ctx context.Context, from, to string) ([]CbRow, error) {
	var out []CbRow
	_, err := dcPages(ctx, e, DataCenterQuery{
		ReportName:  corpActionReportCb,
		Filter:      fmt.Sprintf("(PUBLIC_START_DATE>='%s')(PUBLIC_START_DATE<='%s')", from, to),
		SortColumns: "PUBLIC_START_DATE",
		SortTypes:   "1",
	}, "可转债申购", func(r DcRow) error {
		row, ok, perr := parseCbRow(r)
		if perr != nil {
			return fmt.Errorf("%w: 可转债申购行结构无效: %v", ErrUpstream, perr)
		}
		if ok {
			out = append(out, row)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// parseCbRow 可转债申购单行解析。申购代码取 CORRECODE（网上申购代码，与转债代码不同，
// 用户实际下单敲的是它）；申购日取 PUBLIC_START_DATE（网上申购起始日）。
func parseCbRow(r DcRow) (CbRow, bool, error) {
	code := r.String("SECURITY_CODE")
	applyDate := r.Date("PUBLIC_START_DATE")
	applyCode := r.String("CORRECODE")
	if code == "" || applyDate == "" || applyCode == "" {
		return CbRow{}, false, errors.New("缺少转债代码/申购日/申购代码必填字段")
	}
	stockCode := r.String("CONVERT_STOCK_CODE")
	if !cnAShareCode(stockCode) {
		return CbRow{}, false, nil // 正股不可识别：无法关联持仓/自选，不入库
	}
	return CbRow{
		Symbol:       code,
		Name:         r.String("SECURITY_NAME_ABBR"),
		ApplyCode:    applyCode,
		ApplyDate:    applyDate,
		IssuePrice:   r.Float("ISSUE_PRICE"),
		ListDate:     r.Date("LISTING_DATE"),
		StockCode:    stockCode,
		StockName:    r.String("SECURITY_SHORT_NAME"),
		Rating:       r.String("RATING"),
		IssueScaleYi: r.Float("ACTUAL_ISSUE_SCALE"),
	}, true, nil
}
