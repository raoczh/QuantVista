package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 东财公告接口（F1）：np-anotice-stock.eastmoney.com/api/security/ann，无鉴权、最稳公告源。
// 原文链接按 art_code 拼 data.eastmoney.com/notices/detail/{symbol}/{art_code}.html。

// EMAnnouncement 公告条目（标准化输出）。
type EMAnnouncement struct {
	ArtCode    string
	Title      string
	NoticeType string // 公告类型（columns[0].column_name，如「董事会决议公告」）
	NoticeDate time.Time
	Symbol     string
	Name       string
	URL        string
}

type emAnnResp struct {
	Success int `json:"success"`
	Data    struct {
		List []struct {
			ArtCode string `json:"art_code"`
			Title   string `json:"title"`
			Codes   []struct {
				StockCode string `json:"stock_code"`
				ShortName string `json:"short_name"`
			} `json:"codes"`
			Columns []struct {
				ColumnName string `json:"column_name"`
			} `json:"columns"`
			NoticeDate string `json:"notice_date"` // 2026-07-03 00:00:00
		} `json:"list"`
		TotalHits int `json:"total_hits"`
	} `json:"data"`
}

// GetEMAnnouncements 拉取单只 A 股的最新公告一页（按时间倒序）。
func GetEMAnnouncements(ctx context.Context, symbol string, pageSize int) ([]EMAnnouncement, error) {
	if pageSize <= 0 || pageSize > 100 {
		pageSize = 30
	}
	u := fmt.Sprintf("https://np-anotice-stock.eastmoney.com/api/security/ann?sr=-1&page_size=%d&page_index=1&ann_type=A&client_source=web&stock_list=%s&f_node=0&s_node=0",
		pageSize, symbol)
	body, status, err := doGet(ctx, u, map[string]string{"Referer": "https://data.eastmoney.com/"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	if status != 200 {
		return nil, fmt.Errorf("%w: http %d", ErrUpstream, status)
	}
	var resp emAnnResp
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("%w: 公告解析失败 %v", ErrUpstream, err)
	}
	if resp.Success != 1 {
		return nil, fmt.Errorf("%w: 公告接口 success=%d", ErrUpstream, resp.Success)
	}
	loc := time.FixedZone("CST", 8*3600)
	items := make([]EMAnnouncement, 0, len(resp.Data.List))
	for _, r := range resp.Data.List {
		if strings.TrimSpace(r.ArtCode) == "" {
			continue
		}
		nd, perr := time.ParseInLocation("2006-01-02 15:04:05", r.NoticeDate, loc)
		if perr != nil {
			nd = time.Now()
		}
		a := EMAnnouncement{
			ArtCode:    r.ArtCode,
			Title:      strings.TrimSpace(r.Title),
			NoticeDate: nd,
			Symbol:     symbol,
			URL:        fmt.Sprintf("https://data.eastmoney.com/notices/detail/%s/%s.html", symbol, r.ArtCode),
		}
		if len(r.Columns) > 0 {
			a.NoticeType = strings.TrimSpace(r.Columns[0].ColumnName)
		}
		// codes 里回填与请求代码一致的简称（一份公告可能挂多只关联标的）。
		for _, c := range r.Codes {
			if c.StockCode == symbol {
				a.Name = strings.TrimSpace(c.ShortName)
				break
			}
		}
		items = append(items, a)
	}
	if len(items) == 0 {
		return nil, ErrNoData
	}
	return items, nil
}
