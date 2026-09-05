package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

var (
	losslessExt = map[string]bool{
		".flac": true,
		".alac": true,
	}

	lossyExt = map[string]bool{
		".mp3":  true,
		".aac":  true,
		".ogg":  true,
		".opus": true,
	}

	coverFiles = map[string]bool{
		"cover.jpg":  true,
		"folder.jpg": true,
		"cover.png":  true,
		"folder.png": true,
	}
)

type Job struct {
	Src    string
	Dst    string
	Preset Preset
}

type Preset string

const (
	PresetAAC Preset = "aac"
	PresetMP3 Preset = "mp3"
)

func (p Preset) Ext() string {
	switch p {
	case PresetAAC:
		return ".m4a"
	case PresetMP3:
		return ".mp3"
	default:
		return ".m4a"
	}
}

var activePreset Preset

type candidate struct {
	src     string
	rel     string
	dirRel  string
	isCover bool
	isMusic bool
	preset  Preset
}

type scanResult struct {
	candidates           []candidate
	ignoredFiles         []candidate
	dirPatterns          map[string][]gitignore.Pattern
	dirPresets           map[string]Preset
	dirHasPreset         map[string]bool
	allDirs              []string
	dirHasMusicCandidate map[string]bool
	subtreeHasMusic      map[string]bool
	ignoredCount         map[string]int
	totalScanned         int
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "tree" {
		runTree(os.Args[2:])
		return
	}

	workers := flag.Int("workers", 0, "number of parallel workers")
	presetFlag := flag.String("preset", "aac", "output preset: aac or mp3 (available presets: aac, mp3)")
	flag.Parse()

	// Strict lower-case validation, show available presets
	switch Preset(*presetFlag) {
	case PresetAAC, PresetMP3:
		activePreset = Preset(*presetFlag)
	default:
		fatal("invalid -preset \"" + *presetFlag + "\": available presets: aac, mp3")
	}

	args := flag.Args()
	if len(args) != 2 {
		fatal("usage: converter [-workers N] [-preset aac|mp3] <source_dir> <destination_dir> (available presets: aac, mp3)\n       ozemflac tree [--all] [--json] [--expand] [-preset aac|mp3] <source_dir>")
	}

	srcRoot := filepath.Clean(args[0])
	dstRoot := filepath.Clean(args[1])

	if srcRoot == dstRoot {
		fatal("source and destination must be different directories")
	}

	// Validate source exists and is a directory
	if info, err := os.Stat(srcRoot); err != nil {
		fatal(fmt.Sprintf("source directory %q: %v", srcRoot, err))
	} else if !info.IsDir() {
		fatal(fmt.Sprintf("source %q is not a directory", srcRoot))
	}

	// Destination guard: allow non-existent or empty existing directory
	if info, err := os.Stat(dstRoot); err == nil {
		if !info.IsDir() {
			fatal(fmt.Sprintf("destination %q exists and is not a directory", dstRoot))
		}
		entries, err := os.ReadDir(dstRoot)
		if err != nil {
			fatal(fmt.Sprintf("cannot read destination %q: %v", dstRoot, err))
		}
		if len(entries) > 0 {
			fatal("destination directory already exists and is not empty")
		}
	} else if !os.IsNotExist(err) {
		fatal(fmt.Sprintf("cannot stat destination %q: %v", dstRoot, err))
	}

	// Validate workers flag
	if *workers < 0 {
		fatal("workers must be >= 0")
	}

	// Fail fast if required external binaries are missing (always needed)
	if _, err := exec.LookPath("ffprobe"); err != nil {
		fatal("ffprobe not found in $PATH — install ffmpeg")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fatal("ffmpeg not found in $PATH — install ffmpeg")
	}

	// Gather all files with per-folder ignore/preset (calculate phase)
	sr, err := scanSource(srcRoot, activePreset, nil)
	if err != nil {
		fatal(err.Error())
	}

	// Now schedule jobs: cover only if subtree has music
	var jobs []Job
	needAAC := false
	needMP3 := false
	for _, c := range sr.candidates {
		if c.isCover {
			if !sr.subtreeHasMusic[c.dirRel] {
				continue
			}
			jobs = append(jobs, Job{Src: c.src, Dst: filepath.Join(dstRoot, filepath.FromSlash(c.rel)), Preset: c.preset})
			continue
		}
		if c.isMusic {
			if c.preset == PresetMP3 {
				needMP3 = true
			} else {
				needAAC = true
			}
			jobs = append(jobs, Job{Src: c.src, Dst: filepath.Join(dstRoot, filepath.FromSlash(c.rel)), Preset: c.preset})
		}
	}

	if len(jobs) > 0 {
		hasAACPreset := needAAC
		hasMP3Preset := needMP3
		if hasAACPreset {
			switch runtime.GOOS {
			case "linux":
				out, err := exec.Command("ffmpeg", "-encoders").Output()
				if err != nil || !strings.Contains(string(out), "libfdk_aac") {
					fatal("ffmpeg libfdk_aac encoder not available — install ffmpeg with --enable-libfdk-aac (e.g. ffmpeg-full)")
				}
			case "darwin":
				if _, err := os.Stat("/usr/bin/afconvert"); err != nil {
					fatal("afconvert not found at /usr/bin/afconvert — install Xcode Command Line Tools")
				}
			}
		}
		if hasMP3Preset {
			out, err := exec.Command("ffmpeg", "-encoders").Output()
			if err != nil || !strings.Contains(string(out), "libmp3lame") {
				fatal("ffmpeg libmp3lame encoder not available — install ffmpeg with --enable-libmp3lame")
			}
		}
		_ = hasAACPreset
		_ = hasMP3Preset
	}

	total := len(jobs)
	if total == 0 {
		fmt.Println("No files to process")
		return
	}
	fmt.Printf("Found %d files to process\n", total)

	// Determine worker count
	var numWorkers int
	if *workers > 0 {
		numWorkers = *workers
	} else {
		numWorkers = runtime.NumCPU() / 2
		if numWorkers < 1 {
			numWorkers = 1
		}
	}

	fmt.Printf("Using %d worker(s)\n", numWorkers)

	// Buffered channels to avoid blocking workers on progress printer lag
	jobChan := make(chan Job, numWorkers*2)
	doneChan := make(chan string, numWorkers*2)

	var wg sync.WaitGroup

	// Progress state
	var mu sync.Mutex
	processed := 0
	failed := 0
	var failedFiles []string

	// Progress printer - owns processed counter to reduce contention
	var printerWg sync.WaitGroup
	printerWg.Add(1)
	go func() {
		defer printerWg.Done()
		for name := range doneChan {
			mu.Lock()
			processed++
			percent := float64(processed) / float64(total) * 100
			barLen := 40
			filled := int(percent / 100 * float64(barLen))
			bar := strings.Repeat("█", filled) + strings.Repeat("-", barLen-filled)
			fmt.Printf("\r[%s] %.1f%%  %s", bar, percent, name)
			mu.Unlock()
		}
		fmt.Println()
		if failed > 0 {
			fmt.Printf("Done with %d error(s)\n", failed)
		} else {
			fmt.Println("Done!")
		}
	}()

	// Launch workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				baseName := filepath.Base(job.Src)
				err := processFile(job.Src, job.Dst, job.Preset)
				if err != nil {
					mu.Lock()
					failed++
					failedFiles = append(failedFiles, fmt.Sprintf("%s: %v", job.Src, err))
					mu.Unlock()
					fmt.Fprintf(os.Stderr, "\nerror processing %s: %v\n", job.Src, err)
				}
				doneChan <- baseName
			}
		}()
	}

	// Send jobs
	for _, job := range jobs {
		jobChan <- job
	}
	close(jobChan)

	wg.Wait()
	close(doneChan)
	printerWg.Wait()

	if failed > 0 {
		fmt.Fprintf(os.Stderr, "\n%d file(s) failed:\n", failed)
		for _, f := range failedFiles {
			fmt.Fprintln(os.Stderr, " ", f)
		}
		os.Exit(1)
	}
}

