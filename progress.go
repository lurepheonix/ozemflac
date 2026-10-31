package main

import (
	"fmt"
	"strings"
)

const progressBarLen = 40

// formatProgress renders one convert/sync progress line. The leading \r\033[K
// clears the current terminal line so a shorter filename cannot leave
// trailing characters from the previous, longer line. The filename is
// truncated to fit the terminal width so long names do not wrap.
func formatProgress(processed, total int, name string) string {
	percent := float64(processed) / float64(total) * 100
	filled := int(percent / 100 * float64(progressBarLen))
	if filled < 0 {
		filled = 0
	}
	if filled > progressBarLen {
		filled = progressBarLen
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("-", progressBarLen-filled)
	prefix := fmt.Sprintf("[%s] %.1f%%  ", bar, percent)
	name = truncateVisible(name, termWidth()-visibleWidth(prefix))
	return "\r\033[K" + prefix + name
}
