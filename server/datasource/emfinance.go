package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// F2 财务数据源：
//   - F10 主要财务指标 RPT_F10_FINANCE_MAINFINADATA：datacenter.eastmoney.com/securities/api/data/get
//     单请求可取 200 期（实测 SECUCODE="600519.SH" 口径）。与 datacenter-web v1 网关同属
//     datacenter 家族，共享包级令牌桶 dcThrottle（QPS≤2）与瞬时错误重试退避。
//   - 三大报表关键科目：emweb.securities.eastmoney.com/PC_HSF10/NewFinanceAnalysis 的
//     {zcfzb,lrb,xjllb}AjaxNew，先 lrbDateAjaxNew 取报告期，companyType 4→3→2→1 试探
//     （类型不匹配时返回无 data 的空对象）。dates 逗号分隔多期，按 5 期一批（akshare 同款）。
//
// 二阶段口径：只取最近 8 期关键科目供 AI 上下文与详情页图表，全表明细后置。

// 包级变量便于单测用 httptest 替换。
var (
	f10BaseURL   = "https://datacenter.eastmoney.com/securities/api/data/get"
	emwebBaseURL = "https://emweb.securities.eastmoney.com/PC_HSF10/NewFinanceAnalysis"
)

// SecuCode A 股 6 位代码 → 东财 SECUCODE 口径（600519 → 600519.SH）。
// 6/9 沪、0/2/3 深、4/8/92 京（北交所推荐链路虽排除，口径转换仍保持完整）。
func SecuCode(symbol string) string {
	switch {
	case strings.HasPrefix(symbol, "4") || strings.HasPrefix(symbol, "8") || strings.HasPrefix(symbol, "92"):
		return symbol + ".BJ"
	case strings.HasPrefix(symbol, "6") || strings.HasPrefix(symbol, "9"):
		return symbol + ".SH"
	default:
		return symbol + ".SZ"
	}
}

// emwebCode emweb 接口的 code 口径（600519 → SH600519）。
func emwebCode(symbol string) string {
	sc := SecuCode(symbol)
	i := strings.IndexByte(sc, '.')
	return sc[i+1:] + sc[:i]
}

// f10Resp securities/api/data/get 响应（与 v1 网关结构同形，success 字段可能缺席，
// 以 result.data 是否有行为准）。
type f10Resp struct {
	Result struct {
		Pages int               `json:"pages"`
		Data  []json.RawMessage `json:"data"`
	} `json:"result"`
}

// GetF10MainFinance 拉取 F10 主要财务指标（单请求最多 200 期，REPORT_DATE 降序）。
// 返回原始 DcRow（关键字段：REPORT_DATE/REPORT_DATE_NAME/NOTICE_DATE/EPSJB/BPS/
// TOTALOPERATEREVE(TZ)/PARENTNETPROFIT(TZ)/KCFJCXSYJLR(TZ)/ROEJQ/XSMLL/XSJLL/
// ZCFZL/MGJYXJJE）。无数据返回 ErrNoData。
func GetF10MainFinance(ctx context.Context, symbol string) ([]DcRow, error) {
	v := url.Values{}
	v.Set("type", "RPT_F10_FINANCE_MAINFINADATA")
	v.Set("sty", "APP_F10_MAINFINADATA")
	v.Set("quoteColumns", "")
	v.Set("filter", fmt.Sprintf(`(SECUCODE="%s")`, SecuCode(symbol)))
	v.Set("p", "1")
	v.Set("ps", "200")
	v.Set("sr", "-1")
	v.Set("st", "REPORT_DATE")
	v.Set("source", "HSF10")
	v.Set("client", "PC")
	body, err := finGet(ctx, f10BaseURL+"?"+v.Encode())
	if err != nil {
		return nil, err
	}
	var parsed f10Resp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: F10 解析失败 %v", ErrUpstream, err)
	}
	return rawToDcRows(parsed.Result.Data)
}

// EMStatementRow 三大报表关键科目（同一报告期三表合并为一行；缺失科目为 0）。
type EMStatementRow struct {
	ReportDate string // 2026-03-31
	// 资产负债表（zcfzb）
	MonetaryFunds    float64 // 货币资金
	AccountsRece     float64 // 应收账款
	Inventory        float64 // 存货
	TotalAssets      float64
	TotalLiabilities float64
	TotalEquity      float64
	// 利润表（lrb）
	OperateIncome  float64 // 营业总收入
	OperateCost    float64 // 营业成本
	OperateProfit  float64 // 营业利润
	RDExpense      float64 // 研发费用
	// 现金流量表（xjllb）
	NetcashOperate float64 // 经营活动现金流净额
	NetcashInvest  float64
	NetcashFinance float64
}

const (
	emStmtPeriods   = 8 // 二阶段口径：最近 8 期
	emStmtBatchSize = 5 // dates 每批期数（akshare 同款，防 URL 过长/上游拒绝）
)

