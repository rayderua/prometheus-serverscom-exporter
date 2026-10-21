package cache

// Collector names a group of data fetched from one API endpoint with its own
// refresh interval (--refresh.<name>-interval). Optional collectors can be
// turned off with --no-collector.<name>; a disabled collector is neither
// fetched nor exposed.
type Collector string

// Per-server collectors: one request per server and refresh.
const (
	CollectorConfiguration Collector = "configuration" // the server itself: model, RAM, uplinks; always on
	CollectorDrives        Collector = "drives"        // drive slots
	CollectorPorts         Collector = "ports"         // switch port connections
	CollectorNetworks      Collector = "networks"      // IP networks
	CollectorFeatures      Collector = "features"      // features (port aggregation, OOB access)
	CollectorPowerFeeds    Collector = "power-feeds"   // power feeds
)

// Account-wide collectors.
const (
	CollectorUplinks    Collector = "uplinks"     // uplink model catalogues of the server models in use
	CollectorL2Segments Collector = "l2-segments" // L2 segments, members, networks
	CollectorReference  Collector = "reference"   // locations and racks; always on
)

// HostCollectors are fetched per server.
var HostCollectors = []Collector{
	CollectorConfiguration, CollectorDrives, CollectorPorts, CollectorNetworks, CollectorFeatures, CollectorPowerFeeds,
}

// Collectors lists every collector with a refresh interval.
var Collectors = append(append([]Collector{}, HostCollectors...), CollectorUplinks, CollectorL2Segments, CollectorReference)

// OptionalCollectors can be turned off.
var OptionalCollectors = []Collector{
	CollectorDrives, CollectorPorts, CollectorNetworks, CollectorFeatures, CollectorPowerFeeds,
	CollectorUplinks, CollectorL2Segments,
}

// DisabledCollectors is the set of turned off collectors. The zero value
// enables everything.
type DisabledCollectors map[Collector]bool

// Enabled reports whether c is on.
func (d DisabledCollectors) Enabled(c Collector) bool { return !d[c] }

// List returns the disabled collectors in OptionalCollectors order.
func (d DisabledCollectors) List() []Collector {
	var list []Collector
	for _, c := range OptionalCollectors {
		if d[c] {
			list = append(list, c)
		}
	}
	return list
}
