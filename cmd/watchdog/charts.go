package main

import (
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/abaj8494/macos-watchdog/internal/storage"
)

// Color palette for process charts (distinct, readable on dark bg)
var processColors = []string{
	"#ff6384", "#36a2eb", "#ffce56", "#4bc0c0", "#9966ff",
	"#ff9f40", "#c9cbcf", "#7bc67e", "#e7598b", "#4dc9f6",
	"#f67019", "#f53794", "#537bc4", "#acc236", "#166a8f",
}

type dashboardData struct {
	LastUpdate    string
	PressureColor string
	PressureStr   string
	SwapStr       string
	LoadStr       string
	WindowsJSON   template.JS
	DashboardPath string
	WatchdogBin   string
	// Issue badges: rendered server-side at template time so the indicator is
	// visible immediately on page load (no flicker, no extra fetch).
	OverviewBadge string // "" or "warn"
	AlertsBadge   string // "" or "fail"
}

func generateChartsHTML(store *storage.Store) (string, error) {
	type windowData struct {
		Label   string
		Hours   int
		System  []storage.SystemSample
		Procs   map[string]*storage.ProcessTimeSeries
		Top     []string
		ProcTbl []storage.ProcessTableRow
	}

	windows := []windowData{
		{Label: "4h", Hours: 4},
		{Label: "24h", Hours: 24},
		{Label: "7d", Hours: 168},
		{Label: "30d", Hours: 720},
	}

	for i := range windows {
		w := &windows[i]

		sys, err := store.GetSystemTimeSeries(w.Hours)
		if err != nil {
			return "", fmt.Errorf("system time series (%s): %w", w.Label, err)
		}
		w.System = sys

		topNames, err := store.GetTopProcessNames(w.Hours, 10)
		if err != nil {
			return "", fmt.Errorf("top processes (%s): %w", w.Label, err)
		}
		w.Top = topNames

		w.Procs = make(map[string]*storage.ProcessTimeSeries)
		for _, name := range topNames {
			ts, err := store.GetProcessMemoryTimeSeries(name, w.Hours)
			if err != nil {
				return "", fmt.Errorf("process ts %s (%s): %w", name, w.Label, err)
			}
			w.Procs[name] = ts
		}

		tbl, err := store.GetProcessTable(w.Hours)
		if err != nil {
			return "", fmt.Errorf("process table (%s): %w", w.Label, err)
		}
		w.ProcTbl = tbl
	}

	latest, _ := store.GetLatestSample()

	buildSystemArrays := func(samples []storage.SystemSample) (labels, load1, load5, load15, pressure, swap string) {
		var ls, l1, l5, l15, mp, sw []string
		for _, s := range samples {
			t, _ := time.Parse(time.RFC3339, s.Timestamp)
			ls = append(ls, fmt.Sprintf("'%s'", t.Format("Jan 2 15:04")))
			l1 = append(l1, fmt.Sprintf("%.2f", s.Load1))
			l5 = append(l5, fmt.Sprintf("%.2f", s.Load5))
			l15 = append(l15, fmt.Sprintf("%.2f", s.Load15))
			mp = append(mp, fmt.Sprintf("%d", s.MemPressure))
			sw = append(sw, fmt.Sprintf("%.2f", s.SwapUsedGB))
		}
		return strings.Join(ls, ","), strings.Join(l1, ","), strings.Join(l5, ","),
			strings.Join(l15, ","), strings.Join(mp, ","), strings.Join(sw, ",")
	}

	buildProcessDatasets := func(w windowData) string {
		var datasets []string
		for i, name := range w.Top {
			ts := w.Procs[name]
			if ts == nil {
				continue
			}
			color := processColors[i%len(processColors)]
			var vals []string
			for _, v := range ts.Values {
				vals = append(vals, fmt.Sprintf("%d", v))
			}
			datasets = append(datasets, fmt.Sprintf(`{
				label: '%s',
				data: [%s],
				borderColor: '%s',
				backgroundColor: '%s33',
				fill: true,
				borderWidth: 1.5,
				pointRadius: 0,
				pointHoverRadius: 5,
				pointHitRadius: 15,
				tension: 0.3
			}`, escapeJS(name), strings.Join(vals, ","), color, color))
		}
		return strings.Join(datasets, ",\n")
	}

	buildProcessTable := func(rows []storage.ProcessTableRow) string {
		var sb strings.Builder
		for _, r := range rows {
			currentStr := "-"
			if r.CurrentRSS > 0 {
				currentStr = fmt.Sprintf("%dMB", r.CurrentRSS)
			}
			sb.WriteString(fmt.Sprintf("<tr><td>%s</td><td>%s</td><td>%dMB</td><td>%.0fMB</td><td>%.1f%%</td></tr>\n",
				escapeHTML(r.Name), currentStr, r.PeakRSS, r.AvgRSS, r.AvgCPU))
		}
		return sb.String()
	}

	var windowsJS strings.Builder
	for i, w := range windows {
		labels, l1, l5, l15, pressure, swap := buildSystemArrays(w.System)
		procDatasets := buildProcessDatasets(w)
		procTable := buildProcessTable(w.ProcTbl)
		procLabels := labels

		ncpu := 10
		if len(w.System) > 0 {
			ncpu = w.System[0].Ncpu
		}

		windowsJS.WriteString(fmt.Sprintf(`
		'%s': {
			labels: [%s],
			load1: [%s],
			load5: [%s],
			load15: [%s],
			pressure: [%s],
			swap: [%s],
			ncpu: %d,
			procLabels: [%s],
			procDatasets: [%s],
			procTable: '%s'
		}`, w.Label, labels, l1, l5, l15, pressure, swap, ncpu, procLabels, procDatasets, escapeJSString(procTable)))
		if i < len(windows)-1 {
			windowsJS.WriteString(",")
		}
	}

	data := dashboardData{
		PressureColor: "#4bc0c0",
		PressureStr:   "-",
		SwapStr:       "-",
		LoadStr:       "-",
		LastUpdate:    "-",
		WindowsJSON:   template.JS(windowsJS.String()),
	}

	if latest != nil {
		data.PressureStr = fmt.Sprintf("%d%%", latest.MemPressure)
		data.SwapStr = fmt.Sprintf("%.1fGB", latest.SwapUsedGB)
		data.LoadStr = fmt.Sprintf("%.1f / %.1f / %.1f", latest.Load1, latest.Load5, latest.Load15)
		t, _ := time.Parse(time.RFC3339, latest.Timestamp)
		data.LastUpdate = t.Format("15:04:05")

		if latest.MemPressure > 70 {
			data.PressureColor = "#ff6384"
		} else if latest.MemPressure > 50 {
			data.PressureColor = "#ffce56"
		}

		// Yellow dot on Overview tab if current pressure or swap are above the
		// noise floor — surface the issue without forcing the user to look.
		if latest.MemPressure >= 60 || latest.SwapUsedGB >= 5 {
			data.OverviewBadge = "warn"
		}
	}

	// Red dot on Alerts tab if any alert fired in the last 24h. Best-effort;
	// if the query fails we just omit the badge.
	if n, err := store.CountRecentAlerts(24); err == nil && n > 0 {
		data.AlertsBadge = "fail"
	}

	// Find watchdog binary and dashboard path for refresh button
	home, _ := os.UserHomeDir()
	data.WatchdogBin = filepath.Join(home, ".local", "bin", "watchdog")

	logDir, err := storage.GetLogDir()
	if err != nil {
		return "", err
	}
	htmlPath := filepath.Join(logDir, "dashboard.html")
	data.DashboardPath = htmlPath

	tmpl, err := template.New("dashboard").Parse(dashboardTemplate)
	if err != nil {
		return "", fmt.Errorf("template parse: %w", err)
	}

	f, err := os.Create(htmlPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return "", fmt.Errorf("template execute: %w", err)
	}
	return htmlPath, nil
}

