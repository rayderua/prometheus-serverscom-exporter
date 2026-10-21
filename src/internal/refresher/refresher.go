// Package refresher keeps the cache up to date while spending as few API
// requests as possible.
//
// Every collector (API endpoint) has its own refresh interval, fixed or auto:
//
//	hosts         the server list: power, statuses, labels, updated_at (fixed)
//	host_details  all collectors of new servers and servers whose updated_at
//	              changed, checked every hosts interval (outside the schedule)
//	per server    configuration, drives, ports, networks, features, power-feeds:
//	              one queue per collector, oldest server first, requests spread
//	              evenly over the interval
//	account-wide  uplinks, l2-segments, reference
//
// Auto collectors share one interval: the budget left after the fixed ones,
// minus a headroom, divided by their cost (see demand.go). When the budget
// window runs low, auto collectors pause first and leave the rest to the
// fixed ones and to new servers.
package refresher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// API is the subset of api.Client used by the refresher.
type API interface {
	ListDedicatedServers(ctx context.Context) ([]api.DedicatedServer, error)
	GetDedicatedServer(ctx context.Context, id string) (*api.DedicatedServer, error)
	ListDriveSlots(ctx context.Context, serverID string) ([]api.DriveSlot, error)
	ListConnections(ctx context.Context, serverID string) ([]api.Connection, error)
	ListNetworks(ctx context.Context, serverID string) ([]api.Network, error)
	ListFeatures(ctx context.Context, serverID string) ([]api.Feature, error)
	ListPowerFeeds(ctx context.Context, serverID string) ([]api.PowerFeed, error)
	ListL2Segments(ctx context.Context) ([]api.L2Segment, error)
	ListL2Members(ctx context.Context, segmentID string) ([]api.L2Member, error)
	ListL2Networks(ctx context.Context, segmentID string) ([]api.Network, error)
	ListLocations(ctx context.Context) ([]api.Location, error)
	ListRacks(ctx context.Context) ([]api.Rack, error)
	ListUplinkModels(ctx context.Context, locationID, serverModelID int64) ([]api.UplinkModel, error)
	RateLimit() api.RateLimit
	BudgetRemaining() int
}

// Config holds refresh intervals.
type Config struct {
	// HostsInterval is how often the server list is fetched. New and changed
	// servers are downloaded on the same schedule.
	HostsInterval time.Duration

	// Intervals per collector; a missing entry is auto.
	Intervals map[cache.Collector]Interval

	// Bounds of the auto interval.
	AutoMinInterval time.Duration
	AutoMaxInterval time.Duration

	// Disabled collectors are not fetched.
	Disabled cache.DisabledCollectors

	// The API client settings below are used to compute the auto interval,
	// to estimate the request demand and to recommend a budget.
	PageSize      int
	BudgetPerHour int
	Reserve       int
}

// interval returns the configured interval of c.
func (c Config) interval(collector cache.Collector) Interval {
	if i, ok := c.Intervals[collector]; ok {
		return i
	}
	return AutoInterval
}

// Timing of the schedulers.
const (
	// recheckPeriod is the longest a loop sleeps before looking again: the
	// auto interval and the set of servers change while it waits.
	recheckPeriod = time.Minute

	// retryDelay is the pause after a failed run of an account-wide loop.
	retryDelay = 5 * time.Minute
)

// Refresher runs the refresh loops.
type Refresher struct {
	api    API
	store  *cache.Store
	cfg    Config
	logger *slog.Logger

	errors *prometheus.CounterVec
	demand *demandMetrics

	// autoInterval is the current auto interval in nanoseconds; 0 until the
	// first estimate.
	autoInterval atomic.Int64
	autoPaused   atomic.Bool // auto collectors wait for the budget to reset

	demandMu  sync.Mutex
	lastState string // last logged demand state, to log only changes
}

// New creates a Refresher, registers its metrics and drops cached data that
// the configuration no longer refreshes.
func New(client API, store *cache.Store, cfg Config, logger *slog.Logger, reg prometheus.Registerer) *Refresher {
	r := &Refresher{
		api:    client,
		store:  store,
		cfg:    cfg,
		logger: logger,
		errors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "serverscom_exporter_refresh_errors_total",
			Help: "Failed cache refresh attempts by resource.",
		}, []string{"resource"}),
	}
	reg.MustRegister(r.errors)
	r.demand = newDemandMetrics(reg)
	r.lastState = "-" // log the first estimate even when it has no recommendation
	r.prepareCache()
	return r
}

