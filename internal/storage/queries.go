package storage

import (
	"database/sql"
	"fmt"
	"time"
)

// systemSampleColumns lists every column read into a SystemSample. The
// power/IO columns were added via ALTER TABLE migrations, so old rows have
// NULL values — COALESCE pins them to safe defaults (-1 battery = "unknown",
// "" power source, 0 for the rest) so Scan into non-nullable Go types works.
const systemSampleColumns = `id, timestamp, load_1, load_5, load_15, ncpu, mem_pressure, swap_used_gb,
       pageins, pageouts, compressor_pages, swapins, swapouts,
       COALESCE(battery_pct, -1), COALESCE(power_source, ''), COALESCE(charging, 0),
       COALESCE(disk_read_kb_per_sec, 0), COALESCE(disk_write_kb_per_sec, 0), COALESCE(disk_tps, 0),
       COALESCE(temp_c, 0), COALESCE(fan_rpm, 0), COALESCE(net_rx_bytes, 0), COALESCE(net_tx_bytes, 0),
       COALESCE(net_rx_today, 0), COALESCE(net_tx_today, 0)`

func scanSystemSample(scan func(...interface{}) error, s *SystemSample) error {
	return scan(&s.ID, &s.Timestamp, &s.Load1, &s.Load5, &s.Load15,
		&s.Ncpu, &s.MemPressure, &s.SwapUsedGB,
		&s.Pageins, &s.Pageouts, &s.CompressorPages,
		&s.Swapins, &s.Swapouts,
		&s.BatteryPct, &s.PowerSource, &s.Charging,
		&s.DiskReadKBPerSec, &s.DiskWriteKBPerSec, &s.DiskTPS,
		&s.TempC, &s.FanRPM, &s.NetRxBytes, &s.NetTxBytes,
		&s.NetRxToday, &s.NetTxToday)
}