// GetEMStatements 拉取某 A 股最近 8 期三大报表关键科目。
// companyType 试探 4→3→2→1（通用/保险/券商/银行的模板不同，不匹配返回空 data）。
func GetEMStatements(ctx context.Context, symbol string) ([]EMStatementRow, error) {
	code := emwebCode(symbol)
	dates, companyType, err := emStatementDates(ctx, code)
	if err != nil {
		return nil, err
	}
	if len(dates) > emStmtPeriods {
		dates = dates[:emStmtPeriods]
	}
	byDate := make(map[string]*EMStatementRow, len(dates))
	for _, d := range dates {
		byDate[d] = &EMStatementRow{ReportDate: d}
	}
	for _, kind := range []string{"zcfzb", "lrb", "xjllb"} {
		for i := 0; i < len(dates); i += emStmtBatchSize {
			end := i + emStmtBatchSize
			if end > len(dates) {
				end = len(dates)
			}
			rows, err := emStatementPage(ctx, kind, code, companyType, dates[i:end])
			if err != nil {
				return nil, err
			}
			for _, r := range rows {
				d := r.Date("REPORT_DATE")
				dst, ok := byDate[d]
				if !ok {
					continue
				}
				switch kind {
				case "zcfzb":
					dst.MonetaryFunds = r.Float("MONETARYFUNDS")
					dst.AccountsRece = r.Float("ACCOUNTS_RECE")
					dst.Inventory = r.Float("INVENTORY")
					dst.TotalAssets = r.Float("TOTAL_ASSETS")
					dst.TotalLiabilities = r.Float("TOTAL_LIABILITIES")
					dst.TotalEquity = r.Float("TOTAL_EQUITY")
				case "lrb":
					dst.OperateIncome = r.Float("TOTAL_OPERATE_INCOME")
					dst.OperateCost = r.Float("OPERATE_COST")
					dst.OperateProfit = r.Float("OPERATE_PROFIT")
					dst.RDExpense = r.Float("RESEARCH_EXPENSE")
				case "xjllb":
					dst.NetcashOperate = r.Float("NETCASH_OPERATE")
					dst.NetcashInvest = r.Float("NETCASH_INVEST")
					dst.NetcashFinance = r.Float("NETCASH_FINANCE")
				}
			}
		}
	}
	out := make([]EMStatementRow, 0, len(dates))
	for _, d := range dates { // 保持 REPORT_DATE 降序（dates 即降序）
		out = append(out, *byDate[d])
	}
	return out, nil
}

// emStatementDates 取可用报告期（降序）并确定 companyType。
func emStatementDates(ctx context.Context, code string) ([]string, string, error) {
	for _, ct := range []string{"4", "3", "2", "1"} {
		v := url.Values{}
		v.Set("companyType", ct)
		v.Set("reportDateType", "0")
		v.Set("code", code)
		body, err := finGet(ctx, emwebBaseURL+"/lrbDateAjaxNew?"+v.Encode())
		if err != nil {
			return nil, "", err
		}
		var parsed struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(body, &parsed) != nil || len(parsed.Data) == 0 {
			continue // companyType 不匹配：返回无 data 对象，试下一个
		}
		rows, err := rawToDcRows(parsed.Data)
		if err != nil {
			continue
		}
		dates := make([]string, 0, len(rows))
		for _, r := range rows {
			if d := r.Date("REPORT_DATE"); d != "" {
				dates = append(dates, d)
			}
		}
		if len(dates) > 0 {
			return dates, ct, nil
		}
	}
	return nil, "", ErrNoData
}

// emStatementPage 拉取单表某批报告期。
func emStatementPage(ctx context.Context, kind, code, companyType string, dates []string) ([]DcRow, error) {
	v := url.Values{}
	v.Set("companyType", companyType)
	v.Set("reportDateType", "0")
	v.Set("reportType", "1")
	v.Set("dates", strings.Join(dates, ","))
	v.Set("code", code)
	body, err := finGet(ctx, emwebBaseURL+"/"+kind+"AjaxNew?"+v.Encode())
	if err != nil {
		return nil, err
	}
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: %s 解析失败 %v", ErrUpstream, kind, err)
	}
	return rawToDcRows(parsed.Data)
}

// finGet 财务接口统一取数：包级令牌桶（与 datacenter 网关共享 dcThrottle，QPS≤2）+
// 瞬时错误（网络/5xx）重试退避，与 DataCenterIter.Next 同款纪律。
func finGet(ctx context.Context, u string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * dcRetryBackoff):
			}
		}
		if err := dcThrottle(ctx); err != nil {
			return nil, err
		}
		b, status, err := doGet(ctx, u, map[string]string{"Referer": "https://emweb.securities.eastmoney.com/"})
		if err != nil {
			lastErr = fmt.Errorf("%w: %v", ErrUpstream, err)
			continue
		}
		if status >= 500 {
			lastErr = fmt.Errorf("%w: http %d", ErrUpstream, status)
			continue
		}
		if status != 200 {
			return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
		}
		return b, nil
	}
	return nil, lastErr
}

// rawToDcRows 原始行批量转 DcRow；空集返回 ErrNoData。
func rawToDcRows(raws []json.RawMessage) ([]DcRow, error) {
	if len(raws) == 0 {
		return nil, ErrNoData
	}
	rows := make([]DcRow, 0, len(raws))
	for _, raw := range raws {
		if row, err := ParseDcRow(raw); err == nil {
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, ErrNoData
	}
	return rows, nil
}
