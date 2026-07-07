package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// 东财 datacenter 统一网关（F1 核心资产）：datacenter-web.eastmoney.com/api/data/v1/get，
// 免 token，一个客户端解锁所有 RPT_* 报表（业绩预告/快报/预约披露/龙虎榜/解禁/股东户数等）。
// 统一处理：pageNumber 翻页（result.pages）、全局令牌桶（QPS ≤2，包级共享——
// 多个消费方并发也不会超）、瞬时错误重试退避。所有 RPT_* 查询都必须走这里。

// dcBaseURL 包级变量便于单测用 httptest 替换。
var dcBaseURL = "https://datacenter-web.eastmoney.com/api/data/v1/get"

// dcMinInterval 相邻请求最小间隔（QPS ≤2）。变量便于单测调小。
var dcMinInterval = 500 * time.Millisecond

// dcRetryBackoff 瞬时错误重试的退避基数（第 n 次重试等 n*基数）。变量便于单测调小。
var dcRetryBackoff = time.Second

var (
	dcMu   sync.Mutex
	dcLast time.Time
)

// dcThrottle 全局令牌桶（最简实现：串行化 + 最小间隔）。ctx 取消时提前返回。
func dcThrottle(ctx context.Context) error {
	dcMu.Lock()
	wait := dcMinInterval - time.Since(dcLast)
	if wait > 0 {
		dcMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		dcMu.Lock()
	}
	dcLast = time.Now()
	dcMu.Unlock()
	return nil
}

// DataCenterQuery 一次报表查询的参数。Filter 语法形如 (REPORT_DATE='2026-06-30')(NOTICE_DATE>='2026-07-01')。
type DataCenterQuery struct {
	ReportName  string
	Filter      string
	Columns     string // 空则 ALL
	SortColumns string
	SortTypes   string // "-1" 降序 / "1" 升序，与 SortColumns 一一对应
	PageSize    int    // 上限 500
}

// dcResp datacenter v1 响应。无数据时 success=false、code=9201、result=null。
type dcResp struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Code    int    `json:"code"`
	Result  struct {
		Pages int               `json:"pages"`
		Count int               `json:"count"`
		Data  []json.RawMessage `json:"data"`
	} `json:"result"`
}

const dcNoDataCode = 9201 // 「返回数据为空」：正常业务态（如增量刷新无新行），非错误

// DataCenterIter 翻页迭代器：Next 逐页返回原始行（调用方自行反序列化为具体报表结构）。
type DataCenterIter struct {
	q     DataCenterQuery
	page  int // 下一页页号（1 起）
	pages int // 服务端返回的总页数（首个响应后生效）
	done  bool
}

// DataCenterQuery 创建迭代器。用法：
//
//	it := em.DataCenterQuery(q)
//	for {
//	    rows, err := it.Next(ctx)
//	    if err != nil { ... }        // ErrNoData = 查询无结果
//	    if rows == nil { break }     // 翻页结束
//	    ...
//	}
func (e *EastMoneyAdapter) DataCenterQuery(q DataCenterQuery) *DataCenterIter {
	if q.PageSize <= 0 || q.PageSize > 500 {
		q.PageSize = 500
	}
	if q.Columns == "" {
		q.Columns = "ALL"
	}
	return &DataCenterIter{q: q, page: 1}
}

// Next 拉取下一页。翻页结束返回 (nil, nil)；首页即无数据返回 ErrNoData。
func (it *DataCenterIter) Next(ctx context.Context) ([]json.RawMessage, error) {
	if it.done {
		return nil, nil
	}
	u := dcBaseURL + "?" + dcQueryString(it.q, it.page)

	// 瞬时错误重试退避：网络错误/5xx 最多 3 次尝试，间隔 1s/2s 递增。
	// doGet 自身对网络错误还有一次紧邻重试，这里的退避针对上游短时过载。
	var body []byte
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
		b, status, err := doGet(ctx, u, map[string]string{"Referer": "https://data.eastmoney.com/"})
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
		body = b
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, lastErr
	}

	var parsed dcResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("%w: datacenter 解析失败 %v", ErrUpstream, err)
	}
	if !parsed.Success {
		if parsed.Code == dcNoDataCode {
			it.done = true
			if it.page == 1 {
				return nil, ErrNoData
			}
			return nil, nil // 尾页边界：前页已给过数据，静默结束
		}
		return nil, fmt.Errorf("%w: datacenter code=%d %s", ErrUpstream, parsed.Code, parsed.Message)
	}
	it.pages = parsed.Result.Pages
	rows := parsed.Result.Data
	if len(rows) == 0 && it.page == 1 {
		it.done = true
		return nil, ErrNoData
	}

	it.page++
	if it.page > it.pages || len(rows) == 0 {
		it.done = true
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return rows, nil
}

// dcQueryString 组装查询串（抽出便于单测断言参数拼装）。
func dcQueryString(q DataCenterQuery, page int) string {
	v := url.Values{}
	v.Set("reportName", q.ReportName)
	v.Set("columns", q.Columns)
	if q.Filter != "" {
		v.Set("filter", q.Filter)
	}
	if q.SortColumns != "" {
		v.Set("sortColumns", q.SortColumns)
		v.Set("sortTypes", q.SortTypes)
	}
	v.Set("pageSize", strconv.Itoa(q.PageSize))
	v.Set("pageNumber", strconv.Itoa(page))
	v.Set("source", "WEB")
	v.Set("client", "WEB")
	return v.Encode()
}

// DcString / DcFloat / DcDate 是报表行字段的宽松取值工具：datacenter 数值字段
// 可能为 null 或字符串，日期字段带 " 00:00:00" 尾巴，统一在这里吸收。
type DcRow map[string]json.RawMessage

func ParseDcRow(raw json.RawMessage) (DcRow, error) {
	var m DcRow
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func (r DcRow) String(key string) string {
	raw, ok := r[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

func (r DcRow) Float(key string) float64 {
	raw, ok := r[key]
	if !ok {
		return 0
	}
	v, _ := emNum(raw)
	return v
}

// Date 取日期字段的前 10 位（"2026-06-30 00:00:00" -> "2026-06-30"）。
func (r DcRow) Date(key string) string {
	s := r.String(key)
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
