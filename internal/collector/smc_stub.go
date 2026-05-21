//go:build !darwin || !cgo

package collector

import "errors"

// readSMCThermal stub for non-darwin or cgo-disabled builds. The collector
// soft-fails on this error so the rest of the sample still goes through.
func readSMCThermal() (float64, int, error) {
	return 0, 0, errors.New("SMC unavailable: requires darwin + cgo")
}

// SMCDebugRow / DumpThermalKeys stubs mirror the darwin signature so the
// thermal-debug subcommand compiles on non-darwin builds.
type SMCDebugRow struct {
	Key     string
	Kind    string
	Found   bool
	ReadErr string
	Type    string
	Raw     []byte
	Decoded float64
	OK      bool
}

func DumpThermalKeys() ([]SMCDebugRow, error) {
	return nil, errors.New("SMC unavailable: requires darwin + cgo")
}

func SortDebugRows(in []SMCDebugRow) []SMCDebugRow { return in }
