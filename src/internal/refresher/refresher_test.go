package refresher

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// fakeAPI serves fixed data and counts calls per method.
type fakeAPI struct {
	mu      sync.Mutex
	calls   map[string]int
	servers []api.DedicatedServer

	// failPowerFeeds makes ListPowerFeeds return an API error.
	failPowerFeeds bool

	// features is returned by ListFeatures for every server.
	features []api.Feature

	// budgetRemaining is returned by BudgetRemaining; 0 means 1000.
	budgetRemaining int
}

func (f *fakeAPI) count(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[name]++
}

func (f *fakeAPI) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeAPI) ListDedicatedServers(context.Context) ([]api.DedicatedServer, error) {
	f.count("ListDedicatedServers")
	return f.servers, nil
}

func (f *fakeAPI) GetDedicatedServer(_ context.Context, id string) (*api.DedicatedServer, error) {
	f.count("GetDedicatedServer")
	for _, s := range f.servers {
		if s.ID == id {
			s.ConfigurationDetails = &api.ConfigurationDetails{ServerModelID: 7, PublicUplinkID: 1}
			return &s, nil
		}
	}
	return nil, &api.Error{StatusCode: 404}
}

func (f *fakeAPI) ListDriveSlots(context.Context, string) ([]api.DriveSlot, error) {
	f.count("ListDriveSlots")
	return []api.DriveSlot{{Position: 0}}, nil
}

func (f *fakeAPI) ListConnections(context.Context, string) ([]api.Connection, error) {
	f.count("ListConnections")
	return nil, nil
}

func (f *fakeAPI) ListNetworks(context.Context, string) ([]api.Network, error) { return nil, nil }

func (f *fakeAPI) ListFeatures(context.Context, string) ([]api.Feature, error) {
	f.count("ListFeatures")
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.features, nil
}

func (f *fakeAPI) ListPowerFeeds(context.Context, string) ([]api.PowerFeed, error) {
	f.count("ListPowerFeeds")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPowerFeeds {
		return nil, &api.Error{StatusCode: 403}
	}
	return []api.PowerFeed{{Name: "Power 1", Status: "on"}}, nil
}

func (f *fakeAPI) ListL2Segments(context.Context) ([]api.L2Segment, error) {
	return []api.L2Segment{{ID: "seg1"}}, nil
}

func (f *fakeAPI) ListL2Members(context.Context, string) ([]api.L2Member, error) {
	return []api.L2Member{{ID: "h1"}}, nil
}

func (f *fakeAPI) ListL2Networks(context.Context, string) ([]api.Network, error) { return nil, nil }

func (f *fakeAPI) ListLocations(context.Context) ([]api.Location, error) {
	return []api.Location{{ID: 1, Code: "ams1"}}, nil
}

func (f *fakeAPI) ListRacks(context.Context) ([]api.Rack, error) {
	return []api.Rack{{ID: "rack1", Name: "R1"}}, nil
}

func (f *fakeAPI) ListUplinkModels(context.Context, int64, int64) ([]api.UplinkModel, error) {
	f.count("ListUplinkModels")
	return []api.UplinkModel{{ID: 1, Redundancy: true}}, nil
}

func (f *fakeAPI) RateLimit() api.RateLimit { return api.RateLimit{} }

func (f *fakeAPI) BudgetRemaining() int {
	if f.budgetRemaining == 0 {
		return 1000
	}
	return f.budgetRemaining
}

func newTestRefresher(fake *fakeAPI) *Refresher {
	return newTestRefresherWith(fake, cache.NewStore("", slog.New(slog.DiscardHandler)), nil)
}

func newTestRefresherWith(fake *fakeAPI, store *cache.Store, disabled cache.DisabledCollectors) *Refresher {
	cfg := testConfig()
	cfg.Disabled = disabled
	return newTestRefresherConfig(fake, store, cfg)
}

