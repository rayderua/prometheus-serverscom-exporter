package refresher

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// API request demand of the configured intervals, and how far they are
// stretched to fit into the request budget.

const (
	// requestsPerL2Segment: members and networks of a segment.
	requestsPerL2Segment = 2

	// defaultAccountLimit is the documented account-wide limit, used until
	// the API reports X-RateLimit-Limit.
	defaultAccountLimit = api.DefaultAccountLimit

	// budgetShare is the share of the budget the refresh loops are planned
	// for. The rest is left for new and changed servers and retries.
	budgetShare = 0.8

	// maxStretch caps the stretch factor when the budget cannot even pay for
	// the server list.
	maxStretch = 100
)

// demandLoop is one refresh loop: requests per pass and its intervals.
type demandLoop struct {
	name        string // resource name: hosts or a collector
	requests    float64
	configured  time.Duration
	interval    time.Duration // effective: configured, stretched if needed
	stretchable bool          // every loop but the server list
	stretched   bool          // the effective interval is longer than the configured one
}

// perHour converts requests per pass into requests per hour at the
// effective interval.
func (l demandLoop) perHour() float64 { return perHour(l.requests, l.interval) }

func perHour(requests float64, interval time.Duration) float64 {
	if interval <= 0 {
		return 0
	}
	return requests * float64(time.Hour) / float64(interval)
}

// demandEstimate is the result of estimateDemand.
type demandEstimate struct {
	loops             []demandLoop
	perHour           float64 // all loops at the effective intervals
	configuredPerHour float64 // all loops at the configured intervals
	fullRefresh       float64 // requests to download everything once
	budget            int
	available         int // account limit minus the reserve
	usable            float64
	recommendedBudget int
	stretch           float64 // effective / configured interval; 1 when everything fits
	stretchHosts      bool    // the server list alone does not fit, so it is stretched too
	accountTooSmall   bool    // even the available account limit needs stretching
}

// intervals returns the effective interval of every loop by name.
func (e demandEstimate) intervals() map[string]time.Duration {
	out := make(map[string]time.Duration, len(e.loops))
	for _, l := range e.loops {
		out[l.name] = l.interval
	}
	return out
}

// demandInputs are the sizes and settings the estimate is based on.
type demandInputs struct {
	hosts, segments, locations, racks, uplinkCatalogues int
	pageSize, budget, reserve, accountLimit             int
	cfg                                                 Config
}

// estimateDemand computes the request demand of the configured intervals and
// stretches them when they do not fit into budgetShare of the budget:
//
//	stretch = stretchable requests per hour / (usable budget - server list requests per hour)
//
// Every loop but the server list is stretched by the same factor: passes come
// less often, instead of using up the budget early in the window and pausing
// until the reset. When the server list alone does not fit, every loop, the
// list included, is stretched by configured requests per hour / usable budget.
func estimateDemand(in demandInputs) demandEstimate {
	pages := func(items int) float64 {
		if in.pageSize <= 0 {
			return 1
		}
		return math.Max(1, math.Ceil(float64(items)/float64(in.pageSize)))
	}

	requests := map[cache.Collector]float64{
		cache.CollectorUplinks:    float64(in.uplinkCatalogues),
		cache.CollectorL2Segments: pages(in.segments) + float64(in.segments*requestsPerL2Segment),
		cache.CollectorReference:  pages(in.locations) + pages(in.racks),
	}
	for _, c := range cache.HostCollectors {
		requests[c] = float64(in.hosts)
	}

	loops := []demandLoop{{name: string(cache.ResourceHosts), requests: pages(in.hosts),
		configured: in.cfg.HostsInterval}}
	for _, c := range cache.Collectors {
		if in.cfg.Disabled.Enabled(c) {
			loops = append(loops, demandLoop{name: string(c), requests: requests[c],
				configured: in.cfg.interval(c), stretchable: true})
		}
	}

	accountLimit := in.accountLimit
	if accountLimit <= 0 {
		accountLimit = defaultAccountLimit
	}
	e := demandEstimate{budget: in.budget, available: max(accountLimit-in.reserve, 0), stretch: 1}
	e.usable = float64(min(in.budget, e.available)) * budgetShare

	var fixed, stretchable float64
	for _, l := range loops {
		if l.stretchable {
			stretchable += perHour(l.requests, l.configured)
		} else {
			fixed += perHour(l.requests, l.configured)
		}
		e.fullRefresh += l.requests
	}
	e.configuredPerHour = fixed + stretchable

	if e.configuredPerHour > e.usable {
		e.stretch = maxStretch
		switch left := e.usable - fixed; {
		case left > 0 && stretchable > 0:
			e.stretch = min(stretchable/left, maxStretch)
		case e.usable > 0:
			e.stretchHosts = true
			e.stretch = min(e.configuredPerHour/e.usable, maxStretch)
		default:
			e.stretchHosts = true
		}
	}
	for i := range loops {
		loops[i].interval = loops[i].configured
		if e.stretch > 1 && (loops[i].stretchable || e.stretchHosts) {
			loops[i].interval = roundDuration(time.Duration(float64(loops[i].configured) * e.stretch))
			loops[i].stretched = true
		}
		e.perHour += loops[i].perHour()
	}

	// Budget the configured intervals need within budgetShare, and at least
	// a full refresh within one window after a cold start, capped by what
	// the account leaves.
	needed := e.configuredPerHour / budgetShare
	wanted := roundUp(math.Max(needed, e.fullRefresh), 50)
	e.recommendedBudget = min(wanted, e.available)
	e.accountTooSmall = needed > float64(e.available)
	e.loops = loops
	return e
}