func (r *Refresher) enabled(c cache.Collector) bool { return r.cfg.Disabled.Enabled(c) }

// interval returns the effective interval of c.
func (r *Refresher) interval(c cache.Collector) time.Duration {
	if i := r.cfg.interval(c); !i.Auto {
		return i.Fixed
	}
	if d := time.Duration(r.autoInterval.Load()); d > 0 {
		return d
	}
	return r.cfg.AutoMaxInterval // no estimate yet
}

// prepareCache removes data of disabled collectors, e.g. loaded from a cache
// file written with them on, and refresh timestamps of resources that no
// longer run, so staleness alerts do not fire for them.
func (r *Refresher) prepareCache() {
	active := map[cache.Resource]bool{cache.ResourceHosts: true, cache.ResourceHostDetails: true}
	for _, c := range cache.Collectors {
		if r.enabled(c) {
			active[cache.Resource(c)] = true
		}
	}

	r.store.Update(func(s *cache.Snapshot) {
		for resource := range s.LastSuccess {
			if !active[resource] {
				delete(s.LastSuccess, resource)
			}
		}
		if !r.enabled(cache.CollectorL2Segments) {
			s.Segments = map[string]*cache.Segment{}
		}
		if !r.enabled(cache.CollectorUplinks) {
			s.Uplinks = map[string]*cache.UplinkModels{}
		}
		for id, host := range s.Hosts {
			if host.Details == nil {
				continue
			}
			details := *host.Details // copy-on-write
			details.Fetched = maps.Clone(details.Fetched)
			for _, c := range cache.HostCollectors {
				if !r.enabled(c) {
					clearHostCollector(&details, c)
					delete(details.Fetched, c)
				}
			}
			s.Hosts[id] = &cache.Host{Server: host.Server, Details: &details}
		}
	})
}

// clearHostCollector drops the data of collector c from details.
func clearHostCollector(d *cache.HostDetails, c cache.Collector) {
	copyHostCollector(d, &cache.HostDetails{}, c)
}

// copyHostCollector copies the data of collector c from src to dst.
func copyHostCollector(dst, src *cache.HostDetails, c cache.Collector) {
	switch c {
	case cache.CollectorDrives:
		dst.DriveSlots = src.DriveSlots
	case cache.CollectorPorts:
		dst.Connections = src.Connections
	case cache.CollectorNetworks:
		dst.Networks = src.Networks
	case cache.CollectorFeatures:
		dst.Features = src.Features
	case cache.CollectorPowerFeeds:
		dst.PowerFeeds = src.PowerFeeds
	}
}

// Run starts all loops and blocks until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	snapshot := r.store.Snapshot()
	withDetails := 0
	for _, host := range snapshot.Hosts {
		if host.Details != nil {
			withDetails++
		}
	}
	attrs := []any{
		"cached_hosts", len(snapshot.Hosts), "cached_host_details", withDetails,
		"disabled_collectors", fmt.Sprint(r.cfg.Disabled.List()), "hosts_interval", r.cfg.HostsInterval,
	}
	for _, c := range cache.Collectors {
		if r.enabled(c) {
			attrs = append(attrs, string(c)+"_interval", r.cfg.interval(c).String())
		}
	}
	attrs = append(attrs, "auto_min_interval", r.cfg.AutoMinInterval, "auto_max_interval", r.cfg.AutoMaxInterval)
	r.logger.Info("Refresher started", attrs...)

	// On a cold start fetch racks first: host metrics carry the rack name, and
	// without the rack list they would briefly expose rack IDs, leaving stale
	// values in Grafana's rack variable. Costs two requests.
	if snapshot.LastSuccess[cache.Resource(cache.CollectorReference)].IsZero() {
		if err := r.refreshReference(ctx); err != nil {
			r.errors.WithLabelValues(string(cache.CollectorReference)).Inc()
			r.logger.Warn("Initial reference refresh failed", "err", err)
		}
	}

	// Fetch the server list once before starting the loops, so the other
	// loops have servers and an auto interval right away.
	if err := r.refreshHosts(ctx); err != nil {
		r.errors.WithLabelValues(string(cache.ResourceHosts)).Inc()
		r.logger.Warn("Initial hosts refresh failed", "err", err)
	}

	hostsInterval := func() time.Duration { return r.cfg.HostsInterval }
	loops := []func(){
		func() { r.loop(ctx, cache.ResourceHosts, hostsInterval, false, r.refreshHosts) },
		func() { r.loop(ctx, cache.ResourceHostDetails, hostsInterval, false, r.refreshDetails) },
	}
	for _, c := range cache.HostCollectors {
		if r.enabled(c) {
			loops = append(loops, func() { r.hostLoop(ctx, c) })
		}
	}
	for c, refresh := range map[cache.Collector]func(context.Context) error{
		cache.CollectorUplinks:    r.refreshUplinks,
		cache.CollectorL2Segments: r.refreshL2Segments,
		cache.CollectorReference:  r.refreshReference,
	} {
		if r.enabled(c) {
			interval := func() time.Duration { return r.interval(c) }
			loops = append(loops, func() { r.loop(ctx, cache.Resource(c), interval, r.cfg.interval(c).Auto, refresh) })
		}
	}

	var wg sync.WaitGroup
	for _, loop := range loops {
		wg.Go(loop)
	}
	wg.Wait()
}

