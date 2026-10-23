package main

import (
	"fmt"
	"time"
)

// serverData holds the per-server collections; a missing entry serves the
// shared defaults (driveSlots, connections, ...).
type serverData map[string][]map[string]any

// details is keyed by server ID, segmentMembers by L2 segment ID. Both are
// only filled by generateGrowing.
var (
	details        = map[string]serverData{}
	segmentMembers = map[string][]map[string]any{}
)

// model is a server model of the random inventory.
type model struct {
	id     int64
	name   string
	ram    int
	drives []map[string]any // drive model per slot, nil for an empty slot
	price  float64
	uplink int64 // public uplink model: 11 redundant, 13 single link
}

func drive(name string, capacity int, media, iface, formFactor string) map[string]any {
	return map[string]any{"id": len(name) + capacity, "name": name, "capacity": capacity,
		"interface": iface, "form_factor": formFactor, "media_type": media}
}

var models = []model{
	{100, "Dell R640", 128, repeat(drive("SSD 960GB", 960, "SSD", "SATA3", "2.5"), 2, 4), 250, 11},
	{101, "Dell R740xd", 256, append(repeat(drive("SSD 480GB", 480, "SSD", "SATA3", "2.5"), 2, 2),
		repeat(drive("HDD 8TB", 8000, "HDD", "SAS", "3.5"), 10, 12)...), 480, 11},
	{102, "Supermicro 1029U", 64, repeat(drive("NVMe 1.92TB", 1920, "NVMe", "NVMe", "2.5"), 2, 2), 180, 13},
	{103, "HPE DL380 Gen11", 384, repeat(drive("SSD 3.84TB", 3840, "SSD", "SAS", "2.5"), 8, 8), 620, 11},
}

// repeat returns n copies of d followed by empty slots up to slots.
func repeat(d map[string]any, n, slots int) []map[string]any {
	out := make([]map[string]any, slots)
	for i := range n {
		out[i] = d
	}
	return out
}

// every reports whether the n-th server is one of every k-th, offset so that
// different problems hit different servers.
func every(n, k, offset int) bool { return (n+offset)%k == 0 }

// generateGrowing builds rackCount racks where rack r holds growth*r servers,
// with several models, labels and prices, and a fixed pattern of problems
// (powered off, not operational, power feed down, split bonding, public OOB
// access, missing drives, no prices yet). The inventory is the same on every
// start.
func generateGrowing(rackCount, growth int) {
	racks, servers = nil, nil
	uplinkModels = append(uplinkModels[:2:2],
		map[string]any{"id": 13, "name": "Public 1G", "type": "public", "speed": 1000, "redundancy": false})

	roles := []string{"web", "db", "k8s", "cache", "storage", "batch"}
	envs := []string{"prod", "prod", "prod", "stage", "dev"}
	n := 0
	for r := 1; r <= rackCount; r++ {
		rackID := fmt.Sprintf("rack-%d", r)
		racks = append(racks, map[string]any{"id": rackID, "name": fmt.Sprintf("AMS1-R%02d", r),
			"location_id": locationID, "location_code": "AMS1"})
		for range growth * r {
			n++
			m := models[n%len(models)]
			role := roles[(n/3)%len(roles)]
			id, title := fmt.Sprintf("srv-%d", n), serverName(role, r, n)

			s := server(id, title, "powered_on", rackID)
			s["configuration"] = fmt.Sprintf("%s / %d GB", m.name, m.ram)
			s["private_ipv4_address"] = fmt.Sprintf("10.0.%d.%d", r, n%250+1)
			s["public_ipv4_address"] = fmt.Sprintf("198.51.%d.%d", r, n%250+1)
			s["oob_ipv4_address"] = fmt.Sprintf("10.255.%d.%d", r, n%250+1)
			s["labels"] = map[string]string{"env": envs[n%len(envs)], "role": role}
			cfg := map[string]any{}
			for k, v := range s["configuration_details"].(map[string]any) {
				cfg[k] = v
			}
			cfg["ram_size"], cfg["server_model_id"], cfg["server_model_name"] = m.ram, m.id, m.name
			cfg["public_uplink_id"] = m.uplink
			if m.uplink == 13 {
				cfg["public_uplink_name"] = "Public 1G"
			}
			s["configuration_details"] = cfg
			switch {
			case every(n, 29, 0):
				s["power_status"] = "powered_off"
			case every(n, 37, 5):
				s["operational_status"] = "maintenance"
			}
			if every(n, 97, 11) {
				s["scheduled_release_at"] = time.Now().Add(10 * 24 * time.Hour).UTC().Truncate(time.Hour)
			}
			servers = append(servers, s)
			details[id] = growingDetails(m, n)
		}
	}

	// Three L2 segments, each with every third server.
	l2Segments = nil
	for i, name := range []string{"backend", "storage", "k8s"} {
		segID := fmt.Sprintf("seg-%d", i+1)
		l2Segments = append(l2Segments, map[string]any{"id": segID, "name": name, "type": "private",
			"status": "active", "location_group_id": 1, "location_group_code": "AMS",
			"labels": map[string]string{}, "created_at": created, "updated_at": created})
		for k, s := range servers {
			if k%3 != i {
				continue
			}
			status, mode := "active", "native"
			if every(k, 53, 0) {
				status = "pending"
			}
			if k%2 == 1 {
				mode = "trunk"
			}
			segmentMembers[segID] = append(segmentMembers[segID], map[string]any{
				"id": s["id"], "title": s["title"], "mode": mode, "vlan": 100 + i, "status": status})
		}
	}
}

