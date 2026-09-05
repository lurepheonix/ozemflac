//go:build darwin || linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// convertLossless dispatches to the preset-specific converter
func convertLossless(src, dst string, preset Preset) error {
	switch preset {
	case PresetMP3:
		return convertToMP3(src, dst)
	default:
		return convertToAAC(src, dst)
	}
}

// convertToMP3 uses ffmpeg with libmp3lame to encode to MP3 320k CBR (shared)
func convertToMP3(src, dst string) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".mp3"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmp := dst + ".tmp.mp3"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-i", src,
		"-map", "0:a",
		"-c:a", "libmp3lame",
		"-b:a", "320k",
		"-map_metadata", "0",
		tmp,
	)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(dst)
		return err
	}
	if ctx.Err() == context.DeadlineExceeded {
		_ = os.Remove(tmp)
		_ = os.Remove(dst)
		return ctx.Err()
	}

	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(dst)
		return err
	}

	return nil
}
