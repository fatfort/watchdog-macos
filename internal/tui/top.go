// Package tui implements the `watchdog top` interactive dashboard.
//
// It is a read-only viewer over the SQLite database that `watchdog collect`
// writes every 5 minutes. The TUI never mutates system state and never
// triggers a collection — it just reflects whatever the latest samples are.
package tui

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/storage"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

const (
	refreshInterval   = 2 * time.Second
	sparklineHours    = 1
	processRowLimit   = 15
	zoneRowLimit      = 15
	headerHoursWindow = 1 // for header-bar averages
)

// focus indicates which table currently owns the cursor / scroll keys.
type focus int

const (
	focusProcs focus = iota
	focusZones
)

// tickMsg is emitted by the refresh timer.
type tickMsg time.Time

// dataMsg carries a fresh snapshot from the database back into Update.
type dataMsg struct {
	latest    *storage.SystemSample
	series    []storage.SystemSample
	procs     []storage.ProcessTableRow
	zones     []storage.ZoneTableRow
	uptime    string
	fetchedAt time.Time
	err       error
}

// Model is the Bubble Tea model for `watchdog top`.
type Model struct {
	store *storage.Store

	width  int
	height int

	procs   table.Model
	zones   table.Model
	current focus

	latest    *storage.SystemSample
	series    []storage.SystemSample
	uptime    string
	fetchedAt time.Time
	loadErr   error
}

// Run builds and runs the TUI. It owns the storage handle for the duration of
// the program; the caller does not need to open one.
func Run() error {
	store, err := storage.New()
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()

	m := newModel(store)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

func newModel(store *storage.Store) Model {
	procCols := []table.Column{
		{Title: "Name", Width: 25},
		{Title: "Current", Width: 9},
		{Title: "Peak", Width: 9},
		{Title: "Avg RSS", Width: 9},
		{Title: "Avg CPU", Width: 9},
	}
	zoneCols := []table.Column{
		{Title: "Zone", Width: 30},
		{Title: "Current", Width: 10},
		{Title: "Peak", Width: 10},
		{Title: "Avg", Width: 10},
		{Title: "Elem", Width: 8},
	}

	// Monochrome-friendly styling: bold headers, inverted selection. No colors
	// beyond default + reverse, which mirrors the rest of the project's
	// terminal output.
	tableStyles := table.DefaultStyles()
	tableStyles.Header = tableStyles.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(lipgloss.Color("240")).
		BorderBottom(true).
		Bold(true)
	tableStyles.Selected = tableStyles.Selected.
		Foreground(lipgloss.Color("0")).
		Background(lipgloss.Color("7")).
		Bold(false)

	procT := table.New(
		table.WithColumns(procCols),
		table.WithFocused(true),
		table.WithHeight(processRowLimit),
	)
	procT.SetStyles(tableStyles)

	zoneT := table.New(
		table.WithColumns(zoneCols),
		table.WithFocused(false),
		table.WithHeight(zoneRowLimit),
	)
	zoneT.SetStyles(tableStyles)

	return Model{
		store:   store,
		procs:   procT,
		zones:   zoneT,
		current: focusProcs,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(loadCmd(m.store), tickCmd())
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.layout()
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return m, tea.Quit
		case "r":
			return m, loadCmd(m.store)
		case "tab", "shift+tab":
			m.toggleFocus()
			return m, nil
		}

	case tickMsg:
		return m, tea.Batch(loadCmd(m.store), tickCmd())

	case dataMsg:
		m.applyData(msg)
		return m, nil
	}

	// Forward unhandled messages (arrow keys, j/k, etc.) to the focused table.
	var cmd tea.Cmd
	if m.current == focusProcs {
		m.procs, cmd = m.procs.Update(msg)
	} else {
		m.zones, cmd = m.zones.Update(msg)
	}
	return m, cmd
}

func (m Model) View() string {
	if m.loadErr != nil {
		return fmt.Sprintf("watchdog top: failed to load data: %v\n\nPress q to quit.\n", m.loadErr)
	}
	if m.latest == nil {
		return "watchdog top: no samples yet. Run `watchdog collect` first.\n\nPress q to quit.\n"
	}

	var b strings.Builder
	b.WriteString(m.renderHeader())
	b.WriteString("\n")
	b.WriteString(m.renderSparkline())
	b.WriteString("\n\n")
	b.WriteString(sectionTitle("Top Processes (by avg RSS)", m.current == focusProcs))
	b.WriteString("\n")
	b.WriteString(m.procs.View())
	b.WriteString("\n\n")
	b.WriteString(sectionTitle("Top Kernel Zones (by est_bytes)", m.current == focusZones))
	b.WriteString("\n")
	b.WriteString(m.zones.View())
	b.WriteString("\n\n")
	b.WriteString(m.renderFooter())
	return b.String()
}

