package refresher

import (
	"strings"
	"testing"
	"time"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// testInputs: 20 servers, 2 L2 segments, 3 locations, 4 racks, 1 uplink
// catalogue, budget 400, hosts every 2m, everything else auto.
func testInputs(intervals map[cache.Collector]Interval) demandInputs {
	cfg := testConfig()
	cfg.Intervals = intervals
	return demandInputs{
		hosts: 20, segments: 2, locations: 3, racks: 4, uplinkCatalogues: 1,
		pageSize: 100, budget: 400, reserve: 300, accountLimit: 2000, cfg: cfg,
	}
}

func fixed(c cache.Collector, d time.Duration) map[cache.Collector]Interval {
	return map[cache.Collector]Interval{c: {Fixed: d}}
}

func loopByName(e demandEstimate, name string) (demandLoop, bool) {
	for _, l := range e.loops {
		if l.name == name {
			return l, true
		}
	}
	return demandLoop{}, false
}

func TestAutoInterval(t *testing.T) {
	e := estimateDemand(testInputs(nil))

	// Hosts: 1 page every 2m = 30/h, fixed. Auto per run: 6 collectors x 20
	// servers + 1 uplink catalogue + L2 (1 page + 2x2) + reference (2 pages)
	// = 128, into 400*0.8-30 = 290/h: 26.5m, rounded up to 30m.
	if e.autoInterval != 30*time.Minute || e.autoTooSlow {
		t.Fatalf("auto interval = %v, too slow = %v, want 30m", e.autoInterval, e.autoTooSlow)
	}
	for _, l := range e.loops {
		want := e.autoInterval
		if l.name == "hosts" {
			want = 2 * time.Minute
		}
		if l.interval != want || l.auto != (l.name != "hosts") {
			t.Errorf("%s: interval = %v auto = %v", l.name, l.interval, l.auto)
		}
	}
	if e.fixedPerHour != 30 || e.perHour != 30+128*2 || e.fullRefresh != 1+128 {
		t.Errorf("fixed = %v, per hour = %v, full refresh = %v", e.fixedPerHour, e.perHour, e.fullRefresh)
	}
	if rec := e.recommendation(); rec != "" {
		t.Errorf("recommendation = %q, want none", rec)
	}
}

func TestAutoIntervalWithFixedCollector(t *testing.T) {
	// Features fixed at 15m: 20 x 4 = 80/h, paid first. Auto: 108 per run into
	// 320-110 = 210/h: 30.9m, rounded up to 35m.
	e := estimateDemand(testInputs(fixed(cache.CollectorFeatures, 15*time.Minute)))
	if e.autoInterval != 35*time.Minute {
		t.Fatalf("auto interval = %v, want 35m", e.autoInterval)
	}
	if features, _ := loopByName(e, "features"); features.auto || features.interval != 15*time.Minute {
		t.Fatalf("features = %+v, want fixed 15m", features)
	}
}

func TestAutoIntervalBounds(t *testing.T) {
	in := testInputs(nil)
	in.cfg.AutoMinInterval = 45 * time.Minute
	if e := estimateDemand(in); e.autoInterval != 45*time.Minute {
		t.Errorf("auto interval = %v, want the 45m minimum", e.autoInterval)
	}

	in = testInputs(nil)
	in.cfg.AutoMaxInterval = 20 * time.Minute
	if e := estimateDemand(in); e.autoInterval != 20*time.Minute || !e.autoTooSlow {
		t.Errorf("auto interval = %v, too slow = %v, want the 20m maximum and a warning", e.autoInterval, e.autoTooSlow)
	}
}

func TestAutoCollectorsDoNotFitBesideFixedOnes(t *testing.T) {
	// Features every minute: 1200/h, more than the 320 auto collectors may use.
	e := estimateDemand(testInputs(fixed(cache.CollectorFeatures, time.Minute)))
	if e.autoInterval != 24*time.Hour || !e.autoTooSlow || e.intervalsTooShort {
		t.Fatalf("auto interval = %v, too slow = %v, intervals too short = %v",
			e.autoInterval, e.autoTooSlow, e.intervalsTooShort)
	}
	// (1230 fixed + 108 auto per 24h) / 0.8 = 1543 -> 1550, below 2000 - 300.
	if rec := e.recommendation(); rec != "--api.requests-per-hour=1550" {
		t.Fatalf("recommendation = %q", rec)
	}
}

func TestFixedIntervalsTooShortForTheAccount(t *testing.T) {
	// Features every 30s: 2400/h, more than the whole account leaves.
	in := testInputs(fixed(cache.CollectorFeatures, 30*time.Second))
	e := estimateDemand(in)
	if !e.intervalsTooShort || e.recommendedBudget != 1700 {
		t.Fatalf("intervals too short = %v, recommended budget = %d", e.intervalsTooShort, e.recommendedBudget)
	}
	rec := e.recommendation()
	if !strings.Contains(rec, "--api.requests-per-hour=1700") || !strings.Contains(rec, "--refresh.features-interval=") {
		t.Fatalf("recommendation = %q", rec)
	}

	// With every recommendation applied the fixed loops fit.
	in.budget = e.recommendedBudget
	features, _ := loopByName(e, "features")
	hosts, _ := loopByName(e, "hosts")
	in.cfg.Intervals = fixed(cache.CollectorFeatures, features.recommended)
	in.cfg.HostsInterval = hosts.recommended
	if after := estimateDemand(in); after.intervalsTooShort || after.autoTooSlow {
		t.Fatalf("recommendation does not fit: fixed %v/h, auto interval %v", after.fixedPerHour, after.autoInterval)
	}
}

func TestDisabledCollectorsCostNothing(t *testing.T) {
	in := testInputs(nil)
	in.cfg.Disabled = cache.DisabledCollectors{cache.CollectorDrives: true, cache.CollectorL2Segments: true}
	e := estimateDemand(in)

	for _, name := range []string{"drives", "l2-segments"} {
		if _, ok := loopByName(e, name); ok {
			t.Errorf("disabled %s is estimated", name)
		}
	}
	// Auto: 5 x 20 + 1 uplink catalogue + 2 reference pages = 103 into 290/h:
	// 21.3m, rounded up to 25m.
	if e.autoInterval != 25*time.Minute || e.fullRefresh != 1+103 {
		t.Errorf("auto interval = %v, full refresh = %v", e.autoInterval, e.fullRefresh)
	}
}

func TestIntervalFlag(t *testing.T) {
	var i Interval
	for in, want := range map[string]Interval{"auto": AutoInterval, "15m": {Fixed: 15 * time.Minute}, "1h30m": {Fixed: 90 * time.Minute}} {
		if err := i.Set(in); err != nil || i != want {
			t.Errorf("Set(%q) = %+v, %v; want %+v", in, i, err, want)
		}
		if i.String() != in {
			t.Errorf("String() = %q, want %q", i.String(), in)
		}
	}
	for _, in := range []string{"0", "-5m", "often", ""} {
		if err := i.Set(in); err == nil {
			t.Errorf("Set(%q) accepted", in)
		}
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