// GetSystemTimeSeries returns system samples for the last N hours.
func (s *Store) GetSystemTimeSeries(hours int) ([]SystemSample, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT `+systemSampleColumns+`
		 FROM system_samples WHERE timestamp >= ? ORDER BY timestamp ASC`, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var samples []SystemSample
	for rows.Next() {
		var s SystemSample
		if err := scanSystemSample(rows.Scan, &s); err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

// GetLatestSample returns the most recent system sample.
func (s *Store) GetLatestSample() (*SystemSample, error) {
	var sample SystemSample
	row := s.db.QueryRow(
		`SELECT ` + systemSampleColumns + `
		 FROM system_samples ORDER BY id DESC LIMIT 1`,
	)
	if err := scanSystemSample(row.Scan, &sample); err != nil {
		return nil, err
	}
	return &sample, nil
}

// GetTodayNetworkUsage returns today's cumulative rx/tx bytes by subtracting
// the first sample taken after local midnight from the most recent sample.
// Returns zero values (no error) when there isn't yet a "baseline" sample
// for today — e.g. the daemon was first started after midnight and only one
// sample exists. The launchd collector boots at startup, so once the box has
// been awake for a 5-min cycle past midnight this will produce real numbers.
func (s *Store) GetTodayNetworkUsage() (rx, tx int64, err error) {
	// Each sample stores its OWN running total since midnight (computed
	// at collect time in runCollect with reboot-aware delta logic), so
	// the latest sample's value is authoritative. This survives reboots
	// that reset the raw NetRxBytes / NetTxBytes counters and resets
	// cleanly at midnight without losing the running total when the
	// daemon starts mid-day.
	row := s.db.QueryRow(
		`SELECT COALESCE(net_rx_today, 0), COALESCE(net_tx_today, 0)
		 FROM system_samples ORDER BY id DESC LIMIT 1`,
	)
	if err = row.Scan(&rx, &tx); err != nil {
		return 0, 0, nil // no samples yet — treat as zero, not an error
	}
	return rx, tx, nil
}

// ProcessTimeSeries represents a named process's values over time.
type ProcessTimeSeries struct {
	Name   string
	Times  []string
	Values []int // RSS in MB
}

// GetTopProcessNames returns the N process names with highest average RSS over the period.
func (s *Store) GetTopProcessNames(hours int, limit int) ([]string, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT ps.name, AVG(ps.rss_mb) as avg_rss
		 FROM process_samples ps
		 JOIN system_samples ss ON ps.sample_id = ss.id
		 WHERE ss.timestamp >= ?
		 GROUP BY ps.name
		 ORDER BY avg_rss DESC
		 LIMIT ?`, cutoff, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		var avgRSS float64
		if err := rows.Scan(&name, &avgRSS); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetProcessMemoryTimeSeries returns RSS over time for a specific process.
func (s *Store) GetProcessMemoryTimeSeries(name string, hours int) (*ProcessTimeSeries, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT ss.timestamp, ps.rss_mb
		 FROM process_samples ps
		 JOIN system_samples ss ON ps.sample_id = ss.id
		 WHERE ps.name = ? AND ss.timestamp >= ?
		 ORDER BY ss.timestamp ASC`, name, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ts := &ProcessTimeSeries{Name: name}
	for rows.Next() {
		var t string
		var rss int
		if err := rows.Scan(&t, &rss); err != nil {
			return nil, err
		}
		ts.Times = append(ts.Times, t)
		ts.Values = append(ts.Values, rss)
	}
	return ts, rows.Err()
}

// ProcessTableRow represents a process in the summary table.
type ProcessTableRow struct {
	Name       string
	CurrentRSS int
	PeakRSS    int
	AvgRSS     float64
	AvgCPU     float64
}

// GetProcessTable returns summary stats for all tracked processes over the period.
func (s *Store) GetProcessTable(hours int) ([]ProcessTableRow, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)

	// Get the latest sample ID for "current" values
	var latestID int64
	err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM system_samples WHERE timestamp >= ?`, cutoff).Scan(&latestID)
	if err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT ps.name,
		        COALESCE((SELECT ps2.rss_mb FROM process_samples ps2 WHERE ps2.name = ps.name AND ps2.sample_id = ? LIMIT 1), 0) as current_rss,
		        MAX(ps.rss_mb) as peak_rss,
		        AVG(ps.rss_mb) as avg_rss,
		        AVG(ps.cpu_pct) as avg_cpu
		 FROM process_samples ps
		 JOIN system_samples ss ON ps.sample_id = ss.id
		 WHERE ss.timestamp >= ?
		 GROUP BY ps.name
		 ORDER BY avg_rss DESC`, latestID, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var table []ProcessTableRow
	for rows.Next() {
		var r ProcessTableRow
		if err := rows.Scan(&r.Name, &r.CurrentRSS, &r.PeakRSS, &r.AvgRSS, &r.AvgCPU); err != nil {
			return nil, err
		}
		table = append(table, r)
	}
	return table, rows.Err()
}

// ZoneTableRow represents a kernel zone in the summary table.
type ZoneTableRow struct {
	Name        string
	CurrentBytes int64
	PeakBytes    int64
	AvgBytes     float64
	ElemSize     int64
}

// GetZoneTable returns top kernel zones over the period, ranked by average estimated size.
func (s *Store) GetZoneTable(hours int, limit int) ([]ZoneTableRow, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)

	var latestID int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM system_samples WHERE timestamp >= ?`, cutoff).Scan(&latestID); err != nil {
		return nil, err
	}

	rows, err := s.db.Query(
		`SELECT zs.name,
		        COALESCE((SELECT zs2.est_bytes FROM zone_samples zs2 WHERE zs2.name = zs.name AND zs2.sample_id = ? LIMIT 1), 0) as current_bytes,
		        MAX(zs.est_bytes) as peak_bytes,
		        AVG(zs.est_bytes) as avg_bytes,
		        MAX(zs.elem_size) as elem_size
		 FROM zone_samples zs
		 JOIN system_samples ss ON zs.sample_id = ss.id
		 WHERE ss.timestamp >= ?
		 GROUP BY zs.name
		 ORDER BY avg_bytes DESC
		 LIMIT ?`, latestID, cutoff, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var table []ZoneTableRow
	for rows.Next() {
		var r ZoneTableRow
		if err := rows.Scan(&r.Name, &r.CurrentBytes, &r.PeakBytes, &r.AvgBytes, &r.ElemSize); err != nil {
			return nil, err
		}
		table = append(table, r)
	}
	return table, rows.Err()
}

