package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
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

// chartBarsIcon returns a 22×22 template PNG of four ascending bars — a
// generic "monitoring" mark that macOS recolors to match the menubar
// appearance (white on dark mode, black on light, blue when clicked).
// fyne.io/systray panics on an empty buffer, so any valid template works
// and this one reads as "stats" at a glance.
func chartBarsIcon() []byte {
	const size = 22
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	mark := color.NRGBA{0, 0, 0, 255} // alpha mask — macOS supplies the colour
	heights := []int{6, 10, 14, 18}
	barW, gap := 3, 2
	totalW := len(heights)*barW + (len(heights)-1)*gap
	startX := (size - totalW) / 2
	baseY := size - 2
	for i, h := range heights {
		x0 := startX + i*(barW+gap)
		y0 := baseY - h
		for y := y0; y < baseY; y++ {
			for x := x0; x < x0+barW; x++ {
				img.Set(x, y, mark)
			}
		}
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
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
	// Template icon: macOS recolors to match menubar appearance.
	icon := chartBarsIcon()
	systray.SetTemplateIcon(icon, icon)
	systray.SetTitle(thinSpace + "…")
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

	// Shrink the NSStatusBarButton font so two stacked lines fit inside the
	// menubar's fixed height. fyne.io/systray's default ~14pt overflows; ~9pt
	// gives us a tight but readable two-row layout. Empty title means
	// "configure the cell only — paintTitle will supply the actual text".
	setMenubarFontSize(9, "")

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

// paintTitle assembles every readout into a two-line title that mirrors the
// iStatistica Pro layout: a thermometer prefix on the thermal row and a
// globe prefix on the network row, both rendered in the menubar font.
//
//	🌡 NN° NNNNrpm
//	🌐 ↓X.XX ↑Y.YY
func paintTitle() {
	state.mu.RLock()
	tempC := state.tempC
	fanRPM := state.fanRPM
	rx := state.netRxToday
	tx := state.netTxToday
	state.mu.RUnlock()

	thermal := fmt.Sprintf("🌡 %d° %drpm", int(tempC+0.5), fanRPM)
	net := fmt.Sprintf("🌐 ↓%s ↑%s", formatTitleBytes(rx), formatTitleBytes(tx))
	title := thermal + "\n" + net
	// systray.SetTitle goes through NSButton.title which strips newlines and
	// flattens emoji to plain text; route the real paint through the cgo
	// helper that sets attributedTitle with paragraph spacing + a font that
	// renders the emoji as the inline iStat-style icons.
	systray.SetTitle(title) // keep fyne's internal cache in sync
	setMenubarFontSize(9, title)
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

// formatTitleBytes is the compact formatter used in the menubar title.
// Keeps each value to 4-5 characters max so the full title fits in the
// menubar even when Bartender pushes it left.
func formatTitleBytes(n int64) string {
	gb := float64(n) / (1 << 30)
	switch {
	case gb >= 100:
		return fmt.Sprintf("%.0fG", gb)
	case gb >= 10:
		return fmt.Sprintf("%.1fG", gb)
	case gb >= 1:
		return fmt.Sprintf("%.2fG", gb)
	}
	mb := float64(n) / (1 << 20)
	if mb >= 1 {
		return fmt.Sprintf("%.0fM", mb)
	}
	return "0"
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