func newTestRefresherConfig(fake *fakeAPI, store *cache.Store, cfg Config) *Refresher {
	return New(fake, store, cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
}

func testConfig() Config {
	return Config{
		HostsInterval: 2 * time.Minute, AutoMinInterval: 5 * time.Minute, AutoMaxInterval: 24 * time.Hour,
		PageSize: 100, BudgetPerHour: 400, Reserve: 300,
	}
}

// testServers returns n servers h01..hNN.
func testServers(n int) []api.DedicatedServer {
	servers := make([]api.DedicatedServer, n)
	for i := range servers {
		servers[i] = api.DedicatedServer{ID: fmt.Sprintf("h%02d", i+1), LocationID: 1}
	}
	return servers
}

func TestDetailsAreFetchedOnceAndRefetchedOnChange(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1", LocationID: 1, UpdatedAt: updated}}}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))
	mustRun(t, r.refreshDetails(ctx)) // nothing changed: no new requests

	if got := fake.callCount("ListDriveSlots"); got != 1 {
		t.Fatalf("drive slot requests = %d, want 1", got)
	}
	if got := fake.callCount("ListUplinkModels"); got != 1 {
		t.Fatalf("uplink model requests = %d, want 1 (fetched on first sight)", got)
	}

	// updated_at moves: details must be refreshed on the next check.
	fake.servers[0].UpdatedAt = updated.Add(time.Minute)
	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))

	if got := fake.callCount("ListDriveSlots"); got != 2 {
		t.Fatalf("drive slot requests = %d, want 2", got)
	}
	if got := fake.callCount("ListUplinkModels"); got != 1 {
		t.Fatalf("uplink model requests = %d, want 1 (catalogue already cached)", got)
	}
}

func TestHostsRefreshKeepsDetailsAndDropsReleasedServers(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1"}, {ID: "h2"}}}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))

	fake.servers = fake.servers[:1] // h2 released
	mustRun(t, r.refreshHosts(ctx))

	snapshot := r.store.Snapshot()
	if _, ok := snapshot.Hosts["h2"]; ok {
		t.Fatal("released host h2 is still cached")
	}
	if snapshot.Hosts["h1"].Details == nil {
		t.Fatal("details of h1 were lost on hosts refresh")
	}
}

func TestL2AndReference(t *testing.T) {
	r := newTestRefresher(&fakeAPI{})
	ctx := context.Background()

	mustRun(t, r.refreshL2Segments(ctx))
	mustRun(t, r.refreshReference(ctx))

	snapshot := r.store.Snapshot()
	if len(snapshot.Segments["seg1"].Members) != 1 {
		t.Fatalf("segments = %+v", snapshot.Segments)
	}
	if snapshot.Racks["rack1"].Name != "R1" || snapshot.Locations[1].Code != "ams1" {
		t.Fatalf("reference data not stored: %+v %+v", snapshot.Racks, snapshot.Locations)
	}
	for _, resource := range []cache.Resource{cache.Resource(cache.CollectorL2Segments), cache.Resource(cache.CollectorReference)} {
		if snapshot.LastSuccess[resource].IsZero() {
			t.Errorf("last success of %s not recorded", resource)
		}
	}
}

func TestFailedSubResourceKeepsTheRest(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1"}}}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))

	// Power feeds start failing; force a refetch by expiring the details.
	fake.mu.Lock()
	fake.failPowerFeeds = true
	fake.mu.Unlock()
	if err := r.refreshHostDetails(ctx, "h1"); !errors.Is(err, errPartialDetails) {
		t.Fatalf("err = %v, want errPartialDetails", err)
	}

	details := r.store.Snapshot().Hosts["h1"].Details
	if len(details.DriveSlots) != 1 {
		t.Fatalf("drive slots lost because power feeds failed: %+v", details.DriveSlots)
	}
	if len(details.PowerFeeds) != 1 {
		t.Fatalf("previous power feeds not kept: %+v", details.PowerFeeds)
	}
}

func TestDisabledCollectorsAreNotFetched(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1", LocationID: 1}}}
	store := cache.NewStore("", slog.New(slog.DiscardHandler))
	r := newTestRefresherWith(fake, store, cache.DisabledCollectors{
		cache.CollectorDrives: true, cache.CollectorFeatures: true, cache.CollectorUplinks: true,
	})
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))
	mustRun(t, r.refreshReference(ctx))

	for method, want := range map[string]int{
		"ListDriveSlots": 0, "ListFeatures": 0, "ListUplinkModels": 0,
		"ListConnections": 1, "ListPowerFeeds": 1, "GetDedicatedServer": 1,
	} {
		if got := fake.callCount(method); got != want {
			t.Errorf("%s calls = %d, want %d", method, got, want)
		}
	}
	fetched := store.Snapshot().Hosts["h1"].Details.Fetched
	if _, ok := fetched[cache.CollectorDrives]; ok {
		t.Fatalf("fetched = %v", fetched)
	}
	if _, ok := fetched[cache.CollectorPorts]; !ok {
		t.Fatalf("fetched = %v", fetched)
	}
	if !store.Snapshot().LastSuccess[cache.Resource(cache.CollectorUplinks)].IsZero() {
		t.Fatal("uplinks refresh recorded while disabled")
	}
}

