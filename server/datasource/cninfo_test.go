package datasource

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

const cninfoAnnouncementFixture = `{"announcements":[
{"secCode":"600000","secName":"Test Bank","announcementId":"1225620612","announcementTitle":"<em>Annual</em> &amp; audit report","announcementTime":1792771200000,"adjunctUrl":"finalpage/2026-10-24/1225620612.PDF"},
{"secCode":"000001","announcementId":"1225620613","announcementTitle":"Other issuer","announcementTime":1792771200000,"adjunctUrl":"finalpage/2026-10-24/1225620613.PDF"},
{"secCode":"600000","announcementId":"1225620614","announcementTitle":"Unknown date","adjunctUrl":"finalpage/2026-10-24/1225620614.PDF"},
{"secCode":"600000","announcementId":"1225620615","announcementTitle":"Bad link","announcementTime":1792771200000,"adjunctUrl":"https://example.invalid/forged.PDF"}
]}`

func TestCNInfoAnnouncementIdentityDateAndURL(test *testing.T) {
	announcements, err := parseCNInfoAnnouncements([]byte(cninfoAnnouncementFixture), "600000")
	if err != nil || len(announcements) != 1 {
		test.Fatalf("unexpected announcements: %+v %v", announcements, err)
	}
	announcement := announcements[0]
	if announcement.ArtCode != "cninfo:1225620612" || announcement.Source != "cninfo" ||
		announcement.Title != "Annual & audit report" || announcement.NoticeDate.Format("2006-01-02") != "2026-10-24" ||
		announcement.URL != "https://static.cninfo.com.cn/finalpage/2026-10-24/1225620612.PDF" {
		test.Fatalf("official announcement facts changed: %+v", announcement)
	}
	if _, err := parseCNInfoAnnouncements([]byte(`{"error":"unavailable"}`), "600000"); !errors.Is(err, ErrUpstream) {
		test.Fatalf("missing list was mistaken for no announcements: %v", err)
	}
	if _, err := parseCNInfoAnnouncements([]byte(`{"announcements":[]}`), "600000"); !errors.Is(err, ErrNoData) {
		test.Fatalf("valid empty list should be no data: %v", err)
	}
}

func TestCNInfoLookupRequiresExactSymbol(test *testing.T) {
	response := []byte(`[{"code":"000001","orgId":"wrong"},{"code":"600000","orgId":"official-id"}]`)
	if identifier, err := parseCNInfoOrganization(response, "600000"); err != nil || identifier != "official-id" {
		test.Fatalf("lookup guessed the issuer: %q %v", identifier, err)
	}
	if _, err := parseCNInfoOrganization(response, "600001"); !errors.Is(err, ErrNoData) {
		test.Fatalf("missing symbol must not borrow another organization: %v", err)
	}
}

func TestCNInfoAnnouncementFallbackUsesReadOnlyOfficialEndpoints(test *testing.T) {
	previousClient := httpClient
	previousLimiter := cninfoLimiter
	cninfoLimiter = newHTTPSourceLimiter(1, 0)
	test.Cleanup(func() { httpClient, cninfoLimiter = previousClient, previousLimiter })
	requests := 0
	httpClient = &http.Client{Transport: reviewTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		body, status := "", http.StatusOK
		switch request.URL.Host {
		case "np-anotice-stock.eastmoney.com":
			status = http.StatusForbidden
		case "www.cninfo.com.cn":
			if request.Method != http.MethodPost || request.Header.Get("Referer") != "https://www.cninfo.com.cn/" {
				test.Error("official read-only form request missing expected method or referer")
			}
			if err := request.ParseForm(); err != nil {
				test.Fatal(err)
			}
			if strings.HasSuffix(request.URL.Path, "topSearch/query") {
				if request.Form.Get("keyWord") != "600000" {
					test.Error("lookup symbol changed")
				}
				body = `[{"code":"600000","orgId":"official-id"}]`
			} else if request.URL.Path == "/new/hisAnnouncement/query" {
				if request.Form.Get("stock") != "600000,official-id" || request.Form.Get("column") != "sse" || request.Form.Get("pageSize") != "2" {
					test.Error("announcement parameters lost issuer or page limit")
				}
				body = cninfoAnnouncementFixture
			} else {
				test.Fatalf("unexpected path: %s", request.URL.Path)
			}
		default:
			test.Fatalf("unexpected network host: %s", request.URL.Host)
		}
		return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	manager := NewManagerWithAdapters(NewEastMoneyAdapter(), NewCNInfoAdapter())
	announcements, err := manager.GetAnnouncements(context.Background(), "cn", "600000", 2)
	if err != nil || len(announcements) != 1 || announcements[0].Source != "cninfo" || requests != 3 {
		test.Fatalf("official fallback failed: %+v err=%v requests=%d", announcements, err, requests)
	}
}