// loop runs refresh whenever the last success of resource is older than
// interval, and retries failures after retryDelay at the latest.
func (r *Refresher) loop(ctx context.Context, resource cache.Resource, interval func() time.Duration, auto bool,
	refresh func(context.Context) error) {
	var lastFailure time.Time
	for {
		due := r.store.Snapshot().LastSuccess[resource].Add(interval())
		if !lastFailure.IsZero() {
			if retry := lastFailure.Add(min(retryDelay, interval())); retry.After(due) {
				due = retry
			}
		}
		if wait := time.Until(due); wait > 0 {
			if !sleep(ctx, min(wait, recheckPeriod)) {
				return
			}
			continue
		}
		if auto && !r.waitForAutoBudget(ctx) {
			return
		}

		start := time.Now()
		err := refresh(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			lastFailure = time.Now()
			r.errors.WithLabelValues(string(resource)).Inc()
			r.logger.Warn("Refresh failed or incomplete, cached data is kept", "resource", resource, "err", err)
		default:
			r.logger.Debug("Refresh finished", "resource", resource, "duration", time.Since(start).Round(time.Millisecond))
		}
	}
}

// hostLoop refreshes collector c of every server with details, oldest first,
// spreading the requests evenly over the interval: a server every
// interval/servers.
func (r *Refresher) hostLoop(ctx context.Context, c cache.Collector) {
	resource := cache.Resource(c)
	auto := r.cfg.interval(c).Auto
	var lastRequest time.Time
	for {
		interval := r.interval(c)
		target, servers := r.oldestHost(c)
		wait := recheckPeriod
		if servers > 0 {
			next := lastRequest.Add(interval / time.Duration(servers)) // pacing
			if due := target.fetchedAt.Add(interval); due.After(next) {
				next = due
			}
			wait = time.Until(next)
		}
		if wait > 0 {
			if !sleep(ctx, min(wait, recheckPeriod)) {
				return
			}
			continue
		}
		if auto && !r.waitForAutoBudget(ctx) {
			return
		}

		lastRequest = time.Now()
		err := r.refreshHostCollector(ctx, c, target.id)
		switch {
		case ctx.Err() != nil:
			return
		case err == nil:
			r.markSuccess(resource)
		case api.IsNotFound(err):
			// Released; the next hosts refresh drops it.
			r.logger.Info("Host disappeared during refresh", "host_id", target.id, "title", target.title, "resource", c)
		case api.IsRateLimited(err):
			r.logger.Warn("Refresh interrupted by the API rate limit", "resource", c)
			if !sleep(ctx, recheckPeriod) {
				return
			}
		default:
			r.errors.WithLabelValues(string(resource)).Inc()
			r.logger.Warn("Cannot refresh host data, keeping cached data", "host_id", target.id, "title", target.title,
				"resource", c, "err", err)
		}
	}
}

// hostTarget is a server waiting for a refresh.
type hostTarget struct {
	id, title string
	fetchedAt time.Time // zero if never fetched
}

