package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fixture 均为 2026-07-28 对 datacenter 四张报表的真实响应节选（curl 实测原样保留，
// 仅删去与解析无关的长文本字段）。改动这些常量前先确认上游是否真的变了。

// 分红：实施分配 + 纯派息（送转为 null）。
const bonusFixtureCash = `{"SECUCODE":"600675.SH","SECURITY_NAME_ABBR":"中华企业","SECURITY_CODE":"600675",
"BONUS_IT_RATIO":null,"BONUS_RATIO":null,"IT_RATIO":null,"PRETAX_BONUS_RMB":0.059,
"PLAN_NOTICE_DATE":"2026-03-25 00:00:00","EQUITY_RECORD_DATE":"2026-08-03 00:00:00",
"EX_DIVIDEND_DATE":"2026-08-04 00:00:00","REPORT_DATE":"2025-12-31 00:00:00",
"ASSIGN_PROGRESS":"实施分配","IMPL_PLAN_PROFILE":"10派0.059元(含税,扣税后0.0531元)",
"NOTICE_DATE":"2026-07-28 00:00:00","DIVIDENT_RATIO":0.002731481481,"TOTAL_SHARES":6046135331}`

// 分红：送转 + 派息俱全（10 送 2 转 3 派 1.5）。
const bonusFixtureBonus = `{"SECURITY_CODE":"002998","SECURITY_NAME_ABBR":"优彩资源",
"BONUS_RATIO":2,"IT_RATIO":3,"PRETAX_BONUS_RMB":1.5,
"EQUITY_RECORD_DATE":"2026-07-31 00:00:00","EX_DIVIDEND_DATE":"2026-08-03 00:00:00",
"REPORT_DATE":"2026-06-30 00:00:00","NOTICE_DATE":"2026-07-24 00:00:00",
"ASSIGN_PROGRESS":"实施分配","IMPL_PLAN_PROFILE":"10转3派1.50元(含税)","DIVIDENT_RATIO":0.02743484225}`

// 分红：预案阶段（除权日未定，EX_DIVIDEND_DATE 为 null）——必须放行不能当坏行丢掉。
const bonusFixturePlan = `{"SECURITY_CODE":"000001","SECURITY_NAME_ABBR":"平安银行",
"BONUS_RATIO":null,"IT_RATIO":null,"PRETAX_BONUS_RMB":2.46,"EX_DIVIDEND_DATE":null,
"EQUITY_RECORD_DATE":null,"REPORT_DATE":"2026-06-30 00:00:00","NOTICE_DATE":"2026-07-20 00:00:00",
"ASSIGN_PROGRESS":"董事会预案","IMPL_PLAN_PROFILE":"10派2.46元(含税)","DIVIDENT_RATIO":0.0212}`

// 分红：B 股代码（900901），cnSecid 不识别 → 过滤。
const bonusFixtureBShare = `{"SECURITY_CODE":"900901","SECURITY_NAME_ABBR":"某B股",
"PRETAX_BONUS_RMB":0.1,"REPORT_DATE":"2025-12-31 00:00:00","EX_DIVIDEND_DATE":"2026-08-04 00:00:00"}`

// 解禁：股票行（058001001）。关键验算锚点全部保留。
const liftFixtureStock = `{"SECUCODE":"600626.SH","SECURITY_CODE":"600626","FREE_DATE":"2026-07-28 00:00:00",
"CURRENT_FREE_SHARES":21276.5957,"LIFT_MARKET_CAP":69361.701982,
"FREE_SHARES_TYPE":"定向增发机构配售股份","FREE_SHARES":132074.4667,"NON_FREE_SHARES":0,
"FREE_RATIO":0.192030726836,"MARKET_TYPE":"sh","SECURITY_NAME_ABBR":"申达股份","NEW":3.26,
"TOTAL_RATIO":0.161095450405,"ABLE_FREE_SHARES":21276.5957,"SECURITY_TYPE_CODE":"058001001"}`

// 解禁：科创板股票行（058 前缀同样放行）。
const liftFixtureKcb = `{"SECURITY_CODE":"688785","FREE_DATE":"2026-07-28 00:00:00",
"CURRENT_FREE_SHARES":81.6783,"LIFT_MARKET_CAP":24622.740318,"FREE_SHARES_TYPE":"首发机构配售股份",
"FREE_SHARES":1354.4448,"NON_FREE_SHARES":5415.724,"FREE_RATIO":0.064173829214,
"MARKET_TYPE":"kcb","SECURITY_NAME_ABBR":"恒运昌","NEW":301.46,"TOTAL_RATIO":0.012064440698,
"ABLE_FREE_SHARES":81.6783,"SECURITY_TYPE_CODE":"058001001"}`

