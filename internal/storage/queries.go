package storage

import (
	"fmt"
	"time"
)

// GetSystemTimeSeries returns system samples for the last N hours.
func (s *Store) GetSystemTimeSeries(hours int) ([]SystemSample, error) {
	cutoff := time.Now().Add(-time.Duration(hours) * time.Hour).Format(time.RFC3339)
	rows, err := s.db.Query(
		`SELECT id, timestamp, load_1, load_5, load_15, ncpu, mem_pressure, swap_used_gb,
		        pageins, pageouts, compressor_pages, swapins, swapouts
		 FROM system_samples WHERE timestamp >= ? ORDER BY timestamp ASC`, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var samples []SystemSample
	for rows.Next() {
		var s SystemSample
		if err := rows.Scan(&s.ID, &s.Timestamp, &s.Load1, &s.Load5, &s.Load15,
			&s.Ncpu, &s.MemPressure, &s.SwapUsedGB,
			&s.Pageins, &s.Pageouts, &s.CompressorPages,
			&s.Swapins, &s.Swapouts); err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	return samples, rows.Err()
}

// GetLatestSample returns the most recent system sample.
func (s *Store) GetLatestSample() (*SystemSample, error) {
	var sample SystemSample
	err := s.db.QueryRow(
		`SELECT id, timestamp, load_1, load_5, load_15, ncpu, mem_pressure, swap_used_gb,
		        pageins, pageouts, compressor_pages, swapins, swapouts
		 FROM system_samples ORDER BY id DESC LIMIT 1`,
	).Scan(&sample.ID, &sample.Timestamp, &sample.Load1, &sample.Load5, &sample.Load15,
		&sample.Ncpu, &sample.MemPressure, &sample.SwapUsedGB,
		&sample.Pageins, &sample.Pageouts, &sample.CompressorPages,
		&sample.Swapins, &sample.Swapouts)
	if err != nil {
		return nil, err
	}
	return &sample, nil
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