func mustRun(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestAutoIntervalFollowsTheDemand(t *testing.T) {
	cfg := testConfig()
	cfg.Intervals = map[cache.Collector]Interval{cache.CollectorFeatures: {Fixed: 15 * time.Minute}}
	r := newTestRefresherConfig(&fakeAPI{servers: testServers(20)}, cache.NewStore("", slog.New(slog.DiscardHandler)), cfg)

	// Before the first estimate auto collectors use the maximum.
	if got := r.interval(cache.CollectorDrives); got != 24*time.Hour {
		t.Fatalf("auto interval before the estimate = %v, want 24h", got)
	}

	mustRun(t, r.refreshHosts(context.Background()))

	// Fixed: hosts 30/h + features 20 every 15m = 80/h. Auto: 5 collectors x 20
	// servers + L2 page + 2 reference pages = 103 per run, into 400*0.8-110 =
	// 210/h: 29.4m, rounded up to 30m.
	if got := r.interval(cache.CollectorDrives); got != 30*time.Minute {
		t.Errorf("auto interval = %v, want 30m", got)
	}
	if got := r.interval(cache.CollectorFeatures); got != 15*time.Minute {
		t.Errorf("fixed features interval = %v, want 15m", got)
	}
}

func TestHostCollectorQueueIsOldestFirst(t *testing.T) {
	store := cache.NewStore("", slog.New(slog.DiscardHandler))
	base := time.Now().Add(-time.Hour)
	store.Update(func(s *cache.Snapshot) {
		s.Hosts["h1"] = &cache.Host{Details: &cache.HostDetails{Fetched: map[cache.Collector]time.Time{cache.CollectorDrives: base.Add(2 * time.Minute)}}}
		s.Hosts["h2"] = &cache.Host{Details: &cache.HostDetails{Fetched: map[cache.Collector]time.Time{cache.CollectorDrives: base}}}
		s.Hosts["h3"] = &cache.Host{Details: &cache.HostDetails{Fetched: map[cache.Collector]time.Time{}}} // drives never fetched
		s.Hosts["h4"] = &cache.Host{}                                                                      // new: downloaded by refreshDetails, not queued
	})
	r := newTestRefresherWith(&fakeAPI{}, store, nil)

	for _, want := range []string{"h3", "h2", "h1"} {
		target, servers := r.oldestHost(cache.CollectorDrives)
		if target.id != want || servers != 3 {
			t.Fatalf("oldest = %s of %d servers, want %s of 3", target.id, servers, want)
		}
		mustRun(t, r.refreshHostCollector(context.Background(), cache.CollectorDrives, target.id))
	}
}

func TestHostCollectorRefreshChangesOnlyItsData(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeAPI{
		servers:  []api.DedicatedServer{{ID: "h1", UpdatedAt: updated}},
		features: []api.Feature{{Name: "disaggregated_private_ports", Status: "deactivated"}},
	}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))
	before := r.store.Snapshot().Hosts["h1"].Details

	// The feature is toggled; updated_at stays the same, so only the features
	// collector can notice it.
	fake.mu.Lock()
	fake.features = []api.Feature{{Name: "disaggregated_private_ports", Status: "activated"}}
	fake.mu.Unlock()
	time.Sleep(time.Millisecond) // distinct timestamps
	mustRun(t, r.refreshHostCollector(ctx, cache.CollectorFeatures, "h1"))

	after := r.store.Snapshot().Hosts["h1"].Details
	if len(after.Features) != 1 || after.Features[0].Status != "activated" {
		t.Fatalf("features = %+v, want activated", after.Features)
	}
	if !after.Fetched[cache.CollectorFeatures].After(before.Fetched[cache.CollectorFeatures]) {
		t.Error("features fetch time not updated")
	}
	if !after.Fetched[cache.CollectorDrives].Equal(before.Fetched[cache.CollectorDrives]) {
		t.Error("drives fetch time changed by a features refresh")
	}
	if got := fake.callCount("ListDriveSlots"); got != 1 {
		t.Errorf("drive slot requests = %d, want 1", got)
	}
	if len(before.Features) != 1 || before.Features[0].Status != "deactivated" {
		t.Error("published details were modified in place")
	}
}

