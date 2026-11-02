package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const eastMoneyExtraRankFirst = `{"f12":"600000","f14":"  Sample Bank  ","f2":10.06,"f3":0.4,"f6":1171305072.4,"f8":0.35,"f9":6.19,"f21":335056733298,"f23":0.44}`
const eastMoneyExtraRankSecond = `{"f12":"000001","f14":"Sample Two","f2":"11.5","f3":"-2.1","f6":"240001.5","f8":"1.2","f9":"-3.2","f21":"1200050000","f23":"0.51"}`

// Field values from a single public stock/get fltt=2 response on 2026-11-02;
// name is substituted. The three PE fields intentionally differ, detecting a
// swapped mapping; caps and prices detect a spurious /100 or *1e8 conversion.
const eastMoneyValuationFixture = `{"rc":0,"data":{
	"f43":10.06,"f50":1.59,"f51":11.02,"f52":9.02,
	"f57":"600000","f58":"Sample Bank","f86":1793607106,
	"f116":335056733298,"f117":335056733298,
	"f162":6.19,"f163":6.7,"f164":6.47,"f167":0.44,"f168":0.35,"f171":2.89
}}`

func TestEastMoneyRankingArrayAndNumericObjectHaveSameOrder(test *testing.T) {
	array := `{"rc":0,"data":{"diff":[` + eastMoneyExtraRankFirst + `,` + eastMoneyExtraRankSecond + `]}}`
	object := `{"rc":0,"data":{"diff":{"10":` + eastMoneyExtraRankSecond + `,"2":` + eastMoneyExtraRankFirst + `}}}`
	arrayRows, err := parseEastMoneyStockRanking([]byte(array))
	if err != nil {
		test.Fatal(err)
	}
	objectRows, err := parseEastMoneyStockRanking([]byte(object))
	if err != nil || !reflect.DeepEqual(arrayRows, objectRows) {
		test.Fatalf("numeric object ordering differs: array=%+v object=%+v err=%v", arrayRows, objectRows, err)
	}
	first := arrayRows[0]
	if first.Symbol != "600000" || first.Name != "Sample Bank" || first.Price != 10.06 || first.ChangePct != 0.4 || first.Amount != 1171305072.4 || first.TurnoverRate != 0.35 || first.PE != 6.19 || first.PB != 0.44 || first.FloatCap != 335056733298 || first.Source != "eastmoney" {
		test.Fatalf("ranking units/fields drifted: %+v", first)
	}
	if arrayRows[1].PE != -3.2 || arrayRows[1].Amount != 240001.5 {
		test.Fatalf("numeric strings or loss-making PE lost: %+v", arrayRows[1])
	}
}

