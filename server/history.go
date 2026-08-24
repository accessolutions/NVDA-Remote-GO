package server

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	_ "modernc.org/sqlite"
)

const adminHistorySampleInterval = time.Minute

var historyLocation = mustLoadHistoryLocation()

func mustLoadHistoryLocation() *time.Location {
	location, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		return time.FixedZone("Europe/Paris", 3600)
	}
	return location
}

type adminHistoryStore struct {
	db       *sql.DB
	location *time.Location
}

var adminHistoryState struct {
	sync.Mutex
	store     *adminHistoryStore
	attempted bool
}

func startAdminHistory() {
	adminHistoryState.Lock()
	if adminHistoryState.attempted {
		adminHistoryState.Unlock()
		return
	}
	adminHistoryState.attempted = true
	path := adminDataFile
	if path == "" {
		path = "admin-history.db"
	}
	adminHistoryState.Unlock()

	store, err := openAdminHistory(path)
	if err != nil {
		Log(LOG_INFO, "Warning: unable to start administration history: "+err.Error())
		return
	}

	adminHistoryState.Lock()
	adminHistoryState.store = store
	adminHistoryState.Unlock()

	store.sampleNow()
	go store.sampleLoop()
	Log(LOG_INFO, "Administration history stored in "+path)
}

func currentAdminHistory() *adminHistoryStore {
	adminHistoryState.Lock()
	defer adminHistoryState.Unlock()
	return adminHistoryState.store
}

