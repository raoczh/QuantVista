package datasource

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

const sinaIndustryFixture = `var S_Finance_bankuai_sinaindustry = {"new_glass":"new_glass,Glass,19,13,0.05,0.4128,323075688,3146739794,sh600293,9.89,4,0.36,Glass Leader","new_bank":"new_bank,Banks,42,10,-0.1,-1.0,1000000,5000000000,sh600000,1,10,0.1,Bank Leader"};`

func TestSinaBoardHeatKeepsUnitsClassificationAndMissingCounts(test *testing.T) {
	rows, err := parseSinaBoardHeat([]byte(sinaIndustryFixture), "industry")
	if err != nil || len(rows) != 2 {
		test.Fatalf("rows=%+v err=%v", rows, err)
	}
	if rows[0].Code != "sina:new_bank" || rows[0].Amount != 5000000000 || rows[0].ChangePct != -1 ||
		rows[1].MemberCount != 19 || rows[1].LeaderCode != "600293" || rows[1].Source != "sina" || rows[1].CountsAvailable {
		test.Fatalf("classification, amount units, sorting or missing counts changed: %+v", rows)
	}
	for _, invalid := range []string{
		strings.Replace(sinaIndustryFixture, "0.4128", "NaN", 1),
		strings.Replace(sinaIndustryFixture, "3146739794", "-1", 1),
		strings.Replace(sinaIndustryFixture, "new_glass,Glass", "new_other,Glass", 1),
		strings.Replace(sinaIndustryFixture, "var S_Finance_bankuai_sinaindustry", "var injected", 1),
		sinaIndustryFixture + "anotherStatement()",
	} {
		if rows, err := parseSinaBoardHeat([]byte(invalid), "industry"); !errors.Is(err, ErrUpstream) || rows != nil {
			test.Fatalf("invalid aggregate must not become partial success: %+v %v", rows, err)
		}
	}
}

func TestSinaBoardConstituentsUseDatedQuotesNotUndatedNodePrices(test *testing.T) {
	requests := 0
	withSinaTestHTTP(test, func(request *http.Request) (string, int, error) {
		requests++
		if request.URL.Host == "hq.sinajs.cn" {
			return sinaSpotTestQuote("sh600000", nil), http.StatusOK, nil
		}
		query := request.URL.Query()
		if query.Get("node") != "new_bank" || query.Get("num") != "1" || query.Get("page") != "1" {
			test.Fatalf("unexpected constituent query: %s", request.URL)
		}
		return strings.Replace(sinaSpotTestPage(0, 1), `"per":12.5`, `"mktcap":100,"nmc":80,"per":12.5`, 1), http.StatusOK, nil
	})
	rows, err := NewSinaAdapter().GetBoardConstituents(test.Context(), "sina:new_bank", 1)
	if err != nil || len(rows) != 1 || requests != 2 {
		test.Fatalf("constituent requests failed: %+v requests=%d err=%v", rows, requests, err)
	}
	if rows[0].Price != 10.5 || rows[0].DataTime.Format("2006-01-02") != "2020-11-02" ||
		rows[0].TotalCap != 1e6 || rows[0].FloatCap != 8e5 || rows[0].Source != "sina" {
		test.Fatalf("undated page price or collection date replaced real quote: %+v", rows[0])
	}
}

type boardRouteTestProvider struct {
	probeFakeAdapter
	heatError  error
	heatRows   []BoardHeat
	heatLimit  int
	stockCalls int
}

func (provider *boardRouteTestProvider) GetBoardHeatLimited(_ context.Context, _ string, limit int) ([]BoardHeat, error) {
	provider.heatLimit = limit
	return provider.heatRows, provider.heatError
}

func (provider *boardRouteTestProvider) GetBoardConstituents(context.Context, string, int) ([]BoardStock, error) {
	provider.stockCalls++
	return []BoardStock{{Symbol: "600000", Source: provider.Name()}}, nil
}

func TestBoardRoutesFallbackAndProbeWithoutChangingClassification(test *testing.T) {
	primary := &boardRouteTestProvider{probeFakeAdapter: probeFakeAdapter{name: "eastmoney"}, heatError: ErrUpstream}
	backup := &boardRouteTestProvider{probeFakeAdapter: probeFakeAdapter{name: "sina"},
		heatRows: []BoardHeat{{Code: "sina:new_bank", Source: "sina"}}}
	manager := NewManagerWithAdapters(primary, backup)
	rows, err := manager.GetBoardHeat(test.Context(), "industry")
	if err != nil || len(rows) != 1 || rows[0].Code != "sina:new_bank" {
		test.Fatalf("board fallback failed: %+v %v", rows, err)
	}
	if healthStatFor(test, manager.HealthSnapshot(), "eastmoney", "board_industry", "cn").Errors != 1 ||
		healthStatFor(test, manager.HealthSnapshot(), "sina", "board_industry", "cn").Success != 1 {
		test.Fatal("heatmap and admin diagnostics must share capability observations")
	}
	stocks, err := manager.GetBoardConstituents(test.Context(), rows[0].Code, 1)
	if err != nil || len(stocks) != 1 || primary.stockCalls != 0 || backup.stockCalls != 1 {
		test.Fatalf("Sina code was passed to an EastMoney endpoint: %+v %v", stocks, err)
	}
	if _, err := manager.GetBoardKline(test.Context(), rows[0].Code, 1); !errors.Is(err, ErrNotSupported) {
		test.Fatalf("an unrelated BK index must not substitute for a Sina classification: %v", err)
	}
	ResetProbeLimiterForTest()
	test.Cleanup(ResetProbeLimiterForTest)
	if result, err := manager.Probe(test.Context(), "sina", "board_industry", "cn"); err != nil || result.Outcome != ProbeSuccess || backup.heatLimit != 1 {
		test.Fatalf("board sample probe failed: %+v %v limit=%d", result, err, backup.heatLimit)
	}
	for _, code := range []string{"sina:new_bank&node=hs_a", "sina:../", "sina:BK0447", "new_bank"} {
		if _, err := manager.GetBoardConstituents(test.Context(), code, 1); !errors.Is(err, ErrSymbolInvalid) {
			test.Fatalf("invalid node must be rejected before network access: %s %v", code, err)
		}
	}
}
