package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// ---------- 龙虎榜 ----------

func dcRowFrom(t *testing.T, raw string) DcRow {
	t.Helper()
	var m DcRow
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// parseLhbRow：股票行解析全字段；可转债（SECURITY_TYPE_CODE=060）与非股票类型码过滤。
func TestParseLhbRow(t *testing.T) {
	stock := dcRowFrom(t, `{"TRADE_DATE":"2026-07-07 00:00:00","SECURITY_CODE":"002185","SECURITY_NAME_ABBR":"华天科技",
		"SECURITY_TYPE_CODE":"058001001","CHANGE_TYPE":"137001002001001","EXPLANATION":"日涨幅偏离值达到7%的前5只证券",
		"EXPLAIN":"1家机构买入，成功率58.41%","CLOSE_PRICE":21.93,"CHANGE_RATE":9.9799,
		"BILLBOARD_NET_AMT":1576984146.49,"BILLBOARD_BUY_AMT":2255004980.49,"BILLBOARD_SELL_AMT":678020834,
		"BILLBOARD_DEAL_AMT":2933025814.49,"ACCUM_AMOUNT":11999102685,"DEAL_NET_RATIO":13.142517,
		"TURNOVERRATE":16.9334,"FREE_MARKET_CAP":72865753031.01}`)
	row, ok := parseLhbRow(stock)
	if !ok {
		t.Fatal("股票行应通过过滤")
	}
	if row.Symbol != "002185" || row.TradeDate != "2026-07-07" || row.ChangeType != "137001002001001" {
		t.Errorf("基础字段解析错误: %+v", row)
	}
	if row.Reason != "日涨幅偏离值达到7%的前5只证券" || row.Note == "" {
		t.Errorf("原因/附注解析错误: %q %q", row.Reason, row.Note)
	}
	if row.NetBuy != 1576984146.49 || row.TurnoverRate != 16.9334 {
		t.Errorf("金额字段解析错误: %+v", row)
	}

	// 可转债：类型码 060，必须过滤（代码 123273 六位数字但非股票）。
	bond := dcRowFrom(t, `{"SECURITY_CODE":"123273","SECURITY_TYPE_CODE":"060","TRADE_DATE":"2026-07-07 00:00:00"}`)
	if _, ok := parseLhbRow(bond); ok {
		t.Error("可转债行应被过滤")
	}
	// 类型码不是 058 前缀的一律过滤。
	weird := dcRowFrom(t, `{"SECURITY_CODE":"600000","SECURITY_TYPE_CODE":"061","TRADE_DATE":"2026-07-07 00:00:00"}`)
	if _, ok := parseLhbRow(weird); ok {
		t.Error("非 058 类型码应被过滤")
	}
}

func TestParseLhbOrgRow(t *testing.T) {
	r := dcRowFrom(t, `{"SECURITY_CODE":"000007","SECURITY_NAME_ABBR":"全新好","TRADE_DATE":"2026-07-07 00:00:00",
		"CLOSE_PRICE":11.59,"CHANGE_RATE":-10.0155,"BUY_TIMES":3,"SELL_TIMES":5,
		"BUY_AMT":27210526,"SELL_AMT":61502770.13,"NET_BUY_AMT":-34292244.13,"RATIO":-16.887,
		"EXPLANATION":"日振幅值达到15%的前5只证券"}`)
	row, ok := parseLhbOrgRow(r)
	if !ok {
		t.Fatal("A 股行应通过")
	}
	if row.BuyTimes != 3 || row.SellTimes != 5 || row.NetBuy != -34292244.13 {
		t.Errorf("机构字段解析错误: %+v", row)
	}
	// 北交所代码 cnSecid 不识别，过滤。
	bj := dcRowFrom(t, `{"SECURITY_CODE":"430047","TRADE_DATE":"2026-07-07 00:00:00"}`)
	if _, ok := parseLhbOrgRow(bj); ok {
		t.Error("北交所代码应被过滤")
	}
}

// ---------- 涨停池 ----------

// qdate 校验：上游对无效 date 静默回落到最近交易日数据，必须拒绝（防错日落库）。
func TestZTPoolQDateMismatch(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.fetch = func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		return []byte(`{"rc":0,"data":{"tc":1,"qdate":20260708,"pool":[{"c":"002841","n":"视源股份","p":43080,"zdp":10.01,"lbc":1,"fund":520606032,"zbc":0,"hybk":"消费电子","zttj":{"days":1,"ct":1}}]}}`), 200, nil
	}
	if _, err := e.GetZTPool(context.Background(), "20260707"); !errors.Is(err, ErrNoData) {
		t.Errorf("qdate 不符应报 ErrNoData，got %v", err)
	}
	// qdate 一致：正常解析（价格 ×1000 还原）。
	items, err := e.GetZTPool(context.Background(), "20260708")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Price != 43.08 || items[0].Streak != 1 || items[0].SealFund != 520606032 {
		t.Errorf("涨停池解析错误: %+v", items)
	}
}