// 解禁：可转债行（060 类型码）——必须被 058 过滤挡住。
const liftFixtureBond = `{"SECURITY_CODE":"113050","FREE_DATE":"2026-07-28 00:00:00",
"CURRENT_FREE_SHARES":500,"LIFT_MARKET_CAP":50000,"FREE_SHARES_TYPE":"可转债转股",
"SECURITY_NAME_ABBR":"某转债","SECURITY_TYPE_CODE":"060001001"}`

// 解禁：CURRENT 缺失但 ABLE 有值 → 退一步取 ABLE（绝不退回 FREE_SHARES）。
const liftFixtureAbleOnly = `{"SECURITY_CODE":"300750","FREE_DATE":"2026-08-10 00:00:00",
"CURRENT_FREE_SHARES":null,"ABLE_FREE_SHARES":1000,"FREE_SHARES":50000,"NON_FREE_SHARES":0,
"LIFT_MARKET_CAP":20000,"FREE_RATIO":0.0204,"TOTAL_RATIO":0.02,
"SECURITY_NAME_ABBR":"宁德时代","SECURITY_TYPE_CODE":"058001001"}`

// 新股：已受理未定价（ISSUE_PRICE 为 null）。
const ipoFixtureNoPrice = `{"SECUCODE":"301717.SZ","SECURITY_CODE":"301717","APPLY_CODE":"301717",
"APPLY_DATE":"2026-07-31 00:00:00","LISTING_DATE":null,"BALLOT_PAY_DATE":"2026-08-04 00:00:00",
"BALLOT_NUM_DATE":"2026-08-04 00:00:00","ISSUE_PRICE":null,"PREDICT_ISSUE_PRICE":18.88,
"ONLINE_APPLY_UPPER":5000,"SECURITY_NAME":"超纯应材","MARKET":"深交所创业板",
"SECURITY_TYPE_CODE":"058001001","TOP_APPLY_MARKETCAP":5}`

// 新股：已定价 + 已定上市日（沪市申购代码与股票代码不同）。
const ipoFixturePriced = `{"SECURITY_CODE":"603123","APPLY_CODE":"732123",
"APPLY_DATE":"2026-08-05 00:00:00","LISTING_DATE":"2026-08-15 00:00:00",
"BALLOT_PAY_DATE":"2026-08-07 00:00:00","BALLOT_NUM_DATE":"2026-08-07 00:00:00",
"ISSUE_PRICE":25.6,"ONLINE_APPLY_UPPER":16000,"SECURITY_NAME":"某新股",
"MARKET":"上交所主板","SECURITY_TYPE_CODE":"058001001"}`

// 可转债：申购代码 CORRECODE 与转债代码不同，正股可识别。
const cbFixtureOK = `{"SECURITY_CODE":"113709","SECUCODE":"113709.SH","SECURITY_NAME_ABBR":"振26转债",
"LISTING_DATE":null,"CONVERT_STOCK_CODE":"603067","BOND_EXPIRE":"6","RATING":"AA",
"ACTUAL_ISSUE_SCALE":8.78,"ISSUE_PRICE":100,"PAR_VALUE":100,
"PUBLIC_START_DATE":"2026-07-30 00:00:00","CORRECODE":"754067","CORRECODE_NAME_ABBR":"振26发债",
"SECURITY_SHORT_NAME":"振华股份"}`

// 可转债：正股代码不可识别（港股/空）→ 过滤（消费方要靠正股关联持仓）。
const cbFixtureBadStock = `{"SECURITY_CODE":"113710","SECURITY_NAME_ABBR":"某转债",
"CONVERT_STOCK_CODE":"","RATING":"A+","ISSUE_PRICE":100,
"PUBLIC_START_DATE":"2026-07-30 00:00:00","CORRECODE":"754068"}`

func mustDcRow(t *testing.T, raw string) DcRow {
	t.Helper()
	r, err := ParseDcRow(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("fixture 反序列化失败: %v", err)
	}
	return r
}

