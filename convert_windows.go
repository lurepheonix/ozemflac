//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// convertToAAC uses ffmpeg with its built-in aac encoder to encode a
// lossless file to AAC (Windows). Windows ffmpeg builds typically lack the
// non-free libfdk_aac encoder used on Linux, so the native encoder is used
// at the same 256k target bitrate.
func convertToAAC(src, dst string, preserveArt bool) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".m4a"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	tmp := dst + ".tmp.m4a"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	args := []string{
		"-y",
		"-i", src,
		"-map", "0:a",
		"-c:a", "aac",
		"-b:a", "256k",
		"-map_metadata", "0",
		"-movflags", "+faststart",
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
