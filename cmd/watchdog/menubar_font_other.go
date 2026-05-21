//go:build !darwin || !cgo

package main

// setMenubarPrimary / setMenubarSecondary are no-ops everywhere except
// darwin + cgo. The AppKit private-ivar dance only makes sense for an
// actual NSStatusItem.
func setMenubarPrimary(size float64, symbol, row1, row2 string)   {}
func setMenubarSecondary(size float64, symbol, row1, row2 string) {}
