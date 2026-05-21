//go:build !darwin || !cgo

package main

// setPrimaryWidget / setSecondaryWidget are no-ops everywhere except
// darwin + cgo.
func setPrimaryWidget(textSize, iconSize float64, symbol, row1, row2 string)   {}
func setSecondaryWidget(textSize, iconSize float64, symbol, row1, row2 string) {}
