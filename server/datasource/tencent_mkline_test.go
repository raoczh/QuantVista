package datasource

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// mkline 真实响应精简 fixture（2026-07-09 实测茅台，qt 块删节保留结构）。
const min5Fixture = `{"code":0,"msg":"","data":{"sh600519":{"qt":{"sh600519":["1","贵州茅台","600519","1185.09"],"market":["..."]},"m5":[["202607090950","1182.10","1188.53","1188.88","1182.10","2041.00",{},"1.63"],["202607090955","1188.83","1187.12","1189.50","1185.00","556.00",{},"0.44"],["202607091000","1187.12","1191.00","1191.47","1187.12","652.00",{},"0.52"]],"prec":"1199.30"}}}`

// 非法代码：qt 空数组且无 m5 键（2026-07-09 实测 sh999999）。
const min5InvalidFixture = `{"code":0,"msg":"","data":{"sh999999":{"qt":{"sh999999":[],"market":["..."]}}}}`

// mkline m1 与 m5 同构；保留 prec 锚点与跨日行，消费层负责只取末日。
const min1Fixture = `{"code":0,"msg":"","data":{"sz000001":{"m1":[["202607241500","10.10","10.20","10.25","10.05","900",{},"0.1"],["202607270931","10.30","10.35","10.40","10.28","1200",{},"0.1"],["202607270932","10.35","10.32","10.38","10.30","800",{},"0.1"]],"prec":"10.18"}}}`

func TestParseMin5Response(t *testing.T) {
	bars, err := parseMin5Response([]byte(min5Fixture), "sh600519")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(bars) != 3 {
		t.Fatalf("期望 3 根, got %d", len(bars))
	}
	// 列序锚点：开、收、高、低（腾讯 kline 族惯例）——first 行手工核对。
	b := bars[0]
	if b.Time != "202607090950" {
		t.Errorf("Time = %s", b.Time)
	}
	if b.Open != 1182.10 || b.Close != 1188.53 || b.High != 1188.88 || b.Low != 1182.10 {
		t.Errorf("OHLC 列序错误: O=%v C=%v H=%v L=%v", b.Open, b.Close, b.High, b.Low)
	}
	if b.Volume != 2041 {
		t.Errorf("Volume = %d, 期望 2041（手）", b.Volume)
	}
	// 不变式：high ≥ max(open, close)、low ≤ min(open, close)。
	for i, x := range bars {
		if x.High < x.Open || x.High < x.Close || x.Low > x.Open || x.Low > x.Close {
			t.Errorf("第 %d 根 OHLC 不变式破坏: %+v", i, x)
		}
	}
}

func TestParseMinuteResponseM1(t *testing.T) {
	bars, prec, err := parseMinuteResponse([]byte(min1Fixture), "sz000001", minutePeriodM1)
	if err != nil {
		t.Fatalf("解析 m1 失败: %v", err)
	}
	if len(bars) != 3 || prec != 10.18 {
		t.Fatalf("m1/prec 错误: bars=%d prec=%v", len(bars), prec)
	}
	if bars[1].Time != "202607270931" || bars[1].Open != 10.30 || bars[1].Close != 10.35 || bars[1].Volume != 1200 {
		t.Fatalf("m1 列序/单位错误: %+v", bars[1])
	}

	withoutPrec := `{"code":0,"data":{"sz000001":{"m1":[["202607270931","10","10.1","10.2","9.9","1",{},"0"]]}}}`
	_, prec, err = parseMinuteResponse([]byte(withoutPrec), "sz000001", minutePeriodM1)
	if err != nil || prec != 0 {
		t.Fatalf("prec 缺失应如实返回 0 交由服务层回退: prec=%v err=%v", prec, err)
	}
}

func TestParseMin5ResponseNoData(t *testing.T) {
	if _, err := parseMin5Response([]byte(min5InvalidFixture), "sh999999"); !errors.Is(err, ErrNoData) {
		t.Errorf("非法代码应 ErrNoData, got %v", err)
	}
	// data 中无该代码键。
	if _, err := parseMin5Response([]byte(`{"code":0,"data":{}}`), "sh600519"); !errors.Is(err, ErrNoData) {
		t.Errorf("缺代码键应 ErrNoData, got %v", err)
	}
	// 上游错误码。
	if _, err := parseMin5Response([]byte(`{"code":-1,"msg":"param error","data":{}}`), "sh600519"); !errors.Is(err, ErrUpstream) {
		t.Errorf("code!=0 应 ErrUpstream, got %v", err)
	}
}

func TestParseMin5ResponseBadRows(t *testing.T) {
	// 坏行（列不足/时间非 12 位/价格为 0）逐一跳过，好行保留。
	raw := `{"code":0,"data":{"sz000001":{"m5":[
		["202607090935","10.45","10.46","10.47","10.43","100.00",{},"0.1"],
		["short","10.45","10.46","10.47","10.43","100.00",{},"0.1"],
		["202607090940","0","10.46","10.47","10.43","100.00",{},"0.1"],
		["202607090945"],
		["202607090950","10.45","10.46","10.47","10.43","0",{},"0"]
	]}}}`
	bars, err := parseMin5Response([]byte(raw), "sz000001")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(bars) != 2 {
		t.Fatalf("期望 2 根（坏行跳过、零量根保留）, got %d", len(bars))
	}
	if bars[1].Volume != 0 {
		t.Errorf("零成交根应保留（极冷门股某 5 分钟无成交是合法态）")
	}
}

// TestLiveMin5 真实接口冒烟（LIVE_INTRADAY=1 门控）。
func TestLiveMin5(t *testing.T) {
	if os.Getenv("LIVE_INTRADAY") == "" {
		t.Skip("需要 LIVE_INTRADAY=1")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ta := NewTencentAdapter()
	bars, err := ta.GetMin5Bars(ctx, "cn", "600519", 60)
	if err != nil {
		t.Fatalf("真实拉取失败: %v", err)
	}
	if len(bars) < 40 {
		t.Fatalf("60 根请求实际 %d 根（应接近 60）", len(bars))
	}
	t.Logf("真实 5 分钟线 %d 根: first=%s last=%s", len(bars), bars[0].Time, bars[len(bars)-1].Time)
}
