// Package api is a small read-only client for the Servers.com Public API v1.
//
// It deliberately implements only what the exporter needs:
//   - bearer-token authentication;
//   - Link-header pagination;
//   - the exporter's own hourly request budget: requests are sent without
//     delay until the budget is used up, then the client waits for its reset;
//   - tracking of the account-wide X-RateLimit-* headers, pausing when the
//     account gets close to its limit (the limit is shared with Terraform,
//     CLI and anything else using the same account).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	// DefaultBaseURL is the production API endpoint.
	DefaultBaseURL = "https://api.servers.com/v1"

	// maxErrorBodyBytes limits how much of an error response we keep for logs.
	maxErrorBodyBytes = 512
)

// Options configures a Client.
type Options struct {
	BaseURL   string
	Token     string
	UserAgent string
	Timeout   time.Duration

	// PageSize is sent as per_page for collection endpoints.
	PageSize int

	// BudgetPerHour is the maximum number of requests per hourly window this
	// exporter may issue. Requests are not delayed while the window has room;
	// once it is used up, the client waits until the window resets.
	BudgetPerHour int

	// Reserve is the number of account-wide requests left untouched for other
	// API consumers: once X-RateLimit-Remaining drops to this value the client
	// waits until the rate limit window resets.
	Reserve int

	Logger     *slog.Logger
	Registerer prometheus.Registerer
}

// Client is safe for concurrent use.
type Client struct {
	baseURL    *url.URL
	token      string
	userAgent  string
	pageSize   int
	reserve    int
	httpClient *http.Client
	budget     *budget
	logger     *slog.Logger
	metrics    *clientMetrics

	mu        sync.Mutex
	rateLimit RateLimit

	// pauseLoggedFor is the reset time of the last logged pause, so a pause is
	// logged once rather than by every waiting request.
	pauseLoggedFor time.Time
}

// RateLimit is the account-wide rate limit state reported by the API.
type RateLimit struct {
	Limit     int
	Remaining int
	Reset     time.Time
}

