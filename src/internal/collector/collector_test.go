package collector

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

func testStore() *cache.Store {
	store := cache.NewStore("", slog.New(slog.DiscardHandler))
	store.Update(func(s *cache.Snapshot) {
		s.Racks["rack1"] = api.Rack{ID: "rack1", Name: "R01", LocationCode: "ams1"}

		// h1: redundant public uplink, aggregated; redundant private uplink
		// split with the disaggregation feature.
		s.Hosts["h1"] = &cache.Host{
			Server: api.DedicatedServer{
				ID: "h1", Title: "node-1", LocationID: 1, LocationCode: "ams1", RackID: "rack1",
				PowerStatus: "powered_on", Labels: map[string]string{"env": "prod", "k8s.role": "worker"},
			},
			Details: &cache.HostDetails{
				FetchedAt:     time.Unix(1000, 0),
				Configuration: &api.ConfigurationDetails{ServerModelID: 7, PublicUplinkID: 1, PrivateUplinkID: 2},
				Features: []api.Feature{
					{Name: "disaggregated_private_ports", Status: "activated"},
					{Name: "oob_public_access", Status: "deactivated"},
				},
				DriveSlots: []api.DriveSlot{
					{Position: 0, Interface: "SATA3", FormFactor: "2.5", DriveModel: &api.DriveModel{Name: "SSD 960GB", Capacity: 960, MediaType: "SSD"}},
					{Position: 1, Interface: "SATA3", FormFactor: "2.5"},
				},
			},
		}
		// h2: no labels, powered off, details not loaded yet.
		s.Hosts["h2"] = &cache.Host{Server: api.DedicatedServer{
			ID: "h2", Title: "node-2", LocationCode: "ams1", RackID: "unknown-rack", PowerStatus: "powered_off",
		}}

		s.Uplinks[cache.UplinkKey(1, 7)] = &cache.UplinkModels{Models: []api.UplinkModel{
			{ID: 1, Name: "Public 10 Gbps with redundancy", Speed: 10000, Redundancy: true},
			{ID: 2, Name: "Private 10 Gbps with redundancy", Speed: 10000, Redundancy: true},
		}}
	})
	return store
}

func TestHostMetrics(t *testing.T) {
	c := New(testStore(), []string{"*"})

	expected := `
# HELP serverscom_host_powered_on Whether the server is powered on (1) or not (0).
# TYPE serverscom_host_powered_on gauge
serverscom_host_powered_on{host_id="h1",location="ams1",rack="R01",title="node-1"} 1
serverscom_host_powered_on{host_id="h2",location="ams1",rack="unknown-rack",title="node-2"} 0
# HELP serverscom_host_uplink_aggregated Whether the uplink ports are aggregated on the switch side: redundant uplink without the disaggregated_*_ports feature.
# TYPE serverscom_host_uplink_aggregated gauge
serverscom_host_uplink_aggregated{host_id="h1",location="ams1",rack="R01",title="node-1",uplink="private"} 0
serverscom_host_uplink_aggregated{host_id="h1",location="ams1",rack="R01",title="node-1",uplink="public"} 1
# HELP serverscom_host_labels Servers.com labels of the server as label_* labels.
# TYPE serverscom_host_labels gauge
serverscom_host_labels{host_id="h1",label_env="prod",label_k8s_role="worker",location="ams1",rack="R01",title="node-1"} 1
serverscom_host_labels{host_id="h2",label_env="",label_k8s_role="",location="ams1",rack="unknown-rack",title="node-2"} 1
# HELP serverscom_host_drive_capacity Capacity of the installed drive as reported by the API (drive_model.capacity).
# TYPE serverscom_host_drive_capacity gauge
serverscom_host_drive_capacity{host_id="h1",location="ams1",position="0",rack="R01",title="node-1"} 960
# HELP serverscom_host_feature_enabled Whether a server feature is activated (1) or not (0).
# TYPE serverscom_host_feature_enabled gauge
serverscom_host_feature_enabled{feature="disaggregated_private_ports",host_id="h1",location="ams1",rack="R01",title="node-1"} 1
serverscom_host_feature_enabled{feature="oob_public_access",host_id="h1",location="ams1",rack="R01",title="node-1"} 0
`
	err := testutil.CollectAndCompare(c, strings.NewReader(expected),
		"serverscom_host_powered_on", "serverscom_host_uplink_aggregated", "serverscom_host_labels",
		"serverscom_host_drive_capacity", "serverscom_host_feature_enabled")
	if err != nil {
		t.Fatal(err)
	}
}

func TestHostLabelAllowlist(t *testing.T) {
	c := New(testStore(), []string{"env"})

	expected := `
# HELP serverscom_host_labels Servers.com labels of the server as label_* labels.
# TYPE serverscom_host_labels gauge
serverscom_host_labels{host_id="h1",label_env="prod",location="ams1",rack="R01",title="node-1"} 1
serverscom_host_labels{host_id="h2",label_env="",location="ams1",rack="unknown-rack",title="node-2"} 1
`
	if err := testutil.CollectAndCompare(c, strings.NewReader(expected), "serverscom_host_labels"); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorIsConsistent(t *testing.T) {
	// CollectAndLint catches duplicate series and inconsistent label sets.
	problems, err := testutil.CollectAndLint(New(testStore(), []string{"*"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}

func TestSanitizeLabelName(t *testing.T) {
	for in, want := range map[string]string{
		"env":                    "env",
		"app.kubernetes.io/name": "app_kubernetes_io_name",
		"team-name":              "team_name",
	} {
		if got := sanitizeLabelName(in); got != want {
			t.Errorf("sanitizeLabelName(%q) = %q, want %q", in, got, want)
		}
	}
}
