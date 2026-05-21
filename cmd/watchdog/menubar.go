package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os/exec"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/abaj8494/macos-watchdog/internal/collector"
	"github.com/abaj8494/macos-watchdog/internal/storage"
	"github.com/abaj8494/macos-watchdog/internal/typtel"
	"github.com/spf13/cobra"
)

// transparentIcon returns the smallest valid PNG that fyne.io/systray
// will accept. The primary status item's icon slot ends up blank — the
// visible icons come from the SF Symbol attachments inside each widget's
// attributedTitle, which is the iStat look the user asked for.
func transparentIcon() []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewNRGBA(image.Rect(0, 0, 1, 1)))
	return buf.Bytes()
}

const (
	// SMC is cheap (cgo + IOKit, ~ms); poll every 2s for a live feel.
	smcRefreshInterval = 2 * time.Second
	// Today's-network delta hits sqlite — heavier, poll every 30s.
	netRefreshInterval = 30 * time.Second
	// Title repaint cadence; cheap because it reads cached state under RLock.
	titleRepaintInterval = 2 * time.Second
	dashboardURL         = "http://localhost:9847"
	// thinSpace gives the title room to breathe without looking gappy.
	thinSpace = " "
	// midDot separates logical groups (thermal vs network).
	midDot = " · "
	// Text font for the menubar values. iStatistica's reference shows the
	// numbers at roughly half the menubar height per row — 11pt with the
	// tight line spacing patched into the paragraph style fits cleanly.
	menubarFontSize = 11.0
	// SF Symbol point size for the left icon. Slightly larger than the
	// text so the icon visually dominates the cell the way iStat's does.
	menubarIconSize = 16.0
)

var menubarCmd = &cobra.Command{
	Use:   "menubar",
	Short: "Run a menubar app that shows live temp/fan/network stats",
	Long: `Persistent foreground process that paints today's network throughput,
CPU temperature, and fan RPM into the macOS menubar. Designed to be driven
by its own LaunchAgent; do not run multiple instances.

Menubar title format (all values on one line, refreshes every 2s):
  NN° · ↓X.XX  ↑Y.YY

Fan RPM and full readouts live in the dropdown to keep the title compact.`,
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
	menuQuit       *systray.MenuItem
	menuDashboard  *systray.MenuItem
	menuTitle      *systray.MenuItem
	menuTempLine   *systray.MenuItem
	menuFanLine    *systray.MenuItem
	menuRxLine     *systray.MenuItem
	menuTxLine     *systray.MenuItem
	menuTyptelLine *systray.MenuItem // hidden unless `typtel` is on PATH
	menuStatusLine *systray.MenuItem
)

func menubarOnReady() {
	// fyne.io/systray requires *some* initial icon (NSImage decode panics
	// on an empty buffer); the cgo helper later swaps button.image for
	// the real SF Symbol once paintTitle runs.
	icon := transparentIcon()
	systray.SetTemplateIcon(icon, icon)
	systray.SetTitle("")
	systray.SetTooltip("macos-watchdog — live system stats")

	menuTitle = systray.AddMenuItem("macos-watchdog", "")
	menuTitle.Disable()
	systray.AddSeparator()

	menuTempLine = systray.AddMenuItem("🌡  Temp —", "Current CPU die temperature")
	menuTempLine.Disable()
	menuFanLine = systray.AddMenuItem("❉  Fan —", "Current max fan speed")
	menuFanLine.Disable()
	systray.AddSeparator()
	menuRxLine = systray.AddMenuItem("↓  Net —", "Today's received bytes (since local midnight)")
	menuRxLine.Disable()
	menuTxLine = systray.AddMenuItem("↑  Net —", "Today's transmitted bytes (since local midnight)")
	menuTxLine.Disable()
	systray.AddSeparator()

	// Optional typtel cross-link — hidden when typing-telemetry isn't installed.
	menuTyptelLine = systray.AddMenuItem("⌨  Typing today —", "Today's typing-telemetry stats (requires typtel on PATH)")
	menuTyptelLine.Disable()
	menuTyptelLine.Hide()
	systray.AddSeparator()

	menuStatusLine = systray.AddMenuItem("◌  starting…", "")
	menuStatusLine.Disable()
	systray.AddSeparator()

	menuDashboard = systray.AddMenuItem("Open Dashboard…", "Open http://localhost:9847 in browser")
	menuQuit = systray.AddMenuItem("Quit", "Stop the menubar app")

	go menubarTitleLoop()
	go menubarSMCLoop()
	go menubarNetworkLoop()
	go menubarTyptelLoop()
	go menubarEventLoop()
}

