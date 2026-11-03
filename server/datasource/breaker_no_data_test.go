package datasource

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

func TestBreakerNullDataReachesCapabilitiesWithoutRecovery(test *testing.T) {
	for _, capability := range []struct {
		name   string
		family string
		call   func(context.Context, *EastMoneyAdapter) error
	}{
		{"quote", "push2", func(ctx context.Context, adapter *EastMoneyAdapter) error {
			_, err := adapter.GetQuote(ctx, "cn", "600000")
			return err
		}},
		{"daily", "push2his", func(ctx context.Context, adapter *EastMoneyAdapter) error {
			_, err := adapter.GetDailyBars(ctx, "cn", "600000", 1)
			return err
		}},
		{"empty_event_pool", "push2ex", func(ctx context.Context, adapter *EastMoneyAdapter) error {
			_, err := adapter.GetZBPool(ctx, "20261102")
			return err
		}},
	} {
		test.Run(capability.name, func(test *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				fmt.Fprint(writer, `{"rc":0,"data":null}`)
			}))
			defer server.Close()
			adapter := NewEastMoneyAdapter()
			adapter.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
				return doGet(ctx, server.URL, headers)
			}
			adapter.br.streak[capability.family] = emBreakThreshold - 1
			err := capability.call(test.Context(), adapter)
			if !errors.Is(err, ErrNoData) || errors.Is(err, ErrUpstream) || calls.Load() != 1 || adapter.br.streak[capability.family] != emBreakThreshold-1 {
				test.Fatalf("null data must remain empty without restoring breaker health: calls=%d streak=%d err=%v", calls.Load(), adapter.br.streak[capability.family], err)
			}
		})
	}
}

func TestBreakerNullDataKeepsRecoveryCooldown(test *testing.T) {
	for _, degraded := range []bool{false, true} {
		test.Run(fmt.Sprint(degraded), func(test *testing.T) {
			adapter, hosts := newTestEM(func(string) ([]byte, int, error) {
				return []byte(`{"rc":0,"data":null}`), http.StatusOK, nil
			})
			now := time.Now()
			adapter.br.now = func() time.Time { return now }
			family := "push2his"
			if degraded {
				family = "push2"
				adapter.br.degrade(family)
			} else {
				for failure := 0; failure < emBreakThreshold; failure++ {
					adapter.br.fail(family)
				}
			}
			now = now.Add(emBreakCooldown)
			var err error
			if degraded {
				_, err = adapter.GetQuote(test.Context(), "cn", "600000")
			} else {
				_, err = adapter.GetDailyBars(test.Context(), "cn", "600000", 1)
			}
			if !errors.Is(err, ErrNoData) || len(*hosts) != 1 || adapter.br.probing[family] {
				test.Fatalf("empty recovery probe changed semantics or stayed busy: hosts=%v err=%v", *hosts, err)
			}
			if degraded {
				if !adapter.br.isDegraded(family) || !adapter.br.degradedUntil[family].Equal(now.Add(emBreakCooldown)) {
					test.Fatal("empty primary response cleared degraded routing")
				}
			} else if adapter.br.allow(family) || !adapter.br.openUntil[family].Equal(now.Add(emBreakCooldown)) {
				test.Fatal("empty half-open response cleared circuit cooldown")
			}
		})
	}
}

func TestBreakerNullBackupDoesNotClearPrimaryFailure(test *testing.T) {
	var primaryCalls, backupCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("upstream") == emBackupHost {
			backupCalls.Add(1)
			fmt.Fprint(writer, `{"rc":0,"data":null}`)
			return
		}
		primaryCalls.Add(1)
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	adapter := NewEastMoneyAdapter()
	adapter.br.streak["push2"] = 1
	adapter.fetch = func(ctx context.Context, rawURL string, headers map[string]string) ([]byte, int, error) {
		upstream, err := url.Parse(rawURL)
		if err != nil {
			return nil, 0, err
		}
		return doGetWithPolicy(ctx, server.URL+"?upstream="+url.QueryEscape(upstream.Hostname()), headers, httpRequestPolicy{retryBackoff: time.Millisecond})
	}
	_, err := adapter.GetQuote(test.Context(), "cn", "600000")
	if !errors.Is(err, ErrNoData) || adapter.br.streak["push2"] != 2 || adapter.br.isDegraded("push2") || primaryCalls.Load() != 3 || backupCalls.Load() != 1 {
		test.Fatalf("empty backup was treated as recovery: primary=%d backup=%d streak=%d err=%v", primaryCalls.Load(), backupCalls.Load(), adapter.br.streak["push2"], err)
	}
}

func TestBreakerNullDataValidationPreservesErrorsAndOtherShapes(test *testing.T) {
	if err := validateEMFetch([]byte(`{"rc":0,"data":null}`), http.StatusOK, nil); !errors.Is(err, ErrNoData) {
		test.Fatalf("explicit null data must not validate as recovery: %v", err)
	}
	for _, payload := range []string{`{"rc":0,"data":{"tc":0,"pool":[]}}`, `{"rc":0,"data":{}}`, `{"$type":"1"}`, `{}`} {
		if err := validateEMFetch([]byte(payload), http.StatusOK, nil); err != nil {
			test.Errorf("API-specific empty shape was rejected: payload=%s err=%v", payload, err)
		}
	}
	for _, status := range []int{http.StatusForbidden, http.StatusServiceUnavailable} {
		err := validateEMFetch([]byte(`{"rc":0,"data":null}`), status, nil)
		var statusFailure *httpStatusError
		if !errors.As(err, &statusFailure) || statusFailure.statusCode != status || errors.Is(err, ErrNoData) {
			test.Errorf("null payload masked failing HTTP status %d: %v", status, err)
		}
	}
	for _, payload := range []string{`{"rc":1,"data":null}`, `{"success":false,"rc":0,"data":null}`} {
		if err := validateEMFetch([]byte(payload), http.StatusOK, nil); !errors.Is(err, ErrUpstream) || errors.Is(err, ErrNoData) {
			test.Errorf("null payload masked business rejection: payload=%s err=%v", payload, err)
		}
	}
}

func TestManagerNullDataFallsBackAndRecordsEmpty(test *testing.T) {
	adapter, hosts := newTestEM(func(string) ([]byte, int, error) {
		return []byte(`{"rc":0,"data":null}`), http.StatusOK, nil
	})
	adapter.br.streak["push2"] = 2
	backup := &probeFakeAdapter{name: "tencent", quote: func(context.Context) (*Quote, error) {
		return &Quote{Symbol: "600000", Price: 10, Source: "tencent"}, nil
	}}
	manager := NewManagerWithAdapters(adapter, backup)
	quote, err := manager.GetQuote(test.Context(), "cn", "600000")
	row := probeHealthRow(test, manager, "eastmoney", "quote", "cn")
	if err != nil || quote == nil || quote.Source != "tencent" || row.Empty != 1 || row.Errors != 0 || row.Success != 0 || len(*hosts) != 1 || adapter.br.streak["push2"] != 2 {
		test.Fatalf("null quote incorrectly changed source health or fallback: quote=%+v health=%+v hosts=%v err=%v", quote, row, *hosts, err)
	}
}
