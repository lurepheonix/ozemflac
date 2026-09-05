//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// convertToAAC uses ffmpeg with libfdk_aac to encode a lossless file to high-quality AAC (Linux)
func convertToAAC(src, dst string) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".m4a"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmp := dst + ".tmp.m4a"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-i", src,
		"-map", "0:a",
		"-c:a", "libfdk_aac",
		"-b:a", "256k",
		"-profile:a", "aac_low",
		"-afterburner", "1",
		"-map_metadata", "0",
		"-movflags", "+faststart",
		tmp,
	)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		_ = os.Remove(dst)
		return err
	}
	// Ensure context timeout is surfaced
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
