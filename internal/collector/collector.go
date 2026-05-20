package collector

import (
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/normalize"
	"github.com/abaj8494/macos-watchdog/internal/storage"
)

const (
	TopProcessCount = 15
	TopZoneCount    = 20
)

// CollectResult holds a complete sample of system health data.
type CollectResult struct {
	System    storage.SystemSample
	Processes []storage.ProcessSample
	Zones     []storage.ZoneSample
	LogLine   string // flat text log line for backward compat
}

// Collect gathers system health metrics and returns structured data.
func Collect() (*CollectResult, error) {
	result := &CollectResult{}
	result.System.Timestamp = time.Now().Format(time.RFC3339)

	if err := collectLoadAvg(&result.System); err != nil {
		return nil, fmt.Errorf("load avg: %w", err)
	}
	if err := collectMemPressure(&result.System); err != nil {
		return nil, fmt.Errorf("mem pressure: %w", err)
	}
	if err := collectSwap(&result.System); err != nil {
		return nil, fmt.Errorf("swap: %w", err)
	}
	if err := collectVMStat(&result.System); err != nil {
		return nil, fmt.Errorf("vmstat: %w", err)
	}

	procs, err := collectProcesses()
	if err != nil {
		return nil, fmt.Errorf("processes: %w", err)
	}
	result.Processes = procs

	// Zones are best-effort — if zprint changes format or vanishes, keep collecting
	// the rest. The whole point of this source is to catch the kind of runaway
	// kernel-zone growth that caused the 9 GB data_shakalloc.1024 panic.
	zones, err := collectKernelZones()
	if err == nil {
		result.Zones = zones
	}

	result.LogLine = formatLogLine(result)
	return result, nil
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func collectLoadAvg(s *storage.SystemSample) error {
	// sysctl -n vm.loadavg returns: { 1.23 2.34 3.45 }
	out, err := run("/usr/sbin/sysctl", "-n", "vm.loadavg")
	if err != nil {
		return err
	}
	out = strings.Trim(out, "{ }")
	fields := strings.Fields(out)
	if len(fields) < 3 {
		return fmt.Errorf("unexpected loadavg format: %q", out)
	}
	s.Load1, _ = strconv.ParseFloat(fields[0], 64)
	s.Load5, _ = strconv.ParseFloat(fields[1], 64)
	s.Load15, _ = strconv.ParseFloat(fields[2], 64)

	ncpuStr, err := run("/usr/sbin/sysctl", "-n", "hw.ncpu")
	if err != nil {
		return err
	}
	s.Ncpu, _ = strconv.Atoi(ncpuStr)
	return nil
}

func collectMemPressure(s *storage.SystemSample) error {
	out, err := run("/usr/sbin/sysctl", "-n", "kern.memorystatus_level")
	if err != nil {
		return err
	}
	level, _ := strconv.Atoi(out)
	s.MemPressure = 100 - level
	return nil
}

func collectSwap(s *storage.SystemSample) error {
	// vm.swapusage: total = 3072.00M  used = 1826.06M  free = 1245.94M  (encrypted)
	out, err := run("/usr/sbin/sysctl", "-n", "vm.swapusage")
	if err != nil {
		return err
	}
	for _, part := range strings.Split(out, " ") {
		if strings.HasSuffix(part, "M") && strings.Contains(out[:strings.Index(out, part)], "used") {
			val := strings.TrimSuffix(part, "M")
			mb, _ := strconv.ParseFloat(val, 64)
			s.SwapUsedGB = mb / 1024.0
			return nil
		}
	}
	// Fallback: parse "used = XXXM"
	if idx := strings.Index(out, "used = "); idx >= 0 {
		rest := out[idx+7:]
		rest = strings.TrimSpace(rest)
		for i, c := range rest {
			if c == 'M' || c == 'G' {
				val, _ := strconv.ParseFloat(rest[:i], 64)
				if c == 'G' {
					s.SwapUsedGB = val
				} else {
					s.SwapUsedGB = val / 1024.0
				}
				return nil
			}
		}
	}
	return nil
}

func collectVMStat(s *storage.SystemSample) error {
	out, err := run("/usr/bin/vm_stat")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		val := strings.TrimSuffix(parts[len(parts)-1], ".")
		num, _ := strconv.ParseInt(val, 10, 64)

		switch {
		case strings.HasPrefix(line, "Pageins:"):
			s.Pageins = num
		case strings.HasPrefix(line, "Pageouts:"):
			s.Pageouts = num
		case strings.Contains(line, "stored in compressor"):
			s.CompressorPages = num
		case strings.HasPrefix(line, "Swapins:"):
			s.Swapins = num
		case strings.HasPrefix(line, "Swapouts:"):
			s.Swapouts = num
		}
	}
	return nil
}

type procInfo struct {
	PID    int
	RSSMB  int
	CPUPct float64
	Comm   string
}

