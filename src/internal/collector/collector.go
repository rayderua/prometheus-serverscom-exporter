// Package collector turns the cached snapshot into Prometheus metrics.
//
// The collector never calls the API: a scrape only reads the latest
// snapshot, so scrapes are cheap and do not consume the API rate limit.
//
// Conventions:
//   - every host metric carries host_id, title, location and rack, so Grafana
//     can filter by location and rack without joins;
//   - descriptive attributes live in *_info metrics with value 1;
//   - raw API statuses are exposed as {status="..."} 1, next to a 0/1 metric
//     for the state dashboards care about (powered_on, enabled, up, ...).
package collector

import (
	"slices"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

const namespace = "serverscom"

// API values the exporter interprets.
const (
	powerStatusOn = "powered_on"
	powerFeedOn   = "on"

	// featureStatusActivated is the status of an enabled feature.
	// TODO: verify the full status set against the live API; only the
	// transitional "activation" status is confirmed by the API client fixtures.
	featureStatusActivated = "activated"

	featureDisaggregatedPublic  = "disaggregated_public_ports"
	featureDisaggregatedPrivate = "disaggregated_private_ports"
)

// hostLabelNames are attached to every host metric.
var hostLabelNames = []string{"host_id", "title", "location", "rack"}

// Collector implements prometheus.Collector on top of a cache.Store.
type Collector struct {
	store      *cache.Store
	hostLabels *labelMapper
	disabled   cache.DisabledCollectors

	// labelNames are the label names of all metrics, without label_* host labels.
	labelNames map[string]bool

	hostInfo               *prometheus.Desc
	hostStatus             *prometheus.Desc
	hostOperationalStatus  *prometheus.Desc
	hostPowerStatus        *prometheus.Desc
	hostPoweredOn          *prometheus.Desc
	hostScheduledRelease   *prometheus.Desc
	hostConfigurationInfo  *prometheus.Desc
	hostRAMSize            *prometheus.Desc
	hostDriveSlotInfo      *prometheus.Desc
	hostDriveCapacity      *prometheus.Desc
	hostConnectionInfo     *prometheus.Desc
	hostConnections        *prometheus.Desc
	hostNetworkInfo        *prometheus.Desc
	hostFeatureStatus      *prometheus.Desc
	hostFeatureEnabled     *prometheus.Desc
	hostPowerFeedStatus    *prometheus.Desc
	hostPowerFeedUp        *prometheus.Desc
	hostUplinkInfo         *prometheus.Desc
	hostUplinkRedundant    *prometheus.Desc
	hostUplinkAggregated   *prometheus.Desc
	hostDetailsLastRefresh *prometheus.Desc
	l2SegmentInfo          *prometheus.Desc
	l2SegmentMemberInfo    *prometheus.Desc
	l2SegmentNetworkInfo   *prometheus.Desc
	locationInfo           *prometheus.Desc
	rackInfo               *prometheus.Desc
	refreshLastSuccess     *prometheus.Desc
}

// New creates a collector. allowedHostLabels selects which Servers.com host
// labels become label_* Prometheus labels; "*" allows all of them. Metrics of
// disabled collectors are not exposed.
func New(store *cache.Store, allowedHostLabels []string, disabled cache.DisabledCollectors) *Collector {
	labelNames := map[string]bool{}
	for _, name := range hostLabelNames {
		labelNames[name] = true
	}
	hostDesc := func(name, help string, extra ...string) *prometheus.Desc {
		for _, label := range extra {
			labelNames[label] = true
		}
		labels := append(append([]string{}, hostLabelNames...), extra...)
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "host", name), help, labels, nil)
	}
	desc := func(name, help string, labels ...string) *prometheus.Desc {
		for _, label := range labels {
			labelNames[label] = true
		}
		return prometheus.NewDesc(prometheus.BuildFQName(namespace, "", name), help, labels, nil)
	}

	return &Collector{
		store:      store,
		hostLabels: newLabelMapper(allowedHostLabels),
		disabled:   disabled,
		labelNames: labelNames,

		hostInfo: hostDesc("info", "Dedicated server attributes from the server list.",
			"rack_id", "type", "configuration", "public_ipv4", "private_ipv4", "oob_ipv4", "lease_start"),
		hostStatus: hostDesc("status", "Lifecycle status of the server (value is always 1).",
			"status"),
		hostOperationalStatus: hostDesc("operational_status", "Operational status of the server (value is always 1).",
			"status"),
		hostPowerStatus: hostDesc("power_status", "Power status of the server (value is always 1).",
			"status"),
		hostPoweredOn: hostDesc("powered_on", "Whether the server is powered on (1) or not (0)."),
		hostScheduledRelease: hostDesc("scheduled_release_timestamp_seconds",
			"Unix time the server is scheduled to be released at. Absent if no release is scheduled."),
		hostConfigurationInfo: hostDesc("configuration_info", "Ordered hardware and network configuration of the server.",
			"server_model", "operating_system", "public_uplink", "private_uplink", "bandwidth"),
		hostRAMSize: hostDesc("ram_size", "RAM size as reported by the API (configuration_details.ram_size)."),
		hostDriveSlotInfo: hostDesc("drive_slot_info", "Drive slot of the server and the installed drive model.",
			"position", "interface", "form_factor", "populated", "drive_model", "media_type"),
		hostDriveCapacity: hostDesc("drive_capacity", "Capacity of the installed drive as reported by the API (drive_model.capacity).",
			"position"),
		hostConnectionInfo: hostDesc("connection_info", "Switch port connected to the server.",
			"port", "type", "mac"),
		hostConnections: hostDesc("connections", "Number of switch ports connected to the server by type.",
			"type"),
		hostNetworkInfo: hostDesc("network_info", "IP network attached to the server.",
			"network_id", "network_title", "family", "interface_type", "cidr", "status", "distribution_method", "additional"),
		hostFeatureStatus: hostDesc("feature_status", "Status of a server feature (value is always 1).",
			"feature", "status"),
		hostFeatureEnabled: hostDesc("feature_enabled", "Whether a server feature is activated (1) or not (0).",
			"feature"),
		hostPowerFeedStatus: hostDesc("power_feed_status", "Status of a server power feed (value is always 1).",
			"feed", "type", "status"),
		hostPowerFeedUp: hostDesc("power_feed_up", "Whether a server power feed is on (1) or not (0).",
			"feed"),
		hostUplinkInfo: hostDesc("uplink_info", "Uplink model of the server.",
			"uplink", "model", "speed"),
		hostUplinkRedundant: hostDesc("uplink_redundant",
			"Whether the uplink model provides redundant (aggregatable) links (1) or a single link (0).",
			"uplink"),
		hostUplinkAggregated: hostDesc("uplink_aggregated",
			"Whether the uplink ports are aggregated on the switch side: redundant uplink without the disaggregated_*_ports feature.",
			"uplink"),
		hostDetailsLastRefresh: hostDesc("details_last_refresh_timestamp_seconds",
			"Unix time a per-server collector was last fetched.",
			"collector"),

		l2SegmentInfo: desc("l2_segment_info", "L2 segment attributes.",
			"segment_id", "segment", "type", "status", "location_group"),
		l2SegmentMemberInfo: desc("l2_segment_member_info", "Server attached to an L2 segment.",
			"segment_id", "segment", "host_id", "title", "location", "rack", "mode", "vlan", "status"),
		l2SegmentNetworkInfo: desc("l2_segment_network_info", "Network of an L2 segment.",
			"segment_id", "segment", "network_id", "network_title", "family", "interface_type", "cidr", "status"),
		locationInfo: desc("location_info", "Location (data center) attributes.",
			"location_id", "location", "name", "status"),
		rackInfo: desc("rack_info", "Rack attributes.",
			"rack_id", "rack", "location"),
		refreshLastSuccess: desc("exporter_last_refresh_success_timestamp_seconds",
			"Unix time of the last successful cache refresh by resource.",
			"resource"),
	}
}