// NewClient validates options and builds a Client.
func NewClient(opts Options) (*Client, error) {
	if opts.Token == "" {
		return nil, errors.New("api token is empty")
	}
	if opts.BudgetPerHour <= 0 {
		return nil, errors.New("request budget per hour must be positive")
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultBaseURL
	}
	base, err := url.Parse(strings.TrimRight(opts.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("parse api url: %w", err)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Registerer == nil {
		opts.Registerer = prometheus.NewRegistry()
	}

	c := &Client{
		baseURL:    base,
		token:      opts.Token,
		userAgent:  opts.UserAgent,
		pageSize:   opts.PageSize,
		reserve:    opts.Reserve,
		httpClient: &http.Client{Timeout: opts.Timeout},
		budget:     newBudget(opts.BudgetPerHour, time.Hour),
		logger:     opts.Logger,
		metrics:    newClientMetrics(opts.Registerer, opts.BudgetPerHour, opts.Reserve),
	}
	c.registerBudgetMetrics(opts.Registerer)
	return c, nil
}

// BudgetPerHour returns the exporter's hourly request budget.
func (c *Client) BudgetPerHour() int {
	return c.budget.limit
}

// RateLimit returns the last account-wide rate limit state seen in responses.
func (c *Client) RateLimit() RateLimit {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.rateLimit
}

// Error is returned for non-2xx API responses.
type Error struct {
	Endpoint   string
	StatusCode int
	Body       string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: unexpected status %d: %s", e.Endpoint, e.StatusCode, e.Body)
}

// IsNotFound reports whether err is an API 404, e.g. a server released
// between listing and fetching its details.
func IsNotFound(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// IsRateLimited reports whether err is an API 429.
func IsRateLimited(err error) bool {
	var apiErr *Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests
}

// get fetches one object and decodes it into out.
func (c *Client) get(ctx context.Context, endpoint, path string, out any) error {
	_, err := c.do(ctx, endpoint, c.resolve(path, nil), out)
	return err
}

// list walks all pages of a collection endpoint following rel="next" links.
// It is a function rather than a method because methods cannot be generic.
func list[T any](ctx context.Context, c *Client, endpoint, path string) ([]T, error) {
	query := url.Values{}
	if c.pageSize > 0 {
		query.Set("per_page", strconv.Itoa(c.pageSize))
	}

	var all []T
	next := c.resolve(path, query)
	for next != "" {
		var page []T
		nextURL, err := c.do(ctx, endpoint, next, &page)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		next = nextURL
	}
	return all, nil
}

// resolve builds an absolute URL for an API path.
func (c *Client) resolve(path string, query url.Values) string {
	u := *c.baseURL
	u.Path = c.baseURL.Path + path
	u.RawQuery = query.Encode()
	return u.String()
}

// do performs a single GET request, decodes the JSON body into out and returns
// the URL of the next page, if any.
func (c *Client) do(ctx context.Context, endpoint, rawURL string, out any) (string, error) {
	if err := c.waitForTurn(ctx); err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	c.metrics.duration.WithLabelValues(endpoint).Observe(time.Since(start).Seconds())
	if err != nil {
		c.metrics.requests.WithLabelValues(endpoint, "error").Inc()
		return "", fmt.Errorf("%s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	c.metrics.requests.WithLabelValues(endpoint, strconv.Itoa(resp.StatusCode)).Inc()
	c.updateRateLimit(resp.Header)
	c.logger.Debug("API request", "endpoint", endpoint, "status", resp.StatusCode,
		"duration", time.Since(start).Round(time.Millisecond), "ratelimit_remaining", c.RateLimit().Remaining,
		"budget_remaining", c.budget.state(time.Now()).Remaining)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		if resp.StatusCode == http.StatusTooManyRequests {
			c.pauseAfterRateLimited()
		}
		return "", &Error{Endpoint: endpoint, StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(body))}
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return "", fmt.Errorf("%s: decode response: %w", endpoint, err)
	}
	return c.nextPageURL(resp.Header), nil
}

// waitForTurn blocks until the request fits into both the exporter's own
// budget and the account-wide rate limit reserve.
func (c *Client) waitForTurn(ctx context.Context) error {
	err := c.budget.wait(ctx, func(s budgetState) {
		c.metrics.budgetExhausted.Inc()
		c.logger.Warn("Exporter request budget is used up, waiting for the hourly reset",
			"budget_per_hour", c.budget.limit, "used", s.Used, "reset_at", s.Reset.Format(time.RFC3339),
			"resume_in", time.Until(s.Reset).Round(time.Second))
	})
	if err != nil {
		return err
	}

	rl := c.RateLimit()
	if rl.Limit == 0 || rl.Remaining > c.reserve || !time.Now().Before(rl.Reset) {
		return nil
	}

	wait := time.Until(rl.Reset)
	c.mu.Lock()
	firstWaiter := !c.pauseLoggedFor.Equal(rl.Reset)
	c.pauseLoggedFor = rl.Reset
	c.mu.Unlock()
	if firstWaiter {
		c.logger.Warn("Account rate limit reserve reached, pausing API requests",
			"remaining", rl.Remaining, "reserve", c.reserve, "resume_in", wait.Round(time.Second))
	}
	return sleep(ctx, wait)
}

// updateRateLimit stores the X-RateLimit-* headers of a response.
func (c *Client) updateRateLimit(h http.Header) {
	limit, errLimit := strconv.Atoi(h.Get("X-RateLimit-Limit"))
	remaining, errRemaining := strconv.Atoi(h.Get("X-RateLimit-Remaining"))
	reset, errReset := strconv.ParseInt(h.Get("X-RateLimit-Reset"), 10, 64)
	if errLimit != nil || errRemaining != nil || errReset != nil {
		return
	}

	c.mu.Lock()
	c.rateLimit = RateLimit{Limit: limit, Remaining: remaining, Reset: time.Unix(reset, 0)}
	c.mu.Unlock()

	c.metrics.rateLimit.Set(float64(limit))
	c.metrics.rateRemaining.Set(float64(remaining))
	c.metrics.rateReset.Set(float64(reset))
}

// pauseAfterRateLimited makes the next requests wait for the window reset
// even if the 429 response carried no rate limit headers.
func (c *Client) pauseAfterRateLimited() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rateLimit.Remaining = 0
	if c.rateLimit.Limit == 0 {
		c.rateLimit.Limit = 1
	}
	if !time.Now().Before(c.rateLimit.Reset) {
		c.rateLimit.Reset = time.Now().Add(time.Minute)
	}
}

// nextPageURL extracts the rel="next" target from a Link header. Links to
// other hosts are ignored so the token is never sent anywhere else.
func (c *Client) nextPageURL(h http.Header) string {
	for link := range strings.SplitSeq(h.Get("Link"), ",") {
		parts := strings.Split(link, ";")
		if len(parts) < 2 {
			continue
		}
		target := strings.Trim(strings.TrimSpace(parts[0]), "<>")

		isNext := false
		for _, param := range parts[1:] {
			if strings.TrimSpace(param) == `rel="next"` {
				isNext = true
			}
		}
		if !isNext {
			continue
		}

		next, err := c.baseURL.Parse(target)
		if err != nil || next.Host != c.baseURL.Host {
			c.logger.Warn("Ignoring unexpected pagination link", "link", target)
			return ""
		}
		return next.String()
	}
	return ""
}

// sleep waits for d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// registerBudgetMetrics exposes the state of the exporter's own budget,
// mirroring the serverscom_api_ratelimit_* metrics of the account limit.
func (c *Client) registerBudgetMetrics(reg prometheus.Registerer) {
	state := func() budgetState { return c.budget.state(time.Now()) }
	reg.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "serverscom_api_budget_used",
			Help: "Requests the exporter sent in the current hourly budget window.",
		}, func() float64 { return float64(state().Used) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "serverscom_api_budget_remaining",
			Help: "Requests left in the current hourly budget window of the exporter (--api.requests-per-hour).",
		}, func() float64 { return float64(state().Remaining) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "serverscom_api_budget_reset_timestamp_seconds",
			Help: "Unix time when the exporter's current budget window resets; 0 when no window is open.",
		}, func() float64 {
			if reset := state().Reset; !reset.IsZero() {
				return float64(reset.Unix())
			}
			return 0
		}),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "serverscom_api_budget_exhausted",
			Help: "1 if the exporter used up its budget in the current window and waits (or waited) for the reset.",
		}, func() float64 {
			if state().Exhausted {
				return 1
			}
			return 0
		}),
	)
}

