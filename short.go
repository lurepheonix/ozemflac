package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type shortTrackResult struct {
	Rel      string
	Duration float64
	IsShort  bool
}

type shortDirResult struct {
	Dir    string
	Tracks []shortTrackResult // all probed tracks, sorted by base name
	Short  int
	Total  int
}

// shortTracks returns slash-form rels of tracks inside the match window.
func (r shortDirResult) shortList() []shortTrackResult {
	var out []shortTrackResult
	for _, t := range r.Tracks {
		if t.IsShort {
			out = append(out, t)
		}
	}
	return out
}

func runShort(args []string) {
	fs := flag.NewFlagSet("short", flag.ExitOnError)
	showAll := fs.Bool("all", false, "show dirs without short tracks in addition to matches")
	jsonOut := fs.Bool("json", false, "output JSON")
	under := fs.Float64("under", 0, "upper duration bound in seconds (tracks with duration <= under are listed)")
	over := fs.Float64("over", 0, "lower duration bound in seconds, inclusive (tracks with duration >= over are listed)")
	if err := fs.Parse(args); err != nil {
		fatal(err.Error())
	}
	if *under <= 0 {
		fatal("usage: ozemflac short [--all] [--json] --under <seconds> [--over <seconds>] <source_dir>")
	}
	if *over < 0 {
		fatal("--over must be >= 0")
	}
	hasOver := *over > 0
	if hasOver && *over > *under {
		fatal("--over must be <= --under")
	}

	rest := fs.Args()
	if len(rest) != 1 {
		fatal("usage: ozemflac short [--all] [--json] --under <seconds> [--over <seconds>] <source_dir>")
	}
	srcRoot := filepath.Clean(rest[0])
	if info, err := os.Stat(srcRoot); err != nil {
		fatal(fmt.Sprintf("source directory %q: %v", srcRoot, err))
	} else if !info.IsDir() {
		fatal(fmt.Sprintf("source %q is not a directory", srcRoot))
	}

	var scanned atomic.Int64
	var probed atomic.Int64
	var phase atomic.Value
	phase.Store("scanning")
	done := make(chan struct{})
	useColorSpinner := isTerminal(os.Stderr)
	useColor := !*jsonOut && isTerminal(os.Stdout)
	if !*jsonOut {
		go withSpinnerAfter(1*time.Second, func() string {
			if p, ok := phase.Load().(string); ok && p == "probing" {
				return fmt.Sprintf("Probing… %d files", probed.Load())
			}
			return fmt.Sprintf("Scanning… %d files", scanned.Load())
		}, done, useColorSpinner)
	}

	sr, err := scanSource(srcRoot, activePresetForCovers(), func(n int) { scanned.Store(int64(n)) })
	if err != nil {
		close(done)
		fatal(err.Error())
	}

	var musicDirs []string
	for dir := range sr.dirHasMusicCandidate {
		musicDirs = append(musicDirs, dir)
	}
	sort.Strings(musicDirs)

	// Fail fast when probing is needed but ffprobe is unavailable.
	if len(musicDirs) > 0 {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			close(done)
			fatal("ffprobe not found in $PATH — install ffmpeg")
		}
		phase.Store("probing")
	}

	threshold := *under
	lower := *over
	var results []shortDirResult
	var failed []string
	for _, dir := range musicDirs {
		var tracks []shortTrackResult
		short := 0
		for _, c := range musicCandidatesInDir(sr, dir) {
			probed.Add(1)
			dur, err := audioDurationSec(c.src)
			if err != nil {
				failed = append(failed, fmt.Sprintf("%s: %v", c.rel, err))
				continue
			}
			isShort := dur <= threshold && dur >= lower
			if isShort {
				short++
			}
			tracks = append(tracks, shortTrackResult{Rel: c.rel, Duration: dur, IsShort: isShort})
		}
		results = append(results, shortDirResult{Dir: dir, Tracks: tracks, Short: short, Total: len(tracks)})
	}
	close(done)
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}
	for _, f := range failed {
		fmt.Fprintf(os.Stderr, "warning: could not probe duration for %s\n", f)
	}

	if *jsonOut {
		printShortJSON(srcRoot, lower, threshold, hasOver, results, *showAll)
		return
	}
	printShortText(srcRoot, lower, threshold, hasOver, results, *showAll, useColor)
	if len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "%d file(s) could not be probed\n", len(failed))
	}
}

