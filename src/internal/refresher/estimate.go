package refresher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"text/tabwriter"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// Sizes are the inventory sizes the estimate is based on.
type Sizes struct {
	Hosts            int
	Segments         int
	Locations        int
	Racks            int
	UplinkCatalogues int

	// AccountLimit is the account-wide hourly limit; 0 means the documented
	// one.
	AccountLimit int
	// Source says where the sizes come from, e.g. "API" or the cache file.
	Source string
}

// SizesOf returns the sizes of the inventory in s.
func SizesOf(s *cache.Snapshot) Sizes {
	return Sizes{
		Hosts:            len(s.Hosts),
		Segments:         len(s.Segments),
		Locations:        len(s.Locations),
		Racks:            len(s.Racks),
		UplinkCatalogues: len(s.Uplinks),
	}
}

// FetchSizes reads the inventory sizes from the API with as few requests as
// possible: the server list, and the L2 segment list when that collector is
// enabled. Locations and racks are counted from the servers, which is what
// matters for the page count. Uplink catalogues are the distinct pairs of
// location and server model; when the list has no configuration_details,
// the configuration name stands in for the server model.
func FetchSizes(ctx context.Context, client API, cfg Config) (Sizes, error) {
	servers, err := client.ListDedicatedServers(ctx)
	if err != nil {
		return Sizes{}, err
	}
	locations := map[int64]bool{}
	racks := map[string]bool{}
	catalogues := map[string]bool{}
	for _, s := range servers {
		locations[s.LocationID] = true
		if s.RackID != "" {
			racks[s.RackID] = true
		}
		if d := s.ConfigurationDetails; d != nil && d.ServerModelID != 0 {
			catalogues[cache.UplinkKey(s.LocationID, d.ServerModelID)] = true
		} else if s.Configuration != "" {
			catalogues[fmt.Sprint(s.LocationID, "/", s.Configuration)] = true
		}
	}
	sizes := Sizes{
		Hosts:            len(servers),
		Locations:        len(locations),
		Racks:            len(racks),
		UplinkCatalogues: len(catalogues),
		Source:           "API",
	}
	if cfg.Disabled.Enabled(cache.CollectorL2Segments) {
		segments, err := client.ListL2Segments(ctx)
		if err != nil {
			return Sizes{}, err
		}
		sizes.Segments = len(segments)
	}
	sizes.AccountLimit = client.RateLimit().Limit
	return sizes, nil
}

// PrintEstimate writes the refresh intervals, the request demand and the
// recommendations of cfg for an inventory of the given sizes, the same way
// the running exporter computes them. format is the log format: "json"
// prints a JSON document, anything else a table for people.
func PrintEstimate(w io.Writer, cfg Config, sizes Sizes, format string) error {
	e := estimateDemand(demandInputs{
		hosts:            sizes.Hosts,
		segments:         sizes.Segments,
		locations:        sizes.Locations,
		racks:            sizes.Racks,
		uplinkCatalogues: sizes.UplinkCatalogues,
		pageSize:         cfg.PageSize,
		budget:           cfg.BudgetPerHour,
		reserve:          cfg.Reserve,
		accountLimit:     sizes.AccountLimit,
		cfg:              cfg,
	})
	accountLimit := sizes.AccountLimit
	if accountLimit <= 0 {
		accountLimit = defaultAccountLimit
	}

	report := estimateReport{
		Source: sizes.Source,
		Inventory: estimateInventory{
			Servers: sizes.Hosts, L2Segments: sizes.Segments, Locations: sizes.Locations, Racks: sizes.Racks,
			UplinkCatalogues: sizes.UplinkCatalogues,
		},
		Budget: estimateBudget{
			PerHour: e.budget, PlannedShare: budgetShare, Planned: math.Round(e.usable),
			AccountLimit: accountLimit, Reserve: cfg.Reserve, AccountAvailable: e.available,
		},
		RequestsPerHour:           math.Round(e.perHour),
		RequestsPerHourConfigured: math.Round(e.configuredPerHour),
		FullRefreshRequests:       math.Round(e.fullRefresh),
		FullRefreshBudgetWindows:  math.Ceil(e.fullRefresh / float64(max(e.budget, 1))),
		StretchFactor:             math.Round(e.stretch*100) / 100,
		RecommendedBudget:         e.recommendedBudget,
		RecommendedFlags:          e.recommendation(),
		Warnings:                  []string{},
	}
	for _, l := range e.loops {
		report.Loops = append(report.Loops, estimateLoop{
			Name:              l.name,
			Configured:        formatDuration(l.configured),
			Effective:         formatDuration(l.interval),
			ConfiguredSeconds: l.configured.Seconds(),
			EffectiveSeconds:  l.interval.Seconds(),
			Stretched:         l.stretched,
			RequestsPerPass:   l.requests,
			RequestsPerHour:   math.Round(l.perHour()),
		})
	}
	switch {
	case e.accountTooSmall:
		report.Warnings = append(report.Warnings, "the configured intervals need more requests than the account "+
			"limit leaves for the exporter; they are stretched, raise them to make it explicit")
	case e.stretch > 1:
		report.Warnings = append(report.Warnings,
			"the configured intervals do not fit into the budget and are stretched; raise the budget to keep them")
	}
	if report.FullRefreshBudgetWindows > 1 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"a start without a cache file needs about %.0f budget windows (hours)", report.FullRefreshBudgetWindows))
	}

	if format == "json" {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	return report.writeText(w)
}

