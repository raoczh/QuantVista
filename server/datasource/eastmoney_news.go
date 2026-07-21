package datasource

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// 东财新闻两路（N1）：
//  1. 7×24 快讯 np-weblist getFastNewsList —— req_trace=uuid 缺失返回空（实测坑），
//     sortEnd 为翻页游标（首页传空）。
//  2. 个股新闻 search-api-web JSONP —— type=cmsArticleWebOld，剥 callback 壳 + 去 <em> 高亮。
//     有 TLS 指纹风险：标准 client 实测可用则用，被拒由调用方降级（不阻塞前两源）。

// EMNewsItem 东财新闻条目（标准化输出）。
type EMNewsItem struct {
	SourceID    string
	Title       string
	Summary     string
	URL         string
	PublishTime time.Time
	Symbols     []string // 已归一为本项目 6 位代码
}

// randomTrace 生成 req_trace 用的 32 位 hex（等价 uuid4().hex）。
func randomTrace() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// normalizeEMStockCode 东财 stockList 代码形如 "1.688035"（市场.代码）或 "90.BK1175"（板块）。
// 只保留 0/1 市场前缀的 6 位数字代码，板块与其它市场丢弃。
func normalizeEMStockCode(code string) (string, bool) {
	parts := strings.SplitN(strings.TrimSpace(code), ".", 2)
	if len(parts) != 2 {
		return "", false
	}
	if parts[0] != "0" && parts[0] != "1" {
		return "", false
	}
	s := parts[1]
	if len(s) != 6 {
		return "", false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return "", false
		}
	}
	return s, true
}

type emFastResp struct {
	Code string `json:"code"`
	Data struct {
		SortEnd      string `json:"sortEnd"`
		FastNewsList []struct {
			Code      string   `json:"code"`
			Title     string   `json:"title"`
			Summary   string   `json:"summary"`
			ShowTime  string   `json:"showTime"`  // 2026-07-06 16:56:06（北京时间）
			StockList []string `json:"stockList"` // 元素形如 "1.688035" / "90.BK1175"
		} `json:"fastNewsList"`
	} `json:"data"`
}

func parseEMPublishTime(value string) (time.Time, bool) {
	loc := time.FixedZone("CST", 8*3600)
	pt, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(value), loc)
	return pt, err == nil
}

// GetEMFastNews 拉取东财 7×24 快讯一页；返回条目与下一页游标 sortEnd。
func GetEMFastNews(ctx context.Context, sortEnd string, pageSize int) ([]EMNewsItem, string, error) {
	if pageSize <= 0 || pageSize > 50 {
		pageSize = 20
	}
	u := fmt.Sprintf("https://np-weblist.eastmoney.com/comm/web/getFastNewsList?client=web&biz=web_724&fastColumn=102&sortEnd=%s&pageSize=%d&req_trace=%s",
		url.QueryEscape(sortEnd), pageSize, randomTrace())
	body, status, err := doGet(ctx, u, map[string]string{
		"Referer": "https://www.eastmoney.com/",
	})
	if err != nil {
		return nil, "", err
	}
	if status != 200 {
		return nil, "", fmt.Errorf("em fastnews HTTP %d", status)
	}
	var resp emFastResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, "", fmt.Errorf("em fastnews 解析失败: %w", err)
	}
	if resp.Code != "1" {
		return nil, "", fmt.Errorf("em fastnews code=%s", resp.Code)
	}
	items := make([]EMNewsItem, 0, len(resp.Data.FastNewsList))
	for _, r := range resp.Data.FastNewsList {
		pt, ok := parseEMPublishTime(r.ShowTime)
		if !ok {
			continue
		}
		var syms []string
		for _, st := range r.StockList {
			if code, ok := normalizeEMStockCode(st); ok {
				syms = append(syms, code)
			}
		}
		items = append(items, EMNewsItem{
			SourceID:    r.Code,
			Title:       strings.TrimSpace(r.Title),
			Summary:     strings.TrimSpace(r.Summary),
			PublishTime: pt,
			Symbols:     syms,
		})
	}
	return items, resp.Data.SortEnd, nil
}

// --- 个股新闻搜索（JSONP） ---

var (
	emTagRe   = regexp.MustCompile(`</?em>`)
	jsonpWrap = regexp.MustCompile(`(?s)^[^(]*\((.*)\)\s*;?\s*$`)
)

type emSearchResp struct {
	Code   int `json:"code"`
	Result struct {
		CmsArticleWebOld []struct {
			Code    string `json:"code"`
			Title   string `json:"title"`
			Content string `json:"content"`
			URL     string `json:"url"`
			Date    string `json:"date"` // 2026-07-04 10:20:11
		} `json:"cmsArticleWebOld"`
	} `json:"result"`
}

// GetEMStockNews 按 6 位代码搜索个股新闻（最多 pageSize 条，按时间倒序）。
// TLS 指纹被拒时返回错误，调用方降级不阻断其它源。
func GetEMStockNews(ctx context.Context, symbol string, pageSize int) ([]EMNewsItem, error) {
	if pageSize <= 0 || pageSize > 20 {
		pageSize = 10
	}
	param := fmt.Sprintf(`{"uid":"","keyword":"%s","type":["cmsArticleWebOld"],"client":"web","clientType":"web","clientVersion":"curr","param":{"cmsArticleWebOld":{"searchScope":"default","sort":"time","pageIndex":1,"pageSize":%d,"preTag":"<em>","postTag":"</em>"}}}`,
		symbol, pageSize)
	u := "https://search-api-web.eastmoney.com/search/jsonp?cb=jq&param=" + url.QueryEscape(param)
	body, status, err := doGet(ctx, u, map[string]string{
		"Referer": "https://guba.eastmoney.com/",
	})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return nil, fmt.Errorf("em stocknews HTTP %d", status)
	}
	payload := stripJSONP(string(body))
	var resp emSearchResp
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return nil, fmt.Errorf("em stocknews 解析失败: %w", err)
	}
	items := make([]EMNewsItem, 0, len(resp.Result.CmsArticleWebOld))
	for _, r := range resp.Result.CmsArticleWebOld {
		pt, ok := parseEMPublishTime(r.Date)
		if !ok {
			continue
		}
		items = append(items, EMNewsItem{
			SourceID:    r.Code,
			Title:       stripEMTags(r.Title),
			Summary:     stripEMTags(r.Content),
			URL:         strings.TrimSpace(r.URL),
			PublishTime: pt,
			Symbols:     []string{symbol},
		})
	}
	return items, nil
}

// stripJSONP 剥掉 jq(...) callback 壳；无壳则原样返回。
func stripJSONP(s string) string {
	s = strings.TrimSpace(s)
	if m := jsonpWrap.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

// stripEMTags 去掉搜索高亮 <em></em> 标签。
func stripEMTags(s string) string {
	return strings.TrimSpace(emTagRe.ReplaceAllString(s, ""))
}
