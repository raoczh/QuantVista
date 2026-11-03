package datasource

import (
	"context"
	"encoding/json"
	"errors"
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
	dcMu            sync.Mutex
	dcLast          time.Time
	dcCooldownUntil time.Time
)

// dcThrottle 统一检查请求间隔与跨请求冷却；DC/F10 只经过这一层节流。
func dcThrottle(ctx context.Context) error {
	return waitSourceInterval(ctx, &dcMu, &dcLast, dcMinInterval, &dcCooldownUntil)
}

func dcDeferUntil(deadline time.Time) {
	dcMu.Lock()
	defer dcMu.Unlock()
	if deadline.After(dcCooldownUntil) {
		dcCooldownUntil = deadline
	}
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

	// 只有 HTTP 层重试；每次真实尝试（包括网络错误重试）都重新经过 dcThrottle。
	body, status, err := doGetWithPolicy(ctx, u, map[string]string{"Referer": "https://data.eastmoney.com/"},
		httpRequestPolicy{beforeAttempt: dcThrottle, onRetryAfter: dcDeferUntil, retryBackoff: dcRetryBackoff})
	if errors.Is(err, ErrNoData) {
		it.done = true
		if it.page == 1 {
			return nil, ErrNoData
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
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
	v, _ := r.FloatOK(key)
	return v
}

// FloatOK 同时返回可用性，财务等场景必须区分真实零、null 和非法数值。
func (r DcRow) FloatOK(key string) (float64, bool) {
	raw, ok := r[key]
	if !ok {
		return 0, false
	}
	return emNum(raw)
}

// Date 取日期字段的前 10 位（"2026-06-30 00:00:00" -> "2026-06-30"）。
func (r DcRow) Date(key string) string {
	s := r.String(key)
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
