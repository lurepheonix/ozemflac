package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"
)

type coverStatus string

const (
	coverOKFile     coverStatus = "ok_file"
	coverOKEmbedded coverStatus = "ok_embedded"
	coverMissingNoArt coverStatus = "no_cover_no_embedded"
	coverMissingDisabled coverStatus = "extraction_disabled"
)

type coverDirResult struct {
	Dir    string
	Status coverStatus
}

func runCovers(args []string) {
	fs := flag.NewFlagSet("covers", flag.ExitOnError)
	showAll := fs.Bool("all", false, "show dirs with covers in addition to missing")
	jsonOut := fs.Bool("json", false, "output JSON")
	noExtractCover := fs.Bool("no-extract-cover", false, "do not consider embedded cover extraction")
	if err := fs.Parse(args); err != nil {
		fatal(err.Error())
	}
	defExtract := !*noExtractCover

	rest := fs.Args()
	if len(rest) != 1 {
		fatal("usage: ozemflac covers [--all] [--json] [--no-extract-cover] <source_dir>")
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

	// Dirs with music, sorted for stable output.
	var musicDirs []string
	for dir := range sr.dirHasMusicCandidate {
		musicDirs = append(musicDirs, dir)
	}
	sort.Strings(musicDirs)

	dirHasCover := make(map[string]bool)
	for _, c := range sr.candidates {
		if c.isCover {
			dirHasCover[c.dirRel] = true
		}
	}

	needSet := make(map[string]bool)
	for _, d := range dirsNeedingCoverExtraction(sr, defExtract) {
		needSet[d] = true
	}

	// Fail fast when probing is needed but ffprobe is unavailable.
	// hasEmbeddedArt would otherwise silently report "no art" for every dir.
	if len(needSet) > 0 {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			close(done)
			fatal("ffprobe not found in $PATH — install ffmpeg")
		}
		phase.Store("probing")
	}

	var results []coverDirResult
	for _, dir := range musicDirs {
		if dirHasCover[dir] {
			results = append(results, coverDirResult{Dir: dir, Status: coverOKFile})
			continue
		}
		if !needSet[dir] {
			// No cover file and extraction disabled for this dir.
			results = append(results, coverDirResult{Dir: dir, Status: coverMissingDisabled})
			continue
		}
		found := false
		for _, src := range musicFilesInDir(sr, dir) {
			probed.Add(1)
			if hasEmbeddedArt(src) {
				found = true
				break
			}
		}
		if found {
			results = append(results, coverDirResult{Dir: dir, Status: coverOKEmbedded})
		} else {
			results = append(results, coverDirResult{Dir: dir, Status: coverMissingNoArt})
		}
	}
	close(done)
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}

	if *jsonOut {
		printCoversJSON(srcRoot, results, *showAll)
		return
	}
	printCoversText(srcRoot, results, *showAll, useColor)
}

// activePresetForCovers returns a preset for scanSource. Covers don't depend
// on the preset, so the default is fine (scan records per-dir presets anyway).
func activePresetForCovers() Preset {
	if activePreset != "" {
		return activePreset
	}
	return PresetAAC
}

func coverDisplayName(dir string) string {
	if dir == "." {
		return ". (root)"
	}
	return dir
}

func coverReason(s coverStatus) string {
	switch s {
	case coverOKFile:
		return "cover file"
	case coverOKEmbedded:
		return "embedded → cover.jpg"
	case coverMissingDisabled:
		return "extraction disabled, no cover file"
	case coverMissingNoArt:
		return "no cover file, no embedded art"
	default:
		return string(s)
	}
}

func coverJSONReason(s coverStatus) string {
	switch s {
	case coverMissingDisabled:
		return "extraction_disabled"
	case coverMissingNoArt:
		return "no_cover_no_embedded"
	case coverOKFile:
		return "cover_file"
	case coverOKEmbedded:
		return "embedded"
	default:
		return string(s)
	}
}

func isCoverMissing(s coverStatus) bool {
	return s == coverMissingNoArt || s == coverMissingDisabled
}

func printCoversText(srcRoot string, results []coverDirResult, showAll, useColor bool) {
	var missing, okFile, okEmbedded int
	for _, r := range results {
		switch r.Status {
		case coverOKFile:
			okFile++
		case coverOKEmbedded:
			okEmbedded++
		default:
			missing++
		}
	}

	shown := 0
	for _, r := range results {
		miss := isCoverMissing(r.Status)
		if !miss && !showAll {
			continue
		}
		shown++
		name := coverDisplayName(r.Dir)
		reason := coverReason(r.Status)
		if miss {
			fmt.Printf("%s [%s]\n",
				colorize(name, colorRed, useColor),
				colorize("missing — "+reason, colorRed, useColor))
		} else {
			fmt.Printf("%s [%s]\n",
				name,
				colorize("ok — "+reason, colorGreen, useColor))
		}
	}
	if shown == 0 && !showAll {
		fmt.Printf("%s: all covers present\n", srcRoot)
	}
	fmt.Printf("\nSummary: %s dirs missing covers, %s ok (%s file, %s embedded)\n",
		colorize(fmt.Sprintf("%d", missing), colorRed, useColor),
		colorize(fmt.Sprintf("%d", okFile+okEmbedded), colorGreen, useColor),
		colorize(fmt.Sprintf("%d", okFile), colorGreen, useColor),
		colorize(fmt.Sprintf("%d", okEmbedded), colorGreen, useColor),
	)
}

func printCoversJSON(srcRoot string, results []coverDirResult, showAll bool) {
	type jsonMissing struct {
		Dir    string `json:"dir"`
		Reason string `json:"reason"`
	}
	type jsonOK struct {
		Dir    string `json:"dir"`
		Source string `json:"source"`
	}
	out := struct {
		Source  string        `json:"source"`
		Missing []jsonMissing `json:"missing,omitempty"`
		OK      []jsonOK      `json:"ok,omitempty"`
	}{
		Source: srcRoot,
	}
	for _, r := range results {
		if isCoverMissing(r.Status) {
			out.Missing = append(out.Missing, jsonMissing{Dir: r.Dir, Reason: coverJSONReason(r.Status)})
		} else if showAll {
			out.OK = append(out.OK, jsonOK{Dir: r.Dir, Source: coverJSONReason(r.Status)})
		}
	}
	// Encode empty slices as [] rather than null for stable output.
	if out.Missing == nil {
		out.Missing = []jsonMissing{}
	}
	if showAll && out.OK == nil {
		out.OK = []jsonOK{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}