// serverName mixes short, medium and long server names, e.g. db-003,
// db-primary-004 and db-primary-prod-ams1-r01-005.
func serverName(role string, rack, n int) string {
	suffix := map[string]string{"web": "frontend", "db": "primary", "k8s": "worker", "cache": "redis",
		"storage": "ceph-osd", "batch": "spark-executor"}[role]
	switch n % 3 {
	case 1:
		return fmt.Sprintf("%s-%s-%03d", role, suffix, n)
	case 2:
		return fmt.Sprintf("%s-%s-prod-ams1-r%02d-%03d", role, suffix, rack, n)
	}
	return fmt.Sprintf("%s-%03d", role, n)
}

// growingDetails builds the collections of the n-th server.
func growingDetails(m model, n int) serverData {
	status := func(on bool) string {
		if on {
			return "activated"
		}
		return "deactivated"
	}

	var slots []map[string]any
	for pos, d := range m.drives {
		var dm any
		if d != nil && (pos != 0 || !every(n, 43, 7)) { // now and then a drive is missing
			dm = d
		}
		slots = append(slots, map[string]any{"position": pos, "interface": "SATA3", "form_factor": "2.5", "drive_model": dm})
	}

	feedB := "on"
	if every(n, 41, 3) {
		feedB = "off"
	}

	var services []map[string]any
	if !every(n, 47, 13) { // just ordered: no services in the current period yet
		price := m.price * (1 - float64(n%3)*0.05)
		services = append(services, svc(n, 1, "Server hardware", "server_base", price))
		services = append(services, svc(n, 2, "Bandwidth 20 TB", "bandwidth", float64(n%3*25)))
		if every(n, 10, 0) {
			services = append(services, svc(n, 3, "Additional IPv4 /29", "ip_network", 12))
		}
	}

	return serverData{
		"drive_slots": slots,
		"connections": {
			{"port": "eth0", "type": "public", "macaddr": fmt.Sprintf("00:00:00:00:%02x:01", n%256)},
			{"port": "eth1", "type": "private", "macaddr": fmt.Sprintf("00:00:00:00:%02x:02", n%256)},
		},
		"networks": {
			{"id": fmt.Sprintf("net-%d", n), "title": "private", "status": "active", "cidr": fmt.Sprintf("10.%d.0.0/29", n%250),
				"family": "ipv4", "interface_type": "private", "distribution_method": "gateway", "additional": false,
				"created_at": created, "updated_at": created},
		},
		"features": {
			{"name": "oob_public_access", "status": status(every(n, 31, 2))},
			{"name": "disaggregated_public_ports", "status": status(every(n, 19, 4))},
			{"name": "disaggregated_private_ports", "status": status(every(n, 23, 9))},
			{"name": "host_rescue_mode", "status": status(every(n, 101, 17))},
		},
		"power_feeds": {
			{"name": "A", "status": "on", "type": "primary"},
			{"name": "B", "status": feedB, "type": "secondary"},
		},
		"services": services,
	}
}

func svc(n, i int, name, typ string, price float64) map[string]any {
	return map[string]any{"id": fmt.Sprintf("svc-%d-%d", n, i), "name": name, "type": typ, "label": "",
		"currency": "USD", "date_from": "2026-10-01", "date_to": "2026-10-31", "usage_quantity": 1,
		"tax": 0, "total": price, "subtotal": price, "discount_rate": 0}
}
