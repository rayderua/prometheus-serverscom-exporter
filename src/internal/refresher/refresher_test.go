package refresher

import (
	"context"
	"errors"
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

func newTestRefresher(fake *fakeAPI) *Refresher {
	store := cache.NewStore("", slog.New(slog.DiscardHandler))
	cfg := Config{HostsInterval: time.Minute, DetailsInterval: time.Hour, FeaturesInterval: time.Minute, L2Interval: time.Minute, ReferenceInterval: time.Hour}
	return New(fake, store, cfg, slog.New(slog.DiscardHandler), prometheus.NewRegistry())
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

func TestStaleDetailsAreDue(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1"}}}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))

	if due := r.hostsDueForDetails(time.Now()); len(due) != 0 {
		t.Fatalf("fresh details are due: %v", due)
	}
	if due := r.hostsDueForDetails(time.Now().Add(2 * time.Hour)); len(due) != 1 {
		t.Fatalf("stale details are not due: %v", due)
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
	for _, resource := range []cache.Resource{cache.ResourceL2Segments, cache.ResourceLocations, cache.ResourceRacks, cache.ResourceUplinks} {
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

func TestFeaturesRefreshPicksUpChangesWithoutUpdatedAt(t *testing.T) {
	updated := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	fake := &fakeAPI{
		servers:  []api.DedicatedServer{{ID: "h1", UpdatedAt: updated}, {ID: "h2", UpdatedAt: updated}},
		features: []api.Feature{{Name: "disaggregated_private_ports", Status: "deactivated"}},
	}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshDetails(ctx))
	driveSlotCalls := fake.callCount("ListDriveSlots")

	// The feature is toggled; updated_at stays the same.
	fake.mu.Lock()
	fake.features = []api.Feature{{Name: "disaggregated_private_ports", Status: "activated"}}
	fake.mu.Unlock()

	mustRun(t, r.refreshHosts(ctx))
	if due := r.hostsDueForDetails(time.Now()); len(due) != 0 {
		t.Fatalf("details unexpectedly due: %+v", due)
	}
	mustRun(t, r.refreshFeatures(ctx))

	for _, id := range []string{"h1", "h2"} {
		features := r.store.Snapshot().Hosts[id].Details.Features
		if len(features) != 1 || features[0].Status != "activated" {
			t.Fatalf("%s features = %+v, want activated", id, features)
		}
	}
	if got := fake.callCount("ListDriveSlots"); got != driveSlotCalls {
		t.Fatalf("features refresh refetched drive slots: %d -> %d calls", driveSlotCalls, got)
	}
	if r.store.Snapshot().LastSuccess[cache.ResourceFeatures].IsZero() {
		t.Fatal("features refresh success not recorded")
	}
}

func TestFeaturesRefreshSkipsHostsWithoutDetails(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{{ID: "h1"}}}
	r := newTestRefresher(fake)
	ctx := context.Background()

	mustRun(t, r.refreshHosts(ctx))
	mustRun(t, r.refreshFeatures(ctx))

	if got := fake.callCount("ListFeatures"); got != 0 {
		t.Fatalf("ListFeatures calls = %d, want 0 for hosts without details", got)
	}
	if r.store.Snapshot().Hosts["h1"].Details != nil {
		t.Fatal("features refresh created partial details")
	}
}

func mustRun(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
