package api

import "time"

// The types below mirror the subset of the Servers.com Public API v1 schema
// that the exporter needs. Field names follow the JSON payloads; unknown
// fields are ignored by encoding/json, so the API can grow without breaking us.

// DedicatedServer is an Enterprise Bare Metal server.
//
// The list endpoint and the single-object endpoint return the same shape,
// but configuration_details may be missing from list responses, so the
// exporter reads it from the single-object endpoint during detail refresh.
type DedicatedServer struct {
	ID                   string                `json:"id"`
	Type                 string                `json:"type"`
	Title                string                `json:"title"`
	LocationID           int64                 `json:"location_id"`
	LocationCode         string                `json:"location_code"`
	RackID               string                `json:"rack_id"`
	Status               string                `json:"status"`
	OperationalStatus    string                `json:"operational_status"`
	PowerStatus          string                `json:"power_status"`
	Configuration        string                `json:"configuration"`
	PrivateIPv4Address   string                `json:"private_ipv4_address"`
	PublicIPv4Address    string                `json:"public_ipv4_address"`
	OOBIPv4Address       string                `json:"oob_ipv4_address"`
	LeaseStartAt         string                `json:"lease_start_at"`
	ScheduledReleaseAt   *time.Time            `json:"scheduled_release_at"`
	ConfigurationDetails *ConfigurationDetails `json:"configuration_details"`
	Labels               map[string]string     `json:"labels"`
	CreatedAt            time.Time             `json:"created_at"`
	UpdatedAt            time.Time             `json:"updated_at"`
}

// ConfigurationDetails describes the ordered hardware and network configuration.
type ConfigurationDetails struct {
	RAMSize                 int    `json:"ram_size"`
	ServerModelID           int64  `json:"server_model_id"`
	ServerModelName         string `json:"server_model_name"`
	PublicUplinkID          int64  `json:"public_uplink_id"`
	PublicUplinkName        string `json:"public_uplink_name"`
	PrivateUplinkID         int64  `json:"private_uplink_id"`
	PrivateUplinkName       string `json:"private_uplink_name"`
	BandwidthID             int64  `json:"bandwidth_id"`
	BandwidthName           string `json:"bandwidth_name"`
	OperatingSystemID       int64  `json:"operating_system_id"`
	OperatingSystemFullName string `json:"operating_system_full_name"`
}

// DriveSlot is a physical drive bay of a server. DriveModel is nil for an empty slot.
type DriveSlot struct {
	Position   int         `json:"position"`
	Interface  string      `json:"interface"`
	FormFactor string      `json:"form_factor"`
	DriveModel *DriveModel `json:"drive_model"`
}

// DriveModel describes a drive installed into a slot.
type DriveModel struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Capacity   int    `json:"capacity"`
	Interface  string `json:"interface"`
	FormFactor string `json:"form_factor"`
	MediaType  string `json:"media_type"`
}

// Connection is a physical switch port connected to a server.
type Connection struct {
	Port       string `json:"port"`
	Type       string `json:"type"`
	MACAddress string `json:"macaddr"`
}

// Network is an IP network attached to a server or to an L2 segment.
type Network struct {
	ID                 string    `json:"id"`
	Title              string    `json:"title"`
	Status             string    `json:"status"`
	CIDR               string    `json:"cidr"`
	Family             string    `json:"family"`
	InterfaceType      string    `json:"interface_type"`
	DistributionMethod string    `json:"distribution_method"`
	Additional         bool      `json:"additional"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// Feature is an optional server capability, e.g. oob_public_access or
// disaggregated_public_ports.
type Feature struct {
	Name   string `json:"name"`
	Status string `json:"status"`
}

// PowerFeed is a power supply line of a server.
type PowerFeed struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

// Service is a billed service of a server in the current billing period,
// e.g. the server hardware, bandwidth or an additional IP network. Amounts
// are in Currency; Total includes Tax.
type Service struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Type          string  `json:"type"`
	Label         string  `json:"label"`
	Currency      string  `json:"currency"`
	DateFrom      string  `json:"date_from"`
	DateTo        string  `json:"date_to"`
	UsageQuantity float64 `json:"usage_quantity"`
	Tax           float64 `json:"tax"`
	Total         float64 `json:"total"`
	Subtotal      float64 `json:"subtotal"`
	DiscountRate  float64 `json:"discount_rate"`
}

// L2Segment is a private or public L2 segment spanning one location group.
type L2Segment struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Type              string            `json:"type"`
	Status            string            `json:"status"`
	LocationGroupID   int64             `json:"location_group_id"`
	LocationGroupCode string            `json:"location_group_code"`
	Labels            map[string]string `json:"labels"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// L2Member is a server attached to an L2 segment. ID is the host ID.
type L2Member struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Mode   string `json:"mode"`
	VLAN   *int   `json:"vlan"`
	Status string `json:"status"`
}

// Location is a data center.
type Location struct {
	ID                int64    `json:"id"`
	Name              string   `json:"name"`
	Code              string   `json:"code"`
	Status            string   `json:"status"`
	SupportedFeatures []string `json:"supported_features"`
}

// Rack is a (private) rack inside a location.
type Rack struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	LocationID   int64  `json:"location_id"`
	LocationCode string `json:"location_code"`
}

// UplinkModel is an uplink option of a server model in a location.
// Redundancy tells whether the uplink consists of several aggregated links.
type UplinkModel struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	Speed      int    `json:"speed"`
	Redundancy bool   `json:"redundancy"`
}
