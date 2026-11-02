package datasource

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// CNInfoAdapter uses the public disclosure website, not the token-based webapi.
// Organization identifiers are resolved from the official search response.
type CNInfoAdapter struct{}

func NewCNInfoAdapter() *CNInfoAdapter       { return &CNInfoAdapter{} }
func (provider *CNInfoAdapter) Name() string { return "cninfo" }

func (provider *CNInfoAdapter) GetQuote(context.Context, string, string) (*Quote, error) {
	return nil, ErrNotSupported
}

func (provider *CNInfoAdapter) GetDailyBars(context.Context, string, string, int) ([]Bar, error) {
	return nil, ErrNotSupported
}

func (provider *CNInfoAdapter) GetAnnouncements(ctx context.Context, market, symbol string, limit int) ([]EMAnnouncement, error) {
	if market != "cn" {
		return nil, ErrNotSupported
	}
	symbol = strings.TrimSpace(symbol)
	if _, supported := cnSecid(symbol); !supported {
		return nil, ErrSymbolInvalid
	}
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	headers := map[string]string{"Referer": "https://www.cninfo.com.cn/"}
	searchBody, status, err := doPostForm(ctx, "https://www.cninfo.com.cn/new/information/topSearch/query",
		url.Values{"keyWord": {symbol}, "maxNum": {"10"}}, headers)
	if err != nil {
		return nil, fmt.Errorf("%w: cninfo symbol lookup: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: cninfo symbol lookup HTTP %d", ErrUpstream, status)
	}
	organizationID, err := parseCNInfoOrganization(searchBody, symbol)
	if err != nil {
		return nil, err
	}
	column := "szse"
	if strings.HasPrefix(symbol, "6") {
		column = "sse"
	}
	parameters := url.Values{
		"pageNum": {"1"}, "pageSize": {strconv.Itoa(limit)}, "column": {column},
		"tabName": {"fulltext"}, "stock": {symbol + "," + organizationID},
		"searchkey": {""}, "secid": {""}, "plate": {""}, "category": {""},
		"trade": {""}, "seDate": {""}, "sortName": {"time"}, "sortType": {"desc"}, "isHLtitle": {"true"},
	}
	body, status, err := doPostForm(ctx, "https://www.cninfo.com.cn/new/hisAnnouncement/query", parameters, headers)
	if err != nil {
		return nil, fmt.Errorf("%w: cninfo announcements: %w", ErrUpstream, err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%w: cninfo announcements HTTP %d", ErrUpstream, status)
	}
	return parseCNInfoAnnouncements(body, symbol)
}

func parseCNInfoOrganization(body []byte, symbol string) (string, error) {
	var securities []struct {
		Code           string `json:"code"`
		OrganizationID string `json:"orgId"`
	}
	if err := json.Unmarshal(body, &securities); err != nil {
		return "", fmt.Errorf("%w: cninfo symbol response: %w", ErrUpstream, err)
	}
	for _, security := range securities {
		if security.Code == symbol && strings.TrimSpace(security.OrganizationID) != "" {
			return security.OrganizationID, nil
		}
	}
	return "", ErrNoData
}

func parseCNInfoAnnouncements(body []byte, symbol string) ([]EMAnnouncement, error) {
	var response struct {
		Announcements []struct {
			Code       string `json:"secCode"`
			Name       string `json:"secName"`
			Identifier string `json:"announcementId"`
			Title      string `json:"announcementTitle"`
			Timestamp  int64  `json:"announcementTime"`
			Attachment string `json:"adjunctUrl"`
		} `json:"announcements"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("%w: cninfo announcement response: %w", ErrUpstream, err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil || envelope["announcements"] == nil {
		return nil, fmt.Errorf("%w: cninfo announcement list is missing", ErrUpstream)
	}
	announcements := make([]EMAnnouncement, 0, len(response.Announcements))
	for _, record := range response.Announcements {
		if record.Code != symbol || record.Timestamp < 631152000000 || record.Timestamp > 253402300799000 || strings.TrimSpace(record.Title) == "" {
			continue
		}
		if identifier, err := strconv.ParseUint(record.Identifier, 10, 64); err != nil || identifier == 0 {
			continue
		}
		attachment := path.Clean(record.Attachment)
		if !strings.HasPrefix(attachment, "finalpage/") || !strings.EqualFold(path.Ext(attachment), ".pdf") || strings.ContainsAny(attachment, "\\?#") {
			continue
		}
		announcements = append(announcements, EMAnnouncement{
			ArtCode:    "cninfo:" + record.Identifier,
			Title:      html.UnescapeString(stripSinaNewsHTML(record.Title)),
			NoticeDate: time.UnixMilli(record.Timestamp).In(ChinaMarketLocation()),
			Symbol:     symbol, Name: strings.TrimSpace(record.Name), Source: "cninfo",
			URL: "https://static.cninfo.com.cn/" + attachment,
		})
	}
	if len(announcements) == 0 {
		return nil, ErrNoData
	}
	return announcements, nil
}
