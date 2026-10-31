//go:build darwin || linux

package main

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func termWidth() int {
	if s := os.Getenv("COLUMNS"); s != "" {
		if w, err := strconv.Atoi(s); err == nil && w > 20 {
			return w
		}
	}
	if isTerminal(os.Stdout) {
		if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 20 {
			return int(ws.Col)
		}
	}
	return 80
}
