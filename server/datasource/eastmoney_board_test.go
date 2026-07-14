package datasource

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// 板块热度一页真实响应裁剪（2026-07-09 实测 m:90+t:2 行业板块）：
// 第 3 行领涨股字段为 "-"（缺失/停牌），家数字段照 emNum 容错。
const boardHeatFixtureArray = `{"rc":0,"data":{"total":496,"diff":[
{"f3":2.15,"f6":18234567890.0,"f12":"BK1036","f14":"半导体","f104":52,"f105":18,"f128":"中芯国际","f140":"688981"},
{"f3":-1.32,"f6":9876543210.0,"f12":"BK0447","f14":"银行","f104":8,"f105":24,"f128":"招商银行","f140":"600036"},
{"f3":0.0,"f6":1234567.0,"f12":"BK1201","f14":"某板块","f104":"-","f105":"-","f128":"-","f140":"-"}
]}}`

// 对象态 diff（{"0":{...}} 按数字键还原顺序）。
const boardHeatFixtureObject = `{"data":{"diff":{
"1":{"f3":-1.32,"f6":9876543210.0,"f12":"BK0447","f14":"银行","f104":8,"f105":24,"f128":"招商银行","f140":"600036"},
"0":{"f3":2.15,"f6":18234567890.0,"f12":"BK1036","f14":"半导体","f104":52,"f105":18,"f128":"中芯国际","f140":"688981"}
}}}`

// 成分股一页真实响应裁剪：第 3 行停牌（f2/f8 为 "-"）。
const boardStockFixtureArray = `{"data":{"diff":[
{"f2":98.76,"f3":3.21,"f6":5432109876.0,"f8":4.5,"f12":"688981","f14":"中芯国际","f20":780000000000.0,"f21":560000000000.0},
{"f2":12.34,"f3":-0.88,"f6":1234567890.0,"f8":1.2,"f12":"600584","f14":"长电科技","f20":88000000000.0,"f21":85000000000.0},
{"f2":"-","f3":"-","f6":"-","f8":"-","f12":"000988","f14":"华工科技","f20":45000000000.0,"f21":40000000000.0}
]}}`

func TestParseBoardHeatArray(t *testing.T) {
	rows, err := parseBoardHeat([]byte(boardHeatFixtureArray))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	r0 := rows[0]
	if r0.Code != "BK1036" || r0.Name != "半导体" || r0.ChangePct != 2.15 ||
		r0.Amount != 18234567890.0 || r0.Advances != 52 || r0.Declines != 18 ||
		r0.Leader != "中芯国际" || r0.LeaderCode != "688981" {
		t.Fatalf("row0 字段不符: %+v", r0)
	}
	// "-" 容错：家数 0、领涨股空串。
	r2 := rows[2]
	if r2.Advances != 0 || r2.Declines != 0 || r2.Leader != "-" && r2.Leader != "" {
		t.Fatalf("'-' 行容错异常: %+v", r2)
	}
}

func TestParseBoardHeatObject(t *testing.T) {
	rows, err := parseBoardHeat([]byte(boardHeatFixtureObject))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 || rows[0].Code != "BK1036" || rows[1].Code != "BK0447" {
		t.Fatalf("对象态顺序还原错误: %+v", rows)
	}
}

func TestParseBoardHeatEmpty(t *testing.T) {
	if _, err := parseBoardHeat([]byte(`{"data":{"diff":[]}}`)); !errors.Is(err, ErrNoData) {
		t.Fatalf("空 diff 应 ErrNoData, got %v", err)
	}
}

func TestParseBoardStocks(t *testing.T) {
	rows, err := parseBoardStocks([]byte(boardStockFixtureArray))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3（停牌行保留）", len(rows))
	}
	r0 := rows[0]
	if r0.Symbol != "688981" || r0.Name != "中芯国际" || r0.Price != 98.76 ||
		r0.ChangePct != 3.21 || r0.Amount != 5432109876.0 || r0.TurnoverRate != 4.5 ||
		r0.TotalCap != 780000000000.0 || r0.FloatCap != 560000000000.0 {
		t.Fatalf("row0 字段不符: %+v", r0)
	}
	// 停牌行：价格/换手 "-" → 0，市值仍保留，行不丢。
	r2 := rows[2]
	if r2.Symbol != "000988" || r2.Price != 0 || r2.TurnoverRate != 0 || r2.TotalCap != 45000000000.0 {
		t.Fatalf("停牌行容错异常: %+v", r2)
	}
	// 解析层不标 leader/gainer（service 层职责）。
	if r0.IsLeader || r0.IsTopGainer {
		t.Fatalf("解析层不应标注 leader/gainer: %+v", r0)
	}
}

