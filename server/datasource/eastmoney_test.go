package datasource

import (
	"errors"
	"testing"
)

// 用抓取的真实响应做 table-driven 解析测试，防上游字段漂移后无感知崩溃。

func TestParseBreadth(t *testing.T) {
	// 2026-07-01 真实响应片段。
	body := []byte(`{"rc":0,"data":{"qdate":20260701,"fenbu":[{"-1":280},{"-10":4},{"-11":1},{"-2":189},{"-3":159},{"-4":101},{"-5":79},{"-6":52},{"-7":21},{"-8":17},{"-9":7},{"0":25},{"1":495},{"10":80},{"11":134},{"2":783},{"3":929},{"4":878},{"5":509},{"6":252},{"7":171},{"8":84},{"9":52}]}}`)
	b, err := parseBreadth(body)
	if err != nil {
		t.Fatalf("parseBreadth 出错: %v", err)
	}
	if b.Advances != 4367 {
		t.Errorf("上涨家数 = %d, 期望 4367", b.Advances)
	}
	if b.Declines != 910 {
		t.Errorf("下跌家数 = %d, 期望 910", b.Declines)
	}
	if b.Unchanged != 25 {
		t.Errorf("平盘家数 = %d, 期望 25", b.Unchanged)
	}
	if b.LimitUp != 134 {
		t.Errorf("涨停家数 = %d, 期望 134", b.LimitUp)
	}
	if b.LimitDown != 1 {
		t.Errorf("跌停家数 = %d, 期望 1", b.LimitDown)
	}
	if b.TradeDate != "2026-07-01" {
		t.Errorf("交易日 = %q, 期望 2026-07-01", b.TradeDate)
	}
}

func TestParseBreadthEmpty(t *testing.T) {
	if _, err := parseBreadth([]byte(`{"rc":0,"data":{"fenbu":[]}}`)); !errors.Is(err, ErrNoData) {
		t.Errorf("空 fenbu 应返回 ErrNoData, 得到 %v", err)
	}
	if _, err := parseBreadth([]byte(`not json`)); !errors.Is(err, ErrUpstream) {
		t.Errorf("非法 JSON 应返回 ErrUpstream, 得到 %v", err)
	}
}

func TestParseMarketFundFlow(t *testing.T) {
	// 2026-07-01 真实响应：date,主力,小单,中单,大单,超大单。
	body := []byte(`{"rc":0,"data":{"klines":["2026-06-30,-1.0,2.0,3.0,4.0,5.0","2026-07-01,-27360034816.0,21700202496.0,5659836416.0,-7116611584.0,-20243423232.0"]}}`)
	f, err := parseMarketFundFlow(body)
	if err != nil {
		t.Fatalf("parseMarketFundFlow 出错: %v", err)
	}
	if f.TradeDate != "2026-07-01" { // 取最新一行
		t.Errorf("交易日 = %q, 期望 2026-07-01", f.TradeDate)
	}
	if f.MainNet != -27360034816.0 {
		t.Errorf("主力净额 = %v, 期望 -27360034816", f.MainNet)
	}
	// 主力 = 大单 + 超大单（业务不变式校验）。
	if got := f.LargeNet + f.SuperNet; got != f.MainNet {
		t.Errorf("大单+超大单 = %v, 主力 = %v，应相等", got, f.MainNet)
	}
	if f.SmallNet != 21700202496.0 || f.MediumNet != 5659836416.0 {
		t.Errorf("小单/中单解析错误: small=%v medium=%v", f.SmallNet, f.MediumNet)
	}
}

func TestParseMarketFundFlowEmpty(t *testing.T) {
	if _, err := parseMarketFundFlow([]byte(`{"data":{"klines":[]}}`)); !errors.Is(err, ErrNoData) {
		t.Errorf("空 klines 应返回 ErrNoData, 得到 %v", err)
	}
}

// TestCNSymbolMapping 代码映射：个股沪深 + 场内基金（深 15/16/18 放行），可转债（10/11）拒绝。
func TestCNSymbolMapping(t *testing.T) {
	cases := []struct {
		symbol    string
		wantSecid string // "" 表示应 false
		wantSina  string
	}{
		{"600000", "1.600000", "sh600000"}, // 沪主板
		{"000001", "0.000001", "sz000001"}, // 深主板
		{"510300", "1.510300", "sh510300"}, // 沪 ETF（5 开头，天然可用）
		{"588000", "1.588000", "sh588000"}, // 科创50 ETF（沪）
		{"159915", "0.159915", "sz159915"}, // 深 ETF（15 开头，本次放行）
		{"160106", "0.160106", "sz160106"}, // 深 LOF（16）
		{"184688", "0.184688", "sz184688"}, // 封闭基金（18）
		{"110038", "", ""},                 // 沪可转债（11）——不得误当基金放行
		{"123120", "", ""},                 // 深可转债（12）
		{"12345", "", ""},                  // 长度非 6
	}
	for _, tc := range cases {
		gotSecid, ok := cnSecid(tc.symbol)
		if tc.wantSecid == "" {
			if ok {
				t.Errorf("cnSecid(%s) 应 false，得到 %q", tc.symbol, gotSecid)
			}
		} else if !ok || gotSecid != tc.wantSecid {
			t.Errorf("cnSecid(%s) = %q,%v，期望 %q,true", tc.symbol, gotSecid, ok, tc.wantSecid)
		}
		gotSina, ok := sinaCNSymbol(tc.symbol)
		if tc.wantSina == "" {
			if ok {
				t.Errorf("sinaCNSymbol(%s) 应 false，得到 %q", tc.symbol, gotSina)
			}
		} else if !ok || gotSina != tc.wantSina {
			t.Errorf("sinaCNSymbol(%s) = %q,%v，期望 %q,true", tc.symbol, gotSina, ok, tc.wantSina)
		}
	}
}
