package collector

import (
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/abaj8494/macos-watchdog/internal/storage"
)

// readFixture loads a file from testdata/. Fails the test on error so callers
// don't have to repeat the boilerplate.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join("testdata", name)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", path, err)
	}
	return string(b)
}

// floatEq compares floats with a small absolute tolerance.
func floatEq(a, b, eps float64) bool {
	return math.Abs(a-b) < eps
}

func TestParseLoadAvg(t *testing.T) {
	var s storage.SystemSample
	if err := parseLoadAvg("{ 4.32 5.10 4.88 }", "10\n", &s); err != nil {
		t.Fatalf("parseLoadAvg: %v", err)
	}
	if !floatEq(s.Load1, 4.32, 1e-9) {
		t.Errorf("Load1 = %v, want 4.32", s.Load1)
	}
	if !floatEq(s.Load5, 5.10, 1e-9) {
		t.Errorf("Load5 = %v, want 5.10", s.Load5)
	}
	if !floatEq(s.Load15, 4.88, 1e-9) {
		t.Errorf("Load15 = %v, want 4.88", s.Load15)
	}
	if s.Ncpu != 10 {
		t.Errorf("Ncpu = %d, want 10", s.Ncpu)
	}
}

func TestParseLoadAvg_BadFormat(t *testing.T) {
	var s storage.SystemSample
	if err := parseLoadAvg("garbage", "8", &s); err == nil {
		t.Fatal("expected error for malformed loadavg, got nil")
	}
}

func TestParseSwap(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
	}{
		{
			name: "real M output",
			in:   "total = 3072.00M  used = 1826.06M  free = 1245.94M  (encrypted)",
			want: 1826.06 / 1024.0,
		},
		{
			name: "zero swap",
			in:   "total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)",
			want: 0.0,
		},
		{
			name: "GB units fallback",
			// No "M" suffix anywhere — exercises the "used = " fallback branch.
			in:   "total = 8.00G  used = 2.50G  free = 5.50G  (encrypted)",
			want: 2.50,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s storage.SystemSample
			if err := parseSwap(tt.in, &s); err != nil {
				t.Fatalf("parseSwap: %v", err)
			}
			if !floatEq(s.SwapUsedGB, tt.want, 1e-6) {
				t.Errorf("SwapUsedGB = %v, want %v", s.SwapUsedGB, tt.want)
			}
		})
	}
}

func TestParseVMStat(t *testing.T) {
	out := readFixture(t, "vm_stat.txt")
	var s storage.SystemSample
	if err := parseVMStat(out, &s); err != nil {
		t.Fatalf("parseVMStat: %v", err)
	}
	if s.Pageins != 33089057 {
		t.Errorf("Pageins = %d, want 33089057", s.Pageins)
	}
	if s.Pageouts != 20823 {
		t.Errorf("Pageouts = %d, want 20823", s.Pageouts)
	}
	if s.CompressorPages != 398370 {
		t.Errorf("CompressorPages = %d, want 398370", s.CompressorPages)
	}
	if s.Swapins != 64 {
		t.Errorf("Swapins = %d, want 64", s.Swapins)
	}
	if s.Swapouts != 120 {
		t.Errorf("Swapouts = %d, want 120", s.Swapouts)
	}
}

func TestParseProcesses(t *testing.T) {
	out := readFixture(t, "ps.txt")
	samples := parseProcesses(out)

	// Fixture has 19 data rows; TopProcessCount = 15, so we expect 15 samples.
	if len(samples) != TopProcessCount {
		t.Fatalf("len(samples) = %d, want %d", len(samples), TopProcessCount)
	}

	// Top-N must be RSS-descending.
	for i := 1; i < len(samples); i++ {
		if samples[i-1].RSSMB < samples[i].RSSMB {
			t.Errorf("not sorted: samples[%d].RSSMB=%d < samples[%d].RSSMB=%d",
				i-1, samples[i-1].RSSMB, i, samples[i].RSSMB)
		}
	}

	// Top row: claude pid=23521, rss=566272KB → 553MB, cpu=0.2.
	top := samples[0]
	if top.Name != "claude" {
		t.Errorf("top.Name = %q, want \"claude\"", top.Name)
	}
	if top.PID != 23521 {
		t.Errorf("top.PID = %d, want 23521", top.PID)
	}
	if top.RSSMB != 553 {
		t.Errorf("top.RSSMB = %d, want 553 (566272KB / 1024)", top.RSSMB)
	}
	if !floatEq(top.CPUPct, 0.2, 1e-9) {
		t.Errorf("top.CPUPct = %v, want 0.2", top.CPUPct)
	}

	// Spot-check normalization: /Applications/Arc.app/... → "Arc".
	// The 4th row in the fixture is Arc.app.
	var sawArc bool
	for _, p := range samples {
		if p.Name == "Arc" {
			sawArc = true
			break
		}
	}
	if !sawArc {
		t.Errorf("expected at least one process normalized to \"Arc\", got: %+v", samples)
	}
}

