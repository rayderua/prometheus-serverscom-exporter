package refresher

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// API request demand of the current configuration: the auto interval, the
// estimate per collector, and recommendations when the fixed intervals do not
// fit into the request budget.

const (
	// requestsPerL2Segment: members and networks of a segment.
	requestsPerL2Segment = 2

	// defaultAccountLimit is the documented account-wide limit, used until
	// the API reports X-RateLimit-Limit.
	defaultAccountLimit = 2000

	// autoIntervalStep is what the auto interval is rounded up to.
	autoIntervalStep = 5 * time.Minute
)

// demandLoop is one refresh loop: requests per run and its interval.
type demandLoop struct {
	name        string // resource name: hosts or a collector
	requests    float64
	interval    time.Duration // effective, auto included
	auto        bool
	recommended time.Duration
}

// perHour converts requests per run into requests per hour.
func (l demandLoop) perHour() float64 {
	if l.interval <= 0 {
		return 0
	}
	return l.requests * float64(time.Hour) / float64(l.interval)
}

// flag returns the flag that configures the loop's interval.
func (l demandLoop) flag() string { return "--refresh." + l.name + "-interval" }

// demandEstimate is the result of estimateDemand.
type demandEstimate struct {
	loops             []demandLoop
	perHour           float64 // all loops together
	fixedPerHour      float64 // loops with a fixed interval
	fullRefresh       float64 // requests to download everything once
	budget            int
	available         int // account limit minus the reserve
	recommendedBudget int
	autoInterval      time.Duration // 0 without auto collectors
	autoTooSlow       bool          // auto collectors do not fit even at the maximum interval
	intervalsTooShort bool          // fixed loops do not fit into the available account limit
	budgetTooSmall    bool          // fixed loops, auto ones at the maximum, or a full refresh do not fit
}

// demandInputs are the sizes and settings the estimate is based on.
type demandInputs struct {
	hosts, segments, locations, racks, uplinkCatalogues int
	pageSize, budget, reserve, accountLimit             int
	cfg                                                 Config
}

// estimateDemand computes the auto interval and the steady-state request
// demand of the configuration.
//
// Auto collectors get the share of the budget left after the fixed ones:
//
//	auto interval = auto requests per run / (budget * autoBudgetShare - fixed requests per hour)
//
// rounded up to autoIntervalStep and clamped to the configured bounds.
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

	loops := []demandLoop{{name: string(cache.ResourceHosts), requests: pages(in.hosts), interval: in.cfg.HostsInterval}}
	for _, c := range cache.Collectors {
		if !in.cfg.Disabled.Enabled(c) {
			continue
		}
		i := in.cfg.interval(c)
		loops = append(loops, demandLoop{name: string(c), requests: requests[c], interval: i.Fixed, auto: i.Auto})
	}

	accountLimit := in.accountLimit
	if accountLimit <= 0 {
		accountLimit = defaultAccountLimit
	}
	e := demandEstimate{budget: in.budget, available: max(accountLimit-in.reserve, 0)}

	var autoRequests float64
	for _, l := range loops {
		if l.auto {
			autoRequests += l.requests
		} else {
			e.fixedPerHour += l.perHour()
		}
		e.fullRefresh += l.requests
	}

	// Auto interval from what the fixed loops leave.
	usable := float64(min(in.budget, e.available)) * autoBudgetShare
	if autoRequests > 0 {
		left := usable - e.fixedPerHour
		e.autoInterval = in.cfg.AutoMaxInterval
		if left > 0 {
			raw := time.Duration(autoRequests / left * float64(time.Hour))
			e.autoInterval = min(max(roundUpTo(raw, autoIntervalStep), in.cfg.AutoMinInterval), in.cfg.AutoMaxInterval)
			e.autoTooSlow = raw > in.cfg.AutoMaxInterval
		} else {
			e.autoTooSlow = true
		}
	}
	for i := range loops {
		if loops[i].auto {
			loops[i].interval = e.autoInterval
		}
		loops[i].recommended = loops[i].interval
		e.perHour += loops[i].perHour()
	}

	// Budget the configuration needs: the fixed loops and the auto ones at
	// the maximum interval within the auto share, and a full refresh within
	// one window after a cold start, capped by what the account leaves.
	var autoAtMax float64
	if in.cfg.AutoMaxInterval > 0 {
		autoAtMax = autoRequests * float64(time.Hour) / float64(in.cfg.AutoMaxInterval)
	}
	needed := (e.fixedPerHour + autoAtMax) / autoBudgetShare
	wanted := roundUp(math.Max(needed, e.fullRefresh), 50)
	e.recommendedBudget = min(wanted, e.available)
	e.budgetTooSmall = wanted > in.budget

	// If even the available account limit is not enough, raise the fixed
	// intervals proportionally until they fit.
	if limit := float64(e.available) * autoBudgetShare; needed*autoBudgetShare > limit && limit > autoAtMax && e.fixedPerHour > 0 {
		e.intervalsTooShort = true
		scale := e.fixedPerHour / (limit - autoAtMax)
		for i := range loops {
			if !loops[i].auto && loops[i].interval > 0 {
				loops[i].recommended = roundDuration(time.Duration(float64(loops[i].interval) * scale))
			}
		}
	}
	e.loops = loops
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
			flags = append(flags, fmt.Sprintf("%s=%s", l.flag(), formatDuration(l.recommended)))
		}
	}
	return strings.Join(flags, " ")
}

