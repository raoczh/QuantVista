package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 机构观点（P3a）：研报评级 + 机构调研两类上游。
//   - 研报列表：reportapi.eastmoney.com/report/list（独立域名，非 datacenter 网关，
//     自建包级节流对齐 dcThrottle 的 QPS≤2 纪律）。qType=0 个股研报，pageSize=100 实测可用，
//     TotalPage 翻页。
//   - 机构调研：datacenter RPT_ORG_SURVEYNEW，走 DataCenterQuery 网关（包级令牌桶）。
//     一机构一行明细：同次调研每家参与机构各一行（RECEIVE_START_DATE 同值），
//     调用方按 (symbol, 调研日) 聚合；SUM=该股总行数、NUMBERNEW=行序号。
//
// 研报字段锚点（2026-07-10 实测）：infoCode 全局唯一；emRatingName/emRatingValue 东财归一
// 评级（买入/增持/中性/减持/卖出，可空）；lastEmRatingName 上次评级（可空——上调样本也常空，
// 判变动以 ratingChange 为准）；ratingChange 0=上调 1=下调（实证 买入→增持）2=首次覆盖
// 3=维持 空串=无评级；indvAimPriceT/L 目标价（字符串数字，覆盖率约 1/4，有值时 T==L）；
// publishDate "2026-05-25 00:00:00.000"；orgSName 机构简称；researcher 分析师。
// 调研字段锚点（2026-07-10 实测）：RECEIVE_START_DATE 调研日；RECEIVE_OBJECT 参与机构名；
// ORG_TYPE 机构类型（证券公司/基金公司…）；RECEIVE_WAY_EXPLAIN 接待方式；NOTICE_DATE 公告日。

// repBaseURL 包级变量便于单测用 httptest 替换。
var repBaseURL = "https://reportapi.eastmoney.com/report/list"

// repMinInterval 相邻请求最小间隔（对齐 datacenter QPS≤2 纪律）。变量便于单测调小。
var repMinInterval = 500 * time.Millisecond

var (
	repMu   sync.Mutex
	repLast time.Time
)

func repThrottle(ctx context.Context) error {
	repMu.Lock()
	wait := repMinInterval - time.Since(repLast)
	if wait > 0 {
		repMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		repMu.Lock()
	}
	repLast = time.Now()
	repMu.Unlock()
	return nil
}

const (
	surveyReport   = "RPT_ORG_SURVEYNEW"
	repPageSize    = 100 // 实测可用（上游未见硬钳）
	repMaxPages    = 5   // 护栏：500 份/股远超 18 个月密集覆盖（茅台一年约 70 份）
	surveyMaxPages = 4   // 护栏：pageSize=500，调研密集股一年也在千行内
)

// ReportRow 一份卖方研报的评级要素。
type ReportRow struct {
	InfoCode     string  // 全局唯一（防重拉的天然唯一键）
	Title        string
	Symbol       string  // 6 位代码
	OrgName      string  // 机构简称（orgSName）
	Researcher   string  // 分析师（可空）
	PublishDate  string  // YYYY-MM-DD
	Rating       string  // 东财归一评级（买入/增持/中性/减持/卖出，可空）
	LastRating   string  // 上次评级（可空，缺失≠首次覆盖）
	RatingChange int     // 0=上调 1=下调 2=首次覆盖 3=维持 -1=缺失/无评级
	TargetPrice  float64 // 目标价（元，0=未给）
}

// SurveyRow 机构调研明细单行（一机构一行）。
type SurveyRow struct {
	Symbol     string
	SurveyDate string // 调研日 YYYY-MM-DD（RECEIVE_START_DATE）
	NoticeDate string
	OrgName    string // 参与机构名
	OrgType    string // 机构类型（证券公司/基金公司…）
	ReceiveWay string // 接待方式说明
}

// repListResp reportapi 响应。注意 TotalPage 大写开头是上游原样。
type repListResp struct {
	Hits      int               `json:"hits"`
	TotalPage int               `json:"TotalPage"`
	Data      []json.RawMessage `json:"data"`
}

