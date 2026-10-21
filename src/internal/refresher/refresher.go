// Package refresher keeps the cache up to date while spending as few API
// requests as possible.
//
// Data is split into independent loops by how often it changes:
//
//	hosts      the server list: power, statuses, labels, updated_at  (minutes)
//	details    per-server drives, ports, networks, features, feeds   (hours, or when updated_at changes)
//	l2         L2 segments with members and networks                  (tens of minutes)
//	reference  locations, racks, uplink model catalogues              (a day)
//
// All loops share one API client, whose pacer keeps the total request rate
// within the configured budget.
package refresher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
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
}

// Config holds refresh intervals.
type Config struct {
	// HostsInterval is how often the server list is fetched.
	HostsInterval time.Duration

	// DetailsInterval is the maximum age of per-server details. Details of a
	// server whose updated_at changed are refreshed on the next check, which
	// runs every HostsInterval.
	DetailsInterval time.Duration

	// FeaturesInterval is how often server features are re-fetched for all
	// servers with details. Toggling a feature (e.g. disaggregated_*_ports
	// or oob_public_access) does not change updated_at, so without this loop
	// such changes would only show up after DetailsInterval. Zero disables it.
	FeaturesInterval time.Duration

	// L2Interval is how often L2 segments are fetched.
	L2Interval time.Duration

	// ReferenceInterval is how often locations, racks and uplink catalogues
	// are fetched.
	ReferenceInterval time.Duration

	// The API client settings below are used to estimate the request demand
	// of the configuration and to recommend a budget and intervals.
	PageSize      int
	BudgetPerHour int
	Reserve       int
}

// Refresher runs the refresh loops.
type Refresher struct {
	api    API
	store  *cache.Store
	cfg    Config
	logger *slog.Logger

	errors *prometheus.CounterVec
	demand *demandMetrics

	demandMu           sync.Mutex
	lastRecommendation string // last logged recommendation, to log only changes
}

// New creates a Refresher and registers its metrics.
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
	// Log the first estimate even when it has no recommendation.
	r.lastRecommendation = "-"
	return r
}

// Run starts all loops and blocks until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	loops := []refreshLoop{
		{cache.ResourceHosts, r.cfg.HostsInterval, r.refreshHosts},
		{cache.ResourceHostDetails, r.cfg.HostsInterval, r.refreshDetails},
		{cache.ResourceL2Segments, r.cfg.L2Interval, r.refreshL2Segments},
		{cache.ResourceLocations, r.cfg.ReferenceInterval, r.refreshReference},
	}
	if r.cfg.FeaturesInterval > 0 {
		loops = append(loops, refreshLoop{cache.ResourceFeatures, r.cfg.FeaturesInterval, r.refreshFeatures})
	}

	snapshot := r.store.Snapshot()
	withDetails := 0
	for _, host := range snapshot.Hosts {
		if host.Details != nil {
			withDetails++
		}
	}
	r.logger.Info("Refresher started",
		"cached_hosts", len(snapshot.Hosts), "cached_host_details", withDetails,
		"hosts_interval", r.cfg.HostsInterval, "details_interval", r.cfg.DetailsInterval,
		"features_interval", r.cfg.FeaturesInterval,
		"l2_interval", r.cfg.L2Interval, "reference_interval", r.cfg.ReferenceInterval)

	// On a cold start (no cache file) fetch racks first: host metrics carry the
	// rack name, and without the rack list they would briefly expose rack IDs,
	// leaving stale values in Grafana's rack variable. Costs two requests.
	if r.store.Snapshot().LastSuccess[cache.ResourceRacks].IsZero() {
		if err := r.refreshReference(ctx); err != nil {
			r.errors.WithLabelValues(string(cache.ResourceLocations)).Inc()
			r.logger.Warn("Initial reference refresh failed", "err", err)
		}
	}

	// Fetch the server list once before starting the loops, so the details
	// loop has hosts to work on right away. On failure the hosts loop retries.
	if err := r.refreshHosts(ctx); err != nil {
		r.errors.WithLabelValues(string(cache.ResourceHosts)).Inc()
		r.logger.Warn("Initial hosts refresh failed", "err", err)
	}

	var wg sync.WaitGroup
	for _, loop := range loops {
		wg.Go(func() {
			r.loop(ctx, loop)
		})
	}
	wg.Wait()
}

