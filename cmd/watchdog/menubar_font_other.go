//go:build !darwin || !cgo

package main

// setMenubarPill is a no-op everywhere except darwin + cgo.
func setMenubarPill(textSize, iconSize float64,
	icon1, row1l, row2l, icon2, row1r, row2r string,
) {
}
