//go:build darwin

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// afconvertExoticExt lists lossless extensions afconvert cannot decode.
// These go straight through ffmpeg decode to WAV before afconvert.
var afconvertExoticExt = map[string]bool{
	".ape": true,
	".wv":  true,
	".tta": true,
}

// convertToAAC uses afconvert to encode a lossless file to high-quality AAC (macOS)
func convertToAAC(src, dst string, preserveArt bool) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".m4a"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	encodeSrc := src
	if afconvertExoticExt[strings.ToLower(filepath.Ext(src))] {
		tmpWav := dst + ".src.wav"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		dec := exec.CommandContext(ctx, "ffmpeg",
			"-y", "-v", "error",
			"-i", src,
			"-map", "0:a",
			"-c:a", "pcm_s16le",
			tmpWav,
		)
		decErr := dec.Run()
		cancel()
		if decErr != nil {
			_ = os.Remove(tmpWav)
			return decErr
		}
		encodeSrc = tmpWav
		defer os.Remove(tmpWav)
	}

	cmd := exec.Command(
		"/usr/bin/afconvert",
		"-f", "m4af",
		"-d", "aac",
		"-b", "256000",
		encodeSrc,
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
	metaArgs := []string{
		"-y",
		"-i", src,
		"-i", dst,
		"-map", "1:a",
		"-map_metadata", "0",
		"-c", "copy",
	}
	if preserveArt {
		metaArgs = append(metaArgs,
			"-map", "0:v?",
			"-c:v", "copy",
			"-disposition:v", "attached_pic",
		)
	}
	metaArgs = append(metaArgs, tmp)
	metaCmd := exec.Command(
		"ffmpeg",
		metaArgs...,
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