// refreshLoop is one periodically refreshed group of data.
type refreshLoop struct {
	resource cache.Resource
	interval time.Duration
	refresh  func(context.Context) error
}

// loop calls refresh every interval. The first run is delayed if the
// resource was refreshed recently, e.g. according to a loaded cache file.
func (r *Refresher) loop(ctx context.Context, l refreshLoop) {
	resource, interval, refresh := l.resource, l.interval, l.refresh
	delay := time.Until(r.store.Snapshot().LastSuccess[resource].Add(interval))

	for {
		if delay > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}

		start := time.Now()
		err := refresh(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			r.errors.WithLabelValues(string(resource)).Inc()
			r.logger.Warn("Refresh failed or incomplete, cached data is kept where needed", "resource", resource, "err", err)
		default:
			r.logger.Debug("Refresh finished", "resource", resource, "duration", time.Since(start).Round(time.Millisecond))
		}

		delay = interval
		r.logger.Debug("Next refresh scheduled", "resource", resource, "in", delay)
	}
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
		"pending_details", len(r.hostsDueForDetails(time.Now())))
	r.updateDemand()
	return nil
}

// refreshDetails downloads details of hosts that are new, changed or stale.
// Hosts are processed one by one and published as soon as they are ready,
// so a long initial download fills the dashboard progressively.
func (r *Refresher) refreshDetails(ctx context.Context) error {
	due := r.hostsDueForDetails(time.Now())
	if len(due) == 0 {
		r.logger.Debug("Host details are up to date")
		r.markSuccess(cache.ResourceHostDetails)
		return nil
	}

	reasons := map[string]int{}
	for _, host := range due {
		reasons[host.reason]++
	}
	r.logger.Info("Refreshing host details", "hosts", len(due),
		"new", reasons[dueNew], "changed", reasons[dueChanged], "stale", reasons[dueStale])

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
			r.logger.Info("Host disappeared during detail refresh", "host_id", host.id, "title", host.title)
		case api.IsRateLimited(err):
			r.logger.Warn("Host details refresh interrupted by the API rate limit, resuming on the next run",
				"processed", i, "remaining", len(due)-i)
			return err
		case errors.Is(err, errPartialDetails):
			partial++ // published without some sub-resources; already logged
		default:
			failed++
			r.logger.Warn("Cannot refresh host details", "host_id", host.id, "title", host.title, "err", err)
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

	r.logger.Info("Host details refresh finished", "refreshed", refreshed, "partial", partial,
		"failed", failed, "gone", gone, "duration", time.Since(start).Round(time.Second))

	if partial+failed > 0 {
		return errors.New("some host details could not be refreshed")
	}
	r.markSuccess(cache.ResourceHostDetails)
	return nil
}

// Reasons a host needs a detail refresh.
const (
	dueNew     = "new"     // details were never downloaded
	dueChanged = "changed" // updated_at of the server changed
	dueStale   = "stale"   // details are older than DetailsInterval
)

// dueHost is a host waiting for a detail refresh.
type dueHost struct {
	id        string
	title     string
	reason    string
	fetchedAt time.Time
}

// hostsDueForDetails returns hosts that need a detail refresh, oldest (or
// never fetched) first.
func (r *Refresher) hostsDueForDetails(now time.Time) []dueHost {
	var due []dueHost
	for id, host := range r.store.Snapshot().Hosts {
		candidate := dueHost{id: id, title: host.Server.Title}
		switch details := host.Details; {
		case details == nil:
			candidate.reason = dueNew
		case !details.SourceUpdatedAt.Equal(host.Server.UpdatedAt):
			candidate.reason, candidate.fetchedAt = dueChanged, details.FetchedAt
		case now.Sub(details.FetchedAt) >= r.cfg.DetailsInterval:
			candidate.reason, candidate.fetchedAt = dueStale, details.FetchedAt
		default:
			continue
		}
		due = append(due, candidate)
	}

	slices.SortFunc(due, func(a, b dueHost) int { return a.fetchedAt.Compare(b.fetchedAt) })
	return due
}

