//go:build !darwin || !cgo

package main

// setMenubarFontSize is a no-op everywhere except darwin + cgo. The
// AppKit private-ivar dance only makes sense for an actual NSStatusItem.
func setMenubarFontSize(size float64) {}