func TestParseCorpActionRow(t *testing.T) {
	row, ok, err := parseCorpActionRow(mustDcRow(t, bonusFixtureCash))
	if err != nil || !ok {
		t.Fatalf("纯派息行应解析成功: ok=%v err=%v", ok, err)
	}
	if row.Symbol != "600675" || row.Name != "中华企业" || row.Market != "cn" {
		t.Fatalf("基础字段错: %+v", row)
	}
	if row.ExDate != "2026-08-04" || row.RecordDate != "2026-08-03" ||
		row.ReportDate != "2025-12-31" || row.PlanNoticeDate != "2026-03-25" ||
		row.NoticeDate != "2026-07-28" {
		t.Fatalf("日期应截 10 位: %+v", row)
	}
	if row.BonusRatio != 0 || row.TransferRatio != 0 {
		t.Fatalf("null 送转应为 0: %+v", row)
	}
	if row.DividendPretax != 0.059 {
		t.Fatalf("每 10 股派息应原值落库: %v", row.DividendPretax)
	}
	// 上游 0.002731481481 是小数，×100 后按 4 位收敛 = 0.2731。
	if row.DividendYield != 0.2731 {
		t.Fatalf("股息率应转百分比数值并 round4，got %v", row.DividendYield)
	}
	if row.Progress != "实施分配" || row.PlanProfile != "10派0.059元(含税,扣税后0.0531元)" {
		t.Fatalf("进度/方案描述错: %+v", row)
	}

	bonus, ok, err := parseCorpActionRow(mustDcRow(t, bonusFixtureBonus))
	if err != nil || !ok {
		t.Fatalf("送转行应解析成功: ok=%v err=%v", ok, err)
	}
	if bonus.BonusRatio != 2 || bonus.TransferRatio != 3 || bonus.DividendPretax != 1.5 {
		t.Fatalf("送转/派息比例错（每 10 股口径原值）: %+v", bonus)
	}

	// 预案阶段无除权日：必须放行（唯一键靠 report_date + 空 ex_date），不是坏行。
	plan, ok, err := parseCorpActionRow(mustDcRow(t, bonusFixturePlan))
	if err != nil || !ok {
		t.Fatalf("预案行应放行: ok=%v err=%v", ok, err)
	}
	if plan.ExDate != "" || plan.Progress != "董事会预案" {
		t.Fatalf("预案行除权日应为空: %+v", plan)
	}

	// B 股：cnSecid 不识别 → 过滤（ok=false 且非错误）。
	if _, ok, err := parseCorpActionRow(mustDcRow(t, bonusFixtureBShare)); ok || err != nil {
		t.Fatalf("B 股应被过滤且不报错: ok=%v err=%v", ok, err)
	}

	// 缺报告期 → 结构无效（唯一键缺一半，不能静默入库）。
	if _, _, err := parseCorpActionRow(mustDcRow(t, `{"SECURITY_CODE":"600000"}`)); err == nil {
		t.Fatal("缺报告期应报结构无效")
	}
}

func TestParseLiftRow(t *testing.T) {
	row, ok, err := parseLiftRow(mustDcRow(t, liftFixtureStock))
	if err != nil || !ok {
		t.Fatalf("股票解禁行应解析成功: ok=%v err=%v", ok, err)
	}
	if row.Symbol != "600626" || row.Name != "申达股份" || row.FreeDate != "2026-07-28" {
		t.Fatalf("基础字段错: %+v", row)
	}
	// **本次解禁量取 CURRENT_FREE_SHARES**（万股→股）；错取 FREE_SHARES 会得到 13.2 亿股。
	if row.FreeShares != 212765957 {
		t.Fatalf("解禁股数应为 CURRENT_FREE_SHARES×1e4=212765957，got %v（若为 1320744667 说明错取了 FREE_SHARES）", row.FreeShares)
	}
	if row.LiftMarketCap != 693617019.82 {
		t.Fatalf("解禁市值应为万元×1e4: %v", row.LiftMarketCap)
	}
	// 上游验算锚点：解禁股数 × 现价 ≈ 解禁市值（容差 1 元）。
	if diff := row.FreeShares*3.26 - row.LiftMarketCap; diff > 1 || diff < -1 {
		t.Fatalf("股数×现价与市值不自洽，差 %v", diff)
	}
	if row.FreeRatio != 19.2031 || row.TotalRatio != 16.1095 {
		t.Fatalf("比例应转百分比数值并 round4: ratio=%v total=%v", row.FreeRatio, row.TotalRatio)
	}
	if row.FreeType != "定向增发机构配售股份" {
		t.Fatalf("解禁类型错: %q", row.FreeType)
	}

	kcb, ok, err := parseLiftRow(mustDcRow(t, liftFixtureKcb))
	if err != nil || !ok {
		t.Fatalf("科创板行应解析成功: ok=%v err=%v", ok, err)
	}
	if kcb.FreeShares != 816783 {
		t.Fatalf("科创板解禁股数错: %v", kcb.FreeShares)
	}

	// 058 过滤：可转债（060）必须被挡住且不报错。
	if _, ok, err := parseLiftRow(mustDcRow(t, liftFixtureBond)); ok || err != nil {
		t.Fatalf("060 可转债行应被 058 过滤: ok=%v err=%v", ok, err)
	}

	// CURRENT 缺失退 ABLE，绝不退 FREE_SHARES（后者 50000 万股 = 5 亿股）。
	able, ok, err := parseLiftRow(mustDcRow(t, liftFixtureAbleOnly))
	if err != nil || !ok {
		t.Fatalf("ABLE 兜底行应解析成功: ok=%v err=%v", ok, err)
	}
	if able.FreeShares != 1e7 {
		t.Fatalf("应取 ABLE_FREE_SHARES×1e4=10000000，got %v", able.FreeShares)
	}

	// 缺解禁日 → 结构无效。
	if _, _, err := parseLiftRow(mustDcRow(t, `{"SECURITY_CODE":"600000","SECURITY_TYPE_CODE":"058001001"}`)); err == nil {
		t.Fatal("缺解禁日应报结构无效")
	}
}

