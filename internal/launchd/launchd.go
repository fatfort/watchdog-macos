// Package launchd surfaces user LaunchAgents under ~/Library/LaunchAgents/
// — their plist config, current launchctl state, log tails, and derived
// health signals (e.g. "runs is incrementing but the log file hasn't grown",
// which is the classic stuck-behind-a-mutex pattern).
package launchd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// MaxLogBytes caps how much log content we return per request.
	MaxLogBytes = 256 * 1024
	// DefaultLabelPrefix filters which agents we surface by default.
	DefaultLabelPrefix = "com.aayushbajaj."
)

// Agent is the user-facing summary of one LaunchAgent.
type Agent struct {
	Label            string   `json:"label"`
	PlistPath        string   `json:"plistPath"`
	Program          string   `json:"program"`
	ProgramArguments []string `json:"programArguments"`
	RunAtLoad        bool     `json:"runAtLoad"`
	StartInterval    int      `json:"startInterval,omitempty"`
	StartCalendar    string   `json:"startCalendar,omitempty"`
	StdoutPath       string   `json:"stdoutPath,omitempty"`
	StderrPath       string   `json:"stderrPath,omitempty"`

	// Live launchctl state.
	State        string `json:"state"`        // "running", "not running", or "unknown"
	PID          int    `json:"pid,omitempty"`
	LastExitCode *int   `json:"lastExitCode,omitempty"`
	Runs         int    `json:"runs"`
	Loaded       bool   `json:"loaded"`

	// File system signals.
	LogFile       string    `json:"logFile,omitempty"`
	LogMTime      time.Time `json:"logMTime,omitempty"`
	LogSize       int64     `json:"logSize,omitempty"`
	PIDStartTime  time.Time `json:"pidStartTime,omitempty"`
	PIDAgeSeconds int64     `json:"pidAgeSeconds,omitempty"`

	// EffectiveLog is the log file actually used for health heuristics and
	// the detail-pane tail — chosen by most-recent mtime across the plist's
	// configured paths and their sibling .launchd/.log variants. It equals
	// LogFile (which is kept for backwards compatibility with existing JSON
	// consumers).
	EffectiveLog string   `json:"effectiveLog,omitempty"`
	// OtherLogs lists the other existing candidate log files we considered
	// but did not pick — surfaced so the user can see why a given file was
	// chosen.
	OtherLogs []string `json:"otherLogs,omitempty"`

	// Derived health.
	Health        string   `json:"health"`        // "ok" | "warn" | "fail" | "unknown"
	HealthReasons []string `json:"healthReasons"` // human-readable bullets
}

// plistRaw is the slice of the plist we care about, after `plutil -convert json`.
type plistRaw struct {
	Label                 string                 `json:"Label"`
	ProgramArguments      []string               `json:"ProgramArguments"`
	Program               string                 `json:"Program"`
	StartInterval         int                    `json:"StartInterval"`
	StartCalendarInterval json.RawMessage        `json:"StartCalendarInterval"`
	RunAtLoad             bool                   `json:"RunAtLoad"`
	StandardOutPath       string                 `json:"StandardOutPath"`
	StandardErrorPath     string                 `json:"StandardErrorPath"`
	EnvironmentVariables  map[string]interface{} `json:"EnvironmentVariables"`
}

// ListAgents enumerates user LaunchAgents, optionally filtered by label prefix.
// Empty prefix returns all of them.
func ListAgents(prefix string) ([]Agent, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}

	uid := strconv.Itoa(os.Getuid())

	var agents []Agent
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".plist") {
			continue
		}
		path := filepath.Join(dir, name)
		// Resolve symlinks so plutil can read the target.
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = path
		}
		a, err := loadAgent(resolved, uid)
		if err != nil {
			// Skip plists we can't parse; surface as a stub so the user notices.
			agents = append(agents, Agent{
				Label:         strings.TrimSuffix(name, ".plist"),
				PlistPath:     path,
				Health:        "unknown",
				HealthReasons: []string{"could not parse plist: " + err.Error()},
			})
			continue
		}
		a.PlistPath = path
		if prefix != "" && !strings.HasPrefix(a.Label, prefix) {
			continue
		}
		agents = append(agents, a)
	}

	sort.Slice(agents, func(i, j int) bool { return agents[i].Label < agents[j].Label })
	return agents, nil
}

// FindAgent returns the agent with the given label, or nil.
func FindAgent(label, prefix string) (*Agent, error) {
	all, err := ListAgents(prefix)
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Label == label {
			return &all[i], nil
		}
	}
	// Try again with no prefix in case the caller's filter excluded it.
	all, err = ListAgents("")
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Label == label {
			return &all[i], nil
		}
	}
	return nil, fmt.Errorf("agent not found: %s", label)
}

