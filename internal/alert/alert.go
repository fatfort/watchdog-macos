// Package alert evaluates collected samples against threshold rules and
// emails the user when one fires. Mirrors the rmsync-mail-touch-report.py
// pattern: msmtp -a gmail, self to self, plain text.
//
// Observation-only contract per project feedback: this package may emit
// notifications but never mutates system state.
package alert

import (
	"bytes"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/storage"
)

const (
	memPressureThreshold      = 70
	memPressureSustainSamples = 2 // ≥10 min sustained at 5-min collect cadence
	swapThresholdGB           = 10.0
	defaultCooldown           = 60 * time.Minute

	// Zone-runaway rule: a single kernel zone growing fast is what caused the
	// 9 GB data_shakalloc.1024 watchdog panic, so this rule is more urgent
	// than sustained memory pressure and gets a shorter per-zone cool-down.
	zoneAbsoluteTripBytes  = 1 << 30        // 1 GiB
	zoneGrowthMinBytes     = 256 << 20      // 256 MiB floor for the 2x rule (noise filter)
	zoneGrowthRatio        = 2.0            // > 2x in `zoneGrowthWindow`
	zoneGrowthWindow       = 1 * time.Hour  // compared against est_bytes 1h ago
	zoneCooldown           = 30 * time.Minute
	zoneRunawayMaxPerCycle = 5 // cap fan-out per Evaluate() so we don't mailbomb

	// Load-spike — a sudden CPU load ≥ 8 caught the 14.73 spike from the
	// 2026-05-21 hang. Single-sample because thrash spikes can be brief
	// before the system locks up; the cool-down stops repeat-firing.
	loadSpikeThreshold = 8.0
	loadSpikeCooldown  = 30 * time.Minute

	// Compressor-pressure — high compressor + active swap together is the
	// real precursor to a thrash hang on Apple Silicon. The DB at the time
	// of the hang showed ~1M compressor pages with 2.5 GB swap for 30+ min
	// before the system stalled; this rule would have fired well in advance.
	compressorPagesThreshold  = 800_000
	compressorSwapThresholdGB = 1.0
	compressorSustainSamples  = 2
	compressorCooldown        = 60 * time.Minute

	// Temp-sustained — Apple Silicon throttles around 95-100 °C; sustained
	// 75 °C means the SoC is running hot enough to be worth knowing about.
	tempThreshold      = 75.0
	tempSustainSamples = 2
	tempCooldown       = 60 * time.Minute

	msmtpBin     = "/opt/local/bin/msmtp"
	msmtpAccount = "gmail"
	recipient    = "aayushbajaj7@gmail.com"
	sender       = "aayushbajaj7@gmail.com"
)

type Alert struct {
	Kind    string
	Value   float64
	Subject string
	Body    string
}

// Evaluate runs every threshold rule and returns the alerts that both
// triggered and aren't still within their per-kind cool-down.
func Evaluate(store *storage.Store, samples []storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) []Alert {
	var fired []Alert

	if a, ok := evalMemPressure(samples, procs, zones); ok && shouldFire(store, a.Kind, defaultCooldown) {
		fired = append(fired, a)
	}
	if a, ok := evalSwap(samples, procs, zones); ok && shouldFire(store, a.Kind, defaultCooldown) {
		fired = append(fired, a)
	}
	for _, a := range evalZoneRunaway(store) {
		if shouldFire(store, a.Kind, zoneCooldown) {
			fired = append(fired, a)
		}
	}
	if a, ok := evalLoadSpike(samples, procs, zones); ok && shouldFire(store, a.Kind, loadSpikeCooldown) {
		fired = append(fired, a)
	}
	if a, ok := evalCompressorPressure(samples, procs, zones); ok && shouldFire(store, a.Kind, compressorCooldown) {
		fired = append(fired, a)
	}
	if a, ok := evalTempSustained(samples, procs, zones); ok && shouldFire(store, a.Kind, tempCooldown) {
		fired = append(fired, a)
	}
	return fired
}