// BK 码校验拒绝路径：非法 code 不触网、直接 ErrSymbolInvalid。
func TestGetBoardConstituentsRejectsBadCode(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		t.Fatal("非法 code 不应触网")
		return nil, 0, nil
	})
	for _, bad := range []string{"", "12345", "BK123", "BK12345", "b:BK1036&fs=x", "SZ1036"} {
		if _, err := e.GetBoardConstituents(context.Background(), bad, 50); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("code=%q 应 ErrSymbolInvalid, got %v", bad, err)
		}
		if _, err := e.GetBoardKline(context.Background(), bad, 120); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("kline code=%q 应 ErrSymbolInvalid, got %v", bad, err)
		}
	}
}

// kind 白名单拒绝路径。
func TestGetBoardHeatRejectsBadKind(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		t.Fatal("非法 kind 不应触网")
		return nil, 0, nil
	})
	if _, err := e.GetBoardHeat(context.Background(), "sector"); !errors.Is(err, ErrSymbolInvalid) {
		t.Errorf("非法 kind 应 ErrSymbolInvalid, got %v", err)
	}
}

// 板块指数日线复用个股 kline 解析（10 列无换手也不崩）。
func TestGetBoardKlineParse(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		// 板块指数样本：末列换手率有值 / 也可能缺，这里给 11 列。
		return []byte(`{"data":{"klines":["2026-07-08,100.0,102.5,103.0,99.5,120000,1.2e9,3.5,2.5,2.5,1.8","2026-07-09,102.5,101.0,103.2,100.8,110000,1.1e9,2.3,-1.46,-1.5,1.6"]}}`), 200, nil
	})
	bars, err := e.GetBoardKline(context.Background(), "BK1036", 120)
	if err != nil {
		t.Fatalf("kline: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	if bars[0].Open != 100.0 || bars[0].Close != 102.5 || bars[0].High != 103.0 || bars[0].Low != 99.5 {
		t.Fatalf("OHLC 列序错误: %+v", bars[0])
	}
	if bars[0].Source != "eastmoney" {
		t.Fatalf("Source 未标注: %+v", bars[0])
	}
}

// P3b：板块资金流复用 parseStockFundFlow（15 列与个股同构，Close=板块指数点位），
// 这里对拍列序 + 校验 secid=90.<code> 拼接与 BK 码拒绝路径。
func TestGetBoardFundFlow(t *testing.T) {
	e := NewEastMoneyAdapter()
	var gotURL string
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		gotURL = url
		return []byte(`{"data":{"klines":[
"2026-07-09,1230000000.0,-340000000.0,-890000000.0,450000000.0,780000000.0,3.21,-0.89,-2.32,1.17,2.04,1687.45,1.25,0.0,0.0",
"2026-07-10,-560000000.0,120000000.0,440000000.0,-260000000.0,-300000000.0,-1.55,0.33,1.22,-0.72,-0.83,1671.02,-0.97,0.0,0.0"
]}}`), 200, nil
	})
	bars, err := e.GetBoardFundFlow(context.Background(), "BK1036", 250)
	if err != nil {
		t.Fatalf("GetBoardFundFlow: %v", err)
	}
	if !strings.Contains(gotURL, "secid=90.BK1036") {
		t.Fatalf("secid 拼接错误: %s", gotURL)
	}
	if len(bars) != 2 {
		t.Fatalf("bars = %d, want 2", len(bars))
	}
	b0 := bars[0]
	if b0.TradeDate != "2026-07-09" || b0.MainNet != 1230000000.0 || b0.SmallNet != -340000000.0 ||
		b0.MediumNet != -890000000.0 || b0.LargeNet != 450000000.0 || b0.SuperNet != 780000000.0 ||
		b0.MainPct != 3.21 || b0.Close != 1687.45 || b0.ChangePct != 1.25 {
		t.Fatalf("15 列列序对拍失败: %+v", b0)
	}
}

func TestGetBoardFundFlowRejectsBadCode(t *testing.T) {
	e := NewEastMoneyAdapter()
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		t.Fatal("非法 code 不应触网")
		return nil, 0, nil
	})
	for _, bad := range []string{"", "BK123", "90.BK1036", "SZ1036"} {
		if _, err := e.GetBoardFundFlow(context.Background(), bad, 250); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("code=%q 应 ErrSymbolInvalid, got %v", bad, err)
		}
	}
}