func TestParseIpoRow(t *testing.T) {
	row, ok, err := parseIpoRow(mustDcRow(t, ipoFixtureNoPrice))
	if err != nil || !ok {
		t.Fatalf("新股行应解析成功: ok=%v err=%v", ok, err)
	}
	if row.Symbol != "301717" || row.ApplyCode != "301717" || row.Name != "超纯应材" {
		t.Fatalf("基础字段错: %+v", row)
	}
	if row.ApplyDate != "2026-07-31" || row.PayDate != "2026-08-04" || row.BallotDate != "2026-08-04" {
		t.Fatalf("日期字段错: %+v", row)
	}
	// 未定价：保持 0，绝不用 PREDICT_ISSUE_PRICE(18.88) 冒充定价。
	if row.IssuePrice != 0 {
		t.Fatalf("未定价应为 0（不得取预估价）: %v", row.IssuePrice)
	}
	if row.ApplyUpper != 5000 || row.Board != "深交所创业板" {
		t.Fatalf("申购上限/板块错: %+v", row)
	}
	if row.ListDate != "" {
		t.Fatalf("上市日未定应为空: %q", row.ListDate)
	}

	priced, ok, err := parseIpoRow(mustDcRow(t, ipoFixturePriced))
	if err != nil || !ok {
		t.Fatalf("已定价行应解析成功: ok=%v err=%v", ok, err)
	}
	if priced.IssuePrice != 25.6 || priced.ApplyCode != "732123" || priced.ListDate != "2026-08-15" {
		t.Fatalf("已定价行字段错: %+v", priced)
	}
	if priced.ApplyCode == priced.Symbol {
		t.Fatal("沪市申购代码不应等于股票代码（用户下单敲的是申购代码）")
	}

	// 缺申购代码 → 结构无效。
	if _, _, err := parseIpoRow(mustDcRow(t,
		`{"SECURITY_CODE":"301717","APPLY_DATE":"2026-07-31 00:00:00","SECURITY_TYPE_CODE":"058001001"}`)); err == nil {
		t.Fatal("缺申购代码应报结构无效")
	}
}

func TestParseCbRow(t *testing.T) {
	row, ok, err := parseCbRow(mustDcRow(t, cbFixtureOK))
	if err != nil || !ok {
		t.Fatalf("可转债行应解析成功: ok=%v err=%v", ok, err)
	}
	if row.Symbol != "113709" || row.Name != "振26转债" {
		t.Fatalf("转债代码/简称错: %+v", row)
	}
	// 申购代码取 CORRECODE，与转债代码不同——这是用户实际敲的代码。
	if row.ApplyCode != "754067" {
		t.Fatalf("申购代码应取 CORRECODE: %q", row.ApplyCode)
	}
	if row.ApplyDate != "2026-07-30" {
		t.Fatalf("申购日应取 PUBLIC_START_DATE: %q", row.ApplyDate)
	}
	if row.StockCode != "603067" || row.StockName != "振华股份" {
		t.Fatalf("正股字段错: %+v", row)
	}
	if row.Rating != "AA" || row.IssuePrice != 100 || row.IssueScaleYi != 8.78 {
		t.Fatalf("评级/发行价/规模错: %+v", row)
	}

	// 正股不可识别 → 过滤（无法关联持仓/自选）。
	if _, ok, err := parseCbRow(mustDcRow(t, cbFixtureBadStock)); ok || err != nil {
		t.Fatalf("正股不可识别应被过滤: ok=%v err=%v", ok, err)
	}

	// 缺申购日 → 结构无效。
	if _, _, err := parseCbRow(mustDcRow(t,
		`{"SECURITY_CODE":"113709","CORRECODE":"754067","CONVERT_STOCK_CODE":"603067"}`)); err == nil {
		t.Fatal("缺申购日应报结构无效")
	}
}