// demandMetrics exposes the estimate and the recommendations.
type demandMetrics struct {
	perHour             *prometheus.GaugeVec
	fullRefresh         prometheus.Gauge
	interval            *prometheus.GaugeVec
	intervalAuto        *prometheus.GaugeVec
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
			Help: "Effective refresh interval by loop, including the computed auto interval.",
		}, []string{"loop"}),
		intervalAuto: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_auto",
			Help: "Whether the loop's interval is auto (1) or fixed (0).",
		}, []string{"loop"}),
		intervalRecommended: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "serverscom_exporter_refresh_interval_recommended_seconds",
			Help: "Refresh interval that fits the demand into the account rate limit; equals the effective one when it fits.",
		}, []string{"loop"}),
		budgetRecommended: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "serverscom_api_requests_per_hour_recommended",
			Help: "Recommended --api.requests-per-hour: fixed loops and auto ones at the maximum interval within 80% of it, at least one full refresh, capped by the account limit minus the reserve.",
		}),
	}
	reg.MustRegister(m.perHour, m.fullRefresh, m.interval, m.intervalAuto, m.intervalRecommended, m.budgetRecommended)
	return m
}

func (m *demandMetrics) set(e demandEstimate) {
	for _, l := range e.loops {
		m.perHour.WithLabelValues(l.name).Set(l.perHour())
		m.interval.WithLabelValues(l.name).Set(l.interval.Seconds())
		m.intervalAuto.WithLabelValues(l.name).Set(boolValue(l.auto))
		m.intervalRecommended.WithLabelValues(l.name).Set(l.recommended.Seconds())
	}
	m.fullRefresh.Set(e.fullRefresh)
	m.budgetRecommended.Set(float64(e.recommendedBudget))
}

// updateDemand recomputes the estimate from the cache, applies the auto
// interval, publishes the metrics and logs whenever the result changes.
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
	r.autoInterval.Store(int64(e.autoInterval))
	r.demand.set(e)

	recommendation := e.recommendation()
	state := fmt.Sprint(recommendation, e.autoInterval, e.autoTooSlow)
	r.demandMu.Lock()
	changed := state != r.lastState
	r.lastState = state
	r.demandMu.Unlock()
	if !changed {
		return
	}

	attrs := []any{
		"auto_interval", formatDuration(e.autoInterval),
		"requests_per_hour", math.Round(e.perHour), "fixed_requests_per_hour", math.Round(e.fixedPerHour),
		"full_refresh_requests", math.Round(e.fullRefresh), "budget_per_hour", e.budget,
		"account_available", e.available, "hosts", len(s.Hosts),
	}
	for _, l := range e.loops {
		attrs = append(attrs, l.name+"_per_hour", math.Round(l.perHour()))
	}
	if recommendation != "" {
		attrs = append(attrs, "recommended", recommendation)
	}

	switch {
	case e.intervalsTooShort:
		r.logger.Warn("Fixed refresh intervals need more API requests than the account limit leaves for the exporter; "+
			"raise them", attrs...)
	case e.autoTooSlow:
		r.logger.Warn("Auto collectors do not fit into the request budget even at the maximum interval; "+
			"raise the budget or the fixed intervals", attrs...)
	case recommendation != "":
		r.logger.Warn("API demand does not fit into the request budget; the exporter will wait for budget resets", attrs...)
	default:
		r.logger.Info("Refresh intervals computed", attrs...)
	}
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
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
