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
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"serverscom-exporter/internal/api"
	"serverscom-exporter/internal/cache"
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
}

// Config holds refresh intervals.
type Config struct {
	// HostsInterval is how often the server list is fetched.
	HostsInterval time.Duration

	// DetailsInterval is the maximum age of per-server details. Details of a
	// server whose updated_at changed are refreshed on the next check, which
	// runs every HostsInterval.
	DetailsInterval time.Duration

	// L2Interval is how often L2 segments are fetched.
	L2Interval time.Duration

	// ReferenceInterval is how often locations, racks and uplink catalogues
	// are fetched.
	ReferenceInterval time.Duration
}

// Refresher runs the refresh loops.
type Refresher struct {
	api    API
	store  *cache.Store
	cfg    Config
	logger *slog.Logger

	errors *prometheus.CounterVec
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
	return r
}

// Run starts all loops and blocks until ctx is cancelled.
func (r *Refresher) Run(ctx context.Context) {
	loops := []struct {
		resource cache.Resource
		interval time.Duration
		refresh  func(context.Context) error
	}{
		{cache.ResourceHosts, r.cfg.HostsInterval, r.refreshHosts},
		{cache.ResourceHostDetails, r.cfg.HostsInterval, r.refreshDetails},
		{cache.ResourceL2Segments, r.cfg.L2Interval, r.refreshL2Segments},
		{cache.ResourceLocations, r.cfg.ReferenceInterval, r.refreshReference},
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
			r.loop(ctx, loop.resource, loop.interval, loop.refresh)
		})
	}
	wg.Wait()
}

// loop calls refresh every interval. The first run is delayed if the
// resource was refreshed recently, e.g. according to a loaded cache file.
func (r *Refresher) loop(ctx context.Context, resource cache.Resource, interval time.Duration, refresh func(context.Context) error) {
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
			r.logger.Warn("Refresh failed, keeping cached data", "resource", resource, "err", err)
		default:
			r.logger.Debug("Refresh finished", "resource", resource, "duration", time.Since(start).Round(time.Millisecond))
		}

		delay = interval
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

	r.store.Update(func(s *cache.Snapshot) {
		hosts := make(map[string]*cache.Host, len(servers))
		for _, server := range servers {
			host := &cache.Host{Server: server}
			if old, ok := s.Hosts[server.ID]; ok {
				host.Details = old.Details
			}
			hosts[server.ID] = host
		}
		s.Hosts = hosts
		s.LastSuccess[cache.ResourceHosts] = time.Now()
	})
	return nil
}

// refreshDetails downloads details of hosts that are new, changed or stale.
// Hosts are processed one by one and published as soon as they are ready,
// so a long initial download fills the dashboard progressively.
func (r *Refresher) refreshDetails(ctx context.Context) error {
	due := r.hostsDueForDetails(time.Now())
	if len(due) > 0 {
		r.logger.Info("Refreshing host details", "hosts", len(due))
	}

	var failed int
	for _, id := range due {
		err := r.refreshHostDetails(ctx, id)
		switch {
		case err == nil:
		case api.IsNotFound(err):
			// Released between listing and now; the next hosts refresh drops it.
			r.logger.Debug("Host disappeared during detail refresh", "host_id", id)
		case api.IsRateLimited(err), ctx.Err() != nil:
			return err
		default:
			failed++
			r.logger.Warn("Cannot refresh host details", "host_id", id, "err", err)
		}
	}

	if failed > 0 {
		return errors.New("some host details could not be refreshed")
	}
	r.markSuccess(cache.ResourceHostDetails)
	return nil
}

// hostsDueForDetails returns IDs of hosts that need a detail refresh,
// oldest (or never fetched) first.
func (r *Refresher) hostsDueForDetails(now time.Time) []string {
	snapshot := r.store.Snapshot()

	type candidate struct {
		id        string
		fetchedAt time.Time
	}
	var due []candidate
	for id, host := range snapshot.Hosts {
		details := host.Details
		switch {
		case details == nil:
			due = append(due, candidate{id: id})
		case !details.SourceUpdatedAt.Equal(host.Server.UpdatedAt),
			now.Sub(details.FetchedAt) >= r.cfg.DetailsInterval:
			due = append(due, candidate{id: id, fetchedAt: details.FetchedAt})
		}
	}

	slices.SortFunc(due, func(a, b candidate) int { return a.fetchedAt.Compare(b.fetchedAt) })

	ids := make([]string, len(due))
	for i, c := range due {
		ids[i] = c.id
	}
	return ids
}

// refreshHostDetails downloads and publishes details of one host.
func (r *Refresher) refreshHostDetails(ctx context.Context, id string) error {
	server, err := r.api.GetDedicatedServer(ctx, id)
	if err != nil {
		return err
	}

	details := &cache.HostDetails{
		SourceUpdatedAt: server.UpdatedAt,
		Configuration:   server.ConfigurationDetails,
	}
	if details.DriveSlots, err = r.api.ListDriveSlots(ctx, id); err != nil {
		return err
	}
	if details.Connections, err = r.api.ListConnections(ctx, id); err != nil {
		return err
	}
	if details.Networks, err = r.api.ListNetworks(ctx, id); err != nil {
		return err
	}
	if details.Features, err = r.api.ListFeatures(ctx, id); err != nil {
		return err
	}
	if details.PowerFeeds, err = r.api.ListPowerFeeds(ctx, id); err != nil {
		return err
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
	return nil
}

// refreshL2Segments replaces all L2 data. Partial results are discarded so
// the cache never mixes segment states from different refreshes.
func (r *Refresher) refreshL2Segments(ctx context.Context) error {
	list, err := r.api.ListL2Segments(ctx)
	if err != nil {
		return err
	}

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

	for key, p := range inUse {
		if catalogue, ok := snapshot.Uplinks[key]; ok && time.Since(catalogue.FetchedAt) < r.cfg.ReferenceInterval {
			continue
		}
		if err := r.refreshUplinkModels(ctx, p.locationID, p.serverModelID); err != nil {
			return err
		}
	}

	r.markSuccess(cache.ResourceUplinks)
	return nil
}

func (r *Refresher) refreshUplinkModels(ctx context.Context, locationID, serverModelID int64) error {
	models, err := r.api.ListUplinkModels(ctx, locationID, serverModelID)
	if err != nil {
		return err
	}
	r.store.Update(func(s *cache.Snapshot) {
		s.Uplinks[cache.UplinkKey(locationID, serverModelID)] = &cache.UplinkModels{
			FetchedAt: time.Now(),
			Models:    models,
		}
	})
	return nil
}