// scanSource walks srcRoot and collects candidates with stacked ignore/preset handling
// onProgress is called periodically with number of files scanned (or nil for no progress)
func scanSource(srcRoot string, defPreset Preset, onProgress func(int)) (*scanResult, error) {
	dirPatterns := make(map[string][]gitignore.Pattern)
	dirPresets := make(map[string]Preset)
	dirHasPreset := make(map[string]bool)
	var candidates []candidate
	var ignoredFiles []candidate
	dirHasMusicCandidate := make(map[string]bool)
	ignoredCount := make(map[string]int)
	var allDirs []string
	seenDirs := make(map[string]bool)
	scannedFiles := 0

	err := filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			relDir, err := filepath.Rel(srcRoot, path)
			if err != nil {
				return err
			}
			if relDir == "." {
				relDir = "."
			} else {
				relDir = filepath.ToSlash(relDir)
			}
			if !seenDirs[relDir] {
				seenDirs[relDir] = true
				allDirs = append(allDirs, relDir)
			}
			// Load .ozemignore
			ignorePath := filepath.Join(path, ".ozemignore")
			if data, err := os.ReadFile(ignorePath); err == nil {
				lines := strings.Split(string(data), "\n")
				var domain []string
				if relDir != "." {
					domain = strings.Split(relDir, "/")
				}
				var ps []gitignore.Pattern
				for _, s := range lines {
					if strings.HasPrefix(s, "#") || len(strings.TrimSpace(s)) == 0 {
						continue
					}
					ps = append(ps, gitignore.ParsePattern(s, domain))
				}
				if len(ps) > 0 {
					dirPatterns[relDir] = ps
				}
			}
			// Load .ozemrc
			rcPath := filepath.Join(path, ".ozemrc")
			if data, err := os.ReadFile(rcPath); err == nil {
				preset, ok, perr := parseOzemrc(string(data), path)
				if perr != nil {
					return perr
				}
				if ok {
					dirPresets[relDir] = preset
					dirHasPreset[relDir] = true
				}
			}
			return nil
		}

		// File handling
		scannedFiles++
		if onProgress != nil && scannedFiles%20 == 0 {
			onProgress(scannedFiles)
		}

		// Ignore macOS metadata files (extra safety, before gitignore)
		if d.Name() == ".DS_Store" {
			return nil
		}
		if strings.HasPrefix(d.Name(), "._") {
			return nil
		}
		// Never include config files themselves
		if d.Name() == ".ozemignore" || d.Name() == ".ozemrc" {
			return nil
		}

		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		dirRel := filepath.ToSlash(filepath.Dir(rel))
		if dirRel == "." && filepath.Dir(rel) == "." {
			dirRel = "."
		}
		if dirRel == "" {
			dirRel = "."
		}

		if !seenDirs[dirRel] {
			seenDirs[dirRel] = true
			allDirs = append(allDirs, dirRel)
		}

		ext := strings.ToLower(filepath.Ext(d.Name()))
		lcName := strings.ToLower(d.Name())

		isCover := coverFiles[lcName]
		isMusic := false
		if losslessExt[ext] || lossyExt[ext] || ext == ".m4a" {
			isMusic = true
		}
		isKnown := isCover || isMusic

		// Stacked gitignore check (after classification, so we can record ignored files)
		if isIgnored(relSlash, dirRel, dirPatterns) {
			ignoredCount[dirRel]++
			if isKnown {
				// Record for tree --all display
				effPreset := effectivePresetFor(dirRel, dirPresets, dirHasPreset, defPreset)
				ignoredFiles = append(ignoredFiles, candidate{
					src:     path,
					rel:     relSlash,
					dirRel:  dirRel,
					isCover: isCover,
					isMusic: isMusic,
					preset:  effPreset,
				})
			}
			return nil
		}

		if !isKnown {
			return nil
		}

		effPreset := effectivePresetFor(dirRel, dirPresets, dirHasPreset, defPreset)

		candidates = append(candidates, candidate{
			src:     path,
			rel:     relSlash,
			dirRel:  dirRel,
			isCover: isCover,
			isMusic: isMusic,
			preset:  effPreset,
		})
		if isMusic {
			dirHasMusicCandidate[dirRel] = true
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	if onProgress != nil {
		onProgress(scannedFiles)
	}

	// Compute subtreeHasMusic
	subtreeHasMusic := make(map[string]bool)
	for dir := range dirHasMusicCandidate {
		subtreeHasMusic[dir] = true
		for _, anc := range ancestorsOf(dir) {
			subtreeHasMusic[anc] = true
		}
	}

	return &scanResult{
		candidates:           candidates,
		ignoredFiles:         ignoredFiles,
		dirPatterns:          dirPatterns,
		dirPresets:           dirPresets,
		dirHasPreset:         dirHasPreset,
		allDirs:              allDirs,
		dirHasMusicCandidate: dirHasMusicCandidate,
		subtreeHasMusic:      subtreeHasMusic,
		ignoredCount:         ignoredCount,
		totalScanned:         len(candidates) + len(ignoredFiles),
	}, nil
}

