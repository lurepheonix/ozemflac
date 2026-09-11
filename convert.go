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
func convertLossless(src, dst string, preset Preset, preserveArt bool) error {
	switch preset {
	case PresetMP3:
		return convertToMP3(src, dst, preserveArt)
	default:
		return convertToAAC(src, dst, preserveArt)
	}
}

// convertToMP3 uses ffmpeg with libmp3lame to encode to MP3 320k CBR (shared)
func convertToMP3(src, dst string, preserveArt bool) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".mp3"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmp := dst + ".tmp.mp3"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	args := []string{
		"-y",
		"-i", src,
		"-map", "0:a",
		"-c:a", "libmp3lame",
		"-b:a", "320k",
		"-map_metadata", "0",
	}
	if preserveArt {
		args = append(args,
			"-map", "0:v?",
			"-c:v", "copy",
			"-disposition:v", "attached_pic",
		)
	}
	args = append(args, tmp)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
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