const dashboardTemplate = `<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <meta name="color-scheme" content="dark">
    <title>Watchdog - System Health Dashboard</title>
    <link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><polygon points='50,5 95,50 50,95 5,50' fill='none' stroke='%23ff6384' stroke-width='6'/><polygon points='50,22 78,50 50,78 22,50' fill='%23ff638444' stroke='%23ff9f40' stroke-width='3'/><circle cx='50' cy='50' r='8' fill='%23ff6384'/></svg>">
    <script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
    <style>
        /*
         * Design tokens. Centralised so the palette is consistent across the
         * dashboard — chart colors below also reference these via JS where it
         * matters. Hue family kept warm-coral + cool-blue so the dark theme
         * reads as one cohesive surface rather than Chart.js defaults.
         */
        :root {
            --bg:           #0d1117;
            --panel:        #161b22;
            --panel-sunken: #0b0f14;
            --border:       #30363d;
            --border-soft:  #21262d;
            --text:         #c9d1d9;
            --text-dim:     #8b949e;
            --text-muted:   #484f58;

            --accent:      #ff6384;  /* warm coral — primary */
            --accent-soft: #ff638422;
            --accent-glow: #ff638488;
            --warn:        #ffce56;  /* amber */
            --warn-glow:   #ffce5688;
            --ok:          #3fb950;  /* green */
            --info:        #58a6ff;  /* cool blue — links / hover */
            --info-soft:   #1f6feb33;
        }

        * { margin: 0; padding: 0; box-sizing: border-box; }
        html { color-scheme: dark; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: var(--bg);
            color: var(--text);
            min-height: 100vh;
            padding: 30px;
        }
        h1 {
            text-align: center;
            margin-bottom: 8px;
            font-size: 2.2em;
            background: linear-gradient(90deg, #ff6384, #ff9f40);
            -webkit-background-clip: text;
            -webkit-text-fill-color: transparent;
        }
        .subtitle {
            text-align: center;
            color: #484f58;
            margin-bottom: 25px;
            font-size: 0.9em;
        }
        .controls {
            display: flex;
            justify-content: center;
            gap: 8px;
            margin-bottom: 30px;
        }
        .controls button {
            background: #161b22;
            border: 1px solid #30363d;
            border-radius: 6px;
            color: #c9d1d9;
            padding: 6px 18px;
            font-size: 0.85em;
            cursor: pointer;
            transition: all 0.15s;
        }
        .controls button:hover {
            background: #1f2937;
            border-color: #58a6ff;
        }
        .controls button.active {
            background: #1f6feb33;
            border-color: #58a6ff;
            color: #58a6ff;
        }
        .controls .refresh-btn {
            background: #161b22;
            border-color: #ff6384;
            color: #ff6384;
            margin-left: 20px;
        }
        .controls .refresh-btn:hover {
            background: #ff638422;
        }
        .summary-cards {
            display: grid;
            grid-template-columns: repeat(4, 1fr);
            gap: 16px;
            max-width: 1400px;
            margin: 0 auto 30px;
        }
        .card {
            background: #161b22;
            border-radius: 12px;
            padding: 20px;
            text-align: center;
            border: 1px solid #30363d;
        }
        .card .label { color: #484f58; font-size: 0.8em; margin-bottom: 8px; text-transform: uppercase; letter-spacing: 0.05em; }
        .card .value { font-size: 1.8em; font-weight: 600; }
        .charts-container {
            display: grid;
            grid-template-columns: 1fr 1fr;
            gap: 20px;
            max-width: 1400px;
            margin: 0 auto 20px;
        }
        .chart-box {
            background: #161b22;
            border-radius: 12px;
            padding: 20px;
            border: 1px solid #30363d;
        }
        .chart-box.full-width { grid-column: 1 / -1; }
        .chart-box h2 {
            margin-bottom: 12px;
            font-size: 1.05em;
            color: #8b949e;
            font-weight: 500;
        }
        .process-table {
            width: 100%;
            border-collapse: collapse;
            font-size: 0.85em;
        }
        .process-table th {
            text-align: left;
            padding: 8px 12px;
            border-bottom: 1px solid #30363d;
            color: #484f58;
            font-weight: 500;
            text-transform: uppercase;
            font-size: 0.8em;
            letter-spacing: 0.05em;
            cursor: pointer;
            user-select: none;
        }
        .process-table th:hover { color: #c9d1d9; }
        .process-table th .sort-arrow { font-size: 0.7em; margin-left: 4px; }
        .process-table td {
            padding: 6px 12px;
            border-bottom: 1px solid #21262d;
            font-variant-numeric: tabular-nums;
        }
        .process-table tr:hover td { background: #1f2937; }
        .threshold-note {
            color: #484f58;
            font-size: 0.75em;
            margin-top: 6px;
        }
        .tabs {
            display: flex;
            justify-content: center;
            gap: 4px;
            margin-bottom: 20px;
            border-bottom: 1px solid #30363d;
            max-width: 1400px;
            margin-left: auto;
            margin-right: auto;
        }
        .tabs button {
            background: transparent;
            border: none;
            border-bottom: 2px solid transparent;
            color: #8b949e;
            padding: 10px 22px;
            font-size: 0.95em;
            cursor: pointer;
            transition: all 0.15s;
            margin-bottom: -1px;
        }
        .tabs button:hover { color: #c9d1d9; }
        .tabs button.active {
            color: #ff6384;
            border-bottom-color: #ff6384;
        }
        /* Issue-badge: small dot next to a tab title indicating "look here". */
        .tab-badge {
            display: inline-block;
            width: 7px;
            height: 7px;
            border-radius: 50%;
            margin-left: 6px;
            vertical-align: 1px;
        }
        .tab-badge.warn { background: var(--warn); box-shadow: 0 0 6px var(--warn-glow); }
        .tab-badge.fail {
            background: var(--accent);
            box-shadow: 0 0 6px var(--accent-glow);
            animation: pulse 1.8s ease-in-out infinite;
        }
        @keyframes pulse {
            0%, 100% { opacity: 1; }
            50%      { opacity: 0.45; }
        }
        .tab-panel { display: none; max-width: 1400px; margin: 0 auto; }
        .tab-panel.active { display: block; }
        .log-toolbar {
            display: flex;
            align-items: center;
            gap: 8px;
            margin-bottom: 12px;
        }
        .log-toolbar label {
            color: #484f58;
            font-size: 0.8em;
            text-transform: uppercase;
            letter-spacing: 0.05em;
        }
        .log-toolbar select, .log-toolbar button {
            background: #161b22;
            border: 1px solid #30363d;
            border-radius: 6px;
            color: #c9d1d9;
            padding: 5px 12px;
            font-size: 0.85em;
            cursor: pointer;
        }
        .log-toolbar button:hover { border-color: #58a6ff; }
        .log-toolbar .spacer { flex: 1; }
        .log-toolbar .status { color: #484f58; font-size: 0.8em; }
        .log-view {
            background: #0b0f14;
            border: 1px solid #30363d;
            border-radius: 8px;
            padding: 14px 16px;
            font-family: 'SF Mono', Menlo, Consolas, monospace;
            font-size: 0.78em;
            line-height: 1.45;
            color: #c9d1d9;
            white-space: pre-wrap;
            word-break: break-word;
            max-height: 70vh;
            overflow-y: auto;
            min-height: 240px;
        }
        .cron-layout {
            display: grid;
            grid-template-columns: 280px 1fr;
            gap: 16px;
        }
        .cron-sidebar {
            background: #161b22;
            border: 1px solid #30363d;
            border-radius: 8px;
            padding: 6px;
            max-height: 75vh;
            overflow-y: auto;
        }
        .cron-entry {
            padding: 9px 11px;
            border-radius: 6px;
            cursor: pointer;
            border: 1px solid transparent;
            margin-bottom: 2px;
        }
        .cron-entry:hover { background: #1f2937; }
        .cron-entry.active {
            background: #1f6feb22;
            border-color: #58a6ff55;
        }
        .cron-entry .label {
            color: #c9d1d9;
            font-size: 0.88em;
            font-weight: 500;
            word-break: break-word;
        }
        .cron-entry .meta {
            color: #484f58;
            font-size: 0.72em;
            margin-top: 3px;
            font-family: 'SF Mono', Menlo, monospace;
        }
        .cron-entry.active .label { color: #58a6ff; }
        .cron-detail-header {
            display: flex;
            flex-direction: column;
            gap: 4px;
            margin-bottom: 10px;
            padding: 10px 14px;
            background: #161b22;
            border: 1px solid #30363d;
            border-radius: 8px;
        }
        .cron-detail-header .cmd {
            color: #8b949e;
            font-size: 0.78em;
            font-family: 'SF Mono', Menlo, monospace;
            word-break: break-all;
        }
        .cron-detail-header .file {
            color: #484f58;
            font-size: 0.75em;
        }
        /* Agents tab — reuses cron-layout structure. */
        .agent-entry {
            padding: 9px 11px;
            border-radius: 6px;
            cursor: pointer;
            border: 1px solid transparent;
            margin-bottom: 2px;
            display: flex;
            align-items: center;
            gap: 8px;
        }
        .agent-entry:hover { background: #1f2937; }
        .agent-entry.active {
            background: #1f6feb22;
            border-color: #58a6ff55;
        }
        .agent-entry .label-wrap { flex: 1; min-width: 0; }
        .agent-entry .label {
            color: #c9d1d9;
            font-size: 0.88em;
            font-weight: 500;
            word-break: break-word;
        }
        .agent-entry .meta {
            color: #484f58;
            font-size: 0.72em;
            margin-top: 3px;
            font-family: 'SF Mono', Menlo, monospace;
        }
        .agent-entry.active .label { color: #58a6ff; }
        .badge {
            display: inline-block;
            width: 10px;
            height: 10px;
            border-radius: 50%;
            flex-shrink: 0;
        }
        .badge.ok      { background: #3fb950; box-shadow: 0 0 6px #3fb95066; }
        .badge.warn    { background: #ffce56; box-shadow: 0 0 6px #ffce5666; }
        .badge.fail    { background: #ff6384; box-shadow: 0 0 8px #ff6384aa; }
        .badge.unknown { background: #484f58; }
        .agent-detail-header {
            display: flex;
            flex-direction: column;
            gap: 6px;
            margin-bottom: 10px;
            padding: 12px 14px;
            background: #161b22;
            border: 1px solid #30363d;
            border-radius: 8px;
        }
        .agent-detail-header .top {
            display: flex;
            align-items: center;
            gap: 10px;
        }
        .agent-detail-header .title {
            color: #c9d1d9;
            font-size: 1.0em;
            font-weight: 600;
        }
        .agent-detail-header .grid {
            display: grid;
            grid-template-columns: 110px 1fr;
            gap: 4px 12px;
            color: #8b949e;
            font-size: 0.78em;
            font-family: 'SF Mono', Menlo, monospace;
        }
        .agent-detail-header .grid .k { color: #484f58; }
        .agent-detail-header .grid .v { word-break: break-all; }
        .agent-detail-header .reasons {
            font-size: 0.8em;
            margin-top: 4px;
        }
        .agent-detail-header .reasons li { margin-left: 18px; }
        .agent-detail-header .reasons.fail li  { color: #ff6384; }
        .agent-detail-header .reasons.warn li  { color: #ffce56; }
        .agent-detail-header .reasons.ok li    { color: #3fb950; }
        .agent-detail-header .reasons.unknown li { color: #8b949e; }

        /* Keyboard-shortcuts overlay (toggled with the ? key). */
        .shortcuts-overlay {
            position: fixed;
            inset: 0;
            background: rgba(0,0,0,0.6);
            backdrop-filter: blur(3px);
            display: none;
            align-items: center;
            justify-content: center;
            z-index: 100;
        }
        .shortcuts-overlay.open { display: flex; }
        .shortcuts-card {
            background: var(--panel);
            border: 1px solid var(--border);
            border-radius: 12px;
            padding: 28px 32px;
            min-width: 340px;
            max-width: 90vw;
            box-shadow: 0 20px 60px rgba(0,0,0,0.5);
        }
        .shortcuts-card h2 {
            font-size: 1.1em;
            margin-bottom: 16px;
            color: var(--text);
        }
        .shortcuts-card dl {
            display: grid;
            grid-template-columns: auto 1fr;
            gap: 10px 18px;
            font-size: 0.88em;
        }
        .shortcuts-card dt {
            font-family: 'SF Mono', Menlo, monospace;
            color: var(--accent);
            background: var(--accent-soft);
            padding: 2px 8px;
            border-radius: 4px;
            justify-self: start;
            font-size: 0.85em;
        }
        .shortcuts-card dd { color: var(--text-dim); align-self: center; }
        .shortcuts-card .hint {
            margin-top: 16px;
            font-size: 0.78em;
            color: var(--text-muted);
            text-align: right;
        }

        /* Zones tab uses the same chart-box look as the dashboard. */
        .zones-container {
            display: grid;
            grid-template-columns: 1fr;
            gap: 20px;
            max-width: 1400px;
            margin: 0 auto;
        }

        /* Alerts table — reuses .process-table styling but with status pills. */
        .alerts-table .pill {
            display: inline-block;
            padding: 2px 8px;
            border-radius: 4px;
            font-size: 0.78em;
            font-family: 'SF Mono', Menlo, monospace;
        }
        .alerts-table .pill.pressure { background: var(--accent-soft); color: var(--accent); }
        .alerts-table .pill.swap     { background: #ffce5622; color: var(--warn); }
        .alerts-table .pill.zone     { background: #9966ff22; color: #9966ff; }
        .alerts-table .pill.process  { background: #4bc0c022; color: #4bc0c0; }
        .alerts-table .pill.default  { background: var(--border-soft); color: var(--text-dim); }

        /* Responsive: progressively collapse the desktop grid for tablet/phone.
           Breakpoints are intentionally conservative — most readers are on
           wide displays and we don't want to fight the wide-table layouts. */
        @media (max-width: 1100px) {
            body { padding: 18px; }
            .summary-cards { grid-template-columns: repeat(2, 1fr); }
            .charts-container { grid-template-columns: 1fr; }
            .cron-layout { grid-template-columns: 220px 1fr; }
        }
        @media (max-width: 700px) {
            body { padding: 12px; }
            h1 { font-size: 1.6em; }
            .summary-cards { grid-template-columns: 1fr 1fr; gap: 10px; }
            .summary-cards .card { padding: 12px; }
            .summary-cards .card .value { font-size: 1.3em; }
            .tabs { overflow-x: auto; flex-wrap: nowrap; justify-content: flex-start; }
            .tabs button { padding: 10px 14px; font-size: 0.88em; white-space: nowrap; }
            .controls { flex-wrap: wrap; }
            .cron-layout { grid-template-columns: 1fr; }
            .cron-sidebar { max-height: 240px; }
            .process-table { font-size: 0.78em; }
            .process-table th, .process-table td { padding: 5px 6px; }
        }
    </style>
</head>
<body>
    <h1>Watchdog</h1>
    <p class="subtitle">System Health Dashboard &middot; Last sample: {{.LastUpdate}}</p>

    <div class="tabs">
        <button onclick="setTab('dashboard')" id="tab-dashboard" class="active">Dashboard{{if .OverviewBadge}}<span class="tab-badge {{.OverviewBadge}}" title="Memory pressure or swap above noise floor"></span>{{end}}</button>
        <button onclick="setTab('ssh')" id="tab-ssh">SSH</button>
        <button onclick="setTab('crons')" id="tab-crons">Crons</button>
        <button onclick="setTab('agents')" id="tab-agents">Agents</button>
        <button onclick="setTab('zones')" id="tab-zones">Zones</button>
        <button onclick="setTab('alerts')" id="tab-alerts">Alerts{{if .AlertsBadge}}<span class="tab-badge {{.AlertsBadge}}" title="Alert fired in the last 24h"></span>{{end}}</button>
    </div>

    <div id="panel-dashboard" class="tab-panel active">
    <div class="controls">
        <button onclick="setWindow('4h')" id="btn-4h">4h</button>
        <button onclick="setWindow('24h')" id="btn-24h" class="active">24h</button>
        <button onclick="setWindow('7d')" id="btn-7d">7d</button>
        <button onclick="setWindow('30d')" id="btn-30d">30d</button>
        <button class="refresh-btn" id="refreshBtn" onclick="refreshDashboard()">&#8635; Refresh</button>
    </div>

    <div class="summary-cards">
        <div class="card">
            <div class="label">Memory Pressure</div>
            <div class="value" style="color: {{.PressureColor}}">{{.PressureStr}}</div>
        </div>
        <div class="card">
            <div class="label">Swap Used</div>
            <div class="value">{{.SwapStr}}</div>
        </div>
        <div class="card">
            <div class="label">Load Average</div>
            <div class="value" style="font-size:1.2em">{{.LoadStr}}</div>
        </div>
        <div class="card">
            <div class="label">Last Update</div>
            <div class="value" style="font-size:1.4em">{{.LastUpdate}}</div>
        </div>
    </div>

    <div class="charts-container">
        <div class="chart-box">
            <h2>Memory Pressure</h2>
            <canvas id="pressureChart"></canvas>
            <p class="threshold-note">Threshold: 70% (warning)</p>
        </div>
        <div class="chart-box">
            <h2>Swap Usage</h2>
            <canvas id="swapChart"></canvas>
            <p class="threshold-note">Threshold: 8GB (warning)</p>
        </div>
        <div class="chart-box">
            <h2>Load Average</h2>
            <canvas id="loadChart"></canvas>
        </div>
        <div class="chart-box">
            <h2>Per-Process Memory (RSS)</h2>
            <canvas id="procMemChart"></canvas>
        </div>
        <div class="chart-box full-width">
            <h2>Process Summary</h2>
            <table class="process-table">
                <thead>
                    <tr>
                        <th onclick="sortTable(0)">Process<span class="sort-arrow"></span></th>
                        <th onclick="sortTable(1)">Current<span class="sort-arrow"></span></th>
                        <th onclick="sortTable(2)">Peak<span class="sort-arrow"></span></th>
                        <th onclick="sortTable(3)">Avg RSS<span class="sort-arrow"></span></th>
                        <th onclick="sortTable(4)">Avg CPU<span class="sort-arrow"></span></th>
                    </tr>
                </thead>
                <tbody id="procTableBody"></tbody>
            </table>
        </div>
    </div>
    </div><!-- /panel-dashboard -->

    <div id="panel-ssh" class="tab-panel">
        <div class="log-toolbar">
            <label>Window</label>
            <select id="sshHours" onchange="loadSSH()">
                <option value="1">1h</option>
                <option value="24" selected>24h</option>
                <option value="168">7d</option>
            </select>
            <button onclick="loadSSH()">&#8635; Reload</button>
            <span class="spacer"></span>
            <span class="status" id="sshStatus"></span>
        </div>
        <div class="log-view" id="sshLog">Select the SSH tab to load logs.</div>
    </div>

    <div id="panel-agents" class="tab-panel">
        <div class="log-toolbar">
            <label>Prefix</label>
            <select id="agentPrefix" onchange="loadAgents()">
                <option value="" selected>com.aayushbajaj.</option>
                <option value="*">all</option>
            </select>
            <button onclick="loadAgents()">&#8635; Reload</button>
            <span class="spacer"></span>
            <span class="status" id="agentStatus"></span>
        </div>
        <div class="cron-layout">
            <div class="cron-sidebar" id="agentSidebar">Loading...</div>
            <div>
                <div class="agent-detail-header" id="agentHeader" style="display:none">
                    <div class="top">
                        <span class="badge" id="agentBadge"></span>
                        <span class="title" id="agentTitle"></span>
                    </div>
                    <div class="grid" id="agentGrid"></div>
                    <ul class="reasons" id="agentReasons"></ul>
                </div>
                <div class="log-view" id="agentLog">Select an agent on the left.</div>
            </div>
        </div>
    </div>

    <div id="panel-crons" class="tab-panel">
        <div class="log-toolbar">
            <label>Window</label>
            <select id="cronHours" onchange="loadCurrentCron()">
                <option value="1">1h</option>
                <option value="24" selected>24h</option>
                <option value="168">7d</option>
                <option value="720">30d</option>
            </select>
            <button onclick="loadCurrentCron()">&#8635; Reload</button>
            <span class="spacer"></span>
            <span class="status" id="cronStatus"></span>
        </div>
        <div class="cron-layout">
            <div class="cron-sidebar" id="cronSidebar">Loading...</div>
            <div>
                <div class="cron-detail-header" id="cronHeader" style="display:none">
                    <div class="cmd" id="cronCmd"></div>
                    <div class="file" id="cronFile"></div>
                </div>
                <div class="log-view" id="cronLog">Select a cron entry on the left.</div>
            </div>
        </div>
    </div>

    <div id="panel-zones" class="tab-panel">
        <div class="log-toolbar">
            <label>Window</label>
            <select id="zoneHours" onchange="loadZones()">
                <option value="4">4h</option>
                <option value="24" selected>24h</option>
                <option value="168">7d</option>
                <option value="720">30d</option>
            </select>
            <button onclick="loadZones()">&#8635; Reload</button>
            <span class="spacer"></span>
            <span class="status" id="zoneStatus"></span>
        </div>
        <div class="zones-container">
            <div class="chart-box">
                <h2>Top 5 Kernel Zones (estimated bytes)</h2>
                <canvas id="zoneChart"></canvas>
                <p class="threshold-note">Top zones by average est_bytes (elem_size × inuse). Watch for monotonic growth.</p>
            </div>
            <div class="chart-box">
                <h2>Top 20 Zones Summary</h2>
                <table class="process-table" id="zoneTable">
                    <thead>
                        <tr>
                            <th onclick="sortZoneTable(0)">Zone<span class="sort-arrow"></span></th>
                            <th onclick="sortZoneTable(1)">Current<span class="sort-arrow"></span></th>
                            <th onclick="sortZoneTable(2)">Peak<span class="sort-arrow"></span></th>
                            <th onclick="sortZoneTable(3)">Avg<span class="sort-arrow"></span></th>
                            <th onclick="sortZoneTable(4)">Elem Size<span class="sort-arrow"></span></th>
                        </tr>
                    </thead>
                    <tbody id="zoneTableBody"><tr><td colspan="5" style="color:#484f58">Loading...</td></tr></tbody>
                </table>
            </div>
        </div>
    </div>

    <div id="panel-alerts" class="tab-panel">
        <div class="log-toolbar">
            <button onclick="loadAlerts()">&#8635; Reload</button>
            <span class="spacer"></span>
            <span class="status" id="alertStatus"></span>
        </div>
        <div class="chart-box">
            <h2>Recent Alerts (last 50)</h2>
            <table class="process-table alerts-table" id="alertsTable">
                <thead>
                    <tr>
                        <th onclick="sortAlertsTable(0)">Kind<span class="sort-arrow"></span></th>
                        <th onclick="sortAlertsTable(1)">Last Sent<span class="sort-arrow"></span></th>
                        <th onclick="sortAlertsTable(2)">Last Value<span class="sort-arrow"></span></th>
                    </tr>
                </thead>
                <tbody id="alertsTableBody"><tr><td colspan="3" style="color:#484f58">Click the Alerts tab to load.</td></tr></tbody>
            </table>
            <p class="threshold-note">Alerts are de-duplicated server-side per kind with a cool-down; last_sent is the most recent fire.</p>
        </div>
    </div>

    <div class="shortcuts-overlay" id="shortcutsOverlay" onclick="toggleShortcuts(false)">
        <div class="shortcuts-card" onclick="event.stopPropagation()">
            <h2>Keyboard Shortcuts</h2>
            <dl>
                <dt>1</dt><dd>Dashboard</dd>
                <dt>2</dt><dd>SSH</dd>
                <dt>3</dt><dd>Crons</dd>
                <dt>4</dt><dd>Agents</dd>
                <dt>5</dt><dd>Zones</dd>
                <dt>6</dt><dd>Alerts</dd>
                <dt>r</dt><dd>Refresh dashboard data</dd>
                <dt>?</dt><dd>Toggle this help</dd>
                <dt>Esc</dt><dd>Close this help</dd>
            </dl>
            <div class="hint">Press ? again or Esc to close</div>
        </div>
    </div>

<script>
// Force Chart.js dark mode defaults
Chart.defaults.color = '#8b949e';
Chart.defaults.borderColor = '#30363d';

const DATA = { {{.WindowsJSON}} };

let currentWindow = '24h';
let charts = {};

function setWindow(w) {
    currentWindow = w;
    document.querySelectorAll('.controls button:not(.refresh-btn)').forEach(b => b.classList.remove('active'));
    document.getElementById('btn-' + w).classList.add('active');
    updateCharts();
}

function makeChart(id, config) {
    if (charts[id]) charts[id].destroy();
    charts[id] = new Chart(document.getElementById(id), config);
}

function drawThreshold(chart, value, color) {
    const yScale = chart.scales.y;
    const y = yScale.getPixelForValue(value);
    if (y < chart.chartArea.top || y > chart.chartArea.bottom) return;
    const ctx = chart.ctx;
    ctx.save();
    ctx.strokeStyle = color;
    ctx.lineWidth = 1;
    ctx.setLineDash([5, 5]);
    ctx.beginPath();
    ctx.moveTo(chart.chartArea.left, y);
    ctx.lineTo(chart.chartArea.right, y);
    ctx.stroke();
    ctx.restore();
}

const gridColor = '#21262d';
const tickColor = '#484f58';
const defaultScaleX = { ticks: { color: tickColor, maxTicksLimit: 10, maxRotation: 0 }, grid: { display: false } };
const defaultScaleY = { grid: { color: gridColor }, ticks: { color: tickColor } };

// Shared tooltip config: show only the single dataset closest to cursor
const tooltipStyle = {
    backgroundColor: '#161b22ee',
    borderColor: '#30363d',
    borderWidth: 1,
    titleColor: '#c9d1d9',
    bodyColor: '#8b949e',
    padding: 10,
    cornerRadius: 6
};
const hoverConfig = {
    interaction: { mode: 'point', intersect: false },
    hover: { mode: 'point', intersect: false },
    plugins: {
        tooltip: {
            ...tooltipStyle,
            mode: 'point',
            intersect: false,
            // Only show the single closest dataset
            filter: function(tooltipItem, _index, tooltipItems) {
                if (tooltipItems.length <= 1) return true;
                // Find the one with smallest distance to cursor
                let minDist = Infinity, minIdx = 0;
                tooltipItems.forEach((item, i) => {
                    const meta = item.chart.getDatasetMeta(item.datasetIndex);
                    const pt = meta.data[item.dataIndex];
                    if (pt) {
                        const dx = pt.x - item.chart._lastEvent.x;
                        const dy = pt.y - item.chart._lastEvent.y;
                        const dist = dx*dx + dy*dy;
                        if (dist < minDist) { minDist = dist; minIdx = i; }
                    }
                });
                return tooltipItem === tooltipItems[minIdx];
            }
        }
    }
};

function updateCharts() {
    const d = DATA[currentWindow];
    if (!d) return;

    makeChart('pressureChart', {
        type: 'line',
        data: {
            labels: d.labels,
            datasets: [{
                label: 'Memory Pressure %',
                data: d.pressure,
                borderColor: '#ff6384',
                backgroundColor: '#ff638422',
                fill: true,
                tension: 0.3,
                pointRadius: 0,
                pointHoverRadius: 5,
                pointHitRadius: 15
            }]
        },
        options: {
            responsive: true,
            ...hoverConfig,
            scales: {
                y: { ...defaultScaleY, min: 0, max: 100 },
                x: defaultScaleX
            },
            plugins: { ...hoverConfig.plugins, legend: { display: false } }
        },
        plugins: [{ afterDraw: chart => drawThreshold(chart, 70, '#ff638488') }]
    });

    makeChart('swapChart', {
        type: 'line',
        data: {
            labels: d.labels,
            datasets: [{
                label: 'Swap (GB)',
                data: d.swap,
                borderColor: '#ffce56',
                backgroundColor: '#ffce5622',
                fill: true,
                tension: 0.3,
                pointRadius: 0,
                pointHoverRadius: 5,
                pointHitRadius: 15
            }]
        },
        options: {
            responsive: true,
            ...hoverConfig,
            scales: { y: { ...defaultScaleY, min: 0 }, x: defaultScaleX },
            plugins: { ...hoverConfig.plugins, legend: { display: false } }
        },
        plugins: [{ afterDraw: chart => drawThreshold(chart, 8, '#ffce5688') }]
    });

    makeChart('loadChart', {
        type: 'line',
        data: {
            labels: d.labels,
            datasets: [
                { label: '1min', data: d.load1, borderColor: '#36a2eb', borderWidth: 1.5, pointRadius: 0, pointHoverRadius: 5, pointHitRadius: 15, tension: 0.3 },
                { label: '5min', data: d.load5, borderColor: '#4bc0c0', borderWidth: 1.5, pointRadius: 0, pointHoverRadius: 5, pointHitRadius: 15, tension: 0.3 },
                { label: '15min', data: d.load15, borderColor: '#9966ff', borderWidth: 1.5, pointRadius: 0, pointHoverRadius: 5, pointHitRadius: 15, tension: 0.3 }
            ]
        },
        options: {
            responsive: true,
            ...hoverConfig,
            scales: { y: { ...defaultScaleY, min: 0 }, x: defaultScaleX },
            plugins: { ...hoverConfig.plugins, legend: { labels: { color: '#8b949e', boxWidth: 10, padding: 15 } } }
        },
        plugins: [{
            afterDraw: function(chart) {
                drawThreshold(chart, d.ncpu, '#ffffff33');
                const yScale = chart.scales.y;
                const y = yScale.getPixelForValue(d.ncpu);
                if (y >= chart.chartArea.top && y <= chart.chartArea.bottom) {
                    chart.ctx.fillStyle = '#484f58';
                    chart.ctx.font = '10px sans-serif';
                    chart.ctx.fillText(d.ncpu + ' cores', chart.chartArea.right - 48, y - 4);
                }
            }
        }]
    });

    makeChart('procMemChart', {
        type: 'line',
        data: {
            labels: d.procLabels,
            datasets: d.procDatasets
        },
        options: {
            responsive: true,
            ...hoverConfig,
            scales: {
                y: {
                    stacked: true,
                    ...defaultScaleY,
                    ticks: { ...defaultScaleY.ticks, callback: v => v + 'MB' },
                    title: { display: true, text: 'RSS (MB)', color: '#484f58' }
                },
                x: defaultScaleX
            },
            plugins: {
                ...hoverConfig.plugins,
                legend: { labels: { color: '#8b949e', boxWidth: 10, padding: 12 } }
            }
        }
    });

    document.getElementById('procTableBody').innerHTML = d.procTable;
}

let sortCol = -1, sortAsc = true;
function sortTable(col) {
    const tbody = document.getElementById('procTableBody');
    const rows = Array.from(tbody.querySelectorAll('tr'));
    if (sortCol === col) { sortAsc = !sortAsc; } else { sortCol = col; sortAsc = col === 0; }

    rows.sort((a, b) => {
        let av = a.cells[col].textContent.trim();
        let bv = b.cells[col].textContent.trim();
        if (col > 0) {
            av = parseFloat(av.replace(/[^0-9.\-]/g, '')) || 0;
            bv = parseFloat(bv.replace(/[^0-9.\-]/g, '')) || 0;
        }
        let cmp = col === 0 ? av.localeCompare(bv) : av - bv;
        return sortAsc ? cmp : -cmp;
    });

    rows.forEach(r => tbody.appendChild(r));

    document.querySelectorAll('.process-table th .sort-arrow').forEach((el, i) => {
        el.textContent = i === col ? (sortAsc ? ' ▲' : ' ▼') : '';
    });
}

function refreshDashboard() {
    // Navigate to /refresh which collects fresh data and redirects back to /
    window.location.href = '/refresh';
}

let currentTab = 'dashboard';
let sshLoaded = false;
let cronsLoaded = false;
let agentsLoaded = false;
let zonesLoaded = false;
let alertsLoaded = false;
let selectedCronIdx = -1;
let cronEntries = [];
let agentEntries = [];
let selectedAgentLabel = null;

function setTab(tab) {
    currentTab = tab;
    document.querySelectorAll('.tabs button').forEach(b => b.classList.remove('active'));
    document.getElementById('tab-' + tab).classList.add('active');
    document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
    document.getElementById('panel-' + tab).classList.add('active');

    if (tab === 'ssh' && !sshLoaded) loadSSH();
    if (tab === 'crons' && !cronsLoaded) loadCrontabs();
    if (tab === 'agents' && !agentsLoaded) loadAgents();
    if (tab === 'zones' && !zonesLoaded) loadZones();
    if (tab === 'alerts' && !alertsLoaded) loadAlerts();
}

async function loadSSH() {
    const hours = document.getElementById('sshHours').value;
    const status = document.getElementById('sshStatus');
    const view = document.getElementById('sshLog');
    status.textContent = 'Loading...';
    view.textContent = '';
    try {
        const res = await fetch('/api/ssh-log?hours=' + hours);
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        view.textContent = (await res.text()) || '(no entries)';
        status.textContent = 'Loaded ' + new Date().toLocaleTimeString();
        sshLoaded = true;
    } catch (e) {
        view.textContent = 'Error: ' + e.message + '\n\n(Log endpoints require watchdog serve.)';
        status.textContent = '';
    }
}

async function loadCrontabs() {
    const sidebar = document.getElementById('cronSidebar');
    const status = document.getElementById('cronStatus');
    status.textContent = 'Loading...';
    try {
        const res = await fetch('/api/crontabs');
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        cronEntries = await res.json();
        if (!cronEntries || cronEntries.length === 0) {
            sidebar.innerHTML = '<div style="padding:12px;color:#484f58;font-size:0.85em">No crontab entries.</div>';
            status.textContent = '';
            return;
        }
        sidebar.innerHTML = cronEntries.map(e =>
            '<div class="cron-entry" data-idx="' + e.index + '" onclick="selectCron(' + e.index + ')">' +
                '<div class="label">' + escapeHtml(e.label) + '</div>' +
                '<div class="meta">' + escapeHtml(e.schedule) + '</div>' +
            '</div>'
        ).join('');
        cronsLoaded = true;
        status.textContent = '';
        if (selectedCronIdx < 0) selectCron(cronEntries[0].index);
    } catch (e) {
        sidebar.innerHTML = '<div style="padding:12px;color:#ff6384;font-size:0.85em">Error: ' + escapeHtml(e.message) + '<br><br>(Log endpoints require watchdog serve.)</div>';
        status.textContent = '';
    }
}

function selectCron(idx) {
    selectedCronIdx = idx;
    document.querySelectorAll('.cron-entry').forEach(el => {
        el.classList.toggle('active', parseInt(el.dataset.idx) === idx);
    });
    loadCurrentCron();
}

async function loadCurrentCron() {
    if (selectedCronIdx < 0) return;
    const entry = cronEntries.find(e => e.index === selectedCronIdx);
    if (!entry) return;
    const hours = document.getElementById('cronHours').value;
    const status = document.getElementById('cronStatus');
    const view = document.getElementById('cronLog');
    const header = document.getElementById('cronHeader');
    document.getElementById('cronCmd').textContent = entry.command;
    document.getElementById('cronFile').textContent = entry.logFile ? 'Log file: ' + entry.logFile : 'No log file redirect — falling back to unified log';
    header.style.display = 'flex';
    status.textContent = 'Loading...';
    view.textContent = '';
    try {
        const res = await fetch('/api/cron-log?idx=' + selectedCronIdx + '&hours=' + hours);
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        view.textContent = (await res.text()) || '(no entries)';
        status.textContent = 'Loaded ' + new Date().toLocaleTimeString();
    } catch (e) {
        view.textContent = 'Error: ' + e.message;
        status.textContent = '';
    }
}

async function loadAgents() {
    const sidebar = document.getElementById('agentSidebar');
    const status = document.getElementById('agentStatus');
    const prefix = document.getElementById('agentPrefix').value;
    status.textContent = 'Loading...';
    try {
        const url = '/api/launch-agents' + (prefix ? '?prefix=' + encodeURIComponent(prefix) : '');
        const res = await fetch(url);
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        agentEntries = await res.json();
        if (!agentEntries || agentEntries.length === 0) {
            sidebar.innerHTML = '<div style="padding:12px;color:#484f58;font-size:0.85em">No launch agents matched.</div>';
            status.textContent = '';
            return;
        }
        sidebar.innerHTML = agentEntries.map(a => {
            const sched = a.startInterval ? ('every ' + formatInterval(a.startInterval))
                       : (a.startCalendar ? 'calendar' : (a.runAtLoad ? 'RunAtLoad' : '-'));
            const stateBit = a.loaded ? (a.state || 'unknown') : 'not loaded';
            return '<div class="agent-entry" data-label="' + escapeHtml(a.label) + '">' +
                '<span class="badge ' + escapeHtml(a.health) + '" title="' + escapeHtml(a.health) + '"></span>' +
                '<div class="label-wrap">' +
                    '<div class="label">' + escapeHtml(shortLabel(a.label)) + '</div>' +
                    '<div class="meta">' + escapeHtml(sched + '  ·  ' + stateBit + (a.runs ? '  ·  ' + a.runs + ' runs' : '')) + '</div>' +
                '</div>' +
            '</div>';
        }).join('');
        // Event delegation: a single listener on the parent survives re-renders
        // and avoids HTML-attribute quoting issues with labels that contain dots.
        if (!sidebar.dataset.bound) {
            sidebar.addEventListener('click', (ev) => {
                const row = ev.target.closest('.agent-entry');
                if (!row || !sidebar.contains(row)) return;
                const label = row.dataset.label;
                if (label) selectAgent(label);
            });
            sidebar.dataset.bound = '1';
        }
        agentsLoaded = true;
        status.textContent = 'Loaded ' + new Date().toLocaleTimeString();
        if (!selectedAgentLabel || !agentEntries.find(a => a.label === selectedAgentLabel)) {
            // Prefer the first agent in a failing state — that's the whole point of this tab.
            const failing = agentEntries.find(a => a.health === 'fail') ||
                           agentEntries.find(a => a.health === 'warn') ||
                           agentEntries[0];
            selectAgent(failing.label);
        } else {
            renderAgentDetail(selectedAgentLabel);
        }
    } catch (e) {
        sidebar.innerHTML = '<div style="padding:12px;color:#ff6384;font-size:0.85em">Error: ' + escapeHtml(e.message) + '<br><br>(Log endpoints require watchdog serve.)</div>';
        status.textContent = '';
    }
}

function shortLabel(s) {
    // Strip the user's prefix so the list reads more cleanly.
    if (s && s.startsWith('com.aayushbajaj.')) return s.slice('com.aayushbajaj.'.length);
    return s;
}

function formatInterval(sec) {
    if (sec >= 3600 && sec % 3600 === 0) return (sec/3600) + 'h';
    if (sec >= 60 && sec % 60 === 0) return (sec/60) + 'min';
    return sec + 's';
}

function selectAgent(label) {
    selectedAgentLabel = label;
    document.querySelectorAll('.agent-entry').forEach(el => {
        el.classList.toggle('active', el.dataset.label === label);
    });
    renderAgentDetail(label);
    loadAgentLog(label);
}

function renderAgentDetail(label) {
    const a = agentEntries.find(e => e.label === label);
    if (!a) return;
    const header = document.getElementById('agentHeader');
    const badge = document.getElementById('agentBadge');
    const title = document.getElementById('agentTitle');
    const grid = document.getElementById('agentGrid');
    const reasons = document.getElementById('agentReasons');
    header.style.display = 'flex';
    badge.className = 'badge ' + (a.health || 'unknown');
    title.textContent = a.label;

    const rows = [];
    const push = (k, v) => { if (v !== undefined && v !== null && v !== '' && v !== 0) rows.push([k, v]); };
    push('program', (a.programArguments && a.programArguments.join(' ')) || a.program);
    push('plist', a.plistPath);
    if (a.startInterval) push('schedule', 'StartInterval ' + a.startInterval + 's (' + formatInterval(a.startInterval) + ')');
    if (a.startCalendar) push('schedule', 'StartCalendarInterval ' + a.startCalendar);
    if (a.runAtLoad) push('runAtLoad', 'true');
    push('stdout', a.stdoutPath);
    if (a.stderrPath && a.stderrPath !== a.stdoutPath) push('stderr', a.stderrPath);
    // effectiveLog is the file actually used for the stale-log health rule
    // and the tail below — useful to surface when it differs from stdout
    // (e.g. wrapper script's foo.log vs launchd's foo.launchd.log).
    if (a.effectiveLog && a.effectiveLog !== a.stdoutPath) push('effective log', a.effectiveLog);
    if (a.otherLogs && a.otherLogs.length) push('other logs', a.otherLogs.join('\n'));
    push('state', (a.loaded ? a.state : 'not loaded'));
    push('pid', a.pid);
    if (a.lastExitCode !== undefined && a.lastExitCode !== null) push('last exit', a.lastExitCode);
    push('runs', a.runs);
    if (a.logMTime) push('log mtime', a.logMTime);
    if (a.pidAgeSeconds) push('pid age', a.pidAgeSeconds + 's');

    grid.innerHTML = rows.map(r =>
        '<div class="k">' + escapeHtml(r[0]) + '</div><div class="v">' + escapeHtml(String(r[1])) + '</div>'
    ).join('');

    reasons.className = 'reasons ' + (a.health || 'unknown');
    reasons.innerHTML = (a.healthReasons || []).map(r => '<li>' + escapeHtml(r) + '</li>').join('');
}

async function loadAgentLog(label) {
    const view = document.getElementById('agentLog');
    view.textContent = 'Loading...';
    try {
        const res = await fetch('/api/launch-agent-log?label=' + encodeURIComponent(label));
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        const text = await res.text();
        // Show only the last ~40 lines per the spec — keep the rest available by scrolling.
        const lines = text.split('\n');
        const tail = lines.slice(Math.max(0, lines.length - 40)).join('\n');
        view.textContent = tail || '(no entries)';
    } catch (e) {
        view.textContent = 'Error: ' + e.message;
    }
}

function escapeHtml(s) {
    return String(s).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
}

// ---------- Zones tab ----------
function formatBytes(n) {
    if (n >= (1<<30)) return (n/(1<<30)).toFixed(1) + 'GB';
    if (n >= (1<<20)) return (n/(1<<20)).toFixed(0) + 'MB';
    if (n >= (1<<10)) return (n/(1<<10)).toFixed(0) + 'KB';
    return n + 'B';
}

async function loadZones() {
    const hours = document.getElementById('zoneHours').value;
    const status = document.getElementById('zoneStatus');
    const tbody = document.getElementById('zoneTableBody');
    status.textContent = 'Loading...';
    try {
        const res = await fetch('/api/zones?hours=' + hours);
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        const data = await res.json();
        renderZoneChart(data.series || []);
        const rows = (data.table || []).map(z =>
            '<tr><td>' + escapeHtml(z.Name) + '</td>' +
            '<td>' + formatBytes(z.CurrentBytes) + '</td>' +
            '<td>' + formatBytes(z.PeakBytes) + '</td>' +
            '<td>' + formatBytes(Math.round(z.AvgBytes)) + '</td>' +
            '<td>' + z.ElemSize + 'B</td></tr>'
        ).join('');
        tbody.innerHTML = rows || '<tr><td colspan="5" style="color:#484f58">(no zone samples)</td></tr>';
        zonesLoaded = true;
        status.textContent = 'Loaded ' + new Date().toLocaleTimeString();
    } catch (e) {
        tbody.innerHTML = '<tr><td colspan="5" style="color:#ff6384">Error: ' + escapeHtml(e.message) + '</td></tr>';
        status.textContent = '';
    }
}

function renderZoneChart(series) {
    // Build a unified timeline by union of all sample timestamps. Different
    // zones may have slightly different sample sets if one was absent in some
    // ticks — joining by time keeps the chart honest rather than aligning by
    // index.
    const timeSet = new Set();
    series.forEach(s => (s.Times || []).forEach(t => timeSet.add(t)));
    const times = Array.from(timeSet).sort();
    const labels = times.map(t => {
        const d = new Date(t);
        return (d.getMonth()+1) + '/' + d.getDate() + ' ' + String(d.getHours()).padStart(2,'0') + ':' + String(d.getMinutes()).padStart(2,'0');
    });

    const datasets = series.map((s, i) => {
        const color = ['#ff6384','#36a2eb','#ffce56','#4bc0c0','#9966ff'][i % 5];
        const tmap = {};
        (s.Times || []).forEach((t, j) => { tmap[t] = s.Values[j]; });
        return {
            label: s.Name,
            data: times.map(t => (tmap[t] !== undefined) ? (tmap[t] / (1<<20)) : null),
            borderColor: color,
            backgroundColor: color + '22',
            fill: false,
            borderWidth: 1.5,
            pointRadius: 0,
            pointHoverRadius: 5,
            pointHitRadius: 15,
            tension: 0.3,
            spanGaps: true
        };
    });

    makeChart('zoneChart', {
        type: 'line',
        data: { labels: labels, datasets: datasets },
        options: {
            responsive: true,
            ...hoverConfig,
            scales: {
                y: {
                    ...defaultScaleY,
                    min: 0,
                    ticks: { ...defaultScaleY.ticks, callback: v => v + 'MB' },
                    title: { display: true, text: 'est_bytes (MB)', color: '#484f58' }
                },
                x: defaultScaleX
            },
            plugins: {
                ...hoverConfig.plugins,
                legend: { labels: { color: '#8b949e', boxWidth: 10, padding: 12 } }
            }
        }
    });
}

let zoneSortCol = -1, zoneSortAsc = true;
function sortZoneTable(col) {
    const tbody = document.getElementById('zoneTableBody');
    const rows = Array.from(tbody.querySelectorAll('tr'));
    if (rows.length < 2) return;
    if (zoneSortCol === col) { zoneSortAsc = !zoneSortAsc; } else { zoneSortCol = col; zoneSortAsc = col === 0; }
    rows.sort((a, b) => {
        let av = (a.cells[col] || {}).textContent || '';
        let bv = (b.cells[col] || {}).textContent || '';
        if (col === 0) return zoneSortAsc ? av.localeCompare(bv) : bv.localeCompare(av);
        // Convert formatted byte strings back to a sortable number.
        const parse = s => {
            const m = s.match(/([0-9.]+)\s*(GB|MB|KB|B)?/);
            if (!m) return 0;
            const n = parseFloat(m[1]);
            switch ((m[2]||'').toUpperCase()) {
                case 'GB': return n * (1<<30);
                case 'MB': return n * (1<<20);
                case 'KB': return n * (1<<10);
                default:   return n;
            }
        };
        const an = parse(av), bn = parse(bv);
        return zoneSortAsc ? an - bn : bn - an;
    });
    rows.forEach(r => tbody.appendChild(r));
    document.querySelectorAll('#zoneTable th .sort-arrow').forEach((el, i) => {
        el.textContent = i === col ? (zoneSortAsc ? ' ▲' : ' ▼') : '';
    });
}

// ---------- Alerts tab ----------
function alertKindPill(kind) {
    const k = String(kind || '').toLowerCase();
    let cls = 'default';
    if (k.includes('pressure')) cls = 'pressure';
    else if (k.includes('swap')) cls = 'swap';
    else if (k.includes('zone')) cls = 'zone';
    else if (k.includes('process') || k.includes('proc')) cls = 'process';
    return '<span class="pill ' + cls + '">' + escapeHtml(kind) + '</span>';
}

function formatAlertValue(kind, v) {
    const k = String(kind || '').toLowerCase();
    if (v === null || v === undefined) return '-';
    if (k.includes('pressure')) return v.toFixed(0) + '%';
    if (k.includes('swap'))     return v.toFixed(1) + 'GB';
    if (k.includes('zone') || k.includes('bytes')) return formatBytes(v);
    return String(v);
}

async function loadAlerts() {
    const status = document.getElementById('alertStatus');
    const tbody = document.getElementById('alertsTableBody');
    status.textContent = 'Loading...';
    try {
        const res = await fetch('/api/alerts');
        if (!res.ok) throw new Error('HTTP ' + res.status + ' — ' + await res.text());
        const rows = await res.json();
        if (!rows || rows.length === 0) {
            tbody.innerHTML = '<tr><td colspan="3" style="color:#484f58">No alerts on record. Email-alerts will land here when threshold rules fire.</td></tr>';
        } else {
            tbody.innerHTML = rows.map(a => {
                const t = new Date(a.lastSent);
                const isoLocal = isNaN(t) ? a.lastSent : t.toLocaleString();
                return '<tr>' +
                    '<td>' + alertKindPill(a.kind) + '</td>' +
                    '<td>' + escapeHtml(isoLocal) + '</td>' +
                    '<td>' + escapeHtml(formatAlertValue(a.kind, a.lastValue)) + '</td>' +
                '</tr>';
            }).join('');
        }
        alertsLoaded = true;
        status.textContent = 'Loaded ' + new Date().toLocaleTimeString();
    } catch (e) {
        tbody.innerHTML = '<tr><td colspan="3" style="color:#ff6384">Error: ' + escapeHtml(e.message) + '<br>(Alerts endpoint requires watchdog serve.)</td></tr>';
        status.textContent = '';
    }
}

let alertsSortCol = -1, alertsSortAsc = true;
function sortAlertsTable(col) {
    const tbody = document.getElementById('alertsTableBody');
    const rows = Array.from(tbody.querySelectorAll('tr'));
    if (rows.length < 2) return;
    if (alertsSortCol === col) { alertsSortAsc = !alertsSortAsc; } else { alertsSortCol = col; alertsSortAsc = col !== 1; }
    rows.sort((a, b) => {
        const av = (a.cells[col] || {}).textContent.trim();
        const bv = (b.cells[col] || {}).textContent.trim();
        if (col === 1) {
            // Date sort
            return alertsSortAsc ? (new Date(av) - new Date(bv)) : (new Date(bv) - new Date(av));
        }
        if (col === 2) {
            const an = parseFloat(av.replace(/[^0-9.\-]/g, '')) || 0;
            const bn = parseFloat(bv.replace(/[^0-9.\-]/g, '')) || 0;
            return alertsSortAsc ? an - bn : bn - an;
        }
        return alertsSortAsc ? av.localeCompare(bv) : bv.localeCompare(av);
    });
    rows.forEach(r => tbody.appendChild(r));
    document.querySelectorAll('#alertsTable th .sort-arrow').forEach((el, i) => {
        el.textContent = i === col ? (alertsSortAsc ? ' ▲' : ' ▼') : '';
    });
}

// ---------- Keyboard shortcuts ----------
const TAB_KEYS = {
    '1': 'dashboard',
    '2': 'ssh',
    '3': 'crons',
    '4': 'agents',
    '5': 'zones',
    '6': 'alerts'
};

function toggleShortcuts(force) {
    const el = document.getElementById('shortcutsOverlay');
    if (typeof force === 'boolean') el.classList.toggle('open', force);
    else el.classList.toggle('open');
}

document.addEventListener('keydown', (e) => {
    // Bail if focus is in an input/select/textarea — don't hijack typing.
    const t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'SELECT' || t.tagName === 'TEXTAREA' || t.isContentEditable)) return;
    if (e.metaKey || e.ctrlKey || e.altKey) return;

    if (e.key === 'Escape') { toggleShortcuts(false); return; }
    if (e.key === '?') { e.preventDefault(); toggleShortcuts(); return; }
    if (e.key === 'r' || e.key === 'R') { e.preventDefault(); refreshDashboard(); return; }
    const tab = TAB_KEYS[e.key];
    if (tab) { e.preventDefault(); setTab(tab); }
});

updateCharts();
</script>
</body>
</html>`

func escapeJS(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	return s
}

func escapeJSString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `'`, `\'`)
	s = strings.ReplaceAll(s, "\n", `\n`)
	s = strings.ReplaceAll(s, "\r", "")
	return s
}

func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