// processFile decides how to handle each file
func processFile(src, dst string, preset Preset) error {
	ext := strings.ToLower(filepath.Ext(src))
	lcName := strings.ToLower(filepath.Base(src))

	// Ignore macOS metadata files (extra safety)
	if lcName == ".ds_store" {
		return nil
	}

	// Ignore macOS AppleDouble files (extra safety)
	if strings.HasPrefix(lcName, "._") {
		return nil
	}

	switch {
	case coverFiles[lcName]:
		return copyFile(src, dst)
	case losslessExt[ext]:
		return convertLossless(src, dst, preset)
	case ext == ".m4a":
		isALAC, err := isALAC(src)
		if err != nil {
			return fmt.Errorf("could not determine codec: %w", err)
		}
		if isALAC {
			return convertLossless(src, dst, preset)
		}
		return copyFile(src, dst)
	case lossyExt[ext]:
		return copyFile(src, dst)
	default:
		// skip unknown types
		return nil
	}
}

// isALAC uses ffprobe to determine if a .m4a file is ALAC
func isALAC(path string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-select_streams", "a:0",
		"-show_entries", "stream=codec_name",
		"-of", "default=noprint_wrappers=1:nokey=1",
		path)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return false, fmt.Errorf("ffprobe timeout for %q", path)
	}
	if err != nil {
		return false, err
	}
	codec := strings.TrimSpace(string(out))
	if codec == "" {
		return false, nil
	}
	return strings.EqualFold(codec, "alac"), nil
}

