// Package cache keeps the exporter's view of the Servers.com account.
//
// The data is held in an immutable Snapshot. Writers never modify a published
// snapshot: Store.Update hands them a shallow copy, they replace whole
// entries, and the copy is then published atomically. Readers (the
// Prometheus collector) therefore get a consistent view without locks.
package cache

import (
	"maps"
	"strconv"
	"time"

	"serverscom-exporter/internal/api"
)

// Resource names a group of cached data refreshed together.
type Resource string

const (
	ResourceHosts       Resource = "hosts"
	ResourceHostDetails Resource = "host_details"
	ResourceL2Segments  Resource = "l2_segments"
	ResourceLocations   Resource = "locations"
	ResourceRacks       Resource = "racks"
	ResourceUplinks     Resource = "uplink_models"
)

// Snapshot is a point-in-time view of the account.
//
// Treat every value reachable from a published Snapshot as read-only.
// To change an entry, build a new value and store it under the same key.
type Snapshot struct {
	// Hosts is keyed by host ID.
	Hosts map[string]*Host `json:"hosts"`

	// Segments is keyed by L2 segment ID.
	Segments map[string]*Segment `json:"segments"`

	// Locations is keyed by location ID.
	Locations map[int64]api.Location `json:"locations"`

	// Racks is keyed by rack ID.
	Racks map[string]api.Rack `json:"racks"`

	// Uplinks is keyed by UplinkKey(location ID, server model ID).
	Uplinks map[string]*UplinkModels `json:"uplinks"`

	// LastSuccess holds the time of the last successful refresh per resource.
	LastSuccess map[Resource]time.Time `json:"last_success"`
}

// Host combines the frequently refreshed server summary with rarely
// refreshed details.
type Host struct {
	// Server is the entry from the server list (status, power, labels, ...).
	Server api.DedicatedServer `json:"server"`

	// Details is nil until the first detail refresh of this host.
	Details *HostDetails `json:"details,omitempty"`
}

// HostDetails are per-server sub-resources that rarely change.
type HostDetails struct {
	// FetchedAt is when the details were downloaded.
	FetchedAt time.Time `json:"fetched_at"`

	// SourceUpdatedAt is the server's updated_at at download time. When the
	// list reports a different updated_at, the details are refreshed early.
	SourceUpdatedAt time.Time `json:"source_updated_at"`

	Configuration *api.ConfigurationDetails `json:"configuration,omitempty"`
	DriveSlots    []api.DriveSlot           `json:"drive_slots"`
	Connections   []api.Connection          `json:"connections"`
	Networks      []api.Network             `json:"networks"`
	Features      []api.Feature             `json:"features"`
	PowerFeeds    []api.PowerFeed           `json:"power_feeds"`
}

// Segment is an L2 segment with its members and networks.
type Segment struct {
	Segment  api.L2Segment  `json:"segment"`
	Members  []api.L2Member `json:"members"`
	Networks []api.Network  `json:"networks"`
}

// UplinkModels is the uplink catalogue of one server model in one location.
type UplinkModels struct {
	FetchedAt time.Time         `json:"fetched_at"`
	Models    []api.UplinkModel `json:"models"`
}

// UplinkKey builds the Snapshot.Uplinks key.
func UplinkKey(locationID, serverModelID int64) string {
	return strconv.FormatInt(locationID, 10) + "/" + strconv.FormatInt(serverModelID, 10)
}

// NewSnapshot returns an empty snapshot with all maps initialised.
func NewSnapshot() *Snapshot {
	return &Snapshot{
		Hosts:       map[string]*Host{},
		Segments:    map[string]*Segment{},
		Locations:   map[int64]api.Location{},
		Racks:       map[string]api.Rack{},
		Uplinks:     map[string]*UplinkModels{},
		LastSuccess: map[Resource]time.Time{},
	}
}

// clone returns a shallow copy: maps are copied, their values are shared.
func (s *Snapshot) clone() *Snapshot {
	return &Snapshot{
		Hosts:       maps.Clone(s.Hosts),
		Segments:    maps.Clone(s.Segments),
		Locations:   maps.Clone(s.Locations),
		Racks:       maps.Clone(s.Racks),
		Uplinks:     maps.Clone(s.Uplinks),
		LastSuccess: maps.Clone(s.LastSuccess),
	}
}

// ensureMaps replaces nil maps, e.g. after loading an older cache file.
func (s *Snapshot) ensureMaps() {
	empty := NewSnapshot()
	if s.Hosts == nil {
		s.Hosts = empty.Hosts
	}
	if s.Segments == nil {
		s.Segments = empty.Segments
	}
	if s.Locations == nil {
		s.Locations = empty.Locations
	}
	if s.Racks == nil {
		s.Racks = empty.Racks
	}
	if s.Uplinks == nil {
		s.Uplinks = empty.Uplinks
	}
	if s.LastSuccess == nil {
		s.LastSuccess = empty.LastSuccess
	}
}

// Uplink looks up an uplink model by ID in the catalogue of the given
// location and server model.
func (s *Snapshot) Uplink(locationID, serverModelID, uplinkID int64) (api.UplinkModel, bool) {
	catalogue, ok := s.Uplinks[UplinkKey(locationID, serverModelID)]
	if !ok {
		return api.UplinkModel{}, false
	}
	for _, model := range catalogue.Models {
		if model.ID == uplinkID {
			return model, true
		}
	}
	return api.UplinkModel{}, false
}