// ZoneTimeSeries represents a single zone's estimated bytes over time.
type ZoneTimeSeries struct {
	Name   string
	Times  []string
	Values []int64 // bytes
}

// GetTopZoneNames returns the N zone names with highest average est_bytes over the period.
func (s *Store) GetTopZoneNames(hours int, limit int) ([]string, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT zs.name, AVG(zs.est_bytes) as avg_bytes
		 FROM zone_samples zs
		 JOIN system_samples ss ON zs.sample_id = ss.id
		 WHERE ss.timestamp >= ?
		 GROUP BY zs.name
		 ORDER BY avg_bytes DESC
		 LIMIT ?`, cutoff, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		var avg float64
		if err := rows.Scan(&name, &avg); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// GetZoneTimeSeries returns est_bytes over time for a specific zone. Mirrors
// GetProcessMemoryTimeSeries — same shape, different table.
func (s *Store) GetZoneTimeSeries(name string, hours int) (*ZoneTimeSeries, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT ss.timestamp, zs.est_bytes
		 FROM zone_samples zs
		 JOIN system_samples ss ON zs.sample_id = ss.id
		 WHERE zs.name = ? AND ss.timestamp >= ?
		 ORDER BY ss.timestamp ASC`, name, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ts := &ZoneTimeSeries{Name: name}
	for rows.Next() {
		var t string
		var b int64
		if err := rows.Scan(&t, &b); err != nil {
			return nil, err
		}
		ts.Times = append(ts.Times, t)
		ts.Values = append(ts.Values, b)
	}
	return ts, rows.Err()
}

// AlertRow is a single row from the alerts table.
type AlertRow struct {
	Kind      string  `json:"kind"`
	LastSent  string  `json:"lastSent"`
	LastValue float64 `json:"lastValue"`
}

// GetRecentAlerts returns the most recently-fired alerts, newest first.
func (s *Store) GetRecentAlerts(limit int) ([]AlertRow, error) {
	rows, err := s.db.Query(
		`SELECT kind, last_sent, COALESCE(last_value, 0)
		 FROM alerts
		 ORDER BY last_sent DESC
		 LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AlertRow
	for rows.Next() {
		var a AlertRow
		if err := rows.Scan(&a.Kind, &a.LastSent, &a.LastValue); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountRecentAlerts returns how many alerts in the table fired within the last
// `hours` hours. Used by the dashboard template to decide whether to show the
// red issue-badge on the Alerts tab.
func (s *Store) CountRecentAlerts(hours int) (int, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM alerts WHERE last_sent >= ?`, cutoff).Scan(&n)
	return n, err
}

// ZoneGrowth pairs a zone's current sample with its nearest at-or-before
// sample from `window` ago. HasPrior is false when no sample exists at or
// before the cutoff (zone is newer than the window, or DB is too young).
// Used by the alerter to detect the runaway-zone pattern that caused the
// 9 GB data_shakalloc.1024 watchdog panic.
type ZoneGrowth struct {
	Name       string
	ElemSize   int64
	InUse      int64
	CurrBytes  int64
	CurrTime   string
	PriorBytes int64
	PriorTime  string
	HasPrior   bool
}