func loadAgent(plistPath, uid string) (Agent, error) {
	raw, err := parsePlist(plistPath)
	if err != nil {
		return Agent{}, err
	}

	a := Agent{
		Label:            raw.Label,
		Program:          raw.Program,
		ProgramArguments: raw.ProgramArguments,
		RunAtLoad:        raw.RunAtLoad,
		StartInterval:    raw.StartInterval,
		StdoutPath:       raw.StandardOutPath,
		StderrPath:       raw.StandardErrorPath,
	}
	if a.Program == "" && len(a.ProgramArguments) > 0 {
		a.Program = a.ProgramArguments[0]
	}
	if len(raw.StartCalendarInterval) > 0 && string(raw.StartCalendarInterval) != "null" {
		a.StartCalendar = strings.TrimSpace(string(raw.StartCalendarInterval))
	}

	// Pick the most useful log file for tailing. Wrapper scripts often have
	// two adjacent log files — e.g. foo.launchd.log (only written when the
	// launchd exec itself fails) and foo.log (script's own output). The
	// freshest one is almost always the more truthful health signal, so we
	// resolve a small set of candidates and pick by most-recent mtime.
	chosen, others := resolveEffectiveLog(a.StdoutPath, a.StderrPath)
	a.LogFile = chosen
	a.EffectiveLog = chosen
	a.OtherLogs = others
	if a.LogFile != "" {
		if st, err := os.Stat(a.LogFile); err == nil {
			a.LogMTime = st.ModTime()
			a.LogSize = st.Size()
		}
	}

	// Live state from launchctl.
	enrichWithLaunchctl(&a, uid)

	// pid age, if running.
	if a.PID > 0 {
		if st, err := procStartTime(a.PID); err == nil {
			a.PIDStartTime = st
			a.PIDAgeSeconds = int64(time.Since(st).Seconds())
		}
	}

	a.Health, a.HealthReasons = deriveHealth(&a)
	return a, nil
}

// resolveEffectiveLog inspects the plist's StandardOutPath / StandardErrorPath
// and any sibling log files in the same directory, and returns the candidate
// with the most recent mtime plus the other existing candidates we considered.
//
// The motivation: launchd writes its own .launchd.log only when the exec
// itself fails (TCC denial, missing binary, etc.). A wrapper script that
// appends to a sibling .log on every run will have a much fresher mtime
// there, which is the file that should drive the "stale log" health rule.
//
// Candidates derived per configured path:
//   - the path itself
//   - if basename contains ".launchd", the same path with ".launchd" stripped
//   - if basename does not end in ".log", the same path with ".log" appended
func resolveEffectiveLog(stdout, stderr string) (string, []string) {
	var candidates []string
	seen := map[string]struct{}{}
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		candidates = append(candidates, p)
	}
	for _, p := range []string{stdout, stderr} {
		if p == "" {
			continue
		}
		add(p)
		dir := filepath.Dir(p)
		base := filepath.Base(p)
		// Strip .launchd from the basename (handles foo.launchd.log -> foo.log
		// and foo.launchd -> foo).
		if strings.Contains(base, ".launchd") {
			stripped := strings.Replace(base, ".launchd", "", 1)
			add(filepath.Join(dir, stripped))
		}
		// Probe a .log variant when the basename doesn't already end in .log.
		if !strings.HasSuffix(base, ".log") {
			add(p + ".log")
			if strings.Contains(base, ".launchd") {
				stripped := strings.Replace(base, ".launchd", "", 1)
				if !strings.HasSuffix(stripped, ".log") {
					add(filepath.Join(dir, stripped+".log"))
				}
			}
		}
	}

	// Stat each candidate; keep only the ones that exist.
	type existing struct {
		path  string
		mtime time.Time
	}
	var found []existing
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		found = append(found, existing{path: p, mtime: st.ModTime()})
	}
	if len(found) == 0 {
		// Nothing exists — fall back to whichever was configured so TailLog
		// can produce its "(log file does not exist yet)" message.
		if stdout != "" {
			return stdout, nil
		}
		return stderr, nil
	}
	// Pick the most recent mtime; surface the others.
	sort.Slice(found, func(i, j int) bool { return found[i].mtime.After(found[j].mtime) })
	chosen := found[0].path
	var others []string
	for _, f := range found[1:] {
		others = append(others, f.path)
	}
	return chosen, others
}

// parsePlist reads a plist (binary or XML) by shelling out to plutil, which
// is part of macOS — no third-party dep.
func parsePlist(path string) (*plistRaw, error) {
	cmd := exec.Command("/usr/bin/plutil", "-convert", "json", "-o", "-", path)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("plutil: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("plutil: %w", err)
	}
	var p plistRaw
	if err := json.Unmarshal(out, &p); err != nil {
		return nil, fmt.Errorf("plist json: %w", err)
	}
	return &p, nil
}

var (
	lcStateRe    = regexp.MustCompile(`(?m)^\s*state\s*=\s*(.+)$`)
	lcPIDRe      = regexp.MustCompile(`(?m)^\s*pid\s*=\s*(\d+)\s*$`)
	lcRunsRe     = regexp.MustCompile(`(?m)^\s*runs\s*=\s*(\d+)\s*$`)
	lcLastExitRe = regexp.MustCompile(`(?m)^\s*last exit code\s*=\s*(-?\d+)`)
)