// clientMetrics describes the exporter's own API usage.
type clientMetrics struct {
	budgetExhausted prometheus.Counter

	requests      *prometheus.CounterVec
	duration      *prometheus.HistogramVec
	rateLimit     prometheus.Gauge
	rateRemaining prometheus.Gauge
	rateReset     prometheus.Gauge
}

// newClientMetrics registers the client metrics. budgetPerHour and reserve
// are exposed as constant gauges, so dashboards can forecast whether the
// exporter fits into the account rate limit.
func newClientMetrics(reg prometheus.Registerer, budgetPerHour, reserve int) *clientMetrics {
	m := &clientMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "serverscom_api_requests_total",
			Help: "Requests sent to the Servers.com API by endpoint and HTTP status code.",
		}, []string{"endpoint", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "serverscom_api_request_duration_seconds",
			Help:    "Duration of Servers.com API requests.",
			Buckets: []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		}, []string{"endpoint"}),
		rateLimit: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_api_ratelimit_limit",
			Help: "Account-wide API request limit per window, from X-RateLimit-Limit.",
		}),
		rateRemaining: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_api_ratelimit_remaining",
			Help: "Account-wide API requests remaining in the current window, from X-RateLimit-Remaining.",
		}),
		rateReset: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_api_ratelimit_reset_timestamp_seconds",
			Help: "Unix time when the current API rate limit window resets, from X-RateLimit-Reset.",
		}),
	}
	budget := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "serverscom_api_requests_budget_per_hour",
		Help: "Maximum API requests per hour this exporter may issue (--api.requests-per-hour).",
	})
	budget.Set(float64(budgetPerHour))
	reserveGauge := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "serverscom_api_ratelimit_reserve",
		Help: "Account-wide requests left for other API users; the exporter pauses when X-RateLimit-Remaining drops to it (--api.ratelimit-reserve).",
	})
	reserveGauge.Set(float64(reserve))

	m.budgetExhausted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "serverscom_api_budget_exhausted_total",
		Help: "How many hourly budget windows the exporter used up before they reset.",
	})

	reg.MustRegister(m.requests, m.duration, m.rateLimit, m.rateRemaining, m.rateReset, budget, reserveGauge, m.budgetExhausted)
	return m
}