// rc=102（空池/非交易日）应报 ErrNoData 而非解析错误。
func TestZTPoolEmpty(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.fetch = func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		return []byte(`{"rc":102,"data":null}`), 200, nil
	}
	if _, err := e.GetZBPool(context.Background(), "20260708"); !errors.Is(err, ErrNoData) {
		t.Errorf("空池应报 ErrNoData，got %v", err)
	}
}

func TestYesterdayZTPoolParse(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.fetch = func(ctx context.Context, url string, headers map[string]string) ([]byte, int, error) {
		return []byte(`{"rc":0,"data":{"tc":2,"qdate":20260708,"pool":[
			{"c":"000973","n":"佛塑科技","zdp":-9.945,"ylbc":1,"hs":11.58,"hybk":"塑料"},
			{"c":"001206","n":"依依股份","zdp":5.578,"ylbc":1,"hs":15.48,"hybk":"个护用品"}]}}`), 200, nil
	}
	items, err := e.GetYesterdayZTPool(context.Background(), "20260708")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].ChangePct != -9.945 || items[1].YStreak != 1 {
		t.Errorf("昨日涨停池解析错误: %+v", items)
	}
}

// ---------- 人气榜 ----------

// hisRc 负值=新上榜（实测 -3）；北交所前缀排除；rk<=0 丢弃。
func TestParsePopularity(t *testing.T) {
	raw := []byte(`{"status":0,"data":[
		{"sc":"SZ000725","rk":1,"hisRc":4},
		{"sc":"SZ002185","rk":4,"hisRc":-3},
		{"sc":"BJ920099","rk":5,"hisRc":2},
		{"sc":"SH600584","rk":0,"hisRc":1}]}`)
	rows, err := parsePopularity(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应保留 2 行（BJ 与 rk=0 排除），got %d", len(rows))
	}
	if rows[0].Symbol != "000725" || rows[0].PrevRank != 4 {
		t.Errorf("行 0 解析错误: %+v", rows[0])
	}
	if rows[1].PrevRank != -3 {
		t.Errorf("hisRc 负值应原样保留: %+v", rows[1])
	}
	if _, err := parsePopularity([]byte(`{"status":1,"data":[]}`)); !errors.Is(err, ErrNoData) {
		t.Error("status!=0 应报 ErrNoData")
	}
}

// ---------- 资金流 ----------

func TestParseFundFlowRank(t *testing.T) {
	raw := []byte(`{"data":{"total":5535,"diff":[
		{"f2":33.62,"f3":6.8,"f12":"000938","f14":"紫光股份","f62":1857327584.0,"f66":2463759408.0,
		 "f72":-606431824.0,"f78":-1178759744.0,"f84":-678567856.0,"f164":3271178928.0,"f174":4135225888.0,"f184":11.31}]}}`)
	rows, err := parseFundFlowRank(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MainNet != 1857327584 || rows[0].MainPct != 11.31 || rows[0].MainNet5d != 3271178928 {
		t.Errorf("排行解析错误: %+v", rows)
	}
	// 停牌行 f2="-" 容错。
	raw2 := []byte(`{"data":{"diff":[{"f12":"000001","f14":"平安银行","f2":"-","f62":100.0}]}}`)
	rows2, err := parseFundFlowRank(raw2)
	if err != nil || len(rows2) != 1 || rows2[0].Price != 0 {
		t.Errorf("停牌行容错失败: %v %+v", err, rows2)
	}
}

// 15 列 klines 解析（含收盘/涨跌幅）；空 klines 报 ErrNoData。
func TestParseStockFundFlow(t *testing.T) {
	raw := []byte(`{"data":{"klines":[
		"2026-07-07,-139833344.0,-315087.0,140148432.0,10627904.0,-150461248.0,-4.28,-0.01,4.29,0.33,-4.61,1188.80,-1.50,0.00,0.00",
		"2026-07-08,99341680.0,-326243.0,-99015424.0,49638880.0,49702800.0,3.23,-0.01,-3.22,1.62,1.62,1199.30,0.88,0.00,0.00"]}}`)
	bars, err := parseStockFundFlow(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(bars) != 2 {
		t.Fatalf("应 2 根，got %d", len(bars))
	}
	b := bars[0]
	if b.TradeDate != "2026-07-07" || b.MainNet != -139833344 || b.SuperNet != -150461248 ||
		b.MainPct != -4.28 || b.Close != 1188.80 || b.ChangePct != -1.50 {
		t.Errorf("资金流行解析错误: %+v", b)
	}
	if _, err := parseStockFundFlow([]byte(`{"data":{"klines":[]}}`)); !errors.Is(err, ErrNoData) {
		t.Error("空 klines 应报 ErrNoData")
	}
}
