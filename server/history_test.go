package server

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"
)

func TestAdminHistoryTemplatesRender(t *testing.T) {
	pages := []interface{}{
		historyPageData{BasePath: "/admin", View: "day", Title: "Historique par jour"},
		historyDetailPageData{BasePath: "/admin", Date: "2026-08-24", Label: "24/08/2026"},
	}
	for _, page := range pages {
		var output bytes.Buffer
		if err := adminHistoryTemplate.Execute(&output, page); err != nil {
			t.Fatalf("history template did not render: %v", err)
		}
	}
}

func TestAggregateHistoryUsesParisDays(t *testing.T) {
	location, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.August, 23, 23, 30, 0, 0, location).UTC()
	end := time.Date(2026, time.August, 24, 1, 30, 0, 0, location).UTC()
	connections := []historyConnection{{Started: start, Ended: end, IP: "192.0.2.10"}}

	periods := aggregateHistory("day", connections, nil, end, location)
	if len(periods) != 2 {
		t.Fatalf("expected two daily periods, got %d", len(periods))
	}
	if periods[0].Key != "2026-08-24" || periods[0].DurationSeconds != 5400 {
		t.Fatalf("unexpected second day: key=%s duration=%v", periods[0].Key, periods[0].DurationSeconds)
	}
	if periods[1].Key != "2026-08-23" || periods[1].DurationSeconds != 1800 {
		t.Fatalf("unexpected first day: key=%s duration=%v", periods[1].Key, periods[1].DurationSeconds)
	}
	if periods[0].TotalConnections != 1 || periods[1].TotalConnections != 1 {
		t.Fatal("the connection should be counted in both days it occupied")
	}
}

func TestAdminHistoryPersistsConnectionsAndSamples(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin-history.db")
	store, err := openAdminHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, time.August, 24, 10, 0, 0, 0, time.UTC)
	ended := started.Add(15 * time.Minute)
	if _, err := store.db.Exec(`INSERT INTO connection_history
(client_id, ip, connection_type, channel, started_at, ended_at)
VALUES (?, ?, ?, ?, ?, ?)`, 4, "192.0.2.20", "master", "test", started.UnixNano(), ended.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO resource_samples
(sampled_at, system_cpu_pct, process_cpu_pct, memory_used_pct, process_memory_mb)
VALUES (?, ?, ?, ?, ?)`, started.UnixNano(), 12.5, 3.25, 44.0, 8.5); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := openAdminHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.db.Close()
	connections, err := reopened.loadConnections(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := reopened.loadSamples(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(connections) != 1 || connections[0].ClientID != 4 || connections[0].Channel != "test" {
		t.Fatalf("connection was not persisted: %#v", connections)
	}
	if len(samples) != 1 || !samples[0].SystemCPUOK || samples[0].SystemCPU != 12.5 {
		t.Fatalf("sample was not persisted: %#v", samples)
	}
}
