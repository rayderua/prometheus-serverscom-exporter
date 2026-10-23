// Command mockapi is a minimal fake of the Servers.com Public API v1 for smoke
// tests and local development. It serves a generated inventory (by default
// two servers in one rack, one L2 segment), paginates with Link headers like
// the real API and requires a bearer token.
//
// Like the real API it limits requests per window: the window starts at a
// multiple of --ratelimit-window (the full hour by default), X-RateLimit-*
// headers report it, and requests over the limit get 429.
//
//	go run ./test/mockapi --listen=:8080 --token=test
//	go run ./test/mockapi --servers=175 --racks=16 --ratelimit-window=5m
//	go run ./test/mockapi --racks=18 --rack-growth=2   # rack N holds 2*N servers
//	serverscom-exporter --api.url=http://localhost:8080/v1 --api.token=test
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	racks        []map[string]any // see generate
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
	services = []map[string]any{
		{"id": "svc-1", "name": "Server hardware", "type": "server_base", "label": "", "currency": "USD",
			"date_from": "2026-10-01", "date_to": "2026-10-31", "usage_quantity": 1,
			"tax": 0, "total": 250, "subtotal": 250, "discount_rate": 0},
		{"id": "svc-2", "name": "Bandwidth 20 TB", "type": "bandwidth", "label": "", "currency": "USD",
			"date_from": "2026-10-01", "date_to": "2026-10-31", "usage_quantity": 1,
			"tax": 0, "total": 0, "subtotal": 0, "discount_rate": 0},
	}
	powerFeeds = []map[string]any{
		{"name": "A", "status": "on", "type": "primary"},
		{"name": "B", "status": "on", "type": "secondary"},
	}
)

