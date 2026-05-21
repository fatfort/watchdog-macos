package main

import (
	"fmt"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/collector"
	"github.com/abaj8494/macos-watchdog/internal/storage"
	"fyne.io/systray"
	"github.com/spf13/cobra"
)

// Menubar refresh cadence. SMC reads are cheap so we poll every 2s; the
// today's-network query hits sqlite and is comparatively heavy, so it's
// every 30s. The title alternates between the two readouts every 5s, which
// is the same cycle iStatistica uses by default.
const (
	smcRefreshInterval = 2 * time.Second
	netRefreshInterval = 30 * time.Second
	titleCycleInterval = 5 * time.Second
	dashboardURL       = "http://localhost:9847"
)

var menubarCmd = &cobra.Command{
	Use:   "menubar",
	Short: "Run a menubar app that shows live temp/fan/network stats",
	Long: `Persistent foreground process that paints today's network throughput,
CPU temperature, and fan RPM into the macOS menubar. Designed to be driven
by its own LaunchAgent; do not run multiple instances.

The menubar title cycles every ~5s between:
  1. ↓X.XX ↑Y.YY    today's network in GB (rx then tx)
  2. NN°C  NNNNrpm  current temperature + max fan speed`,
	RunE: func(cmd *cobra.Command, args []string) error {
		systray.Run(menubarOnReady, menubarOnExit)
		return nil
	},
}

// menubarState holds the values painted into the title and dropdown. All
// fields are read by the systray main thread and written by the refresh
// goroutines, so we synchronise via a mutex; the title cycle reads under
// the same lock to ensure a consistent snapshot.
type menubarState struct {
	mu         sync.RWMutex
	tempC      float64
	fanRPM     int
	netRxToday int64
	netTxToday int64
	// lastErr is the most recent non-fatal error from a refresh loop. Shown
	// as a disabled menu item so the user can see when SMC reads stop
	// working without needing to open a terminal.
	lastErr string
}

var (
	state          menubarState
	titlePhase     atomic.Int32 // 0 = network, 1 = thermal
	menuQuit       *systray.MenuItem
	menuDashboard  *systray.MenuItem
	menuTitle      *systray.MenuItem
	menuTempLine   *systray.MenuItem
	menuFanLine    *systray.MenuItem
	menuRxLine     *systray.MenuItem
	menuTxLine     *systray.MenuItem
	menuStatusLine *systray.MenuItem
)

func menubarOnReady() {
	// Empty icon — rely on the title text. systray expects PNG bytes; passing
	// nil leaves the icon slot empty so the title is the only visible piece,
	// which matches the iStat "text-only" look.
	systray.SetIcon([]byte{})
	systray.SetTitle("watchdog…")
	systray.SetTooltip("macos-watchdog")

	menuTitle = systray.AddMenuItem("macos-watchdog", "")
	menuTitle.Disable()
	systray.AddSeparator()

	menuRxLine = systray.AddMenuItem("Net ↓ —", "Today's received bytes (since local midnight)")
	menuRxLine.Disable()
	menuTxLine = systray.AddMenuItem("Net ↑ —", "Today's transmitted bytes (since local midnight)")
	menuTxLine.Disable()
	menuTempLine = systray.AddMenuItem("Temp —", "Current CPU die temperature")
	menuTempLine.Disable()
	menuFanLine = systray.AddMenuItem("Fan —", "Current fan speed")
	menuFanLine.Disable()
	systray.AddSeparator()

	menuStatusLine = systray.AddMenuItem("status: starting…", "")
	menuStatusLine.Disable()
	systray.AddSeparator()

	menuDashboard = systray.AddMenuItem("Open Dashboard", "Open http://localhost:9847 in browser")
	menuQuit = systray.AddMenuItem("Quit", "Stop the menubar app")

	go menubarTitleLoop()
	go menubarSMCLoop()
	go menubarNetworkLoop()
	go menubarEventLoop()
}

func menubarOnExit() {}

