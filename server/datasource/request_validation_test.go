package datasource

import (
	"context"
	"errors"
	"testing"
)

func TestCNSymbolRejectsNonDigits(t *testing.T) {
	for _, symbol := range []string{"60000a", "6&x=10", "0?x=12", "000/01", "15a001"} {
		if _, ok := cnSecid(symbol); ok {
			t.Errorf("东财不得把非法代码拼入请求：%q", symbol)
		}
		if _, ok := sinaCNSymbol(symbol); ok {
			t.Errorf("新浪/腾讯不得把非法代码拼入请求：%q", symbol)
		}
	}
	for _, symbol := range []string{"600000", "000001", "300001", "510300", "159915", " 600000 "} {
		if _, ok := cnSecid(symbol); !ok {
			t.Errorf("合法股票/ETF 代码被拒绝：%q", symbol)
		}
		if _, ok := sinaCNSymbol(symbol); !ok {
			t.Errorf("合法股票/ETF 代码被拒绝：%q", symbol)
		}
	}
}

func TestCanceledMarketRequestDoesNotPoisonSourceHealth(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		for _, during := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			a := &fakeAdapter{name: "eastmoney", quote: func() (*Quote, error) {
				cancel()
				return nil, ctx.Err()
			}}
			m := NewManagerWithAdapters(a)
			if !during {
				cancel()
			}
			var err error
			if fresh {
				_, _, err = m.GetQuoteFresh(ctx, "cn", "600000", func(*Quote) bool { return true })
			} else {
				_, err = m.GetQuote(ctx, "cn", "600000")
			}
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Errorf("fresh=%v during=%v：应保留取消原因，实际 %v", fresh, during, err)
			}
			for _, stat := range m.HealthSnapshot() {
				if stat.Observed || stat.Samples != 0 {
					t.Errorf("用户取消不能被记为上游故障：%+v", stat)
				}
			}
		}
	}
}

func TestCanceledProbeDoesNotPoisonSourceHealth(t *testing.T) {
	ResetProbeLimiterForTest()
	t.Cleanup(ResetProbeLimiterForTest)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManagerWithAdapters(&probeFakeAdapter{name: "eastmoney", quote: func(context.Context) (*Quote, error) {
		cancel()
		return nil, errors.New("上游包装后的取消错误")
	}})
	result, err := m.Probe(ctx, "eastmoney", "quote", "cn")
	if !errors.Is(err, context.Canceled) || result.Code != "CANCELED" {
		t.Errorf("探测必须保留用户取消原因：result=%+v err=%v", result, err)
	}
	if stat := probeHealthRow(t, m, "eastmoney", "quote", "cn"); stat.Observed || stat.Samples != 0 {
		t.Errorf("管理员取消探测不能被记为上游故障：%+v", stat)
	}
}

func TestFinancialSourcesRejectInvalidSymbol(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, symbol := range []string{"6&x=10", `6000"1`, "6000/1"} {
		if _, err := GetEMAnnouncements(ctx, symbol, 20); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("公告非法代码未拒绝：%q %v", symbol, err)
		}
		if _, err := GetEMStockNews(ctx, symbol, 20); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("新闻非法代码未拒绝：%q %v", symbol, err)
		}
		if _, err := GetF10MainFinance(ctx, symbol); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("F10 非法代码未拒绝：%q %v", symbol, err)
		}
		if _, err := GetEMStatements(ctx, symbol); !errors.Is(err, ErrSymbolInvalid) {
			t.Errorf("财报非法代码未拒绝：%q %v", symbol, err)
		}
	}
}
