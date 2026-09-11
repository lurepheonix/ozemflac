package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ExtractedCoverName is the filename used when extracting embedded art.
const ExtractedCoverName = "cover.jpg"

// dirsNeedingCoverExtraction returns sorted dirRels that have music but no
// cover file and where extraction is enabled. If sr is nil it returns nil.
// defExtract (false with --no-extract-cover) is only the default: the nearest
// per-folder .ozemrc extract_cover setting wins when present.
func dirsNeedingCoverExtraction(sr *scanResult, defExtract bool) []string {
	if sr == nil {
		return nil
	}
	dirHasCover := make(map[string]bool)
	for _, c := range sr.candidates {
		if c.isCover {
			dirHasCover[c.dirRel] = true
		}
	}
	var dirs []string
	seen := make(map[string]bool)
	for _, dir := range sr.allDirs {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if !sr.dirHasMusicCandidate[dir] {
			continue
		}
		if dirHasCover[dir] {
			continue
		}
		if !effectiveExtractCoverFor(dir, sr.dirExtractCover, sr.dirHasExtractCover, defExtract) {
			continue
		}
		dirs = append(dirs, dir)
	}
	// Also cover dirs that only appear via candidates (safety)
	for dir := range sr.dirHasMusicCandidate {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		if dirHasCover[dir] {
			continue
		}
		if !effectiveExtractCoverFor(dir, sr.dirExtractCover, sr.dirHasExtractCover, defExtract) {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs
}

// musicFilesInDir returns sorted absolute source paths of music candidates in dir.
func musicFilesInDir(sr *scanResult, dirRel string) []string {
	var out []string
	for _, c := range sr.candidates {
		if c.dirRel == dirRel && c.isMusic {
			out = append(out, c.src)
		}
	}
	sort.Strings(out)
	return out
}

// hasEmbeddedArt reports whether src has an attached picture / video stream.
func hasEmbeddedArt(src string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "v",
		"-show_entries", "stream=codec_name:stream_disposition=attached_pic",
		"-of", "default=noprint_wrappers=1:nokey=1",
		src)
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// extractCoverFromMusic extracts the first available embedded picture from
// srcMusic to dstCover (always JPEG). Uses tmp+rename for safety.
func extractCoverFromMusic(srcMusic, dstCover string) error {
	if err := os.MkdirAll(filepath.Dir(dstCover), 0755); err != nil {
		return err
	}
	tmp := dstCover + ".tmp.jpg"
	_ = os.Remove(tmp)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-y",
		"-i", srcMusic,
		"-map", "0:v:0",
		"-frames:v", "1",
		tmp,
	)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if ctx.Err() == context.DeadlineExceeded {
		_ = os.Remove(tmp)
		return ctx.Err()
	}
	if st, err := os.Stat(tmp); err != nil || st.Size() == 0 {
		_ = os.Remove(tmp)
		if err == nil {
			err = os.ErrInvalid
		}
		return err
	}
	if err := os.Rename(tmp, dstCover); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// extractCoverForDir tries each music file in src dir until one succeeds.
// dstDir is the destination directory; output is always cover.jpg.
func extractCoverForDir(sr *scanResult, srcRoot, dstRoot, dirRel string) error {
	srcs := musicFilesInDir(sr, dirRel)
	if len(srcs) == 0 {
		return os.ErrNotExist
	}
	var dstDir string
	if dirRel == "." {
		dstDir = dstRoot
	} else {
		dstDir = filepath.Join(dstRoot, filepath.FromSlash(dirRel))
	}
	dstCover := filepath.Join(dstDir, ExtractedCoverName)
	var lastErr error
	for _, src := range srcs {
		if err := extractCoverFromMusic(src, dstCover); err == nil {
			return nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = os.ErrNotExist
	}
	return lastErr
}
