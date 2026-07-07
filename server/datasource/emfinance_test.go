package datasource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSecuCode(t *testing.T) {
	cases := map[string]string{
		"600519": "600519.SH", "688981": "688981.SH", "900901": "900901.SH",
		"000001": "000001.SZ", "300750": "300750.SZ", "002594": "002594.SZ",
		"430047": "430047.BJ", "830799": "830799.BJ", "920002": "920002.BJ",
	}
	for in, want := range cases {
		if got := SecuCode(in); got != want {
			t.Errorf("SecuCode(%s)=%s want %s", in, got, want)
		}
	}
	if got := emwebCode("600519"); got != "SH600519" {
		t.Errorf("emwebCode=%s want SH600519", got)
	}
	if got := emwebCode("300750"); got != "SZ300750" {
		t.Errorf("emwebCode=%s want SZ300750", got)
	}
}

func TestGetF10MainFinance(t *testing.T) {
	oldURL, oldMin := f10BaseURL, dcMinInterval
	dcMinInterval = time.Millisecond
	defer func() { f10BaseURL, dcMinInterval = oldURL, oldMin }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("type") != "RPT_F10_FINANCE_MAINFINADATA" || q.Get("source") != "HSF10" {
			t.Errorf("参数缺失: %s", r.URL.RawQuery)
		}
		if !strings.Contains(q.Get("filter"), `600519.SH`) {
			t.Errorf("filter 口径错误: %s", q.Get("filter"))
		}
		fmt.Fprint(w, `{"version":"x","result":{"pages":1,"data":[
			{"REPORT_DATE":"2026-03-31 00:00:00","REPORT_DATE_NAME":"2026一季报","NOTICE_DATE":"2026-04-25 00:00:00",
			 "EPSJB":21.76,"BPS":216.32,"TOTALOPERATEREVE":54702912385.23,"TOTALOPERATEREVETZ":6.336,
			 "PARENTNETPROFIT":27242512886.45,"PARENTNETPROFITTZ":1.471,"KCFJCXSYJLR":27239985194.41,"KCFJCXSYJLRTZ":null,
			 "ROEJQ":10.57,"XSMLL":89.759,"XSJLL":52.224,"ZCFZL":12.123,"MGJYXJJE":21.489}
		]}}`)
	}))
	defer srv.Close()
	f10BaseURL = srv.URL

	rows, err := GetF10MainFinance(context.Background(), "600519")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if r.Date("REPORT_DATE") != "2026-03-31" || r.Float("ROEJQ") != 10.57 || r.Float("KCFJCXSYJLRTZ") != 0 {
		t.Errorf("字段解析错误: %v %v", r.Date("REPORT_DATE"), r.Float("ROEJQ"))
	}
}

func TestGetF10MainFinanceNoData(t *testing.T) {
	oldURL, oldMin := f10BaseURL, dcMinInterval
	dcMinInterval = time.Millisecond
	defer func() { f10BaseURL, dcMinInterval = oldURL, oldMin }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"result":null,"success":false,"code":9201}`)
	}))
	defer srv.Close()
	f10BaseURL = srv.URL
	if _, err := GetF10MainFinance(context.Background(), "600519"); !errors.Is(err, ErrNoData) {
		t.Fatalf("want ErrNoData got %v", err)
	}
}

// TestGetEMStatements companyType 试探（4 空 → 3 命中）+ 三表按报告期合并。
func TestGetEMStatements(t *testing.T) {
	oldURL, oldMin := emwebBaseURL, dcMinInterval
	dcMinInterval = time.Millisecond
	defer func() { emwebBaseURL, dcMinInterval = oldURL, oldMin }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct := r.URL.Query().Get("companyType")
		switch {
		case strings.Contains(r.URL.Path, "lrbDateAjaxNew"):
			if ct == "4" { // 类型不匹配：返回无 data 的对象（实测行为）
				fmt.Fprint(w, `{"$type":"1"}`)
				return
			}
			fmt.Fprint(w, `{"pages":1,"data":[
				{"REPORT_DATE":"2026-03-31 00:00:00"},{"REPORT_DATE":"2025-12-31 00:00:00"}]}`)
		case strings.Contains(r.URL.Path, "zcfzbAjaxNew"):
			if ct != "3" {
				t.Errorf("companyType 应为试探命中的 3, got %s", ct)
			}
			fmt.Fprint(w, `{"data":[{"REPORT_DATE":"2026-03-31 00:00:00","MONETARYFUNDS":100.5,"TOTAL_ASSETS":900.25,"TOTAL_LIABILITIES":300,"TOTAL_EQUITY":600.25,"INVENTORY":50,"ACCOUNTS_RECE":10}]}`)
		case strings.Contains(r.URL.Path, "lrbAjaxNew"):
			fmt.Fprint(w, `{"data":[
				{"REPORT_DATE":"2026-03-31 00:00:00","TOTAL_OPERATE_INCOME":500,"OPERATE_COST":200,"OPERATE_PROFIT":250,"RESEARCH_EXPENSE":30},
				{"REPORT_DATE":"2025-12-31 00:00:00","TOTAL_OPERATE_INCOME":1800,"OPERATE_COST":800,"OPERATE_PROFIT":900,"RESEARCH_EXPENSE":120}]}`)
		case strings.Contains(r.URL.Path, "xjllbAjaxNew"):
			fmt.Fprint(w, `{"data":[{"REPORT_DATE":"2025-12-31 00:00:00","NETCASH_OPERATE":615.2,"NETCASH_INVEST":-316.4,"NETCASH_FINANCE":-734.3}]}`)
		default:
			t.Errorf("未知路径 %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	emwebBaseURL = srv.URL

	rows, err := GetEMStatements(context.Background(), "600519")
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows=%d want 2", len(rows))
	}
	// 降序：首行 2026-03-31（有 zcfzb+lrb 无 xjllb），次行 2025-12-31（有 lrb+xjllb）。
	if rows[0].ReportDate != "2026-03-31" || rows[0].MonetaryFunds != 100.5 || rows[0].OperateIncome != 500 || rows[0].NetcashOperate != 0 {
		t.Errorf("首期合并错误: %+v", rows[0])
	}
	if rows[1].ReportDate != "2025-12-31" || rows[1].NetcashOperate != 615.2 || rows[1].OperateIncome != 1800 || rows[1].TotalAssets != 0 {
		t.Errorf("次期合并错误: %+v", rows[1])
	}
}