func openAdminHistory(path string) (*adminHistoryStore, error) {
	path = filepath.Clean(path)
	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeWithError := func(openErr error) (*adminHistoryStore, error) {
		_ = db.Close()
		return nil, openErr
	}

	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		return closeWithError(err)
	}
	if _, err := db.Exec("PRAGMA journal_mode = WAL"); err != nil {
		return closeWithError(err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS connection_history (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	client_id INTEGER NOT NULL,
	ip TEXT NOT NULL,
	connection_type TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	ended_at INTEGER
)`); err != nil {
		return closeWithError(err)
	}
	if _, err := db.Exec(`
CREATE TABLE IF NOT EXISTS resource_samples (
	sampled_at INTEGER PRIMARY KEY,
	system_cpu_pct REAL,
	process_cpu_pct REAL,
	memory_used_pct REAL,
	process_memory_mb REAL
)`); err != nil {
		return closeWithError(err)
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS connection_history_time ON connection_history(started_at, ended_at)"); err != nil {
		return closeWithError(err)
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS resource_samples_time ON resource_samples(sampled_at)"); err != nil {
		return closeWithError(err)
	}
	// A process that was stopped abruptly cannot close its active sessions.
	// Do not count the unknown downtime as connection time after a restart.
	if _, err := db.Exec("UPDATE connection_history SET ended_at = started_at WHERE ended_at IS NULL"); err != nil {
		return closeWithError(err)
	}
	_ = os.Chmod(path, 0o600)

	return &adminHistoryStore{db: db, location: historyLocation}, nil
}

func (s *adminHistoryStore) sampleLoop() {
	ticker := time.NewTicker(adminHistorySampleInterval)
	defer ticker.Stop()
	for range ticker.C {
		s.sampleNow()
	}
}

func (s *adminHistoryStore) sampleNow() {
	now := time.Now().UTC()
	systemCPU, processCPU, cpuOK := cpuMetrics()
	memoryUsed, memoryOK := systemMemUsedPct()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)

	var systemCPUValue, processCPUValue, memoryUsedValue interface{}
	if cpuOK {
		systemCPUValue = systemCPU
		processCPUValue = processCPU
	}
	if memoryOK {
		memoryUsedValue = memoryUsed
	}
	_, err := s.db.Exec(`
INSERT OR REPLACE INTO resource_samples
	(sampled_at, system_cpu_pct, process_cpu_pct, memory_used_pct, process_memory_mb)
VALUES (?, ?, ?, ?, ?)`,
		now.UnixNano(), systemCPUValue, processCPUValue, memoryUsedValue,
		float64(memory.Alloc)/(1024*1024))
	if err != nil {
		Log(LOG_DEBUG, "Unable to store administration resource sample: "+err.Error())
	}
}

func recordHistoryConnection(c *Client) {
	store := currentAdminHistory()
	if store == nil {
		return
	}
	started := c.GetConnectedAt().UTC()
	result, err := store.db.Exec(`
INSERT INTO connection_history
	(client_id, ip, connection_type, channel, started_at)
VALUES (?, ?, ?, ?, ?)`,
		c.GetID(), c.GetIP(), c.GetConnectionType(), "", started.UnixNano())
	if err != nil {
		Log(LOG_DEBUG, "Unable to store administration connection start: "+err.Error())
		return
	}
	id, err := result.LastInsertId()
	if err != nil {
		return
	}
	c.Lock()
	c.historyID = id
	c.Unlock()
}

func updateHistoryClient(c *Client) {
	store := currentAdminHistory()
	if store == nil {
		return
	}
	c.Lock()
	id := c.historyID
	ctype := c.connectionType
	channel := ""
	if c.c != nil {
		channel = c.c.name
	}
	c.Unlock()
	if id == 0 {
		return
	}
	if _, err := store.db.Exec(`
UPDATE connection_history
SET connection_type = ?, channel = ?
WHERE id = ? AND ended_at IS NULL`, ctype, channel, id); err != nil {
		Log(LOG_DEBUG, "Unable to update administration connection details: "+err.Error())
	}
}

func finishHistoryConnection(c *Client) {
	store := currentAdminHistory()
	if store == nil {
		return
	}
	c.Lock()
	id := c.historyID
	ctype := c.connectionType
	channel := ""
	if c.c != nil {
		channel = c.c.name
	}
	c.Unlock()
	if id == 0 {
		return
	}
	if _, err := store.db.Exec(`
UPDATE connection_history
SET ended_at = ?, connection_type = ?, channel = ?
WHERE id = ? AND ended_at IS NULL`, time.Now().UTC().UnixNano(), ctype, channel, id); err != nil {
		Log(LOG_DEBUG, "Unable to store administration connection end: "+err.Error())
	}
}

type historyConnection struct {
	ID          int64
	ClientID    int
	IP          string
	Type        string
	Channel     string
	Started     time.Time
	Ended       time.Time
	EndedKnown  bool
}

type historySample struct {
	At              time.Time
	SystemCPU       float64
	SystemCPUOK     bool
	ProcessCPU      float64
	ProcessCPUOK    bool
	MemoryUsed      float64
	MemoryUsedOK    bool
	ProcessMemoryMB float64
	ProcessMemoryOK bool
}

func (s *adminHistoryStore) loadConnections(start, end *time.Time) ([]historyConnection, error) {
	query := `SELECT id, client_id, ip, connection_type, channel, started_at, ended_at FROM connection_history`
	args := make([]interface{}, 0, 2)
	if start != nil && end != nil {
		query += ` WHERE started_at < ? AND (ended_at IS NULL OR ended_at > ?)`
		args = append(args, end.UTC().UnixNano(), start.UTC().UnixNano())
	}
	query += ` ORDER BY started_at DESC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	now := time.Now().UTC()
	connections := make([]historyConnection, 0)
	for rows.Next() {
		var connection historyConnection
		var startedNS int64
		var endedNS sql.NullInt64
		if err := rows.Scan(&connection.ID, &connection.ClientID, &connection.IP, &connection.Type, &connection.Channel, &startedNS, &endedNS); err != nil {
			return nil, err
		}
		connection.Started = time.Unix(0, startedNS).UTC()
		if endedNS.Valid {
			connection.Ended = time.Unix(0, endedNS.Int64).UTC()
			connection.EndedKnown = true
		} else {
			connection.Ended = now
		}
		connections = append(connections, connection)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return connections, nil
}

func (s *adminHistoryStore) loadSamples(start, end *time.Time) ([]historySample, error) {
	query := `SELECT sampled_at, system_cpu_pct, process_cpu_pct, memory_used_pct, process_memory_mb FROM resource_samples`
	args := make([]interface{}, 0, 2)
	if start != nil && end != nil {
		query += ` WHERE sampled_at >= ? AND sampled_at < ?`
		args = append(args, start.UTC().UnixNano(), end.UTC().UnixNano())
	}
	query += ` ORDER BY sampled_at ASC`
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	samples := make([]historySample, 0)
	for rows.Next() {
		var sample historySample
		var sampledNS int64
		var systemCPU, processCPU, memoryUsed, processMemory sql.NullFloat64
		if err := rows.Scan(&sampledNS, &systemCPU, &processCPU, &memoryUsed, &processMemory); err != nil {
			return nil, err
		}
		sample.At = time.Unix(0, sampledNS).UTC()
		if systemCPU.Valid {
			sample.SystemCPU, sample.SystemCPUOK = systemCPU.Float64, true
		}
		if processCPU.Valid {
			sample.ProcessCPU, sample.ProcessCPUOK = processCPU.Float64, true
		}
		if memoryUsed.Valid {
			sample.MemoryUsed, sample.MemoryUsedOK = memoryUsed.Float64, true
		}
		if processMemory.Valid {
			sample.ProcessMemoryMB, sample.ProcessMemoryOK = processMemory.Float64, true
		}
		samples = append(samples, sample)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return samples, nil
}

type historyMetric struct {
	sum   float64
	max   float64
	count int
}

func (m *historyMetric) add(value float64, ok bool) {
	if !ok {
		return
	}
	m.sum += value
	if m.count == 0 || value > m.max {
		m.max = value
	}
	m.count++
}

func (m historyMetric) average() float64 {
	if m.count == 0 {
		return 0
	}
	return m.sum / float64(m.count)
}

type historyPeriod struct {
	Key                 string
	Label               string
	Start               time.Time
	End                 time.Time
	DurationSeconds     float64
	TotalConnections    int
	UniqueIPs           map[string]struct{}
	SystemCPU           historyMetric
	ProcessCPU          historyMetric
	MemoryUsed          historyMetric
	ProcessMemory       historyMetric
	AverageConcurrence  float64
}

func newHistoryPeriod(key, label string, start, end time.Time) *historyPeriod {
	return &historyPeriod{
		Key: key, Label: label, Start: start, End: end,
		UniqueIPs: make(map[string]struct{}),
	}
}

func historyPeriodBounds(at time.Time, view string, location *time.Location) (string, string, time.Time, time.Time) {
	local := at.In(location)
	var start time.Time
	switch view {
	case "month":
		start = time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)
	case "year":
		start = time.Date(local.Year(), time.January, 1, 0, 0, 0, 0, location)
	default:
		view = "day"
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	}
	var key, label string
	switch view {
	case "month":
		key = start.Format("2006-01")
		label = frenchMonth(start.Month()) + " " + strconv.Itoa(start.Year())
	case "year":
		key = start.Format("2006")
		label = strconv.Itoa(start.Year())
	default:
		key = start.Format("2006-01-02")
		label = start.Format("02/01/2006")
	}
	switch view {
	case "month":
		return key, label, start, start.AddDate(0, 1, 0)
	case "year":
		return key, label, start, start.AddDate(1, 0, 0)
	default:
		return key, label, start, start.AddDate(0, 0, 1)
	}
}

func frenchMonth(month time.Month) string {
	months := [...]string{"janvier", "février", "mars", "avril", "mai", "juin", "juillet", "août", "septembre", "octobre", "novembre", "décembre"}
	return months[int(month)-1]
}

func aggregateHistory(view string, connections []historyConnection, samples []historySample, now time.Time, location *time.Location) []*historyPeriod {
	periods := make(map[string]*historyPeriod)
	getPeriod := func(at time.Time) *historyPeriod {
		key, label, start, end := historyPeriodBounds(at, view, location)
		period := periods[key]
		if period == nil {
			period = newHistoryPeriod(key, label, start, end)
			periods[key] = period
		}
		return period
	}

	for _, connection := range connections {
		start := connection.Started
		end := connection.Ended
		if end.Before(start) {
			continue
		}
		cursor := start
		for cursor.Before(end) {
			period := getPeriod(cursor)
			_, _, _, periodEnd := historyPeriodBounds(cursor, view, location)
			segmentEnd := end
			if periodEnd.Before(segmentEnd) {
				segmentEnd = periodEnd
			}
			period.TotalConnections++
			if strings.TrimSpace(connection.IP) != "" {
				period.UniqueIPs[connection.IP] = struct{}{}
			}
			period.DurationSeconds += segmentEnd.Sub(cursor).Seconds()
			cursor = segmentEnd
		}
	}
	for _, sample := range samples {
		period := getPeriod(sample.At)
		period.SystemCPU.add(sample.SystemCPU, sample.SystemCPUOK)
		period.ProcessCPU.add(sample.ProcessCPU, sample.ProcessCPUOK)
		period.MemoryUsed.add(sample.MemoryUsed, sample.MemoryUsedOK)
		period.ProcessMemory.add(sample.ProcessMemoryMB, sample.ProcessMemoryOK)
	}

	nowLocal := now.In(location)
	result := make([]*historyPeriod, 0, len(periods))
	for _, period := range periods {
		observationEnd := period.End
		if observationEnd.After(nowLocal) {
			observationEnd = nowLocal
		}
		if observationEnd.After(period.Start) {
			period.AverageConcurrence = period.DurationSeconds / observationEnd.Sub(period.Start).Seconds()
		}
		result = append(result, period)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Start.After(result[j].Start) })
	return result
}

func (s *adminHistoryStore) historyPeriods(view string) ([]*historyPeriod, error) {
	connections, err := s.loadConnections(nil, nil)
	if err != nil {
		return nil, err
	}
	samples, err := s.loadSamples(nil, nil)
	if err != nil {
		return nil, err
	}
	return aggregateHistory(view, connections, samples, time.Now().UTC(), s.location), nil
}

func (s *adminHistoryStore) dayData(date string) (*historyPeriod, []historyConnection, []historySample, error) {
	start, err := time.ParseInLocation("2006-01-02", date, s.location)
	if err != nil || start.Format("2006-01-02") != date {
		return nil, nil, nil, os.ErrInvalid
	}
	end := start.AddDate(0, 0, 1)
	connections, err := s.loadConnections(&start, &end)
	if err != nil {
		return nil, nil, nil, err
	}
	samples, err := s.loadSamples(&start, &end)
	if err != nil {
		return nil, nil, nil, err
	}
	period := newHistoryPeriod(date, start.Format("02/01/2006"), start, end)
	now := time.Now().UTC()
	observationEnd := end
	if now.Before(observationEnd) {
		observationEnd = now
	}
	for _, connection := range connections {
		from := connection.Started
		if from.Before(start.UTC()) {
			from = start.UTC()
		}
		to := connection.Ended
		if to.After(end.UTC()) {
			to = end.UTC()
		}
		if to.After(from) {
			period.TotalConnections++
			period.DurationSeconds += to.Sub(from).Seconds()
			if strings.TrimSpace(connection.IP) != "" {
				period.UniqueIPs[connection.IP] = struct{}{}
			}
		}
	}
	for _, sample := range samples {
		period.SystemCPU.add(sample.SystemCPU, sample.SystemCPUOK)
		period.ProcessCPU.add(sample.ProcessCPU, sample.ProcessCPUOK)
		period.MemoryUsed.add(sample.MemoryUsed, sample.MemoryUsedOK)
		period.ProcessMemory.add(sample.ProcessMemoryMB, sample.ProcessMemoryOK)
	}
	if observationEnd.After(start) {
		period.AverageConcurrence = period.DurationSeconds / observationEnd.Sub(start).Seconds()
	}
	return period, connections, samples, nil
}

func historyHourBounds(dayStart time.Time, hour int) (time.Time, time.Time) {
	start := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), hour, 0, 0, 0, dayStart.Location())
	return start, start.Add(time.Hour)
}

