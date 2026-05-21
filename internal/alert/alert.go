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
	return fired
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

// Send hands the alert to msmtp on stdin. Uses an absolute path because
// launchd's default PATH (/usr/bin:/bin:/usr/sbin:/sbin) doesn't include
// MacPorts — same gotcha the rmsync sender works around.
func Send(a Alert) error {
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