// GetStockReports 拉取个股近 days 天的卖方研报评级列表（TotalPage 翻页，护栏 5 页）。
// 无研报返回 ErrNoData。
func (e *EastMoneyAdapter) GetStockReports(ctx context.Context, symbol string, days int) ([]ReportRow, error) {
	if _, ok := cnSecid(symbol); !ok {
		return nil, ErrSymbolInvalid
	}
	if days <= 0 {
		days = 365
	}
	end := time.Now()
	begin := end.AddDate(0, 0, -days)
	var out []ReportRow
	totalPages := 1
	for page := 1; page <= totalPages && page <= repMaxPages; page++ {
		if err := repThrottle(ctx); err != nil {
			return nil, err
		}
		v := url.Values{}
		v.Set("pageNo", strconv.Itoa(page))
		v.Set("pageSize", strconv.Itoa(repPageSize))
		v.Set("code", symbol)
		v.Set("qType", "0")
		v.Set("beginTime", begin.Format("2006-01-02"))
		v.Set("endTime", end.Format("2006-01-02"))
		body, status, err := doGet(ctx, repBaseURL+"?"+v.Encode(),
			map[string]string{"Referer": "https://data.eastmoney.com/report/stock.jshtml"})
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
		}
		if status != 200 {
			return nil, fmt.Errorf("%w: reportapi http %d", ErrUpstream, status)
		}
		var parsed repListResp
		if err := json.Unmarshal(body, &parsed); err != nil {
			return nil, fmt.Errorf("%w: reportapi 解析失败 %v", ErrUpstream, err)
		}
		if parsed.TotalPage > totalPages {
			totalPages = parsed.TotalPage
		}
		for _, raw := range parsed.Data {
			if row, ok := parseReportRow(raw); ok {
				out = append(out, row)
			}
		}
		if len(parsed.Data) == 0 {
			break
		}
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

// parseReportRow 单行解析（抽出便于单测，防上游字段漂移）。复用 DcRow 宽松取值：
// reportapi 数值字段同样是字符串数字/空串混合。
func parseReportRow(raw json.RawMessage) (ReportRow, bool) {
	r, err := ParseDcRow(raw)
	if err != nil {
		return ReportRow{}, false
	}
	info := r.String("infoCode")
	sym := r.String("stockCode")
	if info == "" || sym == "" {
		return ReportRow{}, false
	}
	rc := -1
	if s := strings.TrimSpace(r.String("ratingChange")); s != "" {
		if n, err := strconv.Atoi(s); err == nil {
			rc = n
		}
	}
	target := r.Float("indvAimPriceT")
	if target <= 0 {
		target = r.Float("indvAimPriceL")
	}
	return ReportRow{
		InfoCode:     info,
		Title:        r.String("title"),
		Symbol:       sym,
		OrgName:      r.String("orgSName"),
		Researcher:   r.String("researcher"),
		PublishDate:  r.Date("publishDate"),
		Rating:       r.String("emRatingName"),
		LastRating:   r.String("lastEmRatingName"),
		RatingChange: rc,
		TargetPrice:  target,
	}, true
}

// GetOrgSurveys 拉取个股近 days 天的机构调研明细（一机构一行，调用方按调研日聚合）。
// 无调研返回 ErrNoData。
func (e *EastMoneyAdapter) GetOrgSurveys(ctx context.Context, symbol string, days int) ([]SurveyRow, error) {
	if _, ok := cnSecid(symbol); !ok {
		return nil, ErrSymbolInvalid
	}
	if days <= 0 {
		days = 365
	}
	since := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	it := e.DataCenterQuery(DataCenterQuery{
		ReportName:  surveyReport,
		Filter:      fmt.Sprintf("(SECURITY_CODE=\"%s\")(RECEIVE_START_DATE>='%s')", symbol, since),
		SortColumns: "RECEIVE_START_DATE",
		SortTypes:   "-1",
	})
	var out []SurveyRow
	for page := 0; page < surveyMaxPages; page++ {
		raws, err := it.Next(ctx)
		if err != nil {
			return nil, err
		}
		if raws == nil {
			break
		}
		for _, raw := range raws {
			r, perr := ParseDcRow(raw)
			if perr != nil {
				continue
			}
			if row, ok := parseSurveyRow(r); ok {
				out = append(out, row)
			}
		}
	}
	if len(out) == 0 {
		return nil, ErrNoData
	}
	return out, nil
}

func parseSurveyRow(r DcRow) (SurveyRow, bool) {
	sym := r.String("SECURITY_CODE")
	date := r.Date("RECEIVE_START_DATE")
	if sym == "" || date == "" {
		return SurveyRow{}, false
	}
	return SurveyRow{
		Symbol:     sym,
		SurveyDate: date,
		NoticeDate: r.Date("NOTICE_DATE"),
		OrgName:    r.String("RECEIVE_OBJECT"),
		OrgType:    r.String("ORG_TYPE"),
		ReceiveWay: r.String("RECEIVE_WAY_EXPLAIN"),
	}, true
}
