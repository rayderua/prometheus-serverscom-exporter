package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *prometheus.Registry) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	reg := prometheus.NewRegistry()
	client, err := NewClient(Options{
		BaseURL:       server.URL + "/v1",
		Token:         "secret",
		PageSize:      2,
		BudgetPerHour: 1_000_000, // effectively no pacing in tests
		Reserve:       10,
		Registerer:    reg,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, reg
}

func TestListFollowsPaginationAndSendsToken(t *testing.T) {
	var serverURL string
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if r.URL.Path != "/v1/racks" {
			t.Errorf("path = %q", r.URL.Path)
		}
		switch r.URL.Query().Get("page") {
		case "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/v1/racks?page=2&per_page=2>; rel="next", <%s/v1/racks?page=2&per_page=2>; rel="last"`, serverURL, serverURL))
			_, _ = fmt.Fprint(w, `[{"id":"r1","name":"rack-1"},{"id":"r2","name":"rack-2"}]`)
		case "2":
			_, _ = fmt.Fprint(w, `[{"id":"r3","name":"rack-3"}]`)
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	})
	serverURL = client.baseURL.Scheme + "://" + client.baseURL.Host

	racks, err := client.ListRacks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(racks) != 3 || racks[2].Name != "rack-3" {
		t.Fatalf("racks = %+v", racks)
	}
}

func TestPaginationIgnoresForeignHosts(t *testing.T) {
	calls := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Link", `<https://evil.example.com/v1/racks?page=2>; rel="next"`)
		_, _ = fmt.Fprint(w, `[]`)
	})

	if _, err := client.ListRacks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestRateLimitHeadersAndMetrics(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	client, reg := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "2000")
		w.Header().Set("X-RateLimit-Remaining", "1500")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset))
		_, _ = fmt.Fprint(w, `[]`)
	})

	if _, err := client.ListLocations(context.Background()); err != nil {
		t.Fatal(err)
	}

	rl := client.RateLimit()
	if rl.Limit != 2000 || rl.Remaining != 1500 || rl.Reset.Unix() != reset {
		t.Fatalf("rate limit = %+v", rl)
	}

	expected := `
# HELP serverscom_api_ratelimit_remaining Account-wide API requests remaining in the current window, from X-RateLimit-Remaining.
# TYPE serverscom_api_ratelimit_remaining gauge
serverscom_api_ratelimit_remaining 1500
# HELP serverscom_api_requests_total Requests sent to the Servers.com API by endpoint and HTTP status code.
# TYPE serverscom_api_requests_total counter
serverscom_api_requests_total{code="200",endpoint="locations"} 1
# HELP serverscom_api_ratelimit_reserve Account-wide requests left for other API users; the exporter pauses when X-RateLimit-Remaining drops to it (--api.ratelimit-reserve).
# TYPE serverscom_api_ratelimit_reserve gauge
serverscom_api_ratelimit_reserve 10
# HELP serverscom_api_requests_budget_per_hour Maximum API requests per hour this exporter may issue (--api.requests-per-hour).
# TYPE serverscom_api_requests_budget_per_hour gauge
serverscom_api_requests_budget_per_hour 1e+06
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(expected),
		"serverscom_api_ratelimit_remaining", "serverscom_api_requests_total",
		"serverscom_api_ratelimit_reserve", "serverscom_api_requests_budget_per_hour"); err != nil {
		t.Fatal(err)
	}
}

func TestReservePausesRequests(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, `[]`)
	})
	client.rateLimit = RateLimit{Limit: 2000, Remaining: 5, Reset: time.Now().Add(time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.ListRacks(ctx); err == nil {
		t.Fatal("expected the request to wait for the rate limit reset")
	}
}

func TestErrorClassification(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/missing") {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, "slow down", http.StatusTooManyRequests)
	})

	_, err := client.GetDedicatedServer(context.Background(), "missing")
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}

	_, err = client.GetDedicatedServer(context.Background(), "other")
	if !IsRateLimited(err) {
		t.Fatalf("expected rate limited, got %v", err)
	}
	if rl := client.RateLimit(); rl.Remaining != 0 || !rl.Reset.After(time.Now()) {
		t.Fatalf("expected a pause after 429, got %+v", rl)
	}
}

func TestRateLimitedWithoutHeadersAssumesTheDocumentedLimit(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	if _, err := client.ListRacks(context.Background()); !IsRateLimited(err) {
		t.Fatalf("err = %v, want rate limited", err)
	}
	if rl := client.RateLimit(); rl.Limit != DefaultAccountLimit || rl.Remaining != 0 || !rl.Reset.After(time.Now()) {
		t.Fatalf("rate limit = %+v", rl)
	}
}

func TestRestoredRateLimitPausesBeforeTheFirstRequest(t *testing.T) {
	requests := 0
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		_, _ = fmt.Fprint(w, `[]`)
	})

	// A window that has reset already is ignored.
	client.RestoreRateLimit(RateLimit{Limit: 2000, Remaining: 0, Reset: time.Now().Add(-time.Minute)})
	if _, err := client.ListRacks(context.Background()); err != nil || requests != 1 {
		t.Fatalf("err = %v, requests = %d", err, requests)
	}

	// Remaining at the reserve (10): the next request waits for the reset.
	client.RestoreRateLimit(RateLimit{Limit: 2000, Remaining: 5, Reset: time.Now().Add(time.Hour)})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.ListRacks(ctx); err == nil || requests != 1 {
		t.Fatalf("err = %v, requests = %d; want a pause without a request", err, requests)
	}
}

func TestOnRateLimitGetsEveryHeaderUpdate(t *testing.T) {
	reset := time.Now().Add(30 * time.Minute).Unix()
	client, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "2000")
		w.Header().Set("X-RateLimit-Remaining", "1500")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset))
		_, _ = fmt.Fprint(w, `[]`)
	})
	var got []RateLimit
	client.OnRateLimit(func(rl RateLimit) { got = append(got, rl) })
	if _, err := client.ListRacks(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Remaining != 1500 || got[0].Reset.Unix() != reset {
		t.Fatalf("hook calls = %+v", got)
	}
}