func evalLoadSpike(samples []storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) (Alert, bool) {
	if len(samples) == 0 {
		return Alert{}, false
	}
	latest := samples[len(samples)-1]
	if latest.Load1 < loadSpikeThreshold {
		return Alert{}, false
	}
	return Alert{
		Kind:    "load-spike",
		Value:   latest.Load1,
		Subject: fmt.Sprintf("[watchdog] load %.1f — thrash precursor", latest.Load1),
		Body:    buildBody(fmt.Sprintf("Load average crossed %.1f — system may be about to thrash.", loadSpikeThreshold), latest, procs, zones),
	}, true
}

func evalCompressorPressure(samples []storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) (Alert, bool) {
	if len(samples) < compressorSustainSamples {
		return Alert{}, false
	}
	tail := samples[len(samples)-compressorSustainSamples:]
	for _, s := range tail {
		if s.CompressorPages < compressorPagesThreshold {
			return Alert{}, false
		}
		if s.SwapUsedGB < compressorSwapThresholdGB {
			return Alert{}, false
		}
	}
	latest := samples[len(samples)-1]
	return Alert{
		Kind:    "compressor-pressure",
		Value:   float64(latest.CompressorPages),
		Subject: fmt.Sprintf("[watchdog] compressor %dk pages + swap %.1f GB", latest.CompressorPages/1000, latest.SwapUsedGB),
		Body:    buildBody("Compressor and swap both sustained — the precursor pattern to the 2026-05-21 thrash hang.", latest, procs, zones),
	}, true
}

func evalTempSustained(samples []storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) (Alert, bool) {
	if len(samples) < tempSustainSamples {
		return Alert{}, false
	}
	tail := samples[len(samples)-tempSustainSamples:]
	for _, s := range tail {
		if s.TempC < tempThreshold {
			return Alert{}, false
		}
	}
	latest := samples[len(samples)-1]
	return Alert{
		Kind:    "temp-sustained",
		Value:   latest.TempC,
		Subject: fmt.Sprintf("[watchdog] CPU %.0f °C sustained", latest.TempC),
		Body:    buildBody(fmt.Sprintf("CPU temperature has been ≥ %.0f °C for at least two samples.", tempThreshold), latest, procs, zones),
	}, true
}

func shouldFire(store *storage.Store, kind string, cooldown time.Duration) bool {
	last, ok, err := store.LastAlertTime(kind)
	if err != nil || !ok {
		return true
	}
	return time.Since(last) >= cooldown
}

func evalMemPressure(samples []storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) (Alert, bool) {
	if len(samples) < memPressureSustainSamples {
		return Alert{}, false
	}
	tail := samples[len(samples)-memPressureSustainSamples:]
	for _, s := range tail {
		if s.MemPressure < memPressureThreshold {
			return Alert{}, false
		}
	}
	latest := samples[len(samples)-1]
	return Alert{
		Kind:    "mem-pressure",
		Value:   float64(latest.MemPressure),
		Subject: fmt.Sprintf("[watchdog] memory pressure %d%% sustained", latest.MemPressure),
		Body:    buildBody("Memory pressure sustained above threshold — kernel panic precursor.", latest, procs, zones),
	}, true
}

func evalSwap(samples []storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) (Alert, bool) {
	if len(samples) == 0 {
		return Alert{}, false
	}
	latest := samples[len(samples)-1]
	if latest.SwapUsedGB < swapThresholdGB {
		return Alert{}, false
	}
	return Alert{
		Kind:    "swap-high",
		Value:   latest.SwapUsedGB,
		Subject: fmt.Sprintf("[watchdog] swap %.1f GB", latest.SwapUsedGB),
		Body:    buildBody("Swap usage above threshold — system likely thrashing.", latest, procs, zones),
	}, true
}