// copyFile copies a file from src to dst
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	out, err := os.Create(dst)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()

	if copyErr != nil {
		_ = os.Remove(dst)
		return copyErr
	}
	if syncErr != nil {
		_ = os.Remove(dst)
		return syncErr
	}
	if closeErr != nil {
		_ = os.Remove(dst)
		return closeErr
	}

	return nil
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}

// parseOzemrc parses .ozemrc INI content. Returns preset, ok, error.
func parseOzemrc(content string, dirPath string) (Preset, bool, error) {
	scanner := bufio.NewScanner(strings.NewReader(content))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(strings.ToLower(parts[0]))
		val := strings.TrimSpace(parts[1])
		if key == "preset" {
			switch Preset(val) {
			case PresetAAC, PresetMP3:
				return Preset(val), true, nil
			default:
				return "", false, fmt.Errorf("invalid preset %q in %s: available presets: aac, mp3", val, filepath.Join(dirPath, ".ozemrc"))
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, err
	}
	return "", false, nil
}

// ancestorsOf returns list of ancestors from "." up to dir inclusive in order parent->child
func ancestorsOf(dir string) []string {
	if dir == "." || dir == "" {
		return []string{"."}
	}
	parts := strings.Split(dir, "/")
	var ancestors []string
	ancestors = append(ancestors, ".")
	cur := ""
	for _, p := range parts {
		if p == "." || p == "" {
			continue
		}
		if cur == "" {
			cur = p
		} else {
			cur = cur + "/" + p
		}
		ancestors = append(ancestors, cur)
	}
	return ancestors
}

// effectivePresetFor returns nearest ancestor preset else default
func effectivePresetFor(dirRel string, dirPresets map[string]Preset, dirHasPreset map[string]bool, def Preset) Preset {
	ancestors := ancestorsOf(dirRel)
	for i := len(ancestors) - 1; i >= 0; i-- {
		anc := ancestors[i]
		if dirHasPreset[anc] {
			return dirPresets[anc]
		}
	}
	return def
}

// isIgnored checks if fileRel (slash, from srcRoot) is ignored by stacked dirPatterns using go-git gitignore
func isIgnored(fileRel string, fileDirRel string, dirPatterns map[string][]gitignore.Pattern) bool {
	ancestors := ancestorsOf(fileDirRel)
	var all []gitignore.Pattern
	for _, anc := range ancestors {
		if ps, ok := dirPatterns[anc]; ok {
			all = append(all, ps...)
		}
	}
	if len(all) == 0 {
		return false
	}
	m := gitignore.NewMatcher(all)
	path := strings.Split(fileRel, "/")
	return m.Match(path, false)
}