func TestEastMoneyRankingRequestWhitelistDirectionsAndLimits(test *testing.T) {
	for _, testcase := range []struct {
		sortField string
		fieldID   string
	}{
		{"changepercent", "f3"}, {"amount", "f6"}, {"turnoverratio", "f8"}, {"pb", "f23"},
		{"f12&pz=9999", "f3"},
	} {
		for _, ascending := range []bool{false, true} {
			test.Run(testcase.sortField+"/asc="+strconv.FormatBool(ascending), func(test *testing.T) {
				withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
					query := request.URL.Query()
					wantOrder := "1"
					if ascending {
						wantOrder = "0"
					}
					if request.URL.Path != "/api/qt/clist/get" || query.Get("fid") != testcase.fieldID || query.Get("po") != wantOrder || query.Get("pz") != "1" || query.Get("fs") != clistSpotFS || query.Get("fltt") != "2" || query.Get("fields") != eastMoneyRankingFields {
						test.Errorf("ranking query drifted: %s", request.URL)
					}
					fmt.Fprint(writer, `{"data":{"diff":[`+eastMoneyExtraRankFirst+`,`+eastMoneyExtraRankSecond+`]}}`)
				})
				rows, err := NewEastMoneyAdapter().GetStockRanking(context.Background(), "cn", testcase.sortField, ascending, 1)
				if err != nil || len(rows) != 1 || rows[0].Symbol != "600000" {
					test.Fatalf("bounded ranking failed: rows=%+v err=%v", rows, err)
				}
			})
		}
	}
	for _, limit := range []int{-1, 0, 101, 1000000} {
		test.Run("limit="+strconv.Itoa(limit), func(test *testing.T) {
			withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Query().Get("pz") != "10" {
					test.Errorf("invalid limit must use Sina-compatible default: %s", request.URL)
				}
				fmt.Fprint(writer, `{"data":{"diff":[`+eastMoneyExtraRankFirst+`]}}`)
			})
			if _, err := NewEastMoneyAdapter().GetStockRanking(context.Background(), "cn", "amount", false, limit); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestEastMoneyRankingMissingFieldsRemainMissing(test *testing.T) {
	body := `{"data":{"diff":[
		{"f12":"600000","f14":"Sample","f2":10,"f3":"NaN","f6":"+Inf","f8":null,"f9":"-","f21":"1e309","f23":"NaN"},
		{"f12":"000001","f2":"-"},
		{"f12":"60000x","f2":12}
	]}}`
	rows, err := parseEastMoneyStockRanking([]byte(body))
	if err != nil || len(rows) != 1 {
		test.Fatalf("useful row must survive missing optional fields: rows=%+v err=%v", rows, err)
	}
	row := rows[0]
	if row.ChangePct != 0 || row.Amount != 0 || row.TurnoverRate != 0 || row.PE != 0 || row.PB != 0 || row.FloatCap != 0 {
		test.Fatalf("invalid fields must not become fabricated facts: %+v", row)
	}
	if _, err := json.Marshal(rows); err != nil {
		test.Fatalf("nonfinite numbers escaped parser: %v", err)
	}
	for _, testcase := range []struct {
		body string
		want error
	}{
		{`{"data":null}`, ErrNoData},
		{`{"data":{"diff":[]}}`, ErrNoData},
		{`{"data":{"diff":{"0":{"f12":"600000","f2":"-"}}}}`, ErrNoData},
		{`{"rc":1,"data":{"diff":[` + eastMoneyExtraRankFirst + `]}}`, ErrUpstream},
		{`not json`, ErrUpstream},
		{`{"data":{"diff":["malformed row"]}}`, ErrUpstream},
	} {
		if _, err := parseEastMoneyStockRanking([]byte(testcase.body)); !errors.Is(err, testcase.want) {
			test.Errorf("unexpected failure semantics: body=%s err=%v", testcase.body, err)
		}
	}
}

func TestEastMoneyValuationVerifiedFieldsAndUnits(test *testing.T) {
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		if request.URL.Path != "/api/qt/stock/get" || query.Get("fltt") != "2" || query.Get("secid") != "1.600000" || query.Get("fields") != eastMoneyValuationFields {
			test.Errorf("unexpected valuation query: %s", request.URL)
		}
		if request.Header.Get("Referer") != "https://quote.eastmoney.com/" {
			test.Error("missing EastMoney referer")
		}
		fmt.Fprint(writer, eastMoneyValuationFixture)
	})
	valuation, err := NewEastMoneyAdapter().GetValuation(context.Background(), "cn", "600000")
	if err != nil {
		test.Fatal(err)
	}
	want := &Valuation{
		Symbol: "600000", Market: "cn", Name: "Sample Bank", Source: "eastmoney",
		PEDynamic: 6.19, PEStatic: 6.7, PETTM: 6.47, PB: 0.44,
		TotalCap: 335056733298, FloatCap: 335056733298,
		TurnoverRate: 0.35, Amplitude: 2.89, VolumeRatio: 1.59, LimitUp: 11.02, LimitDown: 9.02,
		DataTime: time.Unix(1793607106, 0).In(ChinaMarketLocation()),
	}
	if !reflect.DeepEqual(valuation, want) {
		test.Fatalf("valuation mapping/scaling drifted:\ngot  %+v\nwant %+v", valuation, want)
	}
}

