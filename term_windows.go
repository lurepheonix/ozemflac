//go:build windows

package main

import (
	"os"
	"strconv"
)

// termWidth reports the terminal width on Windows. Without access to
// TIOCGWINSZ, honor COLUMNS when set and fall back to 80 columns.
func termWidth() int {
	if s := os.Getenv("COLUMNS"); s != "" {
		if w, err := strconv.Atoi(s); err == nil && w > 20 {
			return w
		}
	}
	return 80
}
