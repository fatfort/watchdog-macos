package main

import (
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/collector"
	"github.com/abaj8494/macos-watchdog/internal/storage"
	"github.com/spf13/cobra"
)

const retentionDays = 30

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

	return nil
}

func runServe() error {
	fmt.Println("Watchdog refresh server on http://localhost:9847")
	fmt.Println("Press Ctrl+C to stop")

	http.HandleFunc("/refresh", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")

		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}

		// Collect fresh sample
		if err := runCollect(); err != nil {
			fmt.Fprintf(os.Stderr, "collect error: %v\n", err)
		}

		// Regenerate HTML
		store, err := storage.New()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer store.Close()

		if _, err := generateChartsHTML(store); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "ok")
		fmt.Printf("[%s] Dashboard refreshed\n", time.Now().Format("15:04:05"))
	})

	return http.ListenAndServe(":9847", nil)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
