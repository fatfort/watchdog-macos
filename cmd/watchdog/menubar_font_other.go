//go:build !darwin || !cgo

package main

// setMenubarCombined is a no-op everywhere except darwin + cgo.
func setMenubarCombined(size float64, icon1, row1l, row2l, icon2, row1r, row2r string) {
}