// Describe sends nothing, which makes this an "unchecked" collector.
// It is required because serverscom_host_labels has a label set that depends
// on the labels present in the account.
func (c *Collector) Describe(chan<- *prometheus.Desc) {}

// Collect emits metrics for the current snapshot.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	snapshot := c.store.Snapshot()

	c.collectHostLabels(ch, snapshot)
	for _, host := range snapshot.Hosts {
		common := c.commonHostLabels(snapshot, host.Server)
		c.collectHost(ch, host.Server, common)
		if host.Details != nil {
			c.collectHostDetails(ch, snapshot, host, common)
		}
	}
	if c.enabled(cache.CollectorL2Segments) {
		c.collectL2Segments(ch, snapshot)
	}
	c.collectReference(ch, snapshot)

	for resource, ts := range snapshot.LastSuccess {
		gauge(ch, c.refreshLastSuccess, float64(ts.Unix()), string(resource))
	}
}

func (c *Collector) enabled(name cache.Collector) bool { return c.disabled.Enabled(name) }

// HostLabelPrefix starts the names of labels made from Servers.com host labels.
const HostLabelPrefix = "label_"

// LabelNames returns the label names the collector's metrics use, except the
// HostLabelPrefix ones. Extra labels must not reuse them.
func (c *Collector) LabelNames() []string {
	names := make([]string, 0, len(c.labelNames))
	for name := range c.labelNames {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// commonHostLabels returns values for hostLabelNames.
func (c *Collector) commonHostLabels(snapshot *cache.Snapshot, server api.DedicatedServer) []string {
	return []string{server.ID, server.Title, server.LocationCode, rackName(snapshot, server.RackID)}
}

// rackName resolves a rack ID to its name, falling back to the ID until the
// rack list has been loaded.
func rackName(snapshot *cache.Snapshot, rackID string) string {
	if rack, ok := snapshot.Racks[rackID]; ok && rack.Name != "" {
		return rack.Name
	}
	return rackID
}

func (c *Collector) collectHost(ch chan<- prometheus.Metric, s api.DedicatedServer, common []string) {
	gauge(ch, c.hostInfo, 1, with(common, s.RackID, s.Type, s.Configuration,
		s.PublicIPv4Address, s.PrivateIPv4Address, s.OOBIPv4Address, s.LeaseStartAt)...)
	gauge(ch, c.hostStatus, 1, with(common, s.Status)...)
	gauge(ch, c.hostOperationalStatus, 1, with(common, s.OperationalStatus)...)
	gauge(ch, c.hostPowerStatus, 1, with(common, s.PowerStatus)...)
	gauge(ch, c.hostPoweredOn, boolValue(s.PowerStatus == powerStatusOn), common...)

	if s.ScheduledReleaseAt != nil {
		gauge(ch, c.hostScheduledRelease, float64(s.ScheduledReleaseAt.Unix()), common...)
	}
}

func (c *Collector) collectHostDetails(ch chan<- prometheus.Metric, snapshot *cache.Snapshot, host *cache.Host, common []string) {
	d := host.Details
	for collector, ts := range d.Fetched {
		if c.enabled(collector) {
			gauge(ch, c.hostDetailsLastRefresh, float64(ts.Unix()), with(common, string(collector))...)
		}
	}

	if cfg := d.Configuration; cfg != nil {
		gauge(ch, c.hostConfigurationInfo, 1, with(common, cfg.ServerModelName, cfg.OperatingSystemFullName,
			cfg.PublicUplinkName, cfg.PrivateUplinkName, cfg.BandwidthName)...)
		gauge(ch, c.hostRAMSize, float64(cfg.RAMSize), common...)
	}

	if c.enabled(cache.CollectorDrives) {
		c.collectDrives(ch, d.DriveSlots, common)
	}
	if c.enabled(cache.CollectorPorts) {
		c.collectConnections(ch, d.Connections, common)
	}
	if c.enabled(cache.CollectorNetworks) {
		for _, n := range d.Networks {
			gauge(ch, c.hostNetworkInfo, 1, with(common, n.ID, n.Title, n.Family, n.InterfaceType, n.CIDR,
				n.Status, n.DistributionMethod, strconv.FormatBool(n.Additional))...)
		}
	}

	features := map[string]bool{}
	if c.enabled(cache.CollectorFeatures) {
		for _, f := range d.Features {
			enabled := f.Status == featureStatusActivated
			features[f.Name] = enabled
			gauge(ch, c.hostFeatureStatus, 1, with(common, f.Name, f.Status)...)
			gauge(ch, c.hostFeatureEnabled, boolValue(enabled), with(common, f.Name)...)
		}
	}

	if c.enabled(cache.CollectorPowerFeeds) {
		for _, feed := range d.PowerFeeds {
			gauge(ch, c.hostPowerFeedStatus, 1, with(common, feed.Name, feed.Type, feed.Status)...)
			gauge(ch, c.hostPowerFeedUp, boolValue(feed.Status == powerFeedOn), with(common, feed.Name)...)
		}
	}

	if c.enabled(cache.CollectorUplinks) {
		c.collectUplinks(ch, snapshot, host, features, common)
	}
}

func (c *Collector) collectDrives(ch chan<- prometheus.Metric, slots []api.DriveSlot, common []string) {
	for _, slot := range slots {
		position := strconv.Itoa(slot.Position)
		model, mediaType := "", ""
		if slot.DriveModel != nil {
			model, mediaType = slot.DriveModel.Name, slot.DriveModel.MediaType
			gauge(ch, c.hostDriveCapacity, float64(slot.DriveModel.Capacity), with(common, position)...)
		}
		gauge(ch, c.hostDriveSlotInfo, 1, with(common, position, slot.Interface, slot.FormFactor,
			strconv.FormatBool(slot.DriveModel != nil), model, mediaType)...)
	}
}

func (c *Collector) collectConnections(ch chan<- prometheus.Metric, connections []api.Connection, common []string) {
	connectionsByType := map[string]int{}
	for _, conn := range connections {
		connectionsByType[conn.Type]++
		gauge(ch, c.hostConnectionInfo, 1, with(common, conn.Port, conn.Type, conn.MACAddress)...)
	}
	for connType, count := range connectionsByType {
		gauge(ch, c.hostConnections, float64(count), with(common, connType)...)
	}
}

// collectUplinks combines the uplink model (is it redundant?) with the
// disaggregation features (was the LAG split?) into the aggregation state.
// Without the features collector the aggregation state is unknown and
// serverscom_host_uplink_aggregated is not exposed.
func (c *Collector) collectUplinks(ch chan<- prometheus.Metric, snapshot *cache.Snapshot, host *cache.Host, features map[string]bool, common []string) {
	cfg := host.Details.Configuration
	if cfg == nil {
		return
	}

	uplinks := []struct {
		name          string
		id            int64
		disaggregated string
	}{
		{"public", cfg.PublicUplinkID, featureDisaggregatedPublic},
		{"private", cfg.PrivateUplinkID, featureDisaggregatedPrivate},
	}

	for _, uplink := range uplinks {
		if uplink.id == 0 {
			continue // e.g. a server ordered without a public uplink
		}
		model, ok := snapshot.Uplink(host.Server.LocationID, cfg.ServerModelID, uplink.id)
		if !ok {
			continue // uplink catalogue not loaded yet
		}

		gauge(ch, c.hostUplinkInfo, 1, with(common, uplink.name, model.Name, strconv.Itoa(model.Speed))...)
		gauge(ch, c.hostUplinkRedundant, boolValue(model.Redundancy), with(common, uplink.name)...)
		if !c.enabled(cache.CollectorFeatures) {
			continue
		}
		aggregated := model.Redundancy && !features[uplink.disaggregated]
		gauge(ch, c.hostUplinkAggregated, boolValue(aggregated), with(common, uplink.name)...)
	}
}

func (c *Collector) collectHostLabels(ch chan<- prometheus.Metric, snapshot *cache.Snapshot) {
	all := make([]map[string]string, 0, len(snapshot.Hosts))
	for _, host := range snapshot.Hosts {
		all = append(all, host.Server.Labels)
	}
	names := c.hostLabels.names(all)

	desc := prometheus.NewDesc(prometheus.BuildFQName(namespace, "host", "labels"),
		"Servers.com labels of the server as label_* labels.",
		append(append([]string{}, hostLabelNames...), names.promNames...), nil)

	for _, host := range snapshot.Hosts {
		values := c.commonHostLabels(snapshot, host.Server)
		values = append(values, names.values(host.Server.Labels)...)
		gauge(ch, desc, 1, values...)
	}
}

func (c *Collector) collectL2Segments(ch chan<- prometheus.Metric, snapshot *cache.Snapshot) {
	for _, seg := range snapshot.Segments {
		s := seg.Segment
		gauge(ch, c.l2SegmentInfo, 1, s.ID, s.Name, s.Type, s.Status, s.LocationGroupCode)

		for _, m := range seg.Members {
			title, location, rack := m.Title, "", ""
			if host, ok := snapshot.Hosts[m.ID]; ok {
				title, location, rack = host.Server.Title, host.Server.LocationCode, rackName(snapshot, host.Server.RackID)
			}
			vlan := ""
			if m.VLAN != nil {
				vlan = strconv.Itoa(*m.VLAN)
			}
			gauge(ch, c.l2SegmentMemberInfo, 1, s.ID, s.Name, m.ID, title, location, rack, m.Mode, vlan, m.Status)
		}

		for _, n := range seg.Networks {
			gauge(ch, c.l2SegmentNetworkInfo, 1, s.ID, s.Name, n.ID, n.Title, n.Family, n.InterfaceType, n.CIDR, n.Status)
		}
	}
}

func (c *Collector) collectReference(ch chan<- prometheus.Metric, snapshot *cache.Snapshot) {
	for _, l := range snapshot.Locations {
		gauge(ch, c.locationInfo, 1, strconv.FormatInt(l.ID, 10), l.Code, l.Name, l.Status)
	}
	for _, r := range snapshot.Racks {
		gauge(ch, c.rackInfo, 1, r.ID, r.Name, r.LocationCode)
	}
}

// gauge emits a constant gauge sample.
func gauge(ch chan<- prometheus.Metric, desc *prometheus.Desc, value float64, labels ...string) {
	ch <- prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, labels...)
}

// with returns common followed by extra without modifying common.
func with(common []string, extra ...string) []string {
	return append(append(make([]string, 0, len(common)+len(extra)), common...), extra...)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