// renderHeader builds the single-line system status bar.
func (m Model) renderHeader() string {
	s := m.latest
	parts := []string{
		fmt.Sprintf("load %.2f/%.2f/%.2f", s.Load1, s.Load5, s.Load15),
		fmt.Sprintf("mem %d%%", s.MemPressure),
		fmt.Sprintf("swap %.1fGB", s.SwapUsedGB),
		fmt.Sprintf("uptime %s", m.uptime),
		fmt.Sprintf("sample %s", shortTime(s.Timestamp)),
	}
	bar := strings.Join(parts, "  |  ")
	title := lipgloss.NewStyle().Bold(true).Render("watchdog top")
	return title + "   " + bar
}

// renderSparkline draws an ASCII sparkline of mem_pressure over the last hour.
// We deliberately use plain ASCII (`.` for empty rows, `#` for filled) instead
// of the unicode block-glyphs so the output stays monochrome-friendly and
// matches the rest of the project. Two rows give us crude per-bar resolution.
func (m Model) renderSparkline() string {
	const height = 4
	label := fmt.Sprintf("mem_pressure (last %dh): ", sparklineHours)

	if len(m.series) == 0 {
		return label + "(no data)"
	}

	values := make([]int, len(m.series))
	for i, s := range m.series {
		values[i] = s.MemPressure
	}

	// Each sample is one column wide. mem_pressure is 0-100, scale to height.
	cols := len(values)
	if cols > 120 {
		// Down-sample by averaging into 120 buckets so the bar stays a sane
		// width on narrow terminals. We never go wider than ~120 chars even
		// on huge terminals — the rest of the row is just headroom.
		bucket := make([]int, 120)
		count := make([]int, 120)
		for i, v := range values {
			idx := i * 120 / cols
			bucket[idx] += v
			count[idx]++
		}
		values = bucket[:0]
		for i := 0; i < 120; i++ {
			if count[i] == 0 {
				values = append(values, 0)
				continue
			}
			values = append(values, bucket[i]/count[i])
		}
		cols = 120
	}

	var rows [height]strings.Builder
	for c := 0; c < cols; c++ {
		// Map 0..100 → 0..height. Round up so any nonzero pressure shows.
		filled := (values[c]*height + 99) / 100
		if filled > height {
			filled = height
		}
		for r := 0; r < height; r++ {
			// row 0 is the top
			if (height - r) <= filled {
				rows[r].WriteByte('#')
			} else {
				rows[r].WriteByte('.')
			}
		}
	}

	// Tag the leftmost row with the percent ceiling so the scale is obvious.
	minV, maxV := values[0], values[0]
	for _, v := range values {
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	scale := "100% | "
	zero := "  0% | "
	pad := strings.Repeat(" ", len(scale))

	var out strings.Builder
	out.WriteString(label)
	out.WriteString(fmt.Sprintf("min=%d%%  max=%d%%\n", minV, maxV))
	for r := 0; r < height; r++ {
		switch r {
		case 0:
			out.WriteString(scale)
		case height - 1:
			out.WriteString(zero)
		default:
			out.WriteString(pad)
		}
		out.WriteString(rows[r].String())
		out.WriteByte('\n')
	}
	return out.String()
}

func (m Model) renderFooter() string {
	style := lipgloss.NewStyle().Faint(true)
	keys := "[q] quit  [r] refresh  [tab] switch table  [j/k or arrows] scroll"
	stamp := "last refresh " + m.fetchedAt.Format("15:04:05") +
		"  (auto-refresh " + strconv.Itoa(int(refreshInterval.Seconds())) + "s)"
	return style.Render(keys + "  -  " + stamp)
}

func (m *Model) toggleFocus() {
	if m.current == focusProcs {
		m.current = focusZones
		m.procs.Blur()
		m.zones.Focus()
	} else {
		m.current = focusProcs
		m.zones.Blur()
		m.procs.Focus()
	}
}

// layout adjusts table heights so both fit on screen alongside the header,
// sparkline, and footer. Conservative — we'd rather lose a row than overflow.
func (m *Model) layout() {
	if m.height < 20 {
		return
	}
	// Reserve: 1 header + 6 sparkline + 4 section titles/blank + 2 footer = ~13
	available := m.height - 13
	if available < 10 {
		available = 10
	}
	// Split roughly 60/40 between procs and zones, capped at content size.
	procH := available * 6 / 10
	zoneH := available - procH
	if procH > processRowLimit+1 {
		procH = processRowLimit + 1
	}
	if zoneH > zoneRowLimit+1 {
		zoneH = zoneRowLimit + 1
	}
	if procH < 5 {
		procH = 5
	}
	if zoneH < 5 {
		zoneH = 5
	}
	m.procs.SetHeight(procH)
	m.zones.SetHeight(zoneH)
}

func (m *Model) applyData(msg dataMsg) {
	m.loadErr = msg.err
	if msg.err != nil {
		return
	}
	m.latest = msg.latest
	m.series = msg.series
	m.uptime = msg.uptime
	m.fetchedAt = msg.fetchedAt

	procRows := make([]table.Row, 0, len(msg.procs))
	limit := processRowLimit
	if len(msg.procs) < limit {
		limit = len(msg.procs)
	}
	for i := 0; i < limit; i++ {
		r := msg.procs[i]
		current := "-"
		if r.CurrentRSS > 0 {
			current = fmt.Sprintf("%dMB", r.CurrentRSS)
		}
		procRows = append(procRows, table.Row{
			truncate(r.Name, 25),
			current,
			fmt.Sprintf("%dMB", r.PeakRSS),
			fmt.Sprintf("%.0fMB", r.AvgRSS),
			fmt.Sprintf("%.1f%%", r.AvgCPU),
		})
	}
	m.procs.SetRows(procRows)

	zoneRows := make([]table.Row, 0, len(msg.zones))
	for _, z := range msg.zones {
		zoneRows = append(zoneRows, table.Row{
			truncate(z.Name, 30),
			formatBytes(z.CurrentBytes),
			formatBytes(z.PeakBytes),
			formatBytes(int64(z.AvgBytes)),
			fmt.Sprintf("%dB", z.ElemSize),
		})
	}
	m.zones.SetRows(zoneRows)
}

// --- commands & helpers --------------------------------------------------

func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

func loadCmd(store *storage.Store) tea.Cmd {
	return func() tea.Msg {
		msg := dataMsg{fetchedAt: time.Now()}
		latest, err := store.GetLatestSample()
		if err != nil {
			msg.err = fmt.Errorf("latest sample: %w", err)
			return msg
		}
		msg.latest = latest

		series, err := store.GetSystemTimeSeries(sparklineHours)
		if err != nil {
			msg.err = fmt.Errorf("series: %w", err)
			return msg
		}
		msg.series = series

		procs, err := store.GetProcessTable(headerHoursWindow)
		if err != nil {
			msg.err = fmt.Errorf("process table: %w", err)
			return msg
		}
		msg.procs = procs

		zones, err := store.GetZoneTable(headerHoursWindow, zoneRowLimit)
		if err != nil {
			msg.err = fmt.Errorf("zone table: %w", err)
			return msg
		}
		msg.zones = zones

		msg.uptime = readUptime()
		return msg
	}
}

// readUptime shells out to `uptime` and extracts the human-readable "up X" bit.
// Failures degrade silently — the header just shows "n/a". This is a read-only
// command (no -s flags etc.), so it stays within the project's observation-only
// contract.
func readUptime() string {
	out, err := exec.Command("uptime").Output()
	if err != nil {
		return "n/a"
	}
	s := string(out)
	// Typical macOS output: "10:12  up 3 days, 14:21, 5 users, load averages: ..."
	if i := strings.Index(s, "up "); i >= 0 {
		rest := s[i+3:]
		// Cut at the next comma after the "up X days, HH:MM" pair. The format
		// varies (minutes vs. days), so we take everything up to ", N users" if
		// present, otherwise the first ", load" boundary.
		for _, sep := range []string{", load average", "  load average", "load average"} {
			if j := strings.Index(rest, sep); j >= 0 {
				rest = rest[:j]
				break
			}
		}
		rest = strings.TrimSpace(rest)
		rest = strings.TrimSuffix(rest, ",")
		// Drop a trailing ", N users" if it's still in the tail.
		if j := strings.Index(rest, ", "); j >= 0 {
			tail := rest[j+2:]
			if strings.Contains(tail, "user") {
				rest = rest[:j]
			}
		}
		return rest
	}
	return strings.TrimSpace(s)
}

func sectionTitle(s string, focused bool) string {
	style := lipgloss.NewStyle().Bold(true)
	if focused {
		return style.Render("> " + s)
	}
	return style.Render("  " + s)
}

// shortTime turns an RFC3339 timestamp into HH:MM:SS for display. Anything we
// can't parse falls back to the original string so we still see something.
func shortTime(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("15:04:05")
}

// truncate clips a string to `max` runes with an ellipsis. Mirrors
// cmd/watchdog/main.go's helper so output looks identical between `summary`
// and `top`.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
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
