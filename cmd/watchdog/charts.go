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
        * { margin: 0; padding: 0; box-sizing: border-box; }
        html { color-scheme: dark; }
        body {
            font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
            background: #0d1117;
            color: #c9d1d9;
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
    </style>
</head>
<body>
    <h1>Watchdog</h1>
    <p class="subtitle">System Health Dashboard &middot; Last sample: {{.LastUpdate}}</p>

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
