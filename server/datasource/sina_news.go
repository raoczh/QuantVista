package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// GetSinaFastNews reads the public 7x24 feed's first page without credentials.
// The verified feed supplies id, rich_text, create_time and an optional docurl.
// Embedded stock keywords are not reliable associations and are not propagated.
func GetSinaFastNews(ctx context.Context, pageSize int) ([]EMNewsItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pageSize <= 0 || pageSize > 20 {
		pageSize = 20
	}
	endpoint := fmt.Sprintf("https://zhibo.sina.com.cn/api/zhibo/feed?zhibo_id=152&tag_id=0&page=1&page_size=%d&dire=f", pageSize)
	body, status, err := doGet(ctx, endpoint, map[string]string{"Referer": "https://finance.sina.com.cn/7x24/"})
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, fmt.Errorf("%w: sina fast news request: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: sina fast news HTTP %d", ErrUpstream, status)
	}
	items, err := parseSinaFastNews(body)
	if err != nil {
		return nil, err
	}
	if len(items) > pageSize {
		items = items[:pageSize]
	}
	return items, nil
}

func parseSinaFastNews(body []byte) ([]EMNewsItem, error) {
	var response struct {
		Result struct {
			Status struct {
				Code *int `json:"code"`
			} `json:"status"`
			Data struct {
				Feed struct {
					List []struct {
						ID         json.Number `json:"id"`
						RichText   string      `json:"rich_text"`
						CreateTime string      `json:"create_time"`
						DocURL     string      `json:"docurl"`
						IsDelete   int         `json:"is_delete"`
					} `json:"list"`
				} `json:"feed"`
			} `json:"data"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("%w: sina fast news JSON: %v", ErrUpstream, err)
	}
	if response.Result.Status.Code == nil || *response.Result.Status.Code != 0 {
		return nil, fmt.Errorf("%w: sina fast news unsuccessful response", ErrUpstream)
	}
	items := make([]EMNewsItem, 0, len(response.Result.Data.Feed.List))
	seen := make(map[string]EMNewsItem, len(response.Result.Data.Feed.List))
	for _, entry := range response.Result.Data.Feed.List {
		identifier, err := entry.ID.Int64()
		publishTime, validTime := parseSinaDateTime(entry.CreateTime)
		if err != nil || identifier <= 0 || !validTime || entry.IsDelete != 0 {
			continue // Missing publication time must never become the collection time.
		}
		plainText := stripSinaNewsHTML(entry.RichText)
		if plainText == "" {
			continue
		}
		title := plainText
		// A bracketed headline is part of the source text, not an inferred summary.
		if strings.HasPrefix(plainText, "\u3010") {
			if endIndex := strings.Index(plainText, "\u3011"); endIndex > len("\u3010") {
				title = strings.TrimSpace(plainText[len("\u3010"):endIndex])
			}
		}
		item := EMNewsItem{
			SourceID: entry.ID.String(), Title: truncateSinaNewsText(title, 512),
			Summary: truncateSinaNewsText(plainText, 1024), PublishTime: publishTime,
		}
		articleURL, err := url.Parse(strings.TrimSpace(entry.DocURL))
		if err == nil && articleURL.Host != "" && (articleURL.Scheme == "https" || articleURL.Scheme == "http") {
			item.URL = articleURL.String()
		}
		if previous, duplicate := seen[item.SourceID]; duplicate {
			if previous.Title != item.Title || previous.Summary != item.Summary ||
				previous.URL != item.URL || !previous.PublishTime.Equal(item.PublishTime) {
				return nil, fmt.Errorf("%w: sina fast news conflicting source IDs", ErrUpstream)
			}
			continue
		}
		seen[item.SourceID] = item
		items = append(items, item)
	}
	if len(items) == 0 {
		return nil, ErrNoData
	}
	return items, nil
}

func stripSinaNewsHTML(value string) string {
	tokenizer := html.NewTokenizer(strings.NewReader(value))
	var plainText strings.Builder
	hiddenElement := ""
	for {
		switch tokenizer.Next() {
		case html.ErrorToken:
			return strings.Join(strings.Fields(plainText.String()), " ")
		case html.TextToken:
			if hiddenElement == "" {
				plainText.Write(tokenizer.Text())
			}
		case html.StartTagToken, html.EndTagToken, html.SelfClosingTagToken:
			token := tokenizer.Token()
			if hiddenElement != "" {
				if token.Type == html.EndTagToken && token.Data == hiddenElement {
					hiddenElement = ""
				}
				continue
			}
			switch token.Data {
			case "script", "style", "iframe", "noscript", "template":
				if token.Type == html.StartTagToken {
					hiddenElement = token.Data
				}
			case "br", "p", "div", "li":
				plainText.WriteByte(' ')
			}
		}
	}
}

// Preserve source wording as a prefix, without ellipses or generated sentences.
// Limits match the existing news title and summary storage contracts.
func truncateSinaNewsText(value string, limit int) string {
	characters := []rune(value)
	if len(characters) > limit {
		return string(characters[:limit])
	}
	return value
}