func server(id, title, power, rack string) map[string]any {
	return map[string]any{
		"id": id, "type": "dedicated_server", "title": title,
		"location_id": locationID, "location_code": "AMS1", "rack_id": rack,
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

// servers is the inventory; see generate.
var servers []map[string]any

// generate builds n servers spread over m racks: node-1 (powered on) and
// node-2 (powered off) as the smoke test expects, the rest powered on.
func generate(n, m int) {
	racks = nil
	for i := 1; i <= m; i++ {
		racks = append(racks, map[string]any{"id": fmt.Sprintf("rack-%d", i), "name": fmt.Sprintf("R%d", i),
			"location_id": locationID, "location_code": "AMS1"})
	}
	servers = nil
	for i := 1; i <= n; i++ {
		power := "powered_on"
		if i == 2 {
			power = "powered_off"
		}
		servers = append(servers, server(fmt.Sprintf("srv-%d", i), fmt.Sprintf("node-%d", i), power,
			fmt.Sprintf("rack-%d", (i-1)%m+1)))
	}
}

// rateLimit counts requests per window, like X-RateLimit-* of the real API.
type rateLimit struct {
	limit  int64
	window time.Duration

	mu    sync.Mutex
	start time.Time
	used  int64
}

// take counts a request at now and returns the remaining requests, the reset
// time and whether the request is allowed.
func (l *rateLimit) take(now time.Time) (remaining int64, reset time.Time, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if start := now.Truncate(l.window); !start.Equal(l.start) {
		l.start, l.used = start, 0
	}
	reset = l.start.Add(l.window)
	if l.used >= l.limit {
		return 0, reset, false
	}
	l.used++
	return l.limit - l.used, reset, true
}

func main() {
	listen := flag.String("listen", ":8080", "Listen address.")
	token := flag.String("token", "test", "Accepted bearer token.")
	serverCount := flag.Int("servers", 2, "Number of servers.")
	rackCount := flag.Int("racks", 1, "Number of racks the servers are spread over.")
	limit := flag.Int64("ratelimit", 2000, "Requests per rate limit window (X-RateLimit-Limit).")
	window := flag.Duration("ratelimit-window", time.Hour, "Rate limit window; it starts at a multiple of this duration.")
	growth := flag.Int("rack-growth", 0, "If above 0, rack N holds rack-growth*N servers (--servers is ignored), "+
		"with several models, labels and prices and a fixed pattern of problems: powered off, power feed down, "+
		"split bonding, public OOB access, missing drives, no prices yet.")
	flag.Parse()
	if *serverCount < 2 || *rackCount < 1 || *limit < 1 || *window <= 0 || *growth < 0 {
		log.Fatal("want --servers >= 2, --racks >= 1, --ratelimit >= 1, --rack-growth >= 0 and a positive --ratelimit-window")
	}
	if *growth > 0 {
		generateGrowing(*rackCount, *growth)
		*serverCount = len(servers)
	} else {
		generate(*serverCount, *rackCount)
	}
	rl := &rateLimit{limit: *limit, window: *window}

	mux := http.NewServeMux()

	collection := func(path string, items []map[string]any) {
		mux.HandleFunc("GET /v1"+path, func(w http.ResponseWriter, r *http.Request) {
			writePage(w, r, items)
		})
	}
	// perServer serves the server's own collection (--random) or the shared one.
	perServer := func(name string, shared []map[string]any) {
		mux.HandleFunc("GET /v1/hosts/dedicated_servers/{id}/"+name, func(w http.ResponseWriter, r *http.Request) {
			items, ok := details[r.PathValue("id")][name]
			if !ok {
				items = shared
			}
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
	// Servers can be renamed at runtime (POST /-/rename?id=srv-2&title=db-2),
	// to test how dashboards and alerts handle renames.
	var serversMu sync.Mutex
	currentServers := func() []map[string]any {
		serversMu.Lock()
		defer serversMu.Unlock()
		return slices.Clone(servers)
	}
	findServer := func(id string) map[string]any {
		for _, s := range currentServers() {
			if s["id"] == id {
				return s
			}
		}
		return nil
	}
	mux.HandleFunc("POST /-/rename", func(w http.ResponseWriter, r *http.Request) {
		id, title := r.URL.Query().Get("id"), r.URL.Query().Get("title")
		serversMu.Lock()
		defer serversMu.Unlock()
		for i, s := range servers {
			if s["id"] == id && title != "" {
				renamed := maps.Clone(s)
				renamed["title"], renamed["updated_at"] = title, time.Now().UTC()
				servers[i] = renamed
				log.Printf("renamed %s to %s", id, title)
				return
			}
		}
		http.Error(w, "unknown id or empty title", http.StatusBadRequest)
	})

	mux.HandleFunc("GET /v1/hosts/dedicated_servers", func(w http.ResponseWriter, r *http.Request) {
		writePage(w, r, currentServers())
	})
	object("/hosts/dedicated_servers/{id}", findServer)
	perServer("drive_slots", driveSlots)
	perServer("connections", connections)
	perServer("networks", networks)
	perServer("features", features)
	perServer("power_feeds", powerFeeds)
	perServer("services", services)
	collection("/l2_segments", l2Segments)
	mux.HandleFunc("GET /v1/l2_segments/{id}/members", func(w http.ResponseWriter, r *http.Request) {
		items, ok := segmentMembers[r.PathValue("id")]
		if !ok && *growth == 0 {
			items = l2Members
		}
		writePage(w, r, items)
	})
	collection("/l2_segments/{id}/networks", networks)
	collection("/locations", locations)
	collection("/racks", racks)
	collection("/locations/{location}/order_options/server_models/{model}/uplink_models", uplinkModels)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/-/healthy" {
			_, _ = w.Write([]byte("OK"))
			return
		}
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			remaining, reset, ok := rl.take(time.Now())
			w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(*limit, 10))
			w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(remaining, 10))
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
			if !ok {
				log.Printf("429 %s %s", r.Method, r.URL.RequestURI())
				http.Error(w, `{"message":"rate limit exceeded"}`, http.StatusTooManyRequests)
				return
			}
		}
		if r.Header.Get("Authorization") != "Bearer "+*token {
			http.Error(w, `{"message":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		log.Printf("%s %s", r.Method, r.URL.RequestURI())
		mux.ServeHTTP(w, r)
	})

	log.Printf("mock Servers.com API listening on %s: %d servers in %d racks, %d requests per %s",
		*listen, *serverCount, *rackCount, *limit, *window)
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