func TestParseZprintSize(t *testing.T) {
	tests := []struct {
		in   string
		want int64
	}{
		{"0K", 0},
		{"61K", 62464},                  // 61 * 1024
		{"365M", 382730240},              // 365 * 1024 * 1024
		{"9G", 9663676416},               // 9 * 1024 * 1024 * 1024
		{"----", 0},                      // sentinel for unbounded
		{"", 0},                          // empty
		{"123", 123},                     // no suffix → plain int
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseZprintSize(tt.in)
			if got != tt.want {
				t.Errorf("parseZprintSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseKernelZones(t *testing.T) {
	out := readFixture(t, "zprint.txt")
	zones := parseKernelZones(out)

	if len(zones) == 0 {
		t.Fatal("parseKernelZones returned no zones")
	}

	// Header lines must be skipped — none of them should appear as zone names.
	for _, z := range zones {
		switch z.Name {
		case "elem", "zone", "----":
			t.Errorf("header leaked into zone data: %q", z.Name)
		}
	}

	// Output is sorted by EstBytes descending.
	for i := 1; i < len(zones); i++ {
		if zones[i-1].EstBytes < zones[i].EstBytes {
			t.Errorf("not sorted at idx %d: %d < %d", i, zones[i-1].EstBytes, zones[i].EstBytes)
		}
	}

	// The synthetic data_shakalloc.1024 line has cur_size=9G → must sort to top
	// and use the kernel-reported size (9 GiB), not elem*inuse (1024*9437184 ≈ 9 GB).
	top := zones[0]
	if top.Name != "data_shakalloc.1024" {
		t.Errorf("top zone = %q, want data_shakalloc.1024", top.Name)
	}
	const nineGiB int64 = 9 * 1024 * 1024 * 1024
	if top.EstBytes != nineGiB {
		t.Errorf("top.EstBytes = %d, want %d (9 GiB)", top.EstBytes, nineGiB)
	}
	if top.ElemSize != 1024 {
		t.Errorf("top.ElemSize = %d, want 1024", top.ElemSize)
	}
	if top.InUse != 9437184 {
		t.Errorf("top.InUse = %d, want 9437184", top.InUse)
	}

	// Find data.kalloc.16 — cur_size=160K, so EstBytes must be 160*1024=163840,
	// NOT elem*inuse (16 * 108105 = 1729680). This is the "prefer cur_size" check.
	found := false
	for _, z := range zones {
		if z.Name == "data.kalloc.16" {
			found = true
			if z.EstBytes != 160*1024 {
				t.Errorf("data.kalloc.16 EstBytes = %d, want %d (cur_size 160K, NOT elem*inuse)",
					z.EstBytes, 160*1024)
			}
		}
	}
	if !found {
		t.Error("data.kalloc.16 not present in parsed zones")
	}

	// Flag-suffixed lines (trailing "X") must still parse. vm.pages.array has
	// cur_size=0K so it falls back to elem*inuse = 48 * 1494087 = 71716176.
	foundFlag := false
	for _, z := range zones {
		if z.Name == "vm.pages.array" {
			foundFlag = true
			want := int64(48) * 1494087
			if z.EstBytes != want {
				t.Errorf("vm.pages.array EstBytes = %d, want %d (elem*inuse fallback)", z.EstBytes, want)
			}
			if z.ElemSize != 48 {
				t.Errorf("vm.pages.array ElemSize = %d, want 48", z.ElemSize)
			}
			if z.InUse != 1494087 {
				t.Errorf("vm.pages.array InUse = %d, want 1494087", z.InUse)
			}
		}
	}
	if !foundFlag {
		t.Error("vm.pages.array (flag-suffixed line) not present in parsed zones")
	}

	// "----" sentinel cur_size must fall back to elem*inuse (data.mbuf.cluster.4k:
	// 4096 * 175 = 716800). It also has a trailing "X" flag.
	for _, z := range zones {
		if z.Name == "data.mbuf.cluster.4k" {
			want := int64(4096) * 175
			if z.EstBytes != want {
				t.Errorf("data.mbuf.cluster.4k EstBytes = %d, want %d (---- → elem*inuse fallback)",
					z.EstBytes, want)
			}
		}
	}
}
