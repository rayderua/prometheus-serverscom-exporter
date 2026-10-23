package refresher

import (
	"time"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/cache"
)

// defaultIntervals are the refresh intervals without flags: often for what
// signals a problem (split bonding, public OOB access, a power feed down),
// rarely for inventory that is only shown in overviews. Changed servers are
// downloaded right away anyway (see refreshDetails).
var defaultIntervals = map[cache.Collector]time.Duration{
	cache.CollectorConfiguration: 24 * time.Hour,
	cache.CollectorDrives:        24 * time.Hour,
	cache.CollectorPorts:         24 * time.Hour,
	cache.CollectorNetworks:      24 * time.Hour,
	cache.CollectorFeatures:      15 * time.Minute,
	cache.CollectorPowerFeeds:    15 * time.Minute,
	cache.CollectorServices:      24 * time.Hour, // the API updates prices once a day
	cache.CollectorUplinks:       24 * time.Hour,
	cache.CollectorL2Segments:    time.Hour,
	cache.CollectorReference:     6 * time.Hour,
}

// DefaultInterval returns the default refresh interval of collector c.
func DefaultInterval(c cache.Collector) time.Duration {
	if d, ok := defaultIntervals[c]; ok {
		return d
	}
	return 24 * time.Hour
}
