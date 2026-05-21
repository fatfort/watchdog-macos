//go:build !darwin || !cgo

package collector

import "errors"

// readSMCThermal stub for non-darwin or cgo-disabled builds. The collector
// soft-fails on this error so the rest of the sample still goes through.
func readSMCThermal() (float64, int, error) {
	return 0, 0, errors.New("SMC unavailable: requires darwin + cgo")
}
