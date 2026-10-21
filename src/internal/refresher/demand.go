package refresher

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// API request demand of the current configuration, and recommendations when
// it does not fit into the request budget.

const (
	// requestsPerHostDetails is the number of requests a detail refresh of one
	// server takes: the server itself, drive slots, connections, networks,
	// features and power feeds.
	requestsPerHostDetails = 6

	// requestsPerL2Segment: members and networks of a segment.
	requestsPerL2Segment = 2

	// defaultAccountLimit is the documented account-wide limit, used until
	// the API reports X-RateLimit-Limit.
	defaultAccountLimit = 2000

	// demandHeadroom is the margin recommendations keep above the estimated
	// demand, for host changes, retries and pagination growth.
	demandHeadroom = 1.25
)

// Demand loops, used as the "loop" label and to name refresh flags.
const (
	demandHosts     = "hosts"
	demandDetails   = "host_details"
	demandFeatures  = "host_features"
	demandL2        = "l2_segments"
	demandReference = "reference"
)

// intervalFlags maps loops to the flags that configure their intervals.
var intervalFlags = map[string]string{
	demandHosts:     "--refresh.hosts-interval",
	demandDetails:   "--refresh.details-interval",
	demandFeatures:  "--refresh.features-interval",
	demandL2:        "--refresh.l2-interval",
	demandReference: "--refresh.reference-interval",
}

// demandLoop describes one refresh loop: requests per run and its interval.
type demandLoop struct {
	name        string
	requests    float64
	interval    time.Duration
	recommended time.Duration
	scalable    bool // the interval may be raised to fit the budget
}

// perHour converts requests per run into requests per hour.
func (l demandLoop) perHour() float64 {
	if l.interval <= 0 {
		return 0 // disabled loop
	}
	return l.requests * float64(time.Hour) / float64(l.interval)
}

// demandEstimate is the result of estimateDemand.
type demandEstimate struct {
	loops             []demandLoop
	perHour           float64 // all loops together
	fullRefresh       float64 // requests to download everything once
	budget            int
	available         int // account limit minus the reserve
	recommendedBudget int
	intervalsTooShort bool // demand does not fit even into the available account limit
	budgetTooSmall    bool // demand or a full refresh does not fit into the budget
}

// demandInputs are the sizes and settings the estimate is based on.
type demandInputs struct {
	hosts, segments, locations, racks, uplinkCatalogues int
	pageSize, budget, reserve, accountLimit             int
	cfg                                                 Config
}

// estimateDemand computes the steady-state request demand of the current
// configuration and the budget and intervals that would fit it.
func estimateDemand(in demandInputs) demandEstimate {
	pages := func(items int) float64 {
		if in.pageSize <= 0 {
			return 1
		}
		return math.Max(1, math.Ceil(float64(items)/float64(in.pageSize)))
	}

	loops := []demandLoop{
		{name: demandHosts, requests: pages(in.hosts), interval: in.cfg.HostsInterval, scalable: true},
		{name: demandDetails, requests: float64(in.hosts * requestsPerHostDetails), interval: in.cfg.DetailsInterval, scalable: true},
		{name: demandFeatures, requests: float64(in.hosts), interval: in.cfg.FeaturesInterval, scalable: true},
		{name: demandL2, requests: pages(in.segments) + float64(in.segments*requestsPerL2Segment), interval: in.cfg.L2Interval, scalable: true},
		{name: demandReference, requests: pages(in.locations) + pages(in.racks) + float64(in.uplinkCatalogues), interval: in.cfg.ReferenceInterval},
	}

	e := demandEstimate{loops: loops, budget: in.budget}
	for _, l := range loops {
		e.perHour += l.perHour()
		e.fullRefresh += l.requests
	}
	// Features come with the details on a full refresh.
	e.fullRefresh -= loops[2].requests

	accountLimit := in.accountLimit
	if accountLimit <= 0 {
		accountLimit = defaultAccountLimit
	}
	e.available = max(accountLimit-in.reserve, 0)

	// Budget: the steady-state demand with headroom, and enough to download
	// everything within one window after a cold start, capped by what the
	// account leaves for the exporter.
	wanted := roundUp(math.Max(e.perHour*demandHeadroom, e.fullRefresh), 50)
	e.recommendedBudget = min(wanted, e.available)
	e.budgetTooSmall = wanted > in.budget

	// Intervals: if even the available account limit is not enough, raise
	// the scalable intervals proportionally until the demand fits.
	scale := 1.0
	if limit := float64(e.available); e.perHour*demandHeadroom > limit && limit > 0 {
		e.intervalsTooShort = true
		scale = e.perHour * demandHeadroom / limit
	}
	for i := range e.loops {
		l := &e.loops[i]
		l.recommended = l.interval
		if l.scalable && l.interval > 0 && scale > 1 {
			l.recommended = roundDuration(time.Duration(float64(l.interval) * scale))
		}
	}
	return e
}

