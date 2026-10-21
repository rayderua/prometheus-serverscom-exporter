package refresher

import (
	"strings"
	"testing"
	"time"
)

// 39 servers with the features loop every 5 minutes, as in a real setup.
func testInputs(budget int, features time.Duration) demandInputs {
	return demandInputs{
		hosts: 39, segments: 2, locations: 3, racks: 4, uplinkCatalogues: 1,
		pageSize: 100, budget: budget, reserve: 300, accountLimit: 2000,
		cfg: Config{
			HostsInterval: 2 * time.Minute, DetailsInterval: 12 * time.Hour, FeaturesInterval: features,
			L2Interval: 15 * time.Minute, ReferenceInterval: 24 * time.Hour,
		},
	}
}

func TestEstimateDemand(t *testing.T) {
	e := estimateDemand(testInputs(600, 5*time.Minute))

	perHour := map[string]float64{}
	for _, l := range e.loops {
		perHour[l.name] = l.perHour()
	}
	// hosts: 1 page every 2m; details: 39*6 every 12h; features: 39 every 5m;
	// L2: 1 page + 2*2 segments every 15m; reference: 1+1+1 every 24h.
	want := map[string]float64{
		demandHosts: 30, demandDetails: 19.5, demandFeatures: 468, demandL2: 20, demandReference: 0.125,
	}
	for name, w := range want {
		if got := perHour[name]; got != w {
			t.Errorf("%s per hour = %v, want %v", name, got, w)
		}
	}
	// Full refresh: hosts page + details + L2 + reference, features included in details.
	if e.fullRefresh != 1+234+5+3 {
		t.Errorf("full refresh = %v, want 243", e.fullRefresh)
	}
	// 537.6 * 1.25 = 672 -> 700; fits into 2000 - 300.
	if e.recommendedBudget != 700 || !e.budgetTooSmall || e.intervalsTooShort {
		t.Errorf("recommended budget = %d too small = %v intervals too short = %v",
			e.recommendedBudget, e.budgetTooSmall, e.intervalsTooShort)
	}
	if got := e.recommendation(); got != "--api.requests-per-hour=700" {
		t.Errorf("recommendation = %q", got)
	}
}

func TestEstimateDemandFits(t *testing.T) {
	e := estimateDemand(testInputs(600, time.Hour))
	if e.budgetTooSmall || e.intervalsTooShort || e.recommendation() != "" {
		t.Fatalf("expected no recommendation, got %q (%+v)", e.recommendation(), e)
	}
}

func TestEstimateDemandRecommendsLongerIntervals(t *testing.T) {
	// Features every 30s: 39*120 = 4680/h, far above what the account leaves.
	e := estimateDemand(testInputs(600, 30*time.Second))
	if !e.intervalsTooShort || e.recommendedBudget != 1700 {
		t.Fatalf("intervals too short = %v recommended budget = %d", e.intervalsTooShort, e.recommendedBudget)
	}

	var features demandLoop
	for _, l := range e.loops {
		if l.name == demandFeatures {
			features = l
		}
	}
	if features.recommended <= features.interval {
		t.Fatalf("features interval not raised: %v", features.recommended)
	}

	// With every recommendation applied the demand fits into the account.
	in := testInputs(e.recommendedBudget, features.recommended)
	for _, l := range e.loops {
		switch l.name {
		case demandHosts:
			in.cfg.HostsInterval = l.recommended
		case demandL2:
			in.cfg.L2Interval = l.recommended
		case demandDetails:
			in.cfg.DetailsInterval = l.recommended
		}
	}
	if after := estimateDemand(in); after.intervalsTooShort || after.perHour*demandHeadroom > 1700 {
		t.Fatalf("recommendation does not fit: %v/h", after.perHour)
	}
	if rec := e.recommendation(); !strings.Contains(rec, "--refresh.features-interval=") {
		t.Fatalf("recommendation = %q", rec)
	}
}

func TestEstimateDemandDisabledFeatures(t *testing.T) {
	e := estimateDemand(testInputs(600, 0))
	for _, l := range e.loops {
		if l.name == demandFeatures && (l.perHour() != 0 || l.recommended != 0) {
			t.Fatalf("disabled features loop: %+v", l)
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