// dcFixtureServer 用假 datacenter 服务端跑通「网关 → 翻页 → 过滤 → 结构体」全链路，
// 顺带断言查询参数（reportName/filter）确实按预期拼装。
func dcFixtureServer(t *testing.T, wantReport string, rows ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("reportName"); got != wantReport {
			t.Errorf("reportName 应为 %s，got %s", wantReport, got)
		}
		data := "[" + joinRaw(rows) + "]"
		fmt.Fprintf(w, `{"success":true,"code":0,"result":{"pages":1,"count":%d,"data":%s}}`, len(rows), data)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func joinRaw(rows []string) string {
	out := ""
	for i, r := range rows {
		if i > 0 {
			out += ","
		}
		out += r
	}
	return out
}

func TestCorpActionEndToEnd(t *testing.T) {
	oldBase, oldInterval := dcBaseURL, dcMinInterval
	dcMinInterval = 0
	defer func() { dcBaseURL, dcMinInterval = oldBase, oldInterval }()
	e := &EastMoneyAdapter{}

	// 解禁：两只股票 + 一只可转债 → 只应留下两只股票。
	dcBaseURL = dcFixtureServer(t, corpActionReportLift, liftFixtureStock, liftFixtureBond, liftFixtureKcb).URL
	lifts, err := e.GetLiftReleases(context.Background(), "2026-07-28", "2026-09-26")
	if err != nil {
		t.Fatalf("解禁拉取失败: %v", err)
	}
	if len(lifts) != 2 {
		t.Fatalf("058 过滤后应剩 2 行，got %d: %+v", len(lifts), lifts)
	}
	for _, l := range lifts {
		if l.Symbol == "113050" {
			t.Fatal("可转债不应出现在解禁结果里")
		}
	}

	// 分红：B 股行被过滤。
	dcBaseURL = dcFixtureServer(t, corpActionReportBonus, bonusFixtureCash, bonusFixtureBShare).URL
	actions, err := e.GetCorpActions(context.Background(), "2026-04-29")
	if err != nil {
		t.Fatalf("分红拉取失败: %v", err)
	}
	if len(actions) != 1 || actions[0].Symbol != "600675" {
		t.Fatalf("分红结果应只剩 A 股 1 行: %+v", actions)
	}

	// 新股 / 可转债。
	dcBaseURL = dcFixtureServer(t, corpActionReportIpo, ipoFixtureNoPrice, ipoFixturePriced).URL
	ipos, err := e.GetIpoSubscriptions(context.Background(), "2026-07-28", "2026-09-26")
	if err != nil || len(ipos) != 2 {
		t.Fatalf("新股结果错: n=%d err=%v", len(ipos), err)
	}
	dcBaseURL = dcFixtureServer(t, corpActionReportCb, cbFixtureOK, cbFixtureBadStock).URL
	cbs, err := e.GetCbSubscriptions(context.Background(), "2026-07-28", "2026-09-26")
	if err != nil || len(cbs) != 1 || cbs[0].ApplyCode != "754067" {
		t.Fatalf("可转债结果错: %+v err=%v", cbs, err)
	}
}

// TestCorpActionNoData 窗口内无数据（datacenter 9201）应回 ErrNoData 而非空切片错觉。
func TestCorpActionNoData(t *testing.T) {
	oldBase, oldInterval := dcBaseURL, dcMinInterval
	dcMinInterval = 0
	defer func() { dcBaseURL, dcMinInterval = oldBase, oldInterval }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"success":false,"code":9201,"message":"返回数据为空","result":null}`)
	}))
	defer srv.Close()
	dcBaseURL = srv.URL
	e := &EastMoneyAdapter{}
	if _, err := e.GetLiftReleases(context.Background(), "2026-07-28", "2026-09-26"); !errors.Is(err, ErrNoData) {
		t.Fatalf("空窗口应回 ErrNoData，got %v", err)
	}
	if _, err := e.GetIpoSubscriptions(context.Background(), "2026-07-28", "2026-09-26"); !errors.Is(err, ErrNoData) {
		t.Fatalf("空窗口应回 ErrNoData，got %v", err)
	}
}