// evalZoneRunaway fires for every kernel zone whose current est_bytes either
// crosses an absolute trip-wire (≥ 1 GiB) or has more than doubled relative
// to its value `zoneGrowthWindow` ago (with a 256 MiB floor to suppress
// tiny-zone noise). One Alert per offending zone, kind = "zone-runaway:<name>"
// so cool-downs are per-zone, capped to the top zoneRunawayMaxPerCycle by
// current size to avoid mailbombing.
//
// The 9 GB data_shakalloc.1024 panic this project exists to catch would have
// tripped the absolute rule long before kernel watchdog timeout.
func evalZoneRunaway(store *storage.Store) []Alert {
	growths, err := store.GetZonesWithGrowth(zoneGrowthWindow)
	if err != nil || len(growths) == 0 {
		return nil
	}

	type candidate struct {
		g     storage.ZoneGrowth
		ratio float64 // 0 if no prior; tracked separately via HasPrior
	}
	var hits []candidate
	for _, g := range growths {
		var ratio float64
		if g.HasPrior && g.PriorBytes > 0 {
			ratio = float64(g.CurrBytes) / float64(g.PriorBytes)
		}

		absoluteTrip := g.CurrBytes >= zoneAbsoluteTripBytes
		growthTrip := g.HasPrior && g.PriorBytes > 0 &&
			g.CurrBytes >= zoneGrowthMinBytes &&
			ratio > zoneGrowthRatio

		if absoluteTrip || growthTrip {
			hits = append(hits, candidate{g: g, ratio: ratio})
		}
	}
	if len(hits) == 0 {
		return nil
	}

	// Largest current size first, so the cap keeps the worst offenders.
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].g.CurrBytes > hits[j].g.CurrBytes
	})
	if len(hits) > zoneRunawayMaxPerCycle {
		hits = hits[:zoneRunawayMaxPerCycle]
	}

	alerts := make([]Alert, 0, len(hits))
	for _, c := range hits {
		alerts = append(alerts, buildZoneRunawayAlert(c.g, c.ratio))
	}
	return alerts
}

func buildZoneRunawayAlert(g storage.ZoneGrowth, ratio float64) Alert {
	ratioStr := "?"
	if g.HasPrior && g.PriorBytes > 0 {
		ratioStr = fmt.Sprintf("%.1f", ratio)
	}

	var body strings.Builder
	fmt.Fprintf(&body, "Kernel zone %q is growing in a pattern that matches the runaway-allocation precursor to a watchdog-timeout panic.\n\n", g.Name)
	fmt.Fprintf(&body, "Zone:        %s\n", g.Name)
	fmt.Fprintf(&body, "Current:     %s (%d bytes) at %s\n", fmtBytes(g.CurrBytes), g.CurrBytes, g.CurrTime)
	if g.HasPrior {
		fmt.Fprintf(&body, "1h ago:      %s (%d bytes) at %s\n", fmtBytes(g.PriorBytes), g.PriorBytes, g.PriorTime)
		fmt.Fprintf(&body, "Ratio:       %sx in ~%s\n", ratioStr, zoneGrowthWindow)
	} else {
		fmt.Fprintf(&body, "1h ago:      no prior sample\n")
	}
	fmt.Fprintf(&body, "elem_size:   %d bytes\n", g.ElemSize)
	fmt.Fprintf(&body, "in_use:      %d elements\n\n", g.InUse)
	fmt.Fprintf(&body, "See the dashboard for the zone time series: http://localhost:9847\n")

	return Alert{
		Kind:    "zone-runaway:" + g.Name,
		Value:   float64(g.CurrBytes),
		Subject: fmt.Sprintf("[watchdog] zone %s at %s (%sx in 1h)", g.Name, fmtBytes(g.CurrBytes), ratioStr),
		Body:    body.String(),
	}
}

