package refresher

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
)

func TestPrintEstimateFormats(t *testing.T) {
	sizes := Sizes{Hosts: 20, Segments: 2, Locations: 3, Racks: 4, UplinkCatalogues: 1, Source: "test"}

	var out bytes.Buffer
	if err := PrintEstimate(&out, testConfig(), sizes, "json"); err != nil {
		t.Fatal(err)
	}
	var report estimateReport
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	if report.Inventory.Servers != 20 || report.StretchFactor != 1 || len(report.Loops) != 11 {
		t.Fatalf("report = %+v", report)
	}

	out.Reset()
	if err := PrintEstimate(&out, testConfig(), sizes, "logfmt"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Source: test", "LOOP", "features", "Total:", "Stretch factor: 1.00"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("table lacks %q:\n%s", want, out.String())
		}
	}
}

func TestFetchSizes(t *testing.T) {
	fake := &fakeAPI{servers: []api.DedicatedServer{
		{ID: "a", LocationID: 1, RackID: "r1", ConfigurationDetails: &api.ConfigurationDetails{ServerModelID: 7}},
		{ID: "b", LocationID: 1, RackID: "r1", ConfigurationDetails: &api.ConfigurationDetails{ServerModelID: 7}},
		{ID: "c", LocationID: 2, RackID: "r2", Configuration: "Dell R640"}, // no configuration_details
	}}
	sizes, err := FetchSizes(context.Background(), fake, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if sizes.Hosts != 3 || sizes.Locations != 2 || sizes.Racks != 2 || sizes.UplinkCatalogues != 2 || sizes.Source != "API" {
		t.Fatalf("sizes = %+v", sizes)
	}
	if fake.callCount("ListDedicatedServers") != 1 {
		t.Errorf("server list requests = %d, want 1", fake.callCount("ListDedicatedServers"))
	}
}
