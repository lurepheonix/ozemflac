package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

type coverStatus string

const (
	coverOKFile         coverStatus = "ok_file"
	coverOKEmbedded     coverStatus = "ok_embedded"
	coverPerTrack       coverStatus = "per_track"
	coverMissingNoArt   coverStatus = "no_cover_no_embedded"
	coverMissingStriped coverStatus = "embedded_stripped"
)

type coverTrackResult struct {
	Rel    string // slash-form rel from source root
	HasArt bool
}

type coverDirResult struct {
	Dir     string
	Status  coverStatus
	Tracks  []coverTrackResult // per-track detail (per_track status)
	WithArt int
	Total   int
}

// artless returns slash-form rels of tracks without embedded art.
func (r coverDirResult) artless() []string {
	var out []string
	for _, t := range r.Tracks {
		if !t.HasArt {
			out = append(out, t.Rel)
		}
	}
	return out
}

func runCovers(args []string) {
	fs := flag.NewFlagSet("covers", flag.ExitOnError)
	showAll := fs.Bool("all", false, "show dirs with covers in addition to missing")
	jsonOut := fs.Bool("json", false, "output JSON")
	noExtractCover := fs.Bool("no-extract-cover", false, "do not consider embedded cover extraction")
	preserveArtFlag := fs.Bool("preserve-embedded-art", false, "consider embedded art kept in converted files")
	if err := fs.Parse(args); err != nil {
		fatal(err.Error())
	}
	defExtract := !*noExtractCover
	defPreserve := *preserveArtFlag

	rest := fs.Args()
	if len(rest) != 1 {
		fatal("usage: ozemflac covers [--all] [--json] [--no-extract-cover] [--preserve-embedded-art] <source_dir>")
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

	// Fail fast when probing is needed but ffprobe is unavailable.
	// hasEmbeddedArt would otherwise silently report "no art" for every dir.
	needProbe := false
	for _, dir := range musicDirs {
		if !dirHasCover[dir] {
			needProbe = true
			break
		}
	}
	if needProbe {
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
		// Probe every track: per-track dirs need the full picture.
		var tracks []coverTrackResult
		withArt := 0
		hasConvertible := false
		for _, c := range musicCandidatesInDir(sr, dir) {
			probed.Add(1)
			has := hasEmbeddedArt(c.src)
			if has {
				withArt++
			}
			tracks = append(tracks, coverTrackResult{Rel: c.rel, HasArt: has})
			if !hasConvertible && isConvertibleSource(c.src) {
				hasConvertible = true
			}
		}
		r := coverDirResult{Dir: dir, Tracks: tracks, WithArt: withArt, Total: len(tracks)}
		switch {
		case withArt == 0:
			r.Status = coverMissingNoArt
		case effectiveExtractCoverFor(dir, sr.dirExtractCover, sr.dirHasExtractCover, defExtract):
			r.Status = coverOKEmbedded
		case effectivePreserveArtFor(dir, sr.dirPreserveArt, sr.dirHasPreserveArt, defPreserve) || !hasConvertible:
			// Art lives in the files and survives to dst (kept in
			// converted files, or copies which always keep it).
			r.Status = coverPerTrack
		default:
			r.Status = coverMissingStriped
		}
		results = append(results, r)
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

// musicCandidatesInDir returns music candidates in dir sorted by base name.
func musicCandidatesInDir(sr *scanResult, dirRel string) []candidate {
	var out []candidate
	for _, c := range sr.candidates {
		if c.dirRel == dirRel && c.isMusic {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(filepath.Base(out[i].rel)) < strings.ToLower(filepath.Base(out[j].rel))
	})
	return out
}

// isConvertibleSource reports whether src will be transcoded (art stripped
// unless preserved) rather than copied (copies always keep art).
// Unprobable .m4a counts as copy, matching diff's fallback.
func isConvertibleSource(src string) bool {
	ext := strings.ToLower(filepath.Ext(src))
	if losslessExt[ext] {
		return true
	}
	if ext == ".m4a" {
		alac, err := isALAC(src)
		return err == nil && alac
	}
	return false
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

func coverReason(r coverDirResult) string {
	switch r.Status {
	case coverOKFile:
		return "cover file"
	case coverOKEmbedded:
		return "embedded → cover.jpg"
	case coverPerTrack:
		return fmt.Sprintf("per-track art — %d/%d tracks with embedded art", r.WithArt, r.Total)
	case coverMissingStriped:
		return "embedded art will be stripped"
	case coverMissingNoArt:
		return "no cover file, no embedded art"
	default:
		return string(r.Status)
	}
}

func coverJSONReason(s coverStatus) string {
	switch s {
	case coverMissingStriped:
		return "embedded_stripped"
	case coverMissingNoArt:
		return "no_cover_no_embedded"
	case coverOKFile:
		return "cover_file"
	case coverOKEmbedded:
		return "embedded"
	case coverPerTrack:
		return "per_track"
	default:
		return string(s)
	}
}

func isCoverMissing(s coverStatus) bool {
	return s == coverMissingNoArt || s == coverMissingStriped
}

func printCoversText(srcRoot string, results []coverDirResult, showAll, useColor bool) {
	var missing, ok, perTrackDirs, perTrackGaps int
	for _, r := range results {
		switch {
		case isCoverMissing(r.Status):
			missing++
		case r.Status == coverPerTrack:
			perTrackDirs++
			perTrackGaps += len(r.artless())
		default:
			ok++
		}
	}

	shown := 0
	for _, r := range results {
		name := coverDisplayName(r.Dir)
		switch {
		case isCoverMissing(r.Status):
			shown++
			fmt.Printf("%s [%s]\n",
				colorize(name, colorRed, useColor),
				colorize("missing — "+coverReason(r), colorRed, useColor))
		case r.Status == coverPerTrack:
			if len(r.artless()) == 0 && !showAll {
				continue
			}
			shown++
			fmt.Printf("%s [%s]\n", name, colorize(coverReason(r), colorYellow, useColor))
			for _, rel := range r.artless() {
				fmt.Printf("  ├── %s %s\n", filepath.Base(rel),
					colorize("[no art]", colorRed, useColor))
			}
		default:
			if !showAll {
				continue
			}
			shown++
			fmt.Printf("%s [%s]\n", name, colorize("ok — "+coverReason(r), colorGreen, useColor))
		}
	}
	if shown == 0 && !showAll {
		fmt.Printf("%s: all covers present\n", srcRoot)
	}
	fmt.Printf("\nSummary: %s dirs missing covers, %s per-track dirs (%s tracks without art), %s ok\n",
		colorize(fmt.Sprintf("%d", missing), colorRed, useColor),
		colorize(fmt.Sprintf("%d", perTrackDirs), colorYellow, useColor),
		colorize(fmt.Sprintf("%d", perTrackGaps), colorRed, useColor),
		colorize(fmt.Sprintf("%d", ok), colorGreen, useColor),
	)
}

func printCoversJSON(srcRoot string, results []coverDirResult, showAll bool) {
	type jsonMissing struct {
		Dir    string `json:"dir"`
		Reason string `json:"reason"`
	}
	type jsonPerTrack struct {
		Dir        string   `json:"dir"`
		WithArt    int      `json:"withArt"`
		Total      int      `json:"total"`
		WithoutArt []string `json:"withoutArt"`
	}
	type jsonOK struct {
		Dir    string `json:"dir"`
		Source string `json:"source"`
	}
	out := struct {
		Source   string         `json:"source"`
		Missing  []jsonMissing `json:"missing,omitempty"`
		PerTrack []jsonPerTrack `json:"perTrack,omitempty"`
		OK       []jsonOK       `json:"ok,omitempty"`
	}{
		Source: srcRoot,
	}
	for _, r := range results {
		switch {
		case isCoverMissing(r.Status):
			out.Missing = append(out.Missing, jsonMissing{Dir: r.Dir, Reason: coverJSONReason(r.Status)})
		case r.Status == coverPerTrack:
			artless := r.artless()
			if artless == nil {
				artless = []string{}
			}
			out.PerTrack = append(out.PerTrack, jsonPerTrack{Dir: r.Dir, WithArt: r.WithArt, Total: r.Total, WithoutArt: artless})
		case showAll:
			out.OK = append(out.OK, jsonOK{Dir: r.Dir, Source: coverJSONReason(r.Status)})
		}
	}
	if out.Missing == nil {
		out.Missing = []jsonMissing{}
	}
	if out.PerTrack == nil {
		out.PerTrack = []jsonPerTrack{}
	}
	if showAll && out.OK == nil {
		out.OK = []jsonOK{}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(out)
}