// menubarTitleLoop alternates the menubar title text between the two
// readouts at titleCycleInterval. iStat does the same — the limited
// menubar real estate makes packing both onto one line unreadable.
func menubarTitleLoop() {
	// Paint immediately so the menubar doesn't sit on "watchdog…" for 5s.
	paintTitle()
	t := time.NewTicker(titleCycleInterval)
	defer t.Stop()
	for range t.C {
		titlePhase.Store(1 - titlePhase.Load())
		paintTitle()
	}
}

func paintTitle() {
	state.mu.RLock()
	tempC := state.tempC
	fanRPM := state.fanRPM
	rx := state.netRxToday
	tx := state.netTxToday
	state.mu.RUnlock()

	var title string
	switch titlePhase.Load() {
	case 0:
		title = fmt.Sprintf("↓%s ↑%s", formatBytesShort(rx), formatBytesShort(tx))
	default:
		title = fmt.Sprintf("%d°C %drpm", int(tempC+0.5), fanRPM)
	}
	systray.SetTitle(title)
}

func menubarSMCLoop() {
	refreshSMC()
	t := time.NewTicker(smcRefreshInterval)
	defer t.Stop()
	for range t.C {
		refreshSMC()
	}
}

func refreshSMC() {
	temp, fan, err := collector.ReadThermal()
	state.mu.Lock()
	if err != nil {
		state.lastErr = fmt.Sprintf("smc: %v", err)
	} else {
		state.tempC = temp
		state.fanRPM = fan
	}
	state.mu.Unlock()
	updateDropdown()
}

func menubarNetworkLoop() {
	refreshNetwork()
	t := time.NewTicker(netRefreshInterval)
	defer t.Stop()
	for range t.C {
		refreshNetwork()
	}
}

func refreshNetwork() {
	store, err := storage.New()
	if err != nil {
		state.mu.Lock()
		state.lastErr = fmt.Sprintf("storage: %v", err)
		state.mu.Unlock()
		return
	}
	defer store.Close()
	rx, tx, err := store.GetTodayNetworkUsage()
	state.mu.Lock()
	if err != nil {
		state.lastErr = fmt.Sprintf("net: %v", err)
	} else {
		state.netRxToday = rx
		state.netTxToday = tx
	}
	state.mu.Unlock()
	updateDropdown()
}

// updateDropdown paints the current snapshot into the dropdown items. Called
// after each refresh so the open menu reflects the latest sample without
// needing the user to close & reopen it.
func updateDropdown() {
	state.mu.RLock()
	defer state.mu.RUnlock()
	menuRxLine.SetTitle(fmt.Sprintf("Net ↓ %s today", formatBytesLong(state.netRxToday)))
	menuTxLine.SetTitle(fmt.Sprintf("Net ↑ %s today", formatBytesLong(state.netTxToday)))
	menuTempLine.SetTitle(fmt.Sprintf("Temp %.1f°C", state.tempC))
	menuFanLine.SetTitle(fmt.Sprintf("Fan %d rpm", state.fanRPM))
	if state.lastErr == "" {
		menuStatusLine.SetTitle("status: ok")
	} else {
		menuStatusLine.SetTitle("status: " + state.lastErr)
	}
}

// menubarEventLoop watches the click channels on the actionable menu items
// and dispatches. systray.Run blocks the goroutine that called it, so we
// process clicks on a separate goroutine.
func menubarEventLoop() {
	for {
		select {
		case <-menuDashboard.ClickedCh:
			_ = exec.Command("open", dashboardURL).Start()
		case <-menuQuit.ClickedCh:
			systray.Quit()
			return
		}
	}
}

// formatBytesShort is the compact GB-only formatter used in the menubar
// title (e.g. "3.75"). Matches iStat's display: never longer than 4-5 chars.
func formatBytesShort(n int64) string {
	gb := float64(n) / (1 << 30)
	switch {
	case gb >= 100:
		return fmt.Sprintf("%.0f GB", gb)
	case gb >= 10:
		return fmt.Sprintf("%.1f GB", gb)
	case gb >= 1:
		return fmt.Sprintf("%.2f GB", gb)
	default:
		mb := float64(n) / (1 << 20)
		if mb >= 1 {
			return fmt.Sprintf("%.0f MB", mb)
		}
		return "0"
	}
}

// formatBytesLong is the verbose dropdown formatter ("3.75 GB" / "412 MB").
func formatBytesLong(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
