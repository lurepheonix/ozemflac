package main

import (
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

var ansiRegexp = regexp.MustCompile("\x1b\\[[0-9;]*m")

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

func visibleWidth(s string) int {
	return runewidth.StringWidth(stripANSI(s))
}

func truncateVisible(s string, w int) string {
	if visibleWidth(s) <= w {
		return s
	}
	if w < 1 {
		return ""
	}
	// Need to truncate to w-1 + "…"
	target := w - 1
	var b strings.Builder
	width := 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if width+rw > target {
			break
		}
		b.WriteRune(r)
		width += rw
	}
	b.WriteRune('…')
	return b.String()
}

func padRightVisible(s string, w int) string {
	vis := visibleWidth(s)
	if vis >= w {
		return s
	}
	return s + strings.Repeat(" ", w-vis)
}