// menubarTyptelLoop refreshes the optional typing-telemetry dropdown line
// every 60s. typtel reads its own SQLite so the call is cheap, but
// keystrokes don't change that quickly — minute cadence is fine.
func menubarTyptelLoop() {
	refreshTyptel()
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for range t.C {
		refreshTyptel()
	}
}

func menubarOnExit() {}

// menubarTitleLoop repaints the menubar title every titleRepaintInterval.
// Cheap because paintTitle reads cached state under an RLock — the SMC and
// network loops do the actual work; this just refreshes the text in case a
// repaint was missed between background updates.
func menubarTitleLoop() {
	paintTitle()
	t := time.NewTicker(titleRepaintInterval)
	defer t.Stop()
	for range t.C {
		paintTitle()
	}
}

// paintTitle bakes the whole two-widget pill into one image set as
// button.image — matches iStatistica's layout with big icons and
// stacked rows, all within ONE NSStatusItem (one pill) at the system
// menubar height:
//
//	[🌡] 49°C   [🌐] ↓41 MB
//	    0 rpm        ↑28 MB
func paintTitle() {
	state.mu.RLock()
	tempC := state.tempC
	fanRPM := state.fanRPM
	rx := state.netRxToday
	tx := state.netTxToday
	state.mu.RUnlock()

	tempRow1 := fmt.Sprintf("%d°C", int(tempC+0.5))
	tempRow2 := fmt.Sprintf("%d rpm", fanRPM)
	netRow1 := "↓" + formatTitleBytes(rx)
	netRow2 := "↑" + formatTitleBytes(tx)

	// Plain-text fallback for fyne's cache; the visible content comes
	// from the rendered template image below.
	systray.SetTitle(tempRow1)

	setMenubarPill(menubarFontSize, menubarIconSize,
		"thermometer", tempRow1, tempRow2,
		"globe", netRow1, netRow2,
	)
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
	menuTempLine.SetTitle(fmt.Sprintf("🌡  %.1f °C", state.tempC))
	menuFanLine.SetTitle(fmt.Sprintf("❉  %d rpm", state.fanRPM))
	menuRxLine.SetTitle(fmt.Sprintf("↓  %s received today", formatBytesLong(state.netRxToday)))
	menuTxLine.SetTitle(fmt.Sprintf("↑  %s sent today", formatBytesLong(state.netTxToday)))
	if state.lastErr == "" {
		menuStatusLine.SetTitle("●  status: ok")
	} else {
		menuStatusLine.SetTitle("✕  " + state.lastErr)
	}
}

// refreshTyptel polls typing-telemetry (optional dep). Shows the dropdown
// item only when typtel is installed and responsive; otherwise hidden so the
// menu stays clean for users who don't run the companion app.
func refreshTyptel() {
	stats, ok, err := typtel.Fetch()
	if !ok || err != nil {
		menuTyptelLine.Hide()
		return
	}
	menuTyptelLine.SetTitle(fmt.Sprintf("⌨  %d keys · %d clicks today",
		stats.Keystrokes, stats.MouseClicks))
	menuTyptelLine.Show()
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

// formatTitleBytes matches iStatistica's menubar formatting: a space
// before the unit ("41 MB" / "1.18 GB"), tightening decimals as the
// value grows. The widget has its own icon column so we can spend a
// few more characters on the number than the prior compact formatter.
func formatTitleBytes(n int64) string {
	gb := float64(n) / (1 << 30)
	switch {
	case gb >= 100:
		return fmt.Sprintf("%.0f GB", gb)
	case gb >= 10:
		return fmt.Sprintf("%.1f GB", gb)
	case gb >= 1:
		return fmt.Sprintf("%.2f GB", gb)
	}
	mb := float64(n) / (1 << 20)
	if mb >= 1 {
		return fmt.Sprintf("%.0f MB", mb)
	}
	return "0 MB"
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
