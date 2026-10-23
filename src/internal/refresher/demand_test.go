package refresher

import (
	"testing"
	"time"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// testInputs: 20 servers, 2 L2 segments, 3 locations, 4 racks, 1 uplink
// catalogue, budget 400, default intervals.
func testInputs() demandInputs {
	return demandInputs{
		hosts: 20, segments: 2, locations: 3, racks: 4, uplinkCatalogues: 1,
		pageSize: 100, budget: 400, reserve: 300, accountLimit: 2000, cfg: testConfig(),
	}
}

func loopByName(e demandEstimate, name string) (demandLoop, bool) {
	for _, l := range e.loops {
		if l.name == name {
			return l, true
		}
	}
	return demandLoop{}, false
}

func TestDisabledCollectorsCostNothing(t *testing.T) {
	in := testInputs()
	in.cfg.Disabled = cache.DisabledCollectors{cache.CollectorDrives: true, cache.CollectorL2Segments: true}
	e := estimateDemand(in)

	for _, name := range []string{"drives", "l2-segments"} {
		if _, ok := loopByName(e, name); ok {
			t.Errorf("disabled %s is estimated", name)
		}
	}
	// Full refresh: 1 hosts page + 6 per-server collectors x 20 + 1 uplink
	// catalogue + 2 reference pages.
	if e.fullRefresh != 1+120+1+2 {
		t.Errorf("full refresh = %v", e.fullRefresh)
	}
}

func TestFormatDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		90 * time.Minute: "1h30m", time.Hour: "1h", 5 * time.Minute: "5m", 45 * time.Second: "45s",
	} {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestDefaultIntervalsFitWithoutStretch(t *testing.T) {
	e := estimateDemand(testInputs())
	if e.stretch != 1 {
		t.Fatalf("stretch = %v, want 1", e.stretch)
	}
	for _, l := range e.loops {
		if l.interval != l.configured || l.stretched {
			t.Errorf("%s: interval %v, configured %v", l.name, l.interval, l.configured)
		}
	}
	if rec := e.recommendation(); rec != "" {
		t.Errorf("recommendation = %q, want none", rec)
	}
}

func TestStretchKeepsTheServerList(t *testing.T) {
	// Budget 200: 160/h usable. Hosts 30/h stay; the rest needs 169.5/h at
	// the configured intervals and gets 130/h: stretch 1.30.
	in := testInputs()
	in.budget = 200
	e := estimateDemand(in)
	if e.stretch < 1.30 || e.stretch > 1.31 || e.stretchHosts {
		t.Fatalf("stretch = %v, hosts stretched = %v", e.stretch, e.stretchHosts)
	}
	hosts, _ := loopByName(e, "hosts")
	features, _ := loopByName(e, "features")
	if hosts.interval != 2*time.Minute || hosts.stretched {
		t.Errorf("hosts = %+v, want 2m, not stretched", hosts)
	}
	if features.interval != 20*time.Minute || !features.stretched {
		t.Errorf("features = %+v, want 20m, stretched", features)
	}
	if got := e.intervals()["features"]; got != 20*time.Minute {
		t.Errorf("intervals()[features] = %v", got)
	}
	// 199.5/h configured / 0.8 = 249.4, rounded up to 250.
	if rec := e.recommendation(); rec != "--api.requests-per-hour=250" {
		t.Errorf("recommendation = %q", rec)
	}
}

func TestServerListAloneDoesNotFit(t *testing.T) {
	// Hosts every 10s: 360/h, more than the 320/h usable. Every loop, the
	// list included, is stretched by 529.5 / 320.
	in := testInputs()
	in.cfg.HostsInterval = 10 * time.Second
	e := estimateDemand(in)
	if !e.stretchHosts || e.stretch < 1.65 || e.stretch > 1.66 {
		t.Fatalf("stretch = %v, hosts stretched = %v", e.stretch, e.stretchHosts)
	}
	for _, l := range e.loops {
		if !l.stretched || l.interval <= l.configured {
			t.Errorf("%s not stretched: %+v", l.name, l)
		}
	}
	if e.perHour > e.usable+1 {
		t.Errorf("stretched demand %v/h does not fit into %v/h", e.perHour, e.usable)
	}
}

func TestAccountTooSmall(t *testing.T) {
	// Features every 30s: 2400/h, more than 80% of the 1700 the account
	// leaves even with a larger budget.
	in := testInputs()
	in.budget = 5000
	in.cfg.Intervals = map[cache.Collector]time.Duration{cache.CollectorFeatures: 30 * time.Second}
	e := estimateDemand(in)
	if !e.accountTooSmall || e.stretch <= 1 || e.recommendedBudget != 1700 {
		t.Fatalf("account too small = %v, stretch = %v, recommended = %d", e.accountTooSmall, e.stretch, e.recommendedBudget)
	}
}