func TestFailingHostCollectorKeepsDataAndMovesBack(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1"}}}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))
	before := r.store.Snapshot().Hosts["h1"].Details.Fetched[cache.CollectorPowerFeeds]

	fake.mu.Lock()
	fake.failPowerFeeds = true
	fake.mu.Unlock()
	time.Sleep(time.Millisecond)
	if err := r.refreshHostCollector(ctx, cache.CollectorPowerFeeds, "h1"); err == nil {
		t.Fatal("want an error")
	}

	details := r.store.Snapshot().Hosts["h1"].Details
	if len(details.PowerFeeds) != 1 {
		t.Fatalf("cached power feeds lost: %+v", details.PowerFeeds)
	}
	// Moved to the end of the queue, so a permanent 403 is not retried in a loop.
	if !details.Fetched[cache.CollectorPowerFeeds].After(before) {
		t.Error("failed collector was not moved to the end of the queue")
	}
}

func TestPartialNewHostIsQueuedFirst(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1"}, {ID: "h2"}}, failPowerFeeds: true}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx)) // published without power feeds

	if due := r.hostsDueForDetails(); len(due) != 0 {
		t.Fatalf("partial hosts are due for a full download again: %+v", due)
	}
	target, _ := r.oldestHost(cache.CollectorPowerFeeds)
	if !target.fetchedAt.IsZero() {
		t.Fatalf("power feeds of %s fetched at %v, want never", target.id, target.fetchedAt)
	}
	target, _ = r.oldestHost(cache.CollectorDrives)
	if target.fetchedAt.IsZero() {
		t.Fatal("drives were not recorded as fetched")
	}
}

func TestAutoCollectorsWaitForBudget(t *testing.T) {
	// Budget 400: auto collectors leave the last 20% (80 requests) to fixed ones.
	fake := &fakeAPI{budgetRemaining: 81}
	r := newTestRefresher(fake)
	if !r.waitForAutoBudget(context.Background()) {
		t.Fatal("auto collectors blocked with budget left")
	}

	fake.budgetRemaining = 80
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if r.waitForAutoBudget(ctx) {
		t.Fatal("auto collectors did not wait with 20% of the budget left")
	}
}

func TestPrepareCacheDropsDisabledData(t *testing.T) {
	store := cache.NewStore("", slog.New(slog.DiscardHandler))
	now := time.Now()
	store.Update(func(s *cache.Snapshot) {
		s.Hosts["h1"] = &cache.Host{Details: &cache.HostDetails{
			Fetched:    map[cache.Collector]time.Time{cache.CollectorDrives: now, cache.CollectorFeatures: now},
			DriveSlots: []api.DriveSlot{{Position: 0}},
			Features:   []api.Feature{{Name: "oob_public_access"}},
		}}
		s.Segments["seg1"] = &cache.Segment{}
		s.Uplinks["1/7"] = &cache.UplinkModels{}
		for _, resource := range []cache.Resource{"l2-segments", "uplinks", "features", "hosts", "host_features", "locations"} {
			s.LastSuccess[resource] = now
		}
	})

	newTestRefresherWith(&fakeAPI{}, store, cache.DisabledCollectors{
		cache.CollectorDrives: true, cache.CollectorL2Segments: true, cache.CollectorUplinks: true,
	})

	s := store.Snapshot()
	details := s.Hosts["h1"].Details
	if details.DriveSlots != nil || len(details.Features) != 1 {
		t.Fatalf("details = %+v, want drives dropped and features kept", details)
	}
	if _, ok := details.Fetched[cache.CollectorDrives]; ok {
		t.Error("fetch time of disabled drives kept")
	}
	if len(s.Segments) != 0 || len(s.Uplinks) != 0 {
		t.Errorf("segments = %v, uplinks = %v", s.Segments, s.Uplinks)
	}
	// Disabled and old resource names go; others stay.
	for resource, want := range map[cache.Resource]bool{
		"l2-segments": false, "uplinks": false, "host_features": false, "locations": false, "features": true, "hosts": true,
	} {
		if _, ok := s.LastSuccess[resource]; ok != want {
			t.Errorf("last success of %s kept = %v, want %v", resource, ok, want)
		}
	}
}
