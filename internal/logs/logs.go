package logs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// MaxLogBytes caps how much we return per log request to keep memory bounded.
	MaxLogBytes = 256 * 1024
)

// CrontabEntry represents one scheduled line from `crontab -l`.
type CrontabEntry struct {
	Index    int    `json:"index"`
	Schedule string `json:"schedule"`
	Command  string `json:"command"`
	Label    string `json:"label"`
	LogFile  string `json:"logFile,omitempty"`
}

var (
	// Matches the trailing `>> /path 2>&1` or `> /path` redirect.
	redirectRe = regexp.MustCompile(`>>?\s*(\S+)(?:\s+2>&1)?\s*$`)
	// Matches leading NAME=value or NAME="..." env assignments.
	leadingEnvRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|\S+)\s+`)
)

// GetCrontabEntries parses `crontab -l` and returns scheduled entries.
func GetCrontabEntries() ([]CrontabEntry, error) {
	cmd := exec.Command("/usr/bin/crontab", "-l")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("crontab -l: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("crontab -l: %w", err)
	}

	var entries []CrontabEntry
	idx := 0
	for raw := range strings.SplitSeq(string(out), "\n") {
		schedule, command, ok := parseCronLine(raw)
		if !ok {
			continue
		}
		logFile := ""
		if m := redirectRe.FindStringSubmatch(command); m != nil {
			logFile = m[1]
		}
		entries = append(entries, CrontabEntry{
			Index:    idx,
			Schedule: schedule,
			Command:  command,
			Label:    deriveLabel(command),
			LogFile:  logFile,
		})
		idx++
	}
	return entries, nil
}

// parseCronLine splits a single crontab line into (schedule, command).
// Returns ok=false for blank lines, comments, and env assignments.
func parseCronLine(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	// Env assignment: "=" appears before any whitespace.
	eq := strings.IndexByte(line, '=')
	sp := strings.IndexAny(line, " \t")
	if eq > 0 && (sp < 0 || eq < sp) {
		return "", "", false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", "", false
	}
	if strings.HasPrefix(fields[0], "@") {
		if len(fields) < 2 {
			return "", "", false
		}
		return fields[0], strings.Join(fields[1:], " "), true
	}
	if len(fields) < 6 {
		return "", "", false
	}
	return strings.Join(fields[:5], " "), strings.Join(fields[5:], " "), true
}

// deriveLabel picks a short human-readable label from a cron command —
// the basename of the last path-like token (typically a script) before any redirect.
func deriveLabel(command string) string {
	rest := command
	for {
		m := leadingEnvRe.FindString(rest)
		if m == "" {
			break
		}
		rest = rest[len(m):]
	}
	if i := strings.Index(rest, ">"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.TrimSpace(rest)

	fields := strings.Fields(rest)
	var last string
	for _, tok := range fields {
		if !strings.Contains(tok, "/") {
			continue
		}
		base := filepath.Base(tok)
		ext := filepath.Ext(base)
		if ext != "" {
			last = base
		} else if last == "" {
			last = base
		}
	}
	if last != "" {
		return last
	}
	if len(fields) > 0 {
		return fields[0]
	}
	return command
}

// GetSSHLog returns recent sshd entries from the unified log, capped at MaxLogBytes.
func GetSSHLog(hours int) (string, error) {
	if hours <= 0 {
		hours = 24
	}
	args := []string{
		"show",
		"--predicate", `process == "sshd" OR process == "sshd-session" OR process == "sshd-keygen-wrapper" OR subsystem == "com.openssh.sshd"`,
		"--last", fmt.Sprintf("%dh", hours),
		"--style", "compact",
	}
	return runCapped("/usr/bin/log", args, MaxLogBytes)
}

// GetCronLog returns the log for a single crontab entry.
// If the entry redirects to a file, we tail that file; otherwise we fall back
// to unified-log cron entries for the window.
func GetCronLog(entry CrontabEntry, hours int) (string, error) {
	if hours <= 0 {
		hours = 24
	}
	if entry.LogFile != "" {
		s, err := tailFile(entry.LogFile, MaxLogBytes)
		if err == nil {
			if strings.TrimSpace(s) == "" {
				return fmt.Sprintf("(log file is empty: %s)\n", entry.LogFile), nil
			}
			return s, nil
		}
		if os.IsNotExist(err) {
			return fmt.Sprintf("(log file does not exist yet: %s)\n", entry.LogFile), nil
		}
		return "", err
	}
	banner := fmt.Sprintf("(no log file redirect in cron entry — showing unified-log cron events for the last %dh)\n\n", hours)
	args := []string{
		"show",
		"--predicate", `process == "cron"`,
		"--last", fmt.Sprintf("%dh", hours),
		"--style", "compact",
	}
	out, err := runCapped("/usr/bin/log", args, MaxLogBytes-len(banner))
	if err != nil {
		return "", err
	}
	return banner + out, nil
}

// runCapped runs a command and returns up to maxBytes of its stdout.
// Stops reading once the cap is hit; the process is signalled and waited on
// so no zombies accumulate.
func runCapped(name string, args []string, maxBytes int) (string, error) {
	cmd := exec.Command(name, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.Grow(minInt(maxBytes, 16*1024))
	buf := make([]byte, 8*1024)
	truncated := false
	for {
		n, rerr := stdout.Read(buf)
		if n > 0 {
			remaining := maxBytes - sb.Len()
			if n >= remaining {
				sb.Write(buf[:remaining])
				truncated = true
				break
			}
			sb.Write(buf[:n])
		}
		if rerr != nil {
			break
		}
	}
	if truncated {
		_ = cmd.Process.Kill()
	}
	go io.Copy(io.Discard, stdout)
	_ = cmd.Wait()

	if truncated {
		sb.WriteString(fmt.Sprintf("\n... (truncated at %d bytes)\n", maxBytes))
	}
	return sb.String(), nil
}

// tailFile returns the tail of a file, up to maxBytes.
func tailFile(path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}

	start := int64(0)
	truncated := false
	if stat.Size() > maxBytes {
		start = stat.Size() - maxBytes
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
	return s, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