// oldestHost returns the server with details whose collector c was fetched
// longest ago, and the number of servers with details.
func (r *Refresher) oldestHost(c cache.Collector) (oldest hostTarget, servers int) {
	for id, host := range r.store.Snapshot().Hosts {
		if host.Details == nil {
			continue // new servers are downloaded by refreshDetails
		}
		fetchedAt := host.Details.Fetched[c]
		if servers == 0 || fetchedAt.Before(oldest.fetchedAt) || (fetchedAt.Equal(oldest.fetchedAt) && id < oldest.id) {
			oldest = hostTarget{id: id, title: host.Server.Title, fetchedAt: fetchedAt}
		}
		servers++
	}
	return oldest, servers
}

// autoBudgetShare is the share of the budget auto collectors may use; the
// rest is left for fixed collectors, new and changed servers and retries.
const autoBudgetShare = 0.8

// waitForAutoBudget blocks while the budget window has no more than the
// headroom left, so fixed collectors get the rest. It returns false when ctx
// is cancelled.
func (r *Refresher) waitForAutoBudget(ctx context.Context) bool {
	headroom := int(float64(r.cfg.BudgetPerHour) * (1 - autoBudgetShare))
	for r.api.BudgetRemaining() <= headroom {
		if r.autoPaused.CompareAndSwap(false, true) {
			r.logger.Info("Request budget is running low, auto collectors wait for the reset",
				"budget_remaining", r.api.BudgetRemaining(), "kept_for_fixed", headroom)
		}
		if !sleep(ctx, recheckPeriod) {
			return false
		}
	}
	if r.autoPaused.CompareAndSwap(true, false) {
		r.logger.Info("Request budget reset, auto collectors resume")
	}
	return true
}

// markSuccess records a successful refresh of resources.
func (r *Refresher) markSuccess(resources ...cache.Resource) {
	now := time.Now()
	r.store.Update(func(s *cache.Snapshot) {
		for _, resource := range resources {
			s.LastSuccess[resource] = now
		}
	})
}

// refreshHosts replaces server summaries, keeps known details and drops
// servers that are no longer in the account.
func (r *Refresher) refreshHosts(ctx context.Context) error {
	servers, err := r.api.ListDedicatedServers(ctx)
	if err != nil {
		return err
	}

	var added, removed, changed int
	r.store.Update(func(s *cache.Snapshot) {
		hosts := make(map[string]*cache.Host, len(servers))
		for _, server := range servers {
			host := &cache.Host{Server: server}
			if old, ok := s.Hosts[server.ID]; ok {
				host.Details = old.Details
				if !old.Server.UpdatedAt.Equal(server.UpdatedAt) {
					changed++
				}
			} else {
				added++
			}
			hosts[server.ID] = host
		}
		removed = len(s.Hosts) - (len(hosts) - added)
		s.Hosts = hosts
		s.LastSuccess[cache.ResourceHosts] = time.Now()
	})

	// Routine refreshes without changes are logged at debug level only.
	log := r.logger.Debug
	if added+removed+changed > 0 {
		log = r.logger.Info
	}
	log("Hosts refreshed", "hosts", len(servers), "added", added, "removed", removed, "changed", changed,
		"pending_details", len(r.hostsDueForDetails()))
	r.updateDemand()
	return nil
}

// refreshDetails downloads all collectors of new and changed servers.
// Servers are processed one by one and published as soon as they are ready,
// so a long initial download fills the dashboard progressively.
func (r *Refresher) refreshDetails(ctx context.Context) error {
	due := r.hostsDueForDetails()
	if len(due) == 0 {
		r.markSuccess(cache.ResourceHostDetails)
		return nil
	}

	reasons := map[string]int{}
	for _, host := range due {
		reasons[host.reason]++
	}
	r.logger.Info("Downloading host details", "hosts", len(due), "new", reasons[dueNew], "changed", reasons[dueChanged])

	start := time.Now()
	var refreshed, partial, failed, gone int
	for i, host := range due {
		err := r.refreshHostDetails(ctx, host.id)
		switch {
		case ctx.Err() != nil:
			return ctx.Err() // shutting down
		case err == nil:
			refreshed++
		case api.IsNotFound(err):
			// Released between listing and now; the next hosts refresh drops it.
			gone++
			r.logger.Info("Host disappeared during detail download", "host_id", host.id, "title", host.title)
		case api.IsRateLimited(err):
			r.logger.Warn("Host details download interrupted by the API rate limit, resuming on the next run",
				"processed", i, "remaining", len(due)-i)
			return err
		case errors.Is(err, errPartialDetails):
			partial++ // published without some collectors; already logged
		default:
			failed++
			r.logger.Warn("Cannot download host details", "host_id", host.id, "title", host.title, "err", err)
		}

		processed := i + 1
		remaining := len(due) - processed
		progress := []any{"host_id", host.id, "title", host.title, "reason", host.reason,
			"progress", fmt.Sprintf("%d/%d", processed, len(due)), "remaining", remaining}
		if remaining > 0 {
			perHost := time.Since(start) / time.Duration(processed)
			progress = append(progress, "eta", (perHost * time.Duration(remaining)).Round(time.Second))
		}
		r.logger.Info("Host details processed", progress...)
	}

	r.logger.Info("Host details download finished", "refreshed", refreshed, "partial", partial,
		"failed", failed, "gone", gone, "duration", time.Since(start).Round(time.Second))

	if failed > 0 {
		return errors.New("some host details could not be downloaded")
	}
	// Partial downloads are published; the failed collectors are retried by
	// their own loops.
	r.markSuccess(cache.ResourceHostDetails)
	return nil
}