// recommendation returns the flag to change, e.g. "--api.requests-per-hour=700",
// or "" when the budget is enough.
func (e demandEstimate) recommendation() string {
	if e.recommendedBudget > e.budget && (e.stretch > 1 || e.fullRefresh > float64(e.budget)) {
		return fmt.Sprintf("--api.requests-per-hour=%d", e.recommendedBudget)
	}
	return ""
}

// demandMetrics exposes the estimate and the stretch.
type demandMetrics struct {
	perHour            *prometheus.GaugeVec
	fullRefresh        prometheus.Gauge
	interval           *prometheus.GaugeVec
	intervalConfigured *prometheus.GaugeVec
	stretch            prometheus.Gauge
	budgetRecommended  prometheus.Gauge
}

func newDemandMetrics(reg prometheus.Registerer) *demandMetrics {
	m := &demandMetrics{
		perHour: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_requests_per_hour_estimated",
			Help: "API requests per hour the refresh loops need at their effective intervals, by loop.",
		}, []string{"loop"}),
		fullRefresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_exporter_full_refresh_requests_estimated",
			Help: "API requests needed to download all information once, e.g. after a start without a cache file.",
		}),
		interval: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_seconds",
			Help: "Effective refresh interval by loop: the configured one, stretched when the loops do not fit into the request budget.",
		}, []string{"loop"}),
		intervalConfigured: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_configured_seconds",
			Help: "Refresh interval by loop as configured (--refresh.<collector>-interval).",
		}, []string{"loop"}),
		stretch: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_stretch_factor",
			Help: "How much the refresh intervals are stretched to fit into 80% of the request budget: 1 when the configured intervals fit, above 1 when the budget is the limit.",
		}),
		budgetRecommended: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_api_requests_per_hour_recommended",
			Help: "Recommended --api.requests-per-hour: the configured intervals within 80% of it, at least one full refresh, capped by the account limit minus the reserve.",
		}),
	}
	reg.MustRegister(m.perHour, m.fullRefresh, m.interval, m.intervalConfigured, m.stretch, m.budgetRecommended)
	m.stretch.Set(1) // the configured intervals until the first estimate
	return m
}

func (m *demandMetrics) set(e demandEstimate) {
	for _, l := range e.loops {
		m.perHour.WithLabelValues(l.name).Set(l.perHour())
		m.interval.WithLabelValues(l.name).Set(l.interval.Seconds())
		m.intervalConfigured.WithLabelValues(l.name).Set(l.configured.Seconds())
	}
	m.stretch.Set(e.stretch)
	m.fullRefresh.Set(e.fullRefresh)
	m.budgetRecommended.Set(float64(e.recommendedBudget))
}

// updateDemand recomputes the estimate from the cache, applies the stretch,
// publishes the metrics and logs whenever the result changes.
func (r *Refresher) updateDemand() {
	s := r.store.Snapshot()
	if s.LastSuccess[cache.ResourceHosts].IsZero() {
		return // the estimate depends on the number of servers
	}
	e := estimateDemand(demandInputs{
		hosts:            len(s.Hosts),
		segments:         len(s.Segments),
		locations:        len(s.Locations),
		racks:            len(s.Racks),
		uplinkCatalogues: len(s.Uplinks),
		pageSize:         r.cfg.PageSize,
		budget:           r.cfg.BudgetPerHour,
		reserve:          r.cfg.Reserve,
		accountLimit:     r.api.RateLimit().Limit,
		cfg:              r.cfg,
	})
	intervals := e.intervals()
	r.effective.Store(&intervals)
	r.demand.set(e)

	recommendation := e.recommendation()
	state := fmt.Sprintf("%s %.2f", recommendation, e.stretch)
	r.demandMu.Lock()
	changed := state != r.lastState
	r.lastState = state
	r.demandMu.Unlock()
	if !changed {
		return
	}

	attrs := []any{
		"stretch", math.Round(e.stretch*100) / 100,
		"requests_per_hour", math.Round(e.perHour), "configured_requests_per_hour", math.Round(e.configuredPerHour),
		"full_refresh_requests", math.Round(e.fullRefresh), "budget_per_hour", e.budget,
		"account_available", e.available, "hosts", len(s.Hosts),
	}
	for _, l := range e.loops {
		attrs = append(attrs, l.name+"_interval", formatDuration(l.interval))
	}
	if recommendation != "" {
		attrs = append(attrs, "recommended", recommendation)
	}

	switch {
	case e.accountTooSmall:
		r.logger.Warn("Refresh intervals need more API requests than the account limit leaves for the exporter; "+
			"they are stretched, raise them to make it explicit", attrs...)
	case e.stretch > 1:
		r.logger.Warn("Refresh intervals do not fit into the request budget and are stretched; "+
			"raise the budget to keep them", attrs...)
	case recommendation != "":
		r.logger.Info("Refresh intervals fit into the request budget; a start without a cache file needs more than one budget window",
			attrs...)
	default:
		r.logger.Info("Refresh intervals fit into the request budget", attrs...)
	}
}

// roundUp rounds n up to a multiple of step.
func roundUp(n float64, step int) int {
	return int(math.Ceil(n/float64(step))) * step
}

// roundUpTo rounds d up to a multiple of step.
func roundUpTo(d, step time.Duration) time.Duration {
	return ((d + step - 1) / step) * step
}

// roundDuration rounds d up to a whole minute (or second below a minute).
func roundDuration(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second) + time.Second
	}
	return roundUpTo(d, time.Minute)
}

// FormatDuration prints a duration the way flags accept it, e.g. 1h30m.
func FormatDuration(d time.Duration) string { return formatDuration(d) }

// formatDuration prints durations the way flags accept them, without "0s"
// tails: 1h30m instead of 1h30m0s.
func formatDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
