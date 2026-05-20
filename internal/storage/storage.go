package storage

import (
	"database/sql"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
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

	CREATE INDEX IF NOT EXISTS idx_system_ts ON system_samples(timestamp);
	CREATE INDEX IF NOT EXISTS idx_process_sample ON process_samples(sample_id);
	CREATE INDEX IF NOT EXISTS idx_process_name ON process_samples(name);
	CREATE INDEX IF NOT EXISTS idx_zone_sample ON zone_samples(sample_id);
	CREATE INDEX IF NOT EXISTS idx_zone_name ON zone_samples(name);
	`
	_, err := db.Exec(schema)
	return err
}

func (s *Store) InsertSystemSample(sample SystemSample) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO system_samples (timestamp, load_1, load_5, load_15, ncpu, mem_pressure, swap_used_gb, pageins, pageouts, compressor_pages, swapins, swapouts)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sample.Timestamp, sample.Load1, sample.Load5, sample.Load15,
		sample.Ncpu, sample.MemPressure, sample.SwapUsedGB,
		sample.Pageins, sample.Pageouts, sample.CompressorPages,
		sample.Swapins, sample.Swapouts,
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