// Reasons a server needs a full download.
const (
	dueNew     = "new"     // details were never downloaded
	dueChanged = "changed" // updated_at of the server changed
)

// dueHost is a server waiting for a full download.
type dueHost struct {
	id     string
	title  string
	reason string
}

// hostsDueForDetails returns new and changed servers, new ones first.
func (r *Refresher) hostsDueForDetails() []dueHost {
	var added, changed []dueHost
	for id, host := range r.store.Snapshot().Hosts {
		switch details := host.Details; {
		case details == nil:
			added = append(added, dueHost{id, host.Server.Title, dueNew})
		case !details.SourceUpdatedAt.Equal(host.Server.UpdatedAt):
			changed = append(changed, dueHost{id, host.Server.Title, dueChanged})
		}
	}
	return append(added, changed...)
}

// errPartialDetails reports that some collectors of a server could not be
// fetched; the server was still published with everything else.
var errPartialDetails = errors.New("some host collectors could not be fetched")

// fetchHostCollector fetches collector c (not configuration) of a server and
// returns a function that stores the result in details.
func (r *Refresher) fetchHostCollector(ctx context.Context, c cache.Collector, id string) (func(*cache.HostDetails), error) {
	switch c {
	case cache.CollectorDrives:
		v, err := r.api.ListDriveSlots(ctx, id)
		return func(d *cache.HostDetails) { d.DriveSlots = v }, err
	case cache.CollectorPorts:
		v, err := r.api.ListConnections(ctx, id)
		return func(d *cache.HostDetails) { d.Connections = v }, err
	case cache.CollectorNetworks:
		v, err := r.api.ListNetworks(ctx, id)
		return func(d *cache.HostDetails) { d.Networks = v }, err
	case cache.CollectorFeatures:
		v, err := r.api.ListFeatures(ctx, id)
		return func(d *cache.HostDetails) { d.Features = v }, err
	case cache.CollectorPowerFeeds:
		v, err := r.api.ListPowerFeeds(ctx, id)
		return func(d *cache.HostDetails) { d.PowerFeeds = v }, err
	}
	return nil, fmt.Errorf("unknown host collector %q", c)
}

// refreshHostDetails downloads all enabled collectors of one server.
//
// Collectors are independent: if one of them fails (e.g. an endpoint that
// returns 403 for some servers), the others are still published and the
// failed one keeps its previously cached value until its own loop retries it.
func (r *Refresher) refreshHostDetails(ctx context.Context, id string) error {
	server, err := r.api.GetDedicatedServer(ctx, id)
	if err != nil {
		return err
	}

	details := &cache.HostDetails{
		SourceUpdatedAt: server.UpdatedAt,
		Configuration:   server.ConfigurationDetails,
		Fetched:         map[cache.Collector]time.Time{cache.CollectorConfiguration: time.Now()},
	}
	var previous *cache.HostDetails
	if old, ok := r.store.Snapshot().Hosts[id]; ok {
		previous = old.Details
	}

	partial := false
	for _, c := range cache.HostCollectors {
		if c == cache.CollectorConfiguration || !r.enabled(c) {
			continue
		}
		store, err := r.fetchHostCollector(ctx, c, id)
		switch {
		case err == nil:
			store(details)
			details.Fetched[c] = time.Now()
		case api.IsRateLimited(err), ctx.Err() != nil:
			return err
		default:
			// Keep the previous value with its timestamp. Without one the
			// collector counts as never fetched and its loop picks it first.
			partial = true
			if previous != nil {
				if fetchedAt, ok := previous.Fetched[c]; ok {
					copyHostCollector(details, previous, c)
					details.Fetched[c] = fetchedAt
				}
			}
			r.logger.Warn("Cannot fetch host data, keeping cached data", "host_id", id, "resource", c, "err", err)
		}
	}

	r.store.Update(func(s *cache.Snapshot) {
		if _, ok := s.Hosts[id]; !ok {
			return // removed by a concurrent hosts refresh
		}
		// The single-object response is fresher than the list entry.
		s.Hosts[id] = &cache.Host{Server: *server, Details: details}
	})
	r.ensureUplinkModels(ctx, server)

	if partial {
		return errPartialDetails
	}
	return nil
}