// GetZonesWithGrowth returns, for every zone present in the most recent
// system sample, its current est_bytes/elem_size/in_use plus the est_bytes
// of the nearest zone_sample whose system_sample.timestamp is at-or-before
// (now - window). One query, not N round-trips — the alerter calls this
// every collect cycle.
func (s *Store) GetZonesWithGrowth(window time.Duration) ([]ZoneGrowth, error) {
	cutoff := time.Now().Add(-window).Format(time.RFC3339)

	// Resolve the latest system sample (the "current" snapshot).
	var latestID int64
	var latestTS string
	err := s.db.QueryRow(
		`SELECT id, timestamp FROM system_samples ORDER BY id DESC LIMIT 1`,
	).Scan(&latestID, &latestTS)
	if err != nil {
		return nil, fmt.Errorf("get latest sample for zone growth: %w", err)
	}

	// For each zone in the latest sample, look up the nearest at-or-before
	// est_bytes via a correlated subquery. The subquery returns NULL when no
	// prior sample exists for that zone (DB younger than window, or zone
	// brand new).
	rows, err := s.db.Query(
		`SELECT zs.name, zs.elem_size, zs.in_use, zs.est_bytes,
		        (SELECT zs2.est_bytes
		         FROM zone_samples zs2
		         JOIN system_samples ss2 ON zs2.sample_id = ss2.id
		         WHERE zs2.name = zs.name AND ss2.timestamp <= ?
		         ORDER BY ss2.timestamp DESC
		         LIMIT 1) AS prior_bytes,
		        (SELECT ss2.timestamp
		         FROM zone_samples zs2
		         JOIN system_samples ss2 ON zs2.sample_id = ss2.id
		         WHERE zs2.name = zs.name AND ss2.timestamp <= ?
		         ORDER BY ss2.timestamp DESC
		         LIMIT 1) AS prior_ts
		 FROM zone_samples zs
		 WHERE zs.sample_id = ?`, cutoff, cutoff, latestID,
	)
	if err != nil {
		return nil, fmt.Errorf("query zone growth: %w", err)
	}
	defer rows.Close()

	var out []ZoneGrowth
	for rows.Next() {
		var g ZoneGrowth
		var priorBytes sql.NullInt64
		var priorTS sql.NullString
		if err := rows.Scan(&g.Name, &g.ElemSize, &g.InUse, &g.CurrBytes, &priorBytes, &priorTS); err != nil {
			return nil, err
		}
		g.CurrTime = latestTS
		if priorBytes.Valid {
			g.PriorBytes = priorBytes.Int64
			g.HasPrior = true
		}
		if priorTS.Valid {
			g.PriorTime = priorTS.String
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// SummaryStats holds aggregate stats for the CLI summary.
type SummaryStats struct {
	SampleCount int
	AvgLoad     float64
	MaxLoad     float64
	AvgPressure float64
	MaxPressure int
	AvgSwap     float64
	MaxSwap     float64
}

// GetSummaryStats returns aggregate stats over the period.
func (s *Store) GetSummaryStats(hours int) (*SummaryStats, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	var stats SummaryStats
	err := s.db.QueryRow(
		`SELECT COUNT(*), COALESCE(AVG(load_1),0), COALESCE(MAX(load_1),0),
		        COALESCE(AVG(mem_pressure),0), COALESCE(MAX(mem_pressure),0),
		        COALESCE(AVG(swap_used_gb),0), COALESCE(MAX(swap_used_gb),0)
		 FROM system_samples WHERE timestamp >= ?`, cutoff,
	).Scan(&stats.SampleCount, &stats.AvgLoad, &stats.MaxLoad,
		&stats.AvgPressure, &stats.MaxPressure,
		&stats.AvgSwap, &stats.MaxSwap)
	if err != nil {
		return nil, fmt.Errorf("failed to get summary stats: %w", err)
	}
	return &stats, nil
}