// errPartialDetails reports that some sub-resources of a host could not be
// fetched; the host was still published with everything else.
var errPartialDetails = errors.New("some host sub-resources could not be fetched")

// refreshHostDetails downloads and publishes details of one host.
//
// Sub-resources are independent: if one of them fails (e.g. an endpoint that
// returns 404 or 403 for some servers), the others are still published and
// the failed one keeps its previously cached value.
func (r *Refresher) refreshHostDetails(ctx context.Context, id string) error {
	server, err := r.api.GetDedicatedServer(ctx, id)
	if err != nil {
		return err
	}

	var previous cache.HostDetails
	if old, ok := r.store.Snapshot().Hosts[id]; ok && old.Details != nil {
		previous = *old.Details
	}

	details := &cache.HostDetails{
		SourceUpdatedAt: server.UpdatedAt,
		Configuration:   server.ConfigurationDetails,
	}

	subResources := []struct {
		name  string
		fetch func() error
		keep  func() // restores the previously cached value on failure
	}{
		{"drive_slots",
			func() (err error) { details.DriveSlots, err = r.api.ListDriveSlots(ctx, id); return err },
			func() { details.DriveSlots = previous.DriveSlots }},
		{"connections",
			func() (err error) { details.Connections, err = r.api.ListConnections(ctx, id); return err },
			func() { details.Connections = previous.Connections }},
		{"networks",
			func() (err error) { details.Networks, err = r.api.ListNetworks(ctx, id); return err },
			func() { details.Networks = previous.Networks }},
		{"features",
			func() (err error) { details.Features, err = r.api.ListFeatures(ctx, id); return err },
			func() { details.Features = previous.Features }},
		{"power_feeds",
			func() (err error) { details.PowerFeeds, err = r.api.ListPowerFeeds(ctx, id); return err },
			func() { details.PowerFeeds = previous.PowerFeeds }},
	}

	partial := false
	for _, sub := range subResources {
		err := sub.fetch()
		switch {
		case err == nil:
		case api.IsRateLimited(err), ctx.Err() != nil:
			return err
		default:
			partial = true
			sub.keep()
			r.logger.Warn("Cannot fetch host sub-resource, keeping cached data",
				"host_id", id, "resource", sub.name, "err", err)
		}
	}
	details.FetchedAt = time.Now()

	r.store.Update(func(s *cache.Snapshot) {
		if _, ok := s.Hosts[id]; !ok {
			return // removed by a concurrent hosts refresh
		}
		// The single-object response is fresher than the list entry.
		s.Hosts[id] = &cache.Host{Server: *server, Details: details}
	})

	// A host of a server model we have not seen yet needs its uplink
	// catalogue right away, not on the next daily reference refresh.
	if cfg := server.ConfigurationDetails; cfg != nil && cfg.ServerModelID != 0 {
		key := cache.UplinkKey(server.LocationID, cfg.ServerModelID)
		if _, known := r.store.Snapshot().Uplinks[key]; !known {
			if err := r.refreshUplinkModels(ctx, server.LocationID, cfg.ServerModelID); err != nil {
				r.errors.WithLabelValues(string(cache.ResourceUplinks)).Inc()
				r.logger.Warn("Cannot fetch uplink models", "location_id", server.LocationID,
					"server_model_id", cfg.ServerModelID, "err", err)
			}
		}
	}

	if partial {
		return errPartialDetails
	}
	return nil
}

