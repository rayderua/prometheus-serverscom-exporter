// Command mockapi is a minimal fake of the Servers.com Public API v1 for smoke
// tests and local development. It serves a fixed inventory (two servers, one
// L2 segment), paginates with Link headers like the real API, sends
// X-RateLimit-* headers and requires a bearer token.
//
//	go run ./test/mockapi --listen=:8080 --token=test
//	serverscom-exporter --api.url=http://localhost:8080/v1 --api.token=test
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	locationID    = 1
	serverModelID = 100
)

var (
	created = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	locations = []map[string]any{
		{"id": locationID, "name": "Amsterdam", "code": "AMS1", "status": "active", "supported_features": []string{}},
	}
	racks = []map[string]any{
		{"id": "rack-1", "name": "R1", "location_id": locationID, "location_code": "AMS1"},
	}
	uplinkModels = []map[string]any{
		{"id": 11, "name": "Public 2x10G", "type": "public", "speed": 10000, "redundancy": true},
		{"id": 12, "name": "Private 2x10G", "type": "private", "speed": 10000, "redundancy": true},
	}
	l2Segments = []map[string]any{
		{"id": "seg-1", "name": "backend", "type": "private", "status": "active",
			"location_group_id": 1, "location_group_code": "AMS", "labels": map[string]string{},
			"created_at": created, "updated_at": created},
	}
	l2Members = []map[string]any{
		{"id": "srv-1", "title": "node-1", "mode": "native", "vlan": nil, "status": "active"},
		{"id": "srv-2", "title": "node-2", "mode": "trunk", "vlan": 100, "status": "active"},
	}
	networks = []map[string]any{
		{"id": "net-1", "title": "private", "status": "active", "cidr": "10.0.0.0/29", "family": "ipv4",
			"interface_type": "private", "distribution_method": "gateway", "additional": false,
			"created_at": created, "updated_at": created},
	}
	driveSlots = []map[string]any{
		{"position": 0, "interface": "SATA3", "form_factor": "2.5", "drive_model": map[string]any{
			"id": 1, "name": "SSD 960GB", "capacity": 960, "interface": "SATA3", "form_factor": "2.5", "media_type": "SSD"}},
		{"position": 1, "interface": "SATA3", "form_factor": "2.5", "drive_model": nil},
	}
	connections = []map[string]any{
		{"port": "eth0", "type": "public", "macaddr": "00:00:00:00:00:01"},
		{"port": "eth1", "type": "private", "macaddr": "00:00:00:00:00:02"},
	}
	features = []map[string]any{
		{"name": "oob_public_access", "status": "deactivated"},
		{"name": "disaggregated_private_ports", "status": "deactivated"},
	}
	powerFeeds = []map[string]any{
		{"name": "A", "status": "on", "type": "primary"},
		{"name": "B", "status": "on", "type": "secondary"},
	}
)

func server(id, title, power string) map[string]any {
	return map[string]any{
		"id": id, "type": "dedicated_server", "title": title,
		"location_id": locationID, "location_code": "AMS1", "rack_id": "rack-1",
		"status": "active", "operational_status": "normal", "power_status": power,
		"configuration":        "Dell R640 / 2x Xeon Gold / 128 GB",
		"private_ipv4_address": "10.0.0.1", "public_ipv4_address": "192.0.2.1", "oob_ipv4_address": "10.255.0.1",
		"lease_start_at": "2026-01-01", "scheduled_release_at": nil,
		"labels":     map[string]string{"env": "test", "role": title},
		"created_at": created, "updated_at": created,
		"configuration_details": map[string]any{
			"ram_size": 128, "server_model_id": serverModelID, "server_model_name": "Dell R640",
			"public_uplink_id": 11, "public_uplink_name": "Public 2x10G",
			"private_uplink_id": 12, "private_uplink_name": "Private 2x10G",
			"bandwidth_id": 1, "bandwidth_name": "20 TB",
			"operating_system_id": 1, "operating_system_full_name": "Ubuntu 24.04",
		},
	}
}

var servers = []map[string]any{
	server("srv-1", "node-1", "powered_on"),
	server("srv-2", "node-2", "powered_off"),
}

func main() {
	listen := flag.String("listen", ":8080", "Listen address.")
	token := flag.String("token", "test", "Accepted bearer token.")
	flag.Parse()

	var requests atomic.Int64
	mux := http.NewServeMux()

	collection := func(path string, items []map[string]any) {
		mux.HandleFunc("GET /v1"+path, func(w http.ResponseWriter, r *http.Request) {
			writePage(w, r, items)
		})
	}
	object := func(path string, find func(id string) map[string]any) {
		mux.HandleFunc("GET /v1"+path, func(w http.ResponseWriter, r *http.Request) {
			obj := find(r.PathValue("id"))
			if obj == nil {
				http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
				return
			}
			writeJSON(w, obj)
		})
	}
	findServer := func(id string) map[string]any {
		for _, s := range servers {
			if s["id"] == id {
				return s
			}
		}
		return nil
	}

	collection("/hosts/dedicated_servers", servers)
	object("/hosts/dedicated_servers/{id}", findServer)
	collection("/hosts/dedicated_servers/{id}/drive_slots", driveSlots)
	collection("/hosts/dedicated_servers/{id}/connections", connections)
	collection("/hosts/dedicated_servers/{id}/networks", networks)
	collection("/hosts/dedicated_servers/{id}/features", features)
	collection("/hosts/dedicated_servers/{id}/power_feeds", powerFeeds)
	collection("/l2_segments", l2Segments)
	collection("/l2_segments/{id}/members", l2Members)
	collection("/l2_segments/{id}/networks", networks)
	collection("/locations", locations)
	collection("/racks", racks)
	collection("/locations/{location}/order_options/server_models/{model}/uplink_models", uplinkModels)

	reset := time.Now().Add(time.Hour).Unix()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/-/healthy" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		n := requests.Add(1)
		w.Header().Set("X-RateLimit-Limit", "2000")
		w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(max(2000-n, 0), 10))
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset, 10))
		if r.Header.Get("Authorization") != "Bearer "+*token {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		log.Printf("%s %s", r.Method, r.URL.RequestURI())
		mux.ServeHTTP(w, r)
	})

	log.Printf("mock Servers.com API listening on %s", *listen)
	srv := &http.Server{Addr: *listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second}
	log.Fatal(srv.ListenAndServe())
}

// writePage writes one page of items and a Link header to the next page.
func writePage(w http.ResponseWriter, r *http.Request, items []map[string]any) {
	perPage, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
	if perPage <= 0 {
		perPage = 20
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page <= 0 {
		page = 1
	}

	start := min((page-1)*perPage, len(items))
	end := min(start+perPage, len(items))
	if end < len(items) {
		q := r.URL.Query()
		q.Set("page", strconv.Itoa(page+1))
		q.Set("per_page", strconv.Itoa(perPage))
		next := fmt.Sprintf("http://%s%s?%s", r.Host, r.URL.Path, q.Encode())
		w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
	}
	writeJSON(w, items[start:end])
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil && !strings.Contains(err.Error(), "broken pipe") {
		log.Printf("write response: %v", err)
	}
}