// refreshHostCollector refreshes one collector of a server with details.
func (r *Refresher) refreshHostCollector(ctx context.Context, c cache.Collector, id string) error {
	var server *api.DedicatedServer
	var store func(*cache.HostDetails)
	var err error
	if c == cache.CollectorConfiguration {
		if server, err = r.api.GetDedicatedServer(ctx, id); err == nil {
			store = func(d *cache.HostDetails) { d.Configuration = server.ConfigurationDetails }
		}
	} else {
		store, err = r.fetchHostCollector(ctx, c, id)
	}
	if err != nil {
		if !api.IsRateLimited(err) && !api.IsNotFound(err) && ctx.Err() == nil {
			r.updateHostDetails(id, c, nil) // retry on its next turn, not right away
		}
		return err
	}

	r.updateHostDetails(id, c, store)
	if server != nil {
		r.ensureUplinkModels(ctx, server)
	}
	return nil
}

// updateHostDetails applies store (if any) to the cached details of a server
// and marks collector c as fetched now.
func (r *Refresher) updateHostDetails(id string, c cache.Collector, store func(*cache.HostDetails)) {
	r.store.Update(func(s *cache.Snapshot) {
		host, ok := s.Hosts[id]
		if !ok || host.Details == nil {
			return // removed or not downloaded by a concurrent refresh
		}
		details := *host.Details // copy-on-write
		switch {
		case store == nil:
		case c == cache.CollectorFeatures:
			r.storeFeatures(host, &details, store)
		default:
			store(&details)
		}
		details.Fetched = maps.Clone(details.Fetched)
		if details.Fetched == nil {
			details.Fetched = map[cache.Collector]time.Time{}
		}
		details.Fetched[c] = time.Now()
		s.Hosts[id] = &cache.Host{Server: host.Server, Details: &details}
	})
}

// storeFeatures stores new features in details and logs every change.
func (r *Refresher) storeFeatures(host *cache.Host, details *cache.HostDetails, store func(*cache.HostDetails)) {
	old := map[string]string{}
	for _, f := range details.Features {
		old[f.Name] = f.Status
	}
	store(details)
	id, title := host.Server.ID, host.Server.Title
	for _, f := range details.Features {
		if previous, known := old[f.Name]; !known || previous != f.Status {
			r.logger.Info("Host feature changed", "host_id", id, "title", title,
				"feature", f.Name, "from", previous, "to", f.Status)
		}
		delete(old, f.Name)
	}
	for name, previous := range old {
		r.logger.Info("Host feature removed", "host_id", id, "title", title, "feature", name, "was", previous)
	}
}

// ensureUplinkModels fetches the uplink catalogue of a server model seen for
// the first time right away, not on the next uplinks refresh.
func (r *Refresher) ensureUplinkModels(ctx context.Context, server *api.DedicatedServer) {
	cfg := server.ConfigurationDetails
	if !r.enabled(cache.CollectorUplinks) || cfg == nil || cfg.ServerModelID == 0 {
		return
	}
	if _, known := r.store.Snapshot().Uplinks[cache.UplinkKey(server.LocationID, cfg.ServerModelID)]; known {
		return
	}
	if err := r.refreshUplinkModels(ctx, server.LocationID, cfg.ServerModelID); err != nil {
		r.errors.WithLabelValues(string(cache.CollectorUplinks)).Inc()
		r.logger.Warn("Cannot fetch uplink models", "location_id", server.LocationID,
			"server_model_id", cfg.ServerModelID, "err", err)
	}
}

