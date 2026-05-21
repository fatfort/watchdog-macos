package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/alert"
	"github.com/abaj8494/macos-watchdog/internal/collector"
	"github.com/abaj8494/macos-watchdog/internal/launchd"
	"github.com/abaj8494/macos-watchdog/internal/logs"
	"github.com/abaj8494/macos-watchdog/internal/storage"
	"github.com/spf13/cobra"
)

const (
	retentionDays = 30
	// agentLabelPrefix filters which user LaunchAgents the Agents tab surfaces.
	// Empty string would list every agent; we scope to the user's own.
	agentLabelPrefix = "com.aayushbajaj."
)

var summaryHours int

var rootCmd = &cobra.Command{
	Use:   "watchdog",
	Short: "macOS system health monitor and dashboard",
	Long:  `Monitor system health metrics (memory pressure, swap, load, per-process memory) and visualize trends to prevent kernel panics.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return showSummary()
	},
}

var collectCmd = &cobra.Command{
	Use:   "collect",
	Short: "Collect a system health sample (run by launchd every 5min)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCollect()
	},
}

var viewCmd = &cobra.Command{
	Use:     "view",
	Aliases: []string{"v", "dashboard"},
	Short:   "Open health dashboard in browser",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runView()
	},
}

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Start a tiny local server for dashboard refresh (port 9847)",
	RunE: func(cmd *cobra.Command, args []string) error {
		return runServe()
	},
}

var summaryCmd = &cobra.Command{
	Use:   "summary",
	Short: "Show CLI health summary",
	RunE: func(cmd *cobra.Command, args []string) error {
		return showSummary()
	},
}

func init() {
	summaryCmd.Flags().IntVarP(&summaryHours, "hours", "H", 4, "Lookback window in hours")
	rootCmd.Flags().IntVarP(&summaryHours, "hours", "H", 4, "Lookback window in hours")

	rootCmd.AddCommand(collectCmd)
	rootCmd.AddCommand(viewCmd)
	rootCmd.AddCommand(summaryCmd)
	rootCmd.AddCommand(serveCmd)
	rootCmd.AddCommand(menubarCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCollect() error {
	store, err := storage.New()
	if err != nil {
		return fmt.Errorf("failed to open storage: %w", err)
	}
	defer store.Close()

	result, err := collector.Collect()
	if err != nil {
		return fmt.Errorf("failed to collect: %w", err)
	}

	sampleID, err := store.InsertSystemSample(result.System)
	if err != nil {
		return fmt.Errorf("failed to insert system sample: %w", err)
	}

	if err := store.InsertProcessSamples(sampleID, result.Processes); err != nil {
		return fmt.Errorf("failed to insert process samples: %w", err)
	}

	if err := store.InsertZoneSamples(sampleID, result.Zones); err != nil {
		return fmt.Errorf("failed to insert zone samples: %w", err)
	}

	// Write backward-compatible text log
	logDir, err := storage.GetLogDir()
	if err == nil {
		logPath := filepath.Join(logDir, "health.log")
		f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err == nil {
			fmt.Fprintln(f, result.LogLine)
			f.Close()
		}
	}

	// Evaluate threshold alerts. Best-effort — never block the collect on
	// mail-send failures, but log them so they show up in launchd-stderr.log.
	if recent, err := store.GetSystemTimeSeries(2); err == nil {
		for _, a := range alert.Evaluate(store, recent, result.Processes, result.Zones) {
			if err := alert.Send(a); err != nil {
				fmt.Fprintf(os.Stderr, "alert send (%s): %v\n", a.Kind, err)
				continue
			}
			if err := store.RecordAlertSent(a.Kind, a.Value); err != nil {
				fmt.Fprintf(os.Stderr, "alert record (%s): %v\n", a.Kind, err)
			}
		}
	}

	// Prune old data
	if err := store.Prune(retentionDays); err != nil {
		fmt.Fprintf(os.Stderr, "warning: prune failed: %v\n", err)
	}

	return nil
}

func runView() error {
	store, err := storage.New()
	if err != nil {
		return fmt.Errorf("failed to open storage: %w", err)
	}
	defer store.Close()

	htmlPath, err := generateChartsHTML(store)
	if err != nil {
		return fmt.Errorf("failed to generate charts: %w", err)
	}

	fmt.Printf("Opening dashboard: %s\n", htmlPath)
	return exec.Command("open", htmlPath).Start()
}

func showSummary() error {
	store, err := storage.New()
	if err != nil {
		return fmt.Errorf("failed to open storage: %w", err)
	}
	defer store.Close()

	stats, err := store.GetSummaryStats(summaryHours)
	if err != nil {
		return fmt.Errorf("failed to get stats: %w", err)
	}

	if stats.SampleCount == 0 {
		fmt.Println("No data yet. Run 'watchdog collect' first.")
		return nil
	}

	fmt.Printf("=== System Health Summary (last %dh) ===\n\n", summaryHours)
	fmt.Printf("Samples: %d (every 5min)\n\n", stats.SampleCount)
	fmt.Printf("Load Average (1min):   avg=%.1f  max=%.1f\n", stats.AvgLoad, stats.MaxLoad)
	fmt.Printf("Memory Pressure:       avg=%d%%   max=%d%%\n", int(stats.AvgPressure), stats.MaxPressure)
	fmt.Printf("Swap Usage:            avg=%.1fGB max=%.1fGB\n", stats.AvgSwap, stats.MaxSwap)

	// Show process table
	table, err := store.GetProcessTable(summaryHours)
	if err == nil && len(table) > 0 {
		fmt.Printf("\n=== Top Processes ===\n")
		fmt.Printf("%-25s %8s %8s %8s %8s\n", "Name", "Current", "Peak", "Avg RSS", "Avg CPU")
		fmt.Printf("%-25s %8s %8s %8s %8s\n", "----", "-------", "----", "-------", "-------")
		for _, r := range table {
			currentStr := "-"
			if r.CurrentRSS > 0 {
				currentStr = fmt.Sprintf("%dMB", r.CurrentRSS)
			}
			fmt.Printf("%-25s %8s %7dMB %7.0fMB %6.1f%%\n",
				truncate(r.Name, 25), currentStr, r.PeakRSS, r.AvgRSS, r.AvgCPU)
		}
	}

	zones, err := store.GetZoneTable(summaryHours, 15)
	if err == nil && len(zones) > 0 {
		fmt.Printf("\n=== Top Kernel Zones (estimated, elem_size × inuse) ===\n")
		fmt.Printf("%-30s %10s %10s %10s %8s\n", "Zone", "Current", "Peak", "Avg", "Elem")
		fmt.Printf("%-30s %10s %10s %10s %8s\n", "----", "-------", "----", "---", "----")
		for _, z := range zones {
			fmt.Printf("%-30s %10s %10s %10s %7dB\n",
				truncate(z.Name, 30),
				formatBytes(z.CurrentBytes),
				formatBytes(z.PeakBytes),
				formatBytes(int64(z.AvgBytes)),
				z.ElemSize,
			)
		}
	}

	return nil
}

func formatBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1fGB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0fKB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

func runServe() error {
	// Generate initial dashboard
	regenerate := func() error {
		store, err := storage.New()
		if err != nil {
			return err
		}
		defer store.Close()
		_, err = generateChartsHTML(store)
		return err
	}

	if err := regenerate(); err != nil {
		return fmt.Errorf("initial generate: %w", err)
	}

	logDir, err := storage.GetLogDir()
	if err != nil {
		return err
	}
	htmlPath := filepath.Join(logDir, "dashboard.html")

	// Serve the dashboard HTML at root
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, htmlPath)
	})

	// Refresh endpoint: collect + regenerate, then redirect to /
	http.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		if err := runCollect(); err != nil {
			fmt.Fprintf(os.Stderr, "collect error: %v\n", err)
		}
		if err := regenerate(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		fmt.Printf("[%s] Dashboard refreshed\n", time.Now().Format("15:04:05"))
		http.Redirect(w, r, "/", http.StatusSeeOther)
	})

	http.HandleFunc("/api/ssh-log", func(w http.ResponseWriter, r *http.Request) {
		hours := parseHours(r, 24)
		out, err := logs.GetSSHLog(hours)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, out)
	})

	http.HandleFunc("/api/crontabs", func(w http.ResponseWriter, r *http.Request) {
		entries, err := logs.GetCrontabEntries()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(entries)
	})

	http.HandleFunc("/api/launch-agents", func(w http.ResponseWriter, r *http.Request) {
		prefix := r.URL.Query().Get("prefix")
		if prefix == "" {
			prefix = agentLabelPrefix
		}
		if prefix == "*" {
			prefix = ""
		}
		agents, err := launchd.ListAgents(prefix)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(agents)
	})

	http.HandleFunc("/api/launch-agent-log", func(w http.ResponseWriter, r *http.Request) {
		label := r.URL.Query().Get("label")
		if label == "" {
			http.Error(w, "missing label", 400)
			return
		}
		a, err := launchd.FindAgent(label, agentLabelPrefix)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		out, err := launchd.TailLog(a.LogFile)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, out)
	})

	http.HandleFunc("/api/cron-log", func(w http.ResponseWriter, r *http.Request) {
		idx, err := strconv.Atoi(r.URL.Query().Get("idx"))
		if err != nil {
			http.Error(w, "bad idx", 400)
			return
		}
		hours := parseHours(r, 24)
		entries, err := logs.GetCrontabEntries()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if idx < 0 || idx >= len(entries) {
			http.Error(w, "idx out of range", 400)
			return
		}
		out, err := logs.GetCronLog(entries[idx], hours)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, out)
	})

	fmt.Println("Watchdog dashboard: http://localhost:9847")
	fmt.Println("Press Ctrl+C to stop")

	// Open in browser
	_ = exec.Command("open", "http://localhost:9847").Start()

	return http.ListenAndServe(":9847", nil)
}

func parseHours(r *http.Request, def int) int {
	q := r.URL.Query().Get("hours")
	if q == "" {
		return def
	}
	n, err := strconv.Atoi(q)
	if err != nil || n <= 0 || n > 24*30 {
		return def
	}
	return n
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
