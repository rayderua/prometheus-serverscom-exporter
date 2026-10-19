package api

import (
	"context"
	"fmt"
	"net/url"
)

// The endpoint names passed to list/get become the "endpoint" label of the
// client metrics, so they must stay low-cardinality (no IDs in them).

// ListDedicatedServers returns all Enterprise Bare Metal servers of the account.
func (c *Client) ListDedicatedServers(ctx context.Context) ([]DedicatedServer, error) {
	return list[DedicatedServer](ctx, c, "dedicated_servers", "/hosts/dedicated_servers")
}

// GetDedicatedServer returns a single server including configuration_details.
func (c *Client) GetDedicatedServer(ctx context.Context, id string) (*DedicatedServer, error) {
	var server DedicatedServer
	if err := c.get(ctx, "dedicated_server", serverPath(id, ""), &server); err != nil {
		return nil, err
	}
	return &server, nil
}

// ListDriveSlots returns drive bays of a server with installed drive models.
func (c *Client) ListDriveSlots(ctx context.Context, serverID string) ([]DriveSlot, error) {
	return list[DriveSlot](ctx, c, "dedicated_server_drive_slots", serverPath(serverID, "/drive_slots"))
}

// ListConnections returns switch ports a server is connected to.
func (c *Client) ListConnections(ctx context.Context, serverID string) ([]Connection, error) {
	return list[Connection](ctx, c, "dedicated_server_connections", serverPath(serverID, "/connections"))
}

// ListNetworks returns IP networks attached to a server.
func (c *Client) ListNetworks(ctx context.Context, serverID string) ([]Network, error) {
	return list[Network](ctx, c, "dedicated_server_networks", serverPath(serverID, "/networks"))
}

// ListFeatures returns optional features of a server and their status.
func (c *Client) ListFeatures(ctx context.Context, serverID string) ([]Feature, error) {
	return list[Feature](ctx, c, "dedicated_server_features", serverPath(serverID, "/features"))
}

// ListPowerFeeds returns power feeds of a server and their status.
func (c *Client) ListPowerFeeds(ctx context.Context, serverID string) ([]PowerFeed, error) {
	return list[PowerFeed](ctx, c, "dedicated_server_power_feeds", serverPath(serverID, "/power_feeds"))
}

// ListL2Segments returns all L2 segments of the account.
func (c *Client) ListL2Segments(ctx context.Context) ([]L2Segment, error) {
	return list[L2Segment](ctx, c, "l2_segments", "/l2_segments")
}

// ListL2Members returns servers attached to an L2 segment.
func (c *Client) ListL2Members(ctx context.Context, segmentID string) ([]L2Member, error) {
	return list[L2Member](ctx, c, "l2_segment_members", "/l2_segments/"+url.PathEscape(segmentID)+"/members")
}

// ListL2Networks returns networks of an L2 segment.
func (c *Client) ListL2Networks(ctx context.Context, segmentID string) ([]Network, error) {
	return list[Network](ctx, c, "l2_segment_networks", "/l2_segments/"+url.PathEscape(segmentID)+"/networks")
}

// ListLocations returns data centers available to the account.
func (c *Client) ListLocations(ctx context.Context) ([]Location, error) {
	return list[Location](ctx, c, "locations", "/locations")
}

// ListRacks returns racks of the account.
func (c *Client) ListRacks(ctx context.Context) ([]Rack, error) {
	return list[Rack](ctx, c, "racks", "/racks")
}

// ListUplinkModels returns uplink options of a server model in a location.
func (c *Client) ListUplinkModels(ctx context.Context, locationID, serverModelID int64) ([]UplinkModel, error) {
	path := fmt.Sprintf("/locations/%d/order_options/server_models/%d/uplink_models", locationID, serverModelID)
	return list[UplinkModel](ctx, c, "uplink_models", path)
}

func serverPath(id, suffix string) string {
	return "/hosts/dedicated_servers/" + url.PathEscape(id) + suffix
}