// refreshFeatures re-fetches features of every server whose details are
// cached. Servers without details are skipped: the details loop fetches their
// features together with everything else.
func (r *Refresher) refreshFeatures(ctx context.Context) error {
	type target struct{ id, title string }
	var targets []target
	for id, host := range r.store.Snapshot().Hosts {
		if host.Details != nil {
			targets = append(targets, target{id, host.Server.Title})
		}
	}
	if len(targets) == 0 {
		r.logger.Debug("No hosts with details yet, skipping features refresh")
		return nil
	}
	slices.SortFunc(targets, func(a, b target) int { return strings.Compare(a.title, b.title) })

	r.logger.Info("Refreshing host features", "hosts", len(targets))
	start := time.Now()
	logEvery := max(1, len(targets)/10) // about ten progress lines per run
	var changed, failed int

	for i, t := range targets {
		features, err := r.api.ListFeatures(ctx, t.id)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil:
			if r.storeFeatures(t.id, t.title, features) {
				changed++
			}
		case api.IsNotFound(err):
			r.logger.Info("Host disappeared during features refresh", "host_id", t.id, "title", t.title)
		case api.IsRateLimited(err):
			r.logger.Warn("Features refresh interrupted by the API rate limit, resuming on the next run",
				"processed", i, "remaining", len(targets)-i)
			return err
		default:
			failed++
			r.logger.Warn("Cannot fetch host features, keeping cached data", "host_id", t.id, "title", t.title, "err", err)
		}

		processed := i + 1
		if remaining := len(targets) - processed; remaining > 0 && processed%logEvery == 0 {
			perHost := time.Since(start) / time.Duration(processed)
			r.logger.Info("Host features progress", "progress", fmt.Sprintf("%d/%d", processed, len(targets)),
				"remaining", remaining, "eta", (perHost * time.Duration(remaining)).Round(time.Second))
		}
	}

	r.logger.Info("Host features refresh finished", "hosts", len(targets), "changed", changed,
		"failed", failed, "duration", time.Since(start).Round(time.Second))
	if failed > 0 {
		return errors.New("some host features could not be refreshed")
	}
	r.markSuccess(cache.ResourceFeatures)
	return nil
}

// storeFeatures replaces the cached features of a host, logs every change and
// reports whether anything changed.
func (r *Refresher) storeFeatures(id, title string, features []api.Feature) bool {
	changed := false
	r.store.Update(func(s *cache.Snapshot) {
		host, ok := s.Hosts[id]
		if !ok || host.Details == nil {
			return // removed or not loaded by a concurrent refresh
		}

		old := map[string]string{}
		for _, f := range host.Details.Features {
			old[f.Name] = f.Status
		}
		for _, f := range features {
			if previous, known := old[f.Name]; !known || previous != f.Status {
				changed = true
				r.logger.Info("Host feature changed", "host_id", id, "title", title,
					"feature", f.Name, "from", previous, "to", f.Status)
			}
			delete(old, f.Name)
		}
		for name, previous := range old {
			changed = true
			r.logger.Info("Host feature removed", "host_id", id, "title", title, "feature", name, "was", previous)
		}
		if !changed {
			return
		}

		// Copy-on-write: published values must not be modified.
		details := *host.Details
		details.Features = features
		s.Hosts[id] = &cache.Host{Server: host.Server, Details: &details}
	})
	return changed
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
		s.LastSuccess[cache.ResourceL2Segments] = time.Now()
	})

	members := 0
	for _, segment := range segments {
		members += len(segment.Members)
	}
	r.logger.Info("L2 segments refreshed", "segments", len(segments), "members", members)
	r.updateDemand()
	return nil
}

// refreshReference refreshes locations, racks and stale uplink catalogues.
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
		now := time.Now()
		s.LastSuccess[cache.ResourceLocations] = now
		s.LastSuccess[cache.ResourceRacks] = now
	})
	r.logger.Info("Reference data refreshed", "locations", len(locations), "racks", len(racks))
	r.updateDemand()

	return r.refreshStaleUplinkModels(ctx)
}

// refreshStaleUplinkModels refreshes uplink catalogues of server models that
// are in use and older than the reference interval. Unused ones are dropped.
func (r *Refresher) refreshStaleUplinkModels(ctx context.Context) error {
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

	fetched := 0
	for key, p := range inUse {
		if catalogue, ok := snapshot.Uplinks[key]; ok && time.Since(catalogue.FetchedAt) < r.cfg.ReferenceInterval {
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

	r.markSuccess(cache.ResourceUplinks)
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