func buildBody(headline string, s storage.SystemSample, procs []storage.ProcessSample, zones []storage.ZoneSample) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", headline)
	fmt.Fprintf(&b, "Current sample (%s):\n", s.Timestamp)
	fmt.Fprintf(&b, "  mem_pressure:     %d%%\n", s.MemPressure)
	fmt.Fprintf(&b, "  swap_used:        %.2f GB\n", s.SwapUsedGB)
	fmt.Fprintf(&b, "  load_1/5/15:      %.2f / %.2f / %.2f\n", s.Load1, s.Load5, s.Load15)
	fmt.Fprintf(&b, "  swapouts:         %d\n", s.Swapouts)
	fmt.Fprintf(&b, "  compressor_pages: %d\n\n", s.CompressorPages)

	if len(procs) > 0 {
		fmt.Fprintln(&b, "Top processes by RSS:")
		n := 5
		if len(procs) < n {
			n = len(procs)
		}
		for _, p := range procs[:n] {
			fmt.Fprintf(&b, "  %-25s %5d MB\n", p.Name, p.RSSMB)
		}
		fmt.Fprintln(&b)
	}

	if len(zones) > 0 {
		fmt.Fprintln(&b, "Top kernel zones (est):")
		n := 5
		if len(zones) < n {
			n = len(zones)
		}
		for _, z := range zones[:n] {
			fmt.Fprintf(&b, "  %-30s %s\n", z.Name, fmtBytes(z.EstBytes))
		}
		fmt.Fprintln(&b)
	}

	fmt.Fprintln(&b, "Dashboard: http://localhost:9847")
	return b.String()
}

func fmtBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// Send routes the alert to whichever surface the user is most likely to
// see RIGHT NOW. When the display is on, a macOS notification banner is
// cheaper to act on than an email — the user is at the desk. When the
// display is asleep, the user isn't watching the menubar, so we fall
// back to msmtp so the alert sits in their inbox for the next time they
// check. The two paths share the same Alert subject + body.
func Send(a Alert) error {
	if displayIsAwake() {
		return sendNotification(a)
	}
	return sendEmail(a)
}

// displayIsAwake checks the current power state of the main display.
// `pmset -g powerstate IODisplayWrangler` returns the IOKit power state
// of the display wrangler — 4 = on, 0..3 = various sleep / dimming
// states. Defaults to "awake" on parse failure so we don't silently
// drop alerts into the email void when the user is probably looking
// at the menubar.
func displayIsAwake() bool {
	out, err := exec.Command("/usr/bin/pmset", "-g", "powerstate", "IODisplayWrangler").Output()
	if err != nil {
		return true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "Current Power State:") {
			f := strings.Fields(line)
			if len(f) >= 4 && f[len(f)-1] == "4" {
				return true
			}
			return false
		}
	}
	return true
}

// sendNotification fires a native macOS notification via osascript. The
// banner only carries the subject + a one-line summary — the full alert
// body still lands in the email path when the screen sleeps, so we
// don't try to cram everything into the banner.
func sendNotification(a Alert) error {
	headline := a.Subject
	summary := ""
	for _, l := range strings.SplitN(a.Body, "\n", 2) {
		summary = strings.TrimSpace(l)
		break
	}
	if len(summary) > 200 {
		summary = summary[:197] + "…"
	}
	headline = strings.ReplaceAll(headline, `"`, `\"`)
	summary = strings.ReplaceAll(summary, `"`, `\"`)
	script := fmt.Sprintf(
		`display notification "%s" with title "%s" subtitle "macos-watchdog"`,
		summary, headline,
	)
	cmd := exec.Command("/usr/bin/osascript", "-e", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("osascript: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// sendEmail hands the alert to msmtp on stdin. Uses an absolute path because
// launchd's default PATH (/usr/bin:/bin:/usr/sbin:/sbin) doesn't include
// MacPorts — same gotcha the rmsync sender works around.
func sendEmail(a Alert) error {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "From: %s\r\n", sender)
	fmt.Fprintf(&buf, "To: %s\r\n", recipient)
	fmt.Fprintf(&buf, "Subject: %s\r\n", a.Subject)
	fmt.Fprintf(&buf, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	fmt.Fprintf(&buf, "Message-ID: <watchdog-%d@local>\r\n", time.Now().UnixNano())
	fmt.Fprintf(&buf, "\r\n")
	buf.WriteString(a.Body)

	cmd := exec.Command(msmtpBin, "-a", msmtpAccount, recipient)
	cmd.Stdin = &buf
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("msmtp: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}
