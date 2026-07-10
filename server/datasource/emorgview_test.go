package datasource

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// 真实响应节选（2026-07-10 curl reportapi）：有目标价+维持评级。
const repFixtureFull = `{"title":"2025年年报及2026年一季报点评","stockName":"贵州茅台","stockCode":"600519",
"orgCode":"80045894","orgName":"诚通证券股份有限公司","orgSName":"诚通证券",
"publishDate":"2026-05-25 00:00:00.000","infoCode":"AP202605251822844635",
"emRatingCode":"007","emRatingValue":"3","emRatingName":"买入",
"lastEmRatingCode":"007","lastEmRatingValue":"3","lastEmRatingName":"买入","ratingChange":3,
"indvAimPriceT":"512.0000000000","indvAimPriceL":"512.0000000000","researcher":"陈文倩","market":"SHANGHAI"}`

// 无目标价+首次覆盖（ratingChange=2，lastEmRatingName 空）。
const repFixtureFirst = `{"title":"首次覆盖报告","stockCode":"300750","orgSName":"华鑫证券",
"publishDate":"2026-05-05 00:00:00.000","infoCode":"AP202605051821970230",
"emRatingName":"增持","lastEmRatingName":"","ratingChange":2,
"indvAimPriceT":"","indvAimPriceL":"","researcher":""}`

// 无评级行（ratingChange 空串、评级名空）。
const repFixtureNoRating = `{"title":"行业周报","stockCode":"600519","orgSName":"某证券",
"publishDate":"2026-04-01 00:00:00.000","infoCode":"AP20260401X",
"emRatingName":"","lastEmRatingName":"","ratingChange":"","indvAimPriceT":""}`

func TestParseReportRow(t *testing.T) {
	row, ok := parseReportRow(json.RawMessage(repFixtureFull))
	if !ok {
		t.Fatal("完整行应解析成功")
	}
	if row.InfoCode != "AP202605251822844635" || row.Symbol != "600519" || row.OrgName != "诚通证券" {
		t.Fatalf("基础字段错: %+v", row)
	}
	if row.PublishDate != "2026-05-25" {
		t.Fatalf("日期应截 10 位, got %q", row.PublishDate)
	}
	if row.Rating != "买入" || row.LastRating != "买入" || row.RatingChange != 3 {
		t.Fatalf("评级字段错: %+v", row)
	}
	if row.TargetPrice != 512 {
		t.Fatalf("目标价应解析字符串数字, got %v", row.TargetPrice)
	}

	first, ok := parseReportRow(json.RawMessage(repFixtureFirst))
	if !ok || first.RatingChange != 2 || first.LastRating != "" || first.TargetPrice != 0 {
		t.Fatalf("首次覆盖行解析错: %+v ok=%v", first, ok)
	}

	nr, ok := parseReportRow(json.RawMessage(repFixtureNoRating))
	if !ok || nr.RatingChange != -1 || nr.Rating != "" {
		t.Fatalf("无评级行 ratingChange 应为 -1: %+v ok=%v", nr, ok)
	}

	if _, ok := parseReportRow(json.RawMessage(`{"title":"缺唯一键"}`)); ok {
		t.Fatal("缺 infoCode/stockCode 的行应丢弃")
	}
}

// 真实响应节选（2026-07-10 curl datacenter RPT_ORG_SURVEYNEW）。
const surveyFixture = `{"SECUCODE":"002230.SZ","SECURITY_CODE":"002230","SECURITY_NAME_ABBR":"科大讯飞",
"NOTICE_DATE":"2026-04-29 00:00:00","RECEIVE_START_DATE":"2026-04-29 00:00:00",
"RECEIVE_OBJECT":"国盛证券","ORG_TYPE":"证券公司","RECEIVE_WAY_EXPLAIN":"业绩说明会,现场、价值在线会议及网络直播相结合",
"NUM":17,"SUM":51,"NUMBERNEW":"17"}`

func TestParseSurveyRow(t *testing.T) {
	r, err := ParseDcRow(json.RawMessage(surveyFixture))
	if err != nil {
		t.Fatal(err)
	}
	row, ok := parseSurveyRow(r)
	if !ok {
		t.Fatal("调研行应解析成功")
	}
	if row.Symbol != "002230" || row.SurveyDate != "2026-04-29" || row.OrgName != "国盛证券" ||
		row.OrgType != "证券公司" || row.NoticeDate != "2026-04-29" {
		t.Fatalf("调研字段错: %+v", row)
	}
	bad, _ := ParseDcRow(json.RawMessage(`{"SECURITY_CODE":"002230"}`))
	if _, ok := parseSurveyRow(bad); ok {
		t.Fatal("缺调研日的行应丢弃")
	}
}

