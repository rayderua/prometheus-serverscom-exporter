package cache

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/api"
)

func TestStorePersistsAndLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	logger := slog.New(slog.DiscardHandler)

	store := NewStore(path, logger)
	store.Update(func(s *Snapshot) {
		s.Hosts["h1"] = &Host{
			Server:  api.DedicatedServer{ID: "h1", Title: "node-1"},
			Details: &HostDetails{Fetched: map[Collector]time.Time{CollectorDrives: time.Unix(100, 0).UTC()}},
		}
		s.LastSuccess[ResourceHosts] = time.Unix(200, 0).UTC()
	})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	loaded := NewStore(path, logger)
	loaded.Load()
	snapshot := loaded.Snapshot()

	if snapshot.Hosts["h1"].Server.Title != "node-1" {
		t.Fatalf("host not restored: %+v", snapshot.Hosts)
	}
	if !snapshot.Hosts["h1"].Details.Fetched[CollectorDrives].Equal(time.Unix(100, 0)) {
		t.Fatalf("details not restored: %+v", snapshot.Hosts["h1"].Details)
	}
	if !snapshot.LastSuccess[ResourceHosts].Equal(time.Unix(200, 0)) {
		t.Fatalf("last success not restored: %+v", snapshot.LastSuccess)
	}
	if snapshot.Segments == nil || snapshot.Uplinks == nil {
		t.Fatal("maps not initialised after load")
	}
}

func TestUpdateDoesNotChangePublishedSnapshot(t *testing.T) {
	store := NewStore("", slog.New(slog.DiscardHandler))
	before := store.Snapshot()

	store.Update(func(s *Snapshot) { s.Hosts["h1"] = &Host{} })

	if len(before.Hosts) != 0 {
		t.Fatal("a published snapshot was modified")
	}
	if len(store.Snapshot().Hosts) != 1 {
		t.Fatal("update was not published")
	}
}

func TestSaveSkipsUnchangedSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	store := NewStore(path, slog.New(slog.DiscardHandler))

	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache file written without changes: %v", err)
	}
}

func TestRateLimitIsSavedWithDataAndOnShutdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	logger := slog.New(slog.DiscardHandler)
	reset := time.Unix(2000000000, 0).UTC()

	store := NewStore(path, logger)
	store.Update(func(s *Snapshot) { s.LastSuccess[ResourceHosts] = time.Unix(1, 0) })
	store.SetRateLimit(api.RateLimit{Limit: 2000, Remaining: 1500, Reset: reset}, api.BudgetWindow{Used: 7, Reset: reset})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	loadRemaining := func() int {
		loaded := NewStore(path, logger)
		loaded.Load()
		rl, bw := loaded.RateLimit()
		if bw.Used != 7 {
			t.Errorf("budget window used = %d, want 7", bw.Used)
		}
		return rl.Remaining
	}
	if got := loadRemaining(); got != 1500 {
		t.Fatalf("saved remaining = %d, want 1500", got)
	}

	// Only the rate limit changed: a periodic save skips the file...
	store.SetRateLimit(api.RateLimit{Limit: 2000, Remaining: 900, Reset: reset}, api.BudgetWindow{Used: 7, Reset: reset})
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if got := loadRemaining(); got != 1500 {
		t.Fatalf("remaining after a periodic save = %d, want 1500", got)
	}
	// ...the save on shutdown writes it.
	if err := store.save(true); err != nil {
		t.Fatal(err)
	}
	if got := loadRemaining(); got != 900 {
		t.Fatalf("remaining after the shutdown save = %d, want 900", got)
	}
}

func TestCheckWritable(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	dir := t.TempDir()
	if err := NewStore("", logger).CheckWritable(); err != nil {
		t.Errorf("no path: %v", err)
	}
	if err := NewStore(filepath.Join(dir, "cache.json"), logger).CheckWritable(); err != nil {
		t.Errorf("writable dir: %v", err)
	}
	if err := NewStore(dir, logger).CheckWritable(); err == nil {
		t.Error("a directory as the cache file was accepted")
	}
	if err := NewStore(filepath.Join(dir, "missing", "cache.json"), logger).CheckWritable(); err == nil {
		t.Error("a missing directory was accepted")
	}
}