// printer remembers the first write error, so the report is written without
// checking every line.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) f(format string, args ...any) {
	if p.err == nil {
		_, p.err = fmt.Fprintf(p.w, format, args...)
	}
}

// writeText prints the report as a table with notes.
func (r estimateReport) writeText(out io.Writer) error {
	p := &printer{w: out}
	if r.Source != "" {
		p.f("Source: %s\n", r.Source)
	}
	inv, b := r.Inventory, r.Budget
	p.f("Inventory: %d servers, %d L2 segments, %d locations, %d racks, %d uplink catalogues\n",
		inv.Servers, inv.L2Segments, inv.Locations, inv.Racks, inv.UplinkCatalogues)
	p.f("Budget: %d requests/h, loops are planned for %.0f%% of it (%.0f); account limit %d - reserve %d = %d available\n\n",
		b.PerHour, b.PlannedShare*100, b.Planned, b.AccountLimit, b.Reserve, b.AccountAvailable)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	t := &printer{w: tw}
	t.f("LOOP\tCONFIGURED\tEFFECTIVE\tREQUESTS PER PASS\tREQUESTS/H\n")
	for _, l := range r.Loops {
		effective := l.Effective
		if l.Stretched {
			effective += " (stretched)"
		}
		t.f("%s\t%s\t%s\t%.0f\t%.0f\n", l.Name, l.Configured, effective, l.RequestsPerPass, l.RequestsPerHour)
	}
	if t.err != nil {
		return t.err
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	p.f("A pass refreshes the loop once, e.g. every server once, as fast as the API answers.\n")
	p.f("REQUESTS/H = requests per pass / effective interval.\n")

	p.f("\nTotal: %.0f requests/h (%.0f at the configured intervals), budget %d\n",
		r.RequestsPerHour, r.RequestsPerHourConfigured, b.PerHour)
	p.f("Full refresh (start without a cache file): %.0f requests\n", r.FullRefreshRequests)
	p.f("Stretch factor: %.2f\n", r.StretchFactor)
	p.f("Recommended budget: %d requests/h\n", r.RecommendedBudget)
	if r.RecommendedFlags != "" {
		p.f("Recommended flags: %s\n", r.RecommendedFlags)
	}
	for _, warning := range r.Warnings {
		p.f("WARNING: %s\n", warning)
	}
	return p.err
}

// estimateReport is the JSON document --estimate prints. A pass refreshes a
// loop once (e.g. every server once); requests_per_hour = requests_per_pass /
// effective interval.
type estimateReport struct {
	Source                    string            `json:"source,omitempty"`
	Inventory                 estimateInventory `json:"inventory"`
	Budget                    estimateBudget    `json:"budget"`
	Loops                     []estimateLoop    `json:"loops"`
	RequestsPerHour           float64           `json:"requests_per_hour"`
	RequestsPerHourConfigured float64           `json:"requests_per_hour_configured"`
	FullRefreshRequests       float64           `json:"full_refresh_requests"`
	FullRefreshBudgetWindows  float64           `json:"full_refresh_budget_windows"`
	StretchFactor             float64           `json:"stretch_factor"`
	RecommendedBudget         int               `json:"recommended_budget"`
	RecommendedFlags          string            `json:"recommended_flags,omitempty"`
	Warnings                  []string          `json:"warnings"`
}

type estimateInventory struct {
	Servers          int `json:"servers"`
	L2Segments       int `json:"l2_segments"`
	Locations        int `json:"locations"`
	Racks            int `json:"racks"`
	UplinkCatalogues int `json:"uplink_catalogues"`
}

type estimateBudget struct {
	PerHour          int     `json:"per_hour"`
	PlannedShare     float64 `json:"planned_share"`
	Planned          float64 `json:"planned"`
	AccountLimit     int     `json:"account_limit"`
	Reserve          int     `json:"reserve"`
	AccountAvailable int     `json:"account_available"`
}

type estimateLoop struct {
	Name              string  `json:"name"`
	Configured        string  `json:"configured"`
	Effective         string  `json:"effective"`
	ConfiguredSeconds float64 `json:"configured_seconds"`
	EffectiveSeconds  float64 `json:"effective_seconds"`
	Stretched         bool    `json:"stretched"`
	RequestsPerPass   float64 `json:"requests_per_pass"`
	RequestsPerHour   float64 `json:"requests_per_hour"`
}