// withRepTestServer 把 reportapi 基址指到本地假服务并压缩限流间隔，测毕还原。
func withRepTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	oldURL, oldInterval := repBaseURL, repMinInterval
	repBaseURL = srv.URL
	repMinInterval = time.Millisecond
	t.Cleanup(func() {
		repBaseURL = oldURL
		repMinInterval = oldInterval
		srv.Close()
	})
	return srv
}

// 翻页：TotalPage=2 两页各一行，第三页不应被请求（TotalPage 边界）。
func TestGetStockReportsPaging(t *testing.T) {
	var calls int32
	withRepTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		q := r.URL.Query()
		if q.Get("code") != "600519" || q.Get("qType") != "0" {
			t.Errorf("请求参数错: %v", q)
		}
		switch q.Get("pageNo") {
		case "1":
			fmt.Fprintf(w, `{"hits":2,"TotalPage":2,"size":1,"data":[%s]}`, repFixtureFull)
		case "2":
			fmt.Fprintf(w, `{"hits":2,"TotalPage":2,"size":1,"data":[%s]}`, repFixtureFirst)
		default:
			t.Errorf("不应请求第 %s 页", q.Get("pageNo"))
			fmt.Fprint(w, `{"hits":2,"TotalPage":2,"data":[]}`)
		}
	})
	rows, err := NewEastMoneyAdapter().GetStockReports(context.Background(), "600519", 365)
	if err != nil || len(rows) != 2 {
		t.Fatalf("期望 2 行, got %d err=%v", len(rows), err)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("期望恰好 2 次请求, got %d", calls)
	}
}

// 空数据归一 ErrNoData；非法代码拒绝。
func TestGetStockReportsNoData(t *testing.T) {
	withRepTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"hits":0,"TotalPage":0,"size":0,"data":[]}`)
	})
	em := NewEastMoneyAdapter()
	if _, err := em.GetStockReports(context.Background(), "600519", 365); !errors.Is(err, ErrNoData) {
		t.Fatalf("期望 ErrNoData, got %v", err)
	}
	if _, err := em.GetStockReports(context.Background(), "abc", 365); !errors.Is(err, ErrSymbolInvalid) {
		t.Fatalf("非法代码期望 ErrSymbolInvalid, got %v", err)
	}
}

// GetOrgSurveys 走 datacenter 网关：复用 dc 假服务。
func TestGetOrgSurveys(t *testing.T) {
	withDcTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reportName") != surveyReport {
			t.Errorf("reportName 错: %v", r.URL.Query().Get("reportName"))
		}
		fmt.Fprint(w, dcPage(1, surveyFixture))
	}, time.Millisecond)
	rows, err := NewEastMoneyAdapter().GetOrgSurveys(context.Background(), "002230", 365)
	if err != nil || len(rows) != 1 {
		t.Fatalf("期望 1 行, got %d err=%v", len(rows), err)
	}
	if rows[0].SurveyDate != "2026-04-29" {
		t.Fatalf("调研日错: %+v", rows[0])
	}
}

// LIVE_ORG=1 真实接口冒烟（研报密集股 600519 + 调研密集股 002230）。
func TestLiveOrgView(t *testing.T) {
	if os.Getenv("LIVE_ORG") == "" {
		t.Skip("LIVE_ORG 未设置，跳过真实接口冒烟")
	}
	em := NewEastMoneyAdapter()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	reps, err := em.GetStockReports(ctx, "600519", 400)
	if err != nil {
		t.Fatalf("研报拉取失败: %v", err)
	}
	t.Logf("600519 近 400 天研报 %d 份，首份 %s %s %s 目标价 %.2f",
		len(reps), reps[0].PublishDate, reps[0].OrgName, reps[0].Rating, reps[0].TargetPrice)

	svys, err := em.GetOrgSurveys(ctx, "002230", 400)
	if err != nil {
		t.Fatalf("调研拉取失败: %v", err)
	}
	t.Logf("002230 近 400 天调研明细 %d 行，首行 %s %s", len(svys), svys[0].SurveyDate, svys[0].OrgName)
}