// audioDurationSec returns the audio duration in seconds via ffprobe.
func audioDurationSec(path string) (float64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return 0, fmt.Errorf("ffprobe timeout for %q", path)
	}
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(out))
	if s == "" || s == "N/A" {
		return 0, fmt.Errorf("no duration for %q", path)
	}
	dur, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	return dur, nil
}

// formatShortDuration renders seconds as M:SS (or H:MM:SS), rounded to the
// nearest second: 12 -> "0:12", 62 -> "1:02".
func formatShortDuration(sec float64) string {
	total := int(math.Round(sec))
	if total < 0 {
		total = 0
	}
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

func shortDisplayName(dir string) string {
	if dir == "." {
		return ". (root)"
	}
	return dir
}

func printShortText(srcRoot string, over, under float64, hasOver bool, results []shortDirResult, showAll, useColor bool) {
	var matchDirs, okDirs, matchTracks int
	for _, r := range results {
		if r.Short > 0 {
			matchDirs++
			matchTracks += r.Short
		} else {
			okDirs++
		}
	}

	shown := 0
	matchDesc, okDesc, inDesc := shortDescs(over, under, hasOver)
	for _, r := range results {
		name := shortDisplayName(r.Dir)
		if r.Short > 0 {
			shown++
			fmt.Printf("%s [%s]\n",
				colorize(name, colorRed, useColor),
				colorize(fmt.Sprintf("short — %d/%d %s", r.Short, r.Total, matchDesc), colorRed, useColor))
			for _, t := range r.shortList() {
				fmt.Printf("  ├── %s [%s]\n", filepath.Base(t.Rel),
					colorize(formatShortDuration(t.Duration), colorRed, useColor))
			}
		} else {
			if !showAll {
				continue
			}
			shown++
			fmt.Printf("%s [%s]\n", name, colorize(fmt.Sprintf("ok — %d/%d %s", r.Total, r.Total, okDesc), colorGreen, useColor))
		}
	}
	if shown == 0 && !showAll {
		fmt.Printf("%s: no tracks %s\n", srcRoot, inDesc)
	}
	fmt.Printf("\nSummary: %s dirs with short tracks (%s tracks %s), %s ok\n",
		colorize(fmt.Sprintf("%d", matchDirs), colorRed, useColor),
		colorize(fmt.Sprintf("%d", matchTracks), colorRed, useColor),
		inDesc,
		colorize(fmt.Sprintf("%d", okDirs), colorGreen, useColor),
	)
}

// shortDescs builds human-readable window descriptions: match ("under 1:30"
// or "1:00-1:30"), ok ("over 1:30" or "outside 1:00-1:30") and in ("under
// 1:30" or "in 1:00-1:30").
func shortDescs(over, under float64, hasOver bool) (matchDesc, okDesc, inDesc string) {
	if !hasOver {
		threshStr := formatShortDuration(under)
		return "under " + threshStr, "over " + threshStr, "under " + threshStr
	}
	window := formatShortDuration(over) + "-" + formatShortDuration(under)
	return window, "outside " + window, "in " + window
}

func printShortJSON(srcRoot string, over, under float64, hasOver bool, results []shortDirResult, showAll bool) {
	type jsonTrack struct {
		Rel      string  `json:"rel"`
		Duration float64 `json:"duration"`
	}
	type jsonShort struct {
		Dir    string      `json:"dir"`
		Short  int         `json:"short"`
		Total  int         `json:"total"`
		Tracks []jsonTrack `json:"tracks"`
	}
	type jsonOK struct {
		Dir   string `json:"dir"`
		Total int    `json:"total"`
	}
	out := struct {
		Source    string      `json:"source"`
		Threshold float64     `json:"thresholdSec"`
		Over      float64     `json:"overSec,omitempty"`
		Short     []jsonShort `json:"short"`
		OK        []jsonOK    `json:"ok,omitempty"`
	}{
		Source:    srcRoot,
		Threshold: under,
	}
	if hasOver {
		out.Over = over
	}
	for _, r := range results {
		if r.Short > 0 {
			tracks := []jsonTrack{}
			for _, t := range r.shortList() {
				tracks = append(tracks, jsonTrack{Rel: t.Rel, Duration: t.Duration})
			}
			out.Short = append(out.Short, jsonShort{Dir: r.Dir, Short: r.Short, Total: r.Total, Tracks: tracks})
		} else if showAll {
			out.OK = append(out.OK, jsonOK{Dir: r.Dir, Total: r.Total})
		}
	}
	if out.Short == nil {
		out.Short = []jsonShort{}
	}
	if showAll && out.OK == nil {
		out.OK = []jsonOK{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}
