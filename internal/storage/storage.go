package storage

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	db *sql.DB
}

type SystemSample struct {
	ID              int64
	Timestamp       string
	Load1           float64
	Load5           float64
	Load15          float64
	Ncpu            int
	MemPressure     int
	SwapUsedGB      float64
	Pageins         int64
	Pageouts        int64
	CompressorPages int64
	Swapins         int64
	Swapouts        int64
	// Power — populated by collectPower. BatteryPct is -1 when no battery is
	// present (e.g. desktop Macs); PowerSource is "battery" or "ac".
	BatteryPct  int
	PowerSource string
	Charging    bool
	// IO — populated by collectIO from a 1s iostat sample. macOS `iostat -d`
	// does NOT split reads vs writes, so DiskReadKBPerSec carries the *combined*
	// throughput across all disks and DiskWriteKBPerSec is always 0. The schema
	// keeps the split so a future source (e.g. fs_usage) can fill it in.
	DiskReadKBPerSec  float64
	DiskWriteKBPerSec float64
	DiskTPS           float64
	// TempC is the CPU die temperature in Celsius (0 if SMC unreachable).
	TempC float64
	// FanRPM is the highest fan speed in RPM (0 on fanless Macs).
	FanRPM int
	// NetRxBytes / NetTxBytes are cumulative interface counters (since boot)
	// summed across non-loopback interfaces. Today's totals are computed at
	// query time by subtracting the first sample-after-midnight from the
	// latest sample (see GetTodayNetworkUsage).
	NetRxBytes int64
	NetTxBytes int64
	// NetRxToday / NetTxToday are the cumulative bytes since local midnight,
	// computed at collect time so the value survives reboots that reset the
	// raw NetRxBytes / NetTxBytes counters. Rolls over at midnight local.
	NetRxToday int64
	NetTxToday int64
}

type ProcessSample struct {
	SampleID int64
	Name     string
	PID      int
	RSSMB    int
	CPUPct   float64
}

// ZoneSample captures a single kernel zone at a point in time. EstBytes is
// computed as ElemSize*InUse — `zprint` only reports actual cur_size under
// root, but elem*inuse is a faithful proxy and is what catches the runaway
// growth pattern that caused the watchdog-timeout panic (9 GB in
// data_shakalloc.1024).
type ZoneSample struct {
	SampleID int64
	Name     string
	ElemSize int64
	InUse    int64
	EstBytes int64
}

func getDataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		if u, userErr := user.Current(); userErr == nil {
			home = u.HomeDir
		} else {
			return "", err
		}
	}
	dataDir := filepath.Join(home, ".local", "share", "watchdog")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return "", err
	}
	return dataDir, nil
}

