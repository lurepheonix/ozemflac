//go:build darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// convertToAAC uses afconvert to encode a lossless file to high-quality AAC (macOS)
func convertToAAC(src, dst string) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".m4a"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	cmd := exec.Command(
		"/usr/bin/afconvert",
		"-f", "m4af",
		"-d", "aac",
		"-b", "256000",
		src,
		dst,
	)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		_ = os.Remove(dst)
		return err
	}

	// Copy metadata from source to encoded file using ffmpeg
	tmp := dst + ".tmp.m4a"
	metaCmd := exec.Command(
		"ffmpeg",
		"-y",
		"-i", src,
		"-i", dst,
		"-map", "1:a",
		"-map_metadata", "0",
		"-c", "copy",
		tmp,
	)

	if err := metaCmd.Run(); err != nil {
		_ = os.Remove(dst)
		_ = os.Remove(tmp)
		return err
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(dst)
		return err
	}

	return nil
}