// P3b：板块清单翻页（估值聚合的行业名→BK 码映射源）。
func TestGetBoardListPaged(t *testing.T) {
	e := NewEastMoneyAdapter()
	pages := map[string]string{
		"pn=1": `{"data":{"total":3,"diff":[{"f12":"BK1036","f14":"半导体"},{"f12":"BK0447","f14":"银行"}]}}`,
		"pn=2": `{"data":{"total":3,"diff":[{"f12":"BK0428","f14":"电力行业"}]}}`,
	}
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		for k, v := range pages {
			if strings.Contains(url, k+"&") {
				return []byte(v), 200, nil
			}
		}
		return []byte(`{"data":{"total":3,"diff":[]}}`), 200, nil
	})
	rows, err := e.GetBoardList(context.Background(), "industry")
	if err != nil {
		t.Fatalf("GetBoardList: %v", err)
	}
	if len(rows) != 3 || rows[0].Code != "BK1036" || rows[2].Name != "电力行业" {
		t.Fatalf("翻页聚合错误: %+v", rows)
	}
}

// 半截清单拒收：total=100 只拿到 2 条 → 整轮失败（映射缺一半会静默丢行业）。
func TestGetBoardListRejectsPartial(t *testing.T) {
	e := NewEastMoneyAdapter()
	calls := 0
	e.SetFetchForTest(func(ctx context.Context, url string, h map[string]string) ([]byte, int, error) {
		calls++
		if calls == 1 {
			return []byte(`{"data":{"total":100,"diff":[{"f12":"BK1036","f14":"半导体"},{"f12":"BK0447","f14":"银行"}]}}`), 200, nil
		}
		return []byte(`{"data":{"total":100,"diff":[]}}`), 200, nil
	})
	if _, err := e.GetBoardList(context.Background(), "industry"); err == nil {
		t.Fatal("半截清单应整轮拒收")
	}
	if _, err := e.GetBoardList(context.Background(), "sector"); !errors.Is(err, ErrSymbolInvalid) {
		t.Fatalf("非法 kind 应 ErrSymbolInvalid, got %v", err)
	}
}

// LIVE_BOARD=1 真实冒烟（push2his 本机常被限流，默认跳过；照 LIVE_MOOD/LIVE_INTRADAY 门控）。
func TestLiveBoard(t *testing.T) {
	if os.Getenv("LIVE_BOARD") != "1" {
		t.Skip("设 LIVE_BOARD=1 跑真实冒烟")
	}
	e := NewEastMoneyAdapter()
	heat, err := e.GetBoardHeat(context.Background(), "industry")
	if err != nil {
		t.Fatalf("GetBoardHeat: %v", err)
	}
	t.Logf("行业板块热度 %d 条，Top1=%s %.2f%% 成交额 %.1f 亿",
		len(heat), heat[0].Name, heat[0].ChangePct, heat[0].Amount/1e8)
	if len(heat) < 10 {
		t.Fatalf("行业板块数偏少 %d", len(heat))
	}
	code := heat[0].Code
	stocks, err := e.GetBoardConstituents(context.Background(), code, 50)
	if err != nil {
		t.Fatalf("GetBoardConstituents(%s): %v", code, err)
	}
	t.Logf("%s 成分股 %d 只，龙头=%s", code, len(stocks), stocks[0].Name)
	bars, err := e.GetBoardKline(context.Background(), code, 60)
	if err != nil {
		t.Fatalf("GetBoardKline(%s): %v", code, err)
	}
	t.Logf("%s 指数日线 %d 根，末根 %s 收 %.2f", code, len(bars), bars[len(bars)-1].TradeDate, bars[len(bars)-1].Close)
	// P3b：板块清单 + 板块资金流。
	list, err := e.GetBoardList(context.Background(), "industry")
	if err != nil {
		t.Fatalf("GetBoardList: %v", err)
	}
	t.Logf("行业板块清单 %d 个", len(list))
	flows, err := e.GetBoardFundFlow(context.Background(), code, 30)
	if err != nil {
		t.Fatalf("GetBoardFundFlow(%s): %v", code, err)
	}
	last := flows[len(flows)-1]
	t.Logf("%s 资金流 %d 根，末根 %s 主力 %.2f 亿 收 %.2f", code, len(flows), last.TradeDate, last.MainNet/1e8, last.Close)
}