// recommendation returns the flags to change, e.g. "--api.requests-per-hour=700".
func (e demandEstimate) recommendation() string {
	var flags []string
	if e.recommendedBudget != e.budget && (e.budgetTooSmall || e.intervalsTooShort) {
		flags = append(flags, fmt.Sprintf("--api.requests-per-hour=%d", e.recommendedBudget))
	}
	for _, l := range e.loops {
		if l.recommended != l.interval {
			flags = append(flags, fmt.Sprintf("%s=%s", intervalFlags[l.name], formatDuration(l.recommended)))
		}
	}
	return strings.Join(flags, " ")
}

// demandMetrics exposes the estimate and the recommendations.
type demandMetrics struct {
	perHour             *prometheus.GaugeVec
	fullRefresh         prometheus.Gauge
	interval            *prometheus.GaugeVec
	intervalRecommended *prometheus.GaugeVec
	budgetRecommended   prometheus.Gauge
}

func newDemandMetrics(reg prometheus.Registerer) *demandMetrics {
	m := &demandMetrics{
		perHour: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_requests_per_hour_estimated",
			Help: "API requests per hour the current configuration needs in steady state, by refresh loop.",
		}, []string{"loop"}),
		fullRefresh: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_exporter_full_refresh_requests_estimated",
			Help: "API requests needed to download all information once, e.g. after a start without a cache file.",
		}),
		interval: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_seconds",
			Help: "Configured refresh interval by loop; 0 means the loop is disabled.",
		}, []string{"loop"}),
		intervalRecommended: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_recommended_seconds",
			Help: "Refresh interval that fits the demand into the account rate limit; equals the configured one when it fits.",
		}, []string{"loop"}),
		budgetRecommended: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_api_requests_per_hour_recommended",
			Help: "Recommended --api.requests-per-hour: estimated demand with 25% headroom, at least one full refresh, capped by the account limit minus the reserve.",
		}),
	}
	reg.MustRegister(m.perHour, m.fullRefresh, m.interval, m.intervalRecommended, m.budgetRecommended)
	return m
}

func (m *demandMetrics) set(e demandEstimate) {
	for _, l := range e.loops {
		m.perHour.WithLabelValues(l.name).Set(l.perHour())
		m.interval.WithLabelValues(l.name).Set(l.interval.Seconds())
		m.intervalRecommended.WithLabelValues(l.name).Set(l.recommended.Seconds())
	}
	m.fullRefresh.Set(e.fullRefresh)
	m.budgetRecommended.Set(float64(e.recommendedBudget))
}

// updateDemand recomputes the estimate from the cache, publishes the metrics
// and logs a recommendation whenever it changes.
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
	r.demand.set(e)

	recommendation := e.recommendation()
	r.demandMu.Lock()
	changed := recommendation != r.lastRecommendation
	r.lastRecommendation = recommendation
	r.demandMu.Unlock()
	if !changed {
		return
	}

	attrs := []any{
		"requests_per_hour", math.Round(e.perHour), "full_refresh_requests", math.Round(e.fullRefresh),
		"budget_per_hour", e.budget, "account_available", e.available, "hosts", len(s.Hosts),
	}
	for _, l := range e.loops {
		attrs = append(attrs, l.name+"_per_hour", math.Round(l.perHour()))
	}

	switch {
	case recommendation == "":
		r.logger.Info("API demand fits into the request budget", attrs...)
	case e.intervalsTooShort:
		r.logger.Warn("Refresh intervals need more API requests than the account limit leaves for the exporter; "+
			"raise the intervals", append(attrs, "recommended", recommendation)...)
	default:
		r.logger.Warn("API demand does not fit into the request budget; the exporter will wait for budget resets",
			append(attrs, "recommended", recommendation)...)
	}
}

// roundUp rounds n up to a multiple of step.
func roundUp(n float64, step int) int {
	return int(math.Ceil(n/float64(step))) * step
}

// roundDuration rounds d up to a whole minute (or second below a minute).
func roundDuration(d time.Duration) time.Duration {
	if d < time.Minute {
		return d.Round(time.Second) + time.Second
	}
	return ((d + time.Minute - 1) / time.Minute) * time.Minute
}

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