type historyHour struct {
	Start              time.Time
	End                time.Time
	DurationSeconds    float64
	ActiveConnections  int
	SystemCPU          historyMetric
	ProcessCPU         historyMetric
	MemoryUsed         historyMetric
	ProcessMemory      historyMetric
	AverageConcurrence float64
}

func buildHistoryHours(dayStart time.Time, connections []historyConnection, samples []historySample, now time.Time) []historyHour {
	hours := make([]historyHour, 24)
	for i := range hours {
		hours[i].Start, hours[i].End = historyHourBounds(dayStart, i)
		for _, connection := range connections {
			from := connection.Started
			if from.Before(hours[i].Start.UTC()) {
				from = hours[i].Start.UTC()
			}
			to := connection.Ended
			if to.After(hours[i].End.UTC()) {
				to = hours[i].End.UTC()
			}
			if to.After(from) {
				hours[i].ActiveConnections++
				hours[i].DurationSeconds += to.Sub(from).Seconds()
			}
		}
		for _, sample := range samples {
			if !sample.At.Before(hours[i].Start.UTC()) && sample.At.Before(hours[i].End.UTC()) {
				hours[i].SystemCPU.add(sample.SystemCPU, sample.SystemCPUOK)
				hours[i].ProcessCPU.add(sample.ProcessCPU, sample.ProcessCPUOK)
				hours[i].MemoryUsed.add(sample.MemoryUsed, sample.MemoryUsedOK)
				hours[i].ProcessMemory.add(sample.ProcessMemoryMB, sample.ProcessMemoryOK)
			}
		}
		observationEnd := hours[i].End.UTC()
		if now.Before(observationEnd) {
			observationEnd = now
		}
		if observationEnd.After(hours[i].Start.UTC()) {
			hours[i].AverageConcurrence = hours[i].DurationSeconds / observationEnd.Sub(hours[i].Start.UTC()).Seconds()
		}
	}
	return hours
}