func New() (*Store, error) {
	dataDir, err := getDataDir()
	if err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dataDir, "watchdog.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		return nil, err
	}

	if err := initSchema(db); err != nil {
		return nil, err
	}

	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func initSchema(db *sql.DB) error {
	schema := `
	CREATE TABLE IF NOT EXISTS system_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		load_1 REAL,
		load_5 REAL,
		load_15 REAL,
		ncpu INTEGER,
		mem_pressure INTEGER,
		swap_used_gb REAL,
		pageins INTEGER,
		pageouts INTEGER,
		compressor_pages INTEGER,
		swapins INTEGER,
		swapouts INTEGER
	);

	CREATE TABLE IF NOT EXISTS process_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		sample_id INTEGER REFERENCES system_samples(id),
		name TEXT NOT NULL,
		pid INTEGER,
		rss_mb INTEGER,
		cpu_pct REAL
	);

	CREATE TABLE IF NOT EXISTS zone_samples (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		sample_id INTEGER REFERENCES system_samples(id),
		name TEXT NOT NULL,
		elem_size INTEGER,
		in_use INTEGER,
		est_bytes INTEGER
	);

	CREATE TABLE IF NOT EXISTS alerts (
		kind TEXT PRIMARY KEY,
		last_sent TEXT NOT NULL,
		last_value REAL
	);

	CREATE INDEX IF NOT EXISTS idx_system_ts ON system_samples(timestamp);
	CREATE INDEX IF NOT EXISTS idx_process_sample ON process_samples(sample_id);
	CREATE INDEX IF NOT EXISTS idx_process_name ON process_samples(name);
	CREATE INDEX IF NOT EXISTS idx_zone_sample ON zone_samples(sample_id);
	CREATE INDEX IF NOT EXISTS idx_zone_name ON zone_samples(name);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	// Additive, idempotent migrations. The shared DB at ~/.local/share/watchdog
	// holds months of samples, so every new collector adds its columns via
	// ALTER TABLE and swallows the SQLite "duplicate column name" error so
	// re-runs are no-ops. Never DROP, never rename — that would break old rows.
	migrations := []string{
		`ALTER TABLE system_samples ADD COLUMN battery_pct INTEGER`,
		`ALTER TABLE system_samples ADD COLUMN power_source TEXT`,
		`ALTER TABLE system_samples ADD COLUMN charging INTEGER`,
		`ALTER TABLE system_samples ADD COLUMN disk_read_kb_per_sec REAL`,
		`ALTER TABLE system_samples ADD COLUMN disk_write_kb_per_sec REAL`,
		`ALTER TABLE system_samples ADD COLUMN disk_tps REAL`,
		`ALTER TABLE system_samples ADD COLUMN temp_c REAL DEFAULT 0`,
		`ALTER TABLE system_samples ADD COLUMN fan_rpm INTEGER DEFAULT 0`,
		`ALTER TABLE system_samples ADD COLUMN net_rx_bytes INTEGER DEFAULT 0`,
		`ALTER TABLE system_samples ADD COLUMN net_tx_bytes INTEGER DEFAULT 0`,
		`ALTER TABLE system_samples ADD COLUMN net_rx_today INTEGER DEFAULT 0`,
		`ALTER TABLE system_samples ADD COLUMN net_tx_today INTEGER DEFAULT 0`,
	}
	for _, stmt := range migrations {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			return fmt.Errorf("migration %q: %w", stmt, err)
		}
	}
	return nil
}

func (s *Store) InsertSystemSample(sample SystemSample) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO system_samples (timestamp, load_1, load_5, load_15, ncpu, mem_pressure, swap_used_gb, pageins, pageouts, compressor_pages, swapins, swapouts, battery_pct, power_source, charging, disk_read_kb_per_sec, disk_write_kb_per_sec, disk_tps, temp_c, fan_rpm, net_rx_bytes, net_tx_bytes, net_rx_today, net_tx_today)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sample.Timestamp, sample.Load1, sample.Load5, sample.Load15,
		sample.Ncpu, sample.MemPressure, sample.SwapUsedGB,
		sample.Pageins, sample.Pageouts, sample.CompressorPages,
		sample.Swapins, sample.Swapouts,
		sample.BatteryPct, sample.PowerSource, sample.Charging,
		sample.DiskReadKBPerSec, sample.DiskWriteKBPerSec, sample.DiskTPS,
		sample.TempC, sample.FanRPM, sample.NetRxBytes, sample.NetTxBytes,
		sample.NetRxToday, sample.NetTxToday,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) InsertProcessSamples(sampleID int64, processes []ProcessSample) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO process_samples (sample_id, name, pid, rss_mb, cpu_pct)
		 VALUES (?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, p := range processes {
		if _, err := stmt.Exec(sampleID, p.Name, p.PID, p.RSSMB, p.CPUPct); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func (s *Store) InsertZoneSamples(sampleID int64, zones []ZoneSample) error {
	if len(zones) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(
		`INSERT INTO zone_samples (sample_id, name, elem_size, in_use, est_bytes)
		 VALUES (?, ?, ?, ?, ?)`,
	)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, z := range zones {
		if _, err := stmt.Exec(sampleID, z.Name, z.ElemSize, z.InUse, z.EstBytes); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// LastAlertTime returns the timestamp of the last alert of this kind, plus
// whether one was found. Used by the alerter to enforce per-kind cool-downs
// so we don't spam the inbox with the same condition every 5 minutes.
func (s *Store) LastAlertTime(kind string) (time.Time, bool, error) {
	var ts string
	err := s.db.QueryRow(`SELECT last_sent FROM alerts WHERE kind = ?`, kind).Scan(&ts)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return time.Time{}, false, err
	}
	return t, true, nil
}

// RecordAlertSent upserts the last-sent timestamp + triggering value for an alert kind.
func (s *Store) RecordAlertSent(kind string, value float64) error {
	_, err := s.db.Exec(
		`INSERT INTO alerts (kind, last_sent, last_value) VALUES (?, ?, ?)
		 ON CONFLICT(kind) DO UPDATE SET last_sent=excluded.last_sent, last_value=excluded.last_value`,
		kind, time.Now().Format(time.RFC3339), value,
	)
	return err
}

func (s *Store) Prune(retentionDays int) error {
	cutoff := time.Now().AddDate(0, 0, -retentionDays).Format(time.RFC3339)
	_, err := s.db.Exec(`DELETE FROM process_samples WHERE sample_id IN (SELECT id FROM system_samples WHERE timestamp < ?)`, cutoff)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM zone_samples WHERE sample_id IN (SELECT id FROM system_samples WHERE timestamp < ?)`, cutoff)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`DELETE FROM system_samples WHERE timestamp < ?`, cutoff)
	return err
}

func GetLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	logDir := filepath.Join(home, "Library", "Logs", "system-health")
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create log dir: %w", err)
	}
	return logDir, nil
}
