package cache

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"serverscom-exporter/internal/api"
)

func TestStorePersistsAndLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")
	logger := slog.New(slog.DiscardHandler)

	store := NewStore(path, logger)
	store.Update(func(s *Snapshot) {
		s.Hosts["h1"] = &Host{
			Server:  api.DedicatedServer{ID: "h1", Title: "node-1"},
			Details: &HostDetails{FetchedAt: time.Unix(100, 0).UTC()},
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
	if !snapshot.Hosts["h1"].Details.FetchedAt.Equal(time.Unix(100, 0)) {
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