// refreshL2Segments replaces all L2 data. Partial results are discarded so
// the cache never mixes segment states from different refreshes.
func (r *Refresher) refreshL2Segments(ctx context.Context) error {
	list, err := r.api.ListL2Segments(ctx)
	if err != nil {
		return err
	}

	r.logger.Debug("Fetching L2 segment members and networks", "segments", len(list))
	segments := make(map[string]*cache.Segment, len(list))
	for _, segment := range list {
		members, err := r.api.ListL2Members(ctx, segment.ID)
		if err != nil {
			return err
		}
		networks, err := r.api.ListL2Networks(ctx, segment.ID)
		if err != nil {
			return err
		}
		segments[segment.ID] = &cache.Segment{Segment: segment, Members: members, Networks: networks}
	}

	r.store.Update(func(s *cache.Snapshot) {
		s.Segments = segments
		s.LastSuccess[cache.Resource(cache.CollectorL2Segments)] = time.Now()
	})

	members := 0
	for _, segment := range segments {
		members += len(segment.Members)
	}
	r.logger.Info("L2 segments refreshed", "segments", len(segments), "members", members)
	r.updateDemand()
	return nil
}

// refreshReference refreshes locations and racks.
func (r *Refresher) refreshReference(ctx context.Context) error {
	locations, err := r.api.ListLocations(ctx)
	if err != nil {
		return err
	}
	racks, err := r.api.ListRacks(ctx)
	if err != nil {
		return err
	}

	r.store.Update(func(s *cache.Snapshot) {
		s.Locations = make(map[int64]api.Location, len(locations))
		for _, location := range locations {
			s.Locations[location.ID] = location
		}
		s.Racks = make(map[string]api.Rack, len(racks))
		for _, rack := range racks {
			s.Racks[rack.ID] = rack
		}
		s.LastSuccess[cache.Resource(cache.CollectorReference)] = time.Now()
	})
	r.logger.Info("Reference data refreshed", "locations", len(locations), "racks", len(racks))
	r.updateDemand()
	return nil
}

// refreshUplinks refreshes uplink catalogues of server models that are in use
// and older than the uplinks interval. Unused ones are dropped.
func (r *Refresher) refreshUplinks(ctx context.Context) error {
	snapshot := r.store.Snapshot()

	type pair struct{ locationID, serverModelID int64 }
	inUse := map[string]pair{}
	for _, host := range snapshot.Hosts {
		if host.Details == nil || host.Details.Configuration == nil || host.Details.Configuration.ServerModelID == 0 {
			continue
		}
		cfg := host.Details.Configuration
		inUse[cache.UplinkKey(host.Server.LocationID, cfg.ServerModelID)] = pair{host.Server.LocationID, cfg.ServerModelID}
	}

	r.store.Update(func(s *cache.Snapshot) {
		for key := range s.Uplinks {
			if _, ok := inUse[key]; !ok {
				delete(s.Uplinks, key)
			}
		}
	})

	interval := r.interval(cache.CollectorUplinks)
	fetched := 0
	for key, p := range inUse {
		if catalogue, ok := snapshot.Uplinks[key]; ok && time.Since(catalogue.FetchedAt) < interval {
			continue
		}
		if err := r.refreshUplinkModels(ctx, p.locationID, p.serverModelID); err != nil {
			return err
		}
		fetched++
	}
	if fetched > 0 {
		r.logger.Info("Uplink model catalogues refreshed", "catalogues", fetched, "in_use", len(inUse))
	}

	r.markSuccess(cache.Resource(cache.CollectorUplinks))
	r.updateDemand()
	return nil
}

func (r *Refresher) refreshUplinkModels(ctx context.Context, locationID, serverModelID int64) error {
	models, err := r.api.ListUplinkModels(ctx, locationID, serverModelID)
	if err != nil {
		return err
	}
	r.logger.Debug("Uplink models fetched", "location_id", locationID, "server_model_id", serverModelID, "models", len(models))
	r.store.Update(func(s *cache.Snapshot) {
		s.Uplinks[cache.UplinkKey(locationID, serverModelID)] = &cache.UplinkModels{
			FetchedAt: time.Now(),
			Models:    models,
		}
	})
	return nil
}

// sleep waits for d and reports false if ctx was cancelled meanwhile.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
