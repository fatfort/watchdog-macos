// Package typtel is an OPTIONAL consumer of the typing-telemetry CLI
// (https://github.com/abaj8494/typing-telemetry). When `typtel` is present
// on PATH it shells out to `typtel today --json` and returns the parsed
// payload. When typtel is absent the package no-ops: the watchdog must
// never fail to collect or summarise because a sibling tool is missing.
//
// Watchdog does NOT persist any typtel data — it queries on demand from
// summary and serve surfaces. typing-telemetry owns retention.
package typtel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// commonInstallDirs lists the locations brew / MacPorts / manual installs
// drop binaries on macOS. We probe these in addition to PATH because the
// watchdog runs under launchd, whose default PATH is the narrow
// "/usr/bin:/bin:/usr/sbin:/sbin" — typtel lives at /opt/homebrew/bin on
// Apple Silicon and won't be found by exec.LookPath without help.
var commonInstallDirs = []string{
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"/opt/local/bin",
}

// Stats mirrors the stable JSON schema produced by `typtel today --json`.
// Keep this struct additive only — fields may be added but never renamed
// or removed without a corresponding bump in the typtel CLI contract.
type Stats struct {
	Date            string  `json:"date"`
	Keystrokes      int64   `json:"keystrokes"`
	Words           int64   `json:"words"`
	Letters         int64   `json:"letters"`
	Modifiers       int64   `json:"modifiers"`
	Special         int64   `json:"special"`
	MouseClicks     int64   `json:"mouse_clicks"`
	MouseDistancePx float64 `json:"mouse_distance_px"`
	MouseDistanceM  float64 `json:"mouse_distance_m"`
	ActiveHours     int     `json:"active_hours"`
}

// queryTimeout caps how long we'll wait for typtel to respond. The CLI is
// a thin SQLite read, so this is intentionally short — better to skip the
// section in `watchdog summary` than make the user wait on a stuck process.
const queryTimeout = 3 * time.Second

// lookPath is var-indirected so tests can stub PATH lookups without
// shelling out. The default checks the standard exec.LookPath first
// and then falls back to a fixed set of macOS install locations so
// launchd's narrow default PATH doesn't hide typtel from us.
var lookPath = func(binary string) (string, error) {
	if p, err := exec.LookPath(binary); err == nil {
		return p, nil
	}
	for _, dir := range commonInstallDirs {
		candidate := filepath.Join(dir, binary)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

// runCommand is var-indirected so tests can stub the subprocess invocation.
// In production it execs `typtel today --json` with a timeout.
var runCommand = func(ctx context.Context, binary string) ([]byte, error) {
	return exec.CommandContext(ctx, binary, "today", "--json").Output()
}

// Fetch returns today's typing stats from the `typtel` CLI.
//
// Return contract:
//   - (stats, true,  nil) — typtel found, stats parsed successfully.
//   - (zero,  false, nil) — typtel not on PATH; not an error, just absent.
//   - (zero,  false, err) — typtel found but the call or parse failed.
//
// Callers should treat the second return value as the source of truth for
// "show this section?". Errors are surfaced only so a serve-mode handler
// can decide between 404 (absent) and 500 (broken).
func Fetch() (Stats, bool, error) {
	binary, err := lookPath("typtel")
	if err != nil {
		// exec.ErrNotFound is the only expected error here. Treat any other
		// LookPath failure (permission, etc.) as "absent" too — we never
		// want to escalate a PATH oddity into a watchdog failure.
		return Stats{}, false, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	out, err := runCommand(ctx, binary)
	if err != nil {
		// Distinguish "typtel exists but blew up" from "missing". This
		// branch is the only place we return a real error — the caller
		// can choose whether to surface it (e.g. serve mode) or eat it
		// (e.g. summary mode).
		if exitErr := new(exec.ExitError); errors.As(err, &exitErr) {
			return Stats{}, false, fmt.Errorf("typtel exited %d: %s",
				exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return Stats{}, false, fmt.Errorf("run typtel: %w", err)
	}

	var s Stats
	if err := json.Unmarshal(out, &s); err != nil {
		return Stats{}, false, fmt.Errorf("parse typtel json: %w", err)
	}
	return s, true, nil
}