func TestEastMoneyValuationPartialMissingAndInvalidFields(test *testing.T) {
	body := `{"data":{"f57":"600000","f58":" *ST Sample ","f43":"-", "f164":"-5.2",
		"f162":"-","f163":null,"f167":"NaN","f116":"+Inf","f117":"1e309",
		"f50":"-","f51":null,"f52":"NaN","f168":0,"f171":"-","f86":"bad date"}}`
	valuation, err := parseEastMoneyValuation([]byte(body), "cn", "600000")
	if err != nil {
		test.Fatal(err)
	}
	if valuation.PETTM != -5.2 || !valuation.IsST || !valuation.DataTime.IsZero() || valuation.PB != 0 || valuation.TotalCap != 0 || valuation.FloatCap != 0 || valuation.LimitUp != 0 || valuation.LimitDown != 0 || valuation.PEDynamic != 0 || valuation.PEStatic != 0 {
		test.Fatalf("partial valuation should retain only supplied facts: %+v", valuation)
	}
	if _, err := json.Marshal(valuation); err != nil {
		test.Fatalf("nonfinite valuation fields: %v", err)
	}
	// An absent quote price or even absent name does not erase a confirmed metric.
	for _, body := range []string{`{"data":{"f164":4.5}}`, `{"data":{"f164":0}}`, `{"data":{"f164":-4.5}}`} {
		if _, err := parseEastMoneyValuation([]byte(body), "cn", "600000"); err != nil {
			test.Errorf("partial metric incorrectly discarded: %s %v", body, err)
		}
	}
	for _, timestamp := range []string{`null`, `"NaN"`, `"+Inf"`, `-1`, `1e30`, `1793607106.5`} {
		body := `{"data":{"f164":4.5,"f86":` + timestamp + `}}`
		valuation, err := parseEastMoneyValuation([]byte(body), "cn", "600000")
		if err != nil || !valuation.DataTime.IsZero() {
			test.Errorf("timestamp must remain unknown: %s %+v %v", timestamp, valuation, err)
		}
	}
}

func TestEastMoneyValuationEmptyFailureAndIdentityMismatch(test *testing.T) {
	for _, testcase := range []struct {
		body string
		want error
	}{
		{`{"data":null}`, ErrNoData},
		{`{"data":{}}`, ErrNoData},
		{`{"data":{"f57":"600000","f43":10,"f58":"Sample"}}`, ErrNoData},
		{`{"data":{"f57":"600000","f164":"NaN","f116":"-","f50":"+Inf"}}`, ErrNoData},
		{`{"data":{"f57":"000001","f164":6}}`, ErrUpstream},
		{`{"rc":1,"data":{"f164":6}}`, ErrUpstream},
		{`{"data":[]}`, ErrUpstream},
		{`<html>verification required</html>`, ErrUpstream},
	} {
		valuation, err := parseEastMoneyValuation([]byte(testcase.body), "cn", "600000")
		if !errors.Is(err, testcase.want) || valuation != nil {
			test.Errorf("bad payload escaped as success: body=%s valuation=%+v err=%v", testcase.body, valuation, err)
		}
	}
}

func TestEastMoneyExtraCapabilitiesRespectBreakerAndCancellation(test *testing.T) {
	var backupRequests atomic.Int32
	withFreeDataServer(test, func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.Host, "push2delay.") {
			writer.WriteHeader(http.StatusForbidden)
			fmt.Fprint(writer, "rate limited")
			return
		}
		backupRequests.Add(1)
		switch request.URL.Path {
		case "/api/qt/stock/get":
			fmt.Fprint(writer, eastMoneyValuationFixture)
		case "/api/qt/clist/get":
			fmt.Fprint(writer, `{"data":{"diff":[`+eastMoneyExtraRankFirst+`]}}`)
		default:
			test.Errorf("unexpected request path: %s", request.URL)
		}
	})
	adapter := NewEastMoneyAdapter()
	if _, err := adapter.GetValuation(context.Background(), "cn", "600000"); err != nil {
		test.Fatal(err)
	}
	if _, err := adapter.GetStockRanking(context.Background(), "cn", "amount", false, 1); err != nil {
		test.Fatal(err)
	}
	if backupRequests.Load() != 2 || !adapter.br.isDegraded("push2") {
		test.Fatal("new capabilities must share the existing EastMoney breaker")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.GetValuation(ctx, "cn", "600000"); !errors.Is(err, context.Canceled) {
		test.Fatalf("valuation lost cancellation identity: %v", err)
	}
	if _, err := adapter.GetStockRanking(ctx, "cn", "amount", false, 1); !errors.Is(err, context.Canceled) {
		test.Fatalf("ranking lost cancellation identity: %v", err)
	}
	if _, err := adapter.GetValuation(ctx, "us", "600000"); !errors.Is(err, ErrNotSupported) {
		test.Fatalf("unsupported market: %v", err)
	}
	if _, err := adapter.GetStockRanking(ctx, "us", "amount", false, 1); !errors.Is(err, ErrNotSupported) {
		test.Fatalf("unsupported ranking market: %v", err)
	}
	if _, err := adapter.GetValuation(ctx, "cn", "6&x=10"); !errors.Is(err, ErrSymbolInvalid) {
		test.Fatalf("invalid symbol: %v", err)
	}
}