func collectProcesses() ([]storage.ProcessSample, error) {
	// ps -eo pid,rss,%cpu,comm -m gives processes sorted by RSS (descending)
	out, err := run("/bin/ps", "-eo", "pid,rss,%cpu,comm", "-m")
	if err != nil {
		return nil, err
	}

	var procs []procInfo
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		if i == 0 { // skip header
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		pid, _ := strconv.Atoi(fields[0])
		rssKB, _ := strconv.Atoi(fields[1])
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		comm := strings.Join(fields[3:], " ")

		procs = append(procs, procInfo{
			PID:    pid,
			RSSMB:  rssKB / 1024,
			CPUPct: cpu,
			Comm:   comm,
		})
	}

	// Already sorted by RSS desc from ps -m, take top N
	sort.Slice(procs, func(i, j int) bool {
		return procs[i].RSSMB > procs[j].RSSMB
	})

	limit := TopProcessCount
	if len(procs) < limit {
		limit = len(procs)
	}

	var samples []storage.ProcessSample
	for _, p := range procs[:limit] {
		samples = append(samples, storage.ProcessSample{
			Name:   normalize.ProcessName(p.Comm),
			PID:    p.PID,
			RSSMB:  p.RSSMB,
			CPUPct: p.CPUPct,
		})
	}
	return samples, nil
}

// collectKernelZones samples kernel zone occupancy via `zprint`. Tries sudo
// first (which fills in cur_size columns with real values like "9G"); falls
// back to bare zprint, which only reports element counts — those × elem_size
// is a faithful estimate of resident bytes and would have caught the runaway
// data_shakalloc.1024 zone that triggered the watchdog-timeout panic.
//
// To upgrade unprivileged → privileged, add a sudoers entry:
//
//	echo 'YOURNAME ALL=(root) NOPASSWD: /usr/bin/zprint' | sudo tee /etc/sudoers.d/watchdog
//	sudo chmod 440 /etc/sudoers.d/watchdog
func collectKernelZones() ([]storage.ZoneSample, error) {
	// -L suppresses the trailing wired-memory block so we don't have to skip past it.
	out, err := run("/usr/bin/sudo", "-n", "/usr/bin/zprint", "-L")
	if err != nil {
		out, err = run("/usr/bin/zprint", "-L")
		if err != nil {
			return nil, err
		}
	}

	var zones []storage.ZoneSample
	inData := false
	for _, line := range strings.Split(out, "\n") {
		if !inData {
			// The header ends with a dashed rule; everything after that is zone data.
			if strings.HasPrefix(strings.TrimSpace(line), "----") {
				inData = true
			}
			continue
		}
		fields := strings.Fields(line)
		// Expected columns: name elem cur_size max_size cur_elts max_elts inuse alloc_size alloc_count [flags...]
		if len(fields) < 9 {
			continue
		}
		elemSize, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		inUse, err := strconv.ParseInt(fields[6], 10, 64)
		if err != nil {
			continue
		}
		// Prefer the kernel's reported cur_size when available (root-only); fall back to elem*inuse.
		estBytes := parseZprintSize(fields[2])
		if estBytes == 0 {
			estBytes = elemSize * inUse
		}
		zones = append(zones, storage.ZoneSample{
			Name:     fields[0],
			ElemSize: elemSize,
			InUse:    inUse,
			EstBytes: estBytes,
		})
	}

	sort.Slice(zones, func(i, j int) bool {
		return zones[i].EstBytes > zones[j].EstBytes
	})
	if len(zones) > TopZoneCount {
		zones = zones[:TopZoneCount]
	}
	return zones, nil
}

// parseZprintSize parses zprint's human-readable sizes ("0K", "61K", "365M", "9G")
// into bytes. Returns 0 for the "----" sentinel zprint uses for unbounded zones.
func parseZprintSize(s string) int64 {
	if s == "" || s == "----" {
		return 0
	}
	n := len(s)
	var mult int64
	switch s[n-1] {
	case 'K':
		mult = 1024
	case 'M':
		mult = 1024 * 1024
	case 'G':
		mult = 1024 * 1024 * 1024
	default:
		v, _ := strconv.ParseInt(s, 10, 64)
		return v
	}
	v, _ := strconv.ParseFloat(s[:n-1], 64)
	return int64(v * float64(mult))
}

func formatLogLine(r *CollectResult) string {
	s := r.System

	var topMem, topCPU []string
	for _, p := range r.Processes {
		topMem = append(topMem, fmt.Sprintf("%s(%dMB)", p.Name, p.RSSMB))
		topCPU = append(topCPU, fmt.Sprintf("%s(%.0f%%)", p.Name, p.CPUPct))
	}

	return fmt.Sprintf("[%s] load=%.2f/%.2f/%.2f ncpu=%d mem_pressure=%d%% swap=%.2fGB pageins=%d pageouts=%d compressor_pages=%d swapins=%d swapouts=%d | top_mem: %s | top_cpu: %s",
		time.Now().Format("2006-01-02T15:04:05-0700"),
		s.Load1, s.Load5, s.Load15, s.Ncpu, s.MemPressure, s.SwapUsedGB,
		s.Pageins, s.Pageouts, s.CompressorPages, s.Swapins, s.Swapouts,
		strings.Join(topMem, " "), strings.Join(topCPU, " "),
	)
}