func enrichWithLaunchctl(a *Agent, uid string) {
	target := fmt.Sprintf("gui/%s/%s", uid, a.Label)
	cmd := exec.Command("/bin/launchctl", "print", target)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// "Could not find service" -> not loaded.
		a.State = "not loaded"
		a.Loaded = false
		return
	}
	a.Loaded = true
	s := string(out)
	if m := lcStateRe.FindStringSubmatch(s); m != nil {
		a.State = strings.TrimSpace(m[1])
	}
	if m := lcPIDRe.FindStringSubmatch(s); m != nil {
		a.PID, _ = strconv.Atoi(m[1])
	}
	if m := lcRunsRe.FindStringSubmatch(s); m != nil {
		a.Runs, _ = strconv.Atoi(m[1])
	}
	if m := lcLastExitRe.FindStringSubmatch(s); m != nil {
		if code, err := strconv.Atoi(m[1]); err == nil {
			a.LastExitCode = &code
		}
	}
}

// procStartTime uses `ps` to get the start time of a pid. Returns zero time
// if the pid no longer exists.
func procStartTime(pid int) (time.Time, error) {
	cmd := exec.Command("/bin/ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return time.Time{}, fmt.Errorf("no such pid")
	}
	// ps lstart format: "Mon May 12 14:38:01 2025"
	t, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", s, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return t, nil
}

// deriveHealth implements the rules from the user spec:
//   - last exit code != 0                    -> warn
//   - runs > 0 AND log mtime older than      -> fail (stuck-behind-mutex),
//     StartInterval * 1.5 AND interval > 0       gated on last exit code != 0
//     AND last exit code != 0                    (see comment below)
//   - current pid alive longer than          -> warn (hung script)
//     StartInterval * 2
func deriveHealth(a *Agent) (string, []string) {
	if !a.Loaded {
		return "unknown", []string{"agent is not loaded into launchd (gui/" + strconv.Itoa(os.Getuid()) + ")"}
	}

	var reasons []string
	level := "ok"
	bump := func(to string) {
		if to == "fail" || (to == "warn" && level == "ok") {
			level = to
		}
	}

	if a.LastExitCode != nil && *a.LastExitCode != 0 {
		bump("warn")
		reasons = append(reasons, fmt.Sprintf("last exit code = %d", *a.LastExitCode))
	}

	// The "stale log despite rising runs" rule is gated on a non-zero
	// last exit code. Without the gate, every script that intentionally
	// logs nothing on no-op ticks (wake-watch's early-exit gates,
	// watchdog's own collect command which writes to SQLite, etc.)
	// gets flagged as stuck. Pairing the stale-log signal with a
	// failed last exit keeps the original stuck-mutex detection
	// while skipping the false positives.
	lastExitFailed := a.LastExitCode != nil && *a.LastExitCode != 0
	if lastExitFailed && a.StartInterval > 0 && a.Runs > 0 && !a.LogMTime.IsZero() {
		threshold := time.Duration(float64(a.StartInterval)*1.5) * time.Second
		age := time.Since(a.LogMTime)
		if age > threshold {
			bump("fail")
			reasons = append(reasons,
				fmt.Sprintf("log file mtime is %s old, but agent has run %d times (interval %ds × 1.5 = %s threshold) — likely stuck behind a mutex / silent skip",
					roundDuration(age), a.Runs, a.StartInterval, roundDuration(threshold)))
		}
	}

	if a.StartInterval > 0 && a.PID > 0 && !a.PIDStartTime.IsZero() {
		threshold := time.Duration(a.StartInterval*2) * time.Second
		age := time.Since(a.PIDStartTime)
		if age > threshold {
			bump("warn")
			reasons = append(reasons,
				fmt.Sprintf("pid %d has been alive for %s (interval %ds × 2 = %s threshold)",
					a.PID, roundDuration(age), a.StartInterval, roundDuration(threshold)))
		}
	}

	if level == "ok" {
		reasons = append(reasons, "no failure signals")
	}
	return level, reasons
}

func roundDuration(d time.Duration) string {
	switch {
	case d > 24*time.Hour:
		return fmt.Sprintf("%.1fd", d.Hours()/24)
	case d > time.Hour:
		return fmt.Sprintf("%.1fh", d.Hours())
	case d > time.Minute:
		return fmt.Sprintf("%.0fm", d.Minutes())
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// TailLog returns the tail of an agent's log file, capped at MaxLogBytes.
func TailLog(path string) (string, error) {
	if path == "" {
		return "(no log file configured in plist — set StandardOutPath/StandardErrorPath)\n", nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("(log file does not exist yet: %s)\n", path), nil
		}
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	start := int64(0)
	truncated := false
	if stat.Size() > int64(MaxLogBytes) {
		start = stat.Size() - int64(MaxLogBytes)
		truncated = true
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	s := string(data)
	if truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
		s = fmt.Sprintf("... (showing last %d bytes of %s)\n", len(s), path) + s
	}
	if strings.TrimSpace(s) == "" {
		mtimeStr := ""
		if !stat.ModTime().IsZero() {
			mtimeStr = " (mtime: " + stat.ModTime().Format(time.RFC3339) + ")"
		}
		return fmt.Sprintf("(log file is empty: %s%s)\n", path, mtimeStr), nil
	}
	return s, nil
}
