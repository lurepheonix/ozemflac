package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func runSync(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	presetFlag := fs.String("preset", "aac", "output preset: aac or mp3 (available presets: aac, mp3)")
	showAll := fs.Bool("all", false, "show ignored files")
	jsonOut := fs.Bool("json", false, "output JSON")
	full := fs.Bool("full", false, "show full tree (not only changes)")
	expand := fs.Bool("expand", false, "expand file list even when uniform preset")
	fs.BoolVar(expand, "v", false, "alias for --expand")
	dryRun := fs.Bool("dry-run", false, "preview changes without applying")
	keepMismatched := fs.Bool("keep-mismatched", false, "keep mismatched files (do not replace preset mismatches)")
	deleteExtra := fs.Bool("delete-extra", false, "delete extra files in destination")
	workers := fs.Int("workers", 0, "number of parallel workers (0 = auto)")
	if err := fs.Parse(args); err != nil {
		fatal(err.Error())
	}
	var defPreset Preset
	switch Preset(*presetFlag) {
	case PresetAAC, PresetMP3:
		defPreset = Preset(*presetFlag)
	default:
		fatal("invalid -preset \"" + *presetFlag + "\": available presets: aac, mp3")
	}
	if *workers < 0 {
		fatal("workers must be >= 0")
	}
	rest := fs.Args()
	if len(rest) != 2 {
		fatal("usage: ozemflac sync [--all] [--json] [--full] [--expand] [--dry-run] [--keep-mismatched] [--delete-extra] [-workers N] [-preset aac|mp3] <source_dir> <destination_dir>")
	}
	srcRoot := filepath.Clean(rest[0])
	dstRoot := filepath.Clean(rest[1])

	if srcRoot == dstRoot {
		fatal("source and destination must be different directories")
	}
	if info, err := os.Stat(srcRoot); err != nil {
		fatal(fmt.Sprintf("source directory %q: %v", srcRoot, err))
	} else if !info.IsDir() {
		fatal(fmt.Sprintf("source %q is not a directory", srcRoot))
	}
	if info, err := os.Stat(dstRoot); err != nil {
		fatal(fmt.Sprintf("destination directory %q: %v", dstRoot, err))
	} else if !info.IsDir() {
		fatal(fmt.Sprintf("destination %q is not a directory", dstRoot))
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		fatal("ffprobe not found in $PATH — install ffmpeg")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fatal("ffmpeg not found in $PATH — install ffmpeg")
	}

	// Compute diff with spinner
	var scannedSrc atomic.Int64
	var scannedDst atomic.Int64
	var probed atomic.Int64
	var phase atomic.Value
	phase.Store("scanning src")
	done := make(chan struct{})
	useColorSpinner := isTerminal(os.Stderr)
	if !*jsonOut {
		go withSpinnerAfter(1*time.Second, func() string {
			p, _ := phase.Load().(string)
			if p == "probing" {
				return fmt.Sprintf("Probing… %d .m4a files", probed.Load())
			}
			if p == "scanning dst" {
				return fmt.Sprintf("Scanning dst… %d files", scannedDst.Load())
			}
			return fmt.Sprintf("Scanning src… %d files", scannedSrc.Load())
		}, done, useColorSpinner)
	}

	sr, expectedMap, actualMap, dr, err := computeDiff(srcRoot, dstRoot, defPreset, &scannedSrc, &scannedDst, &probed, &phase)
	if err != nil {
		close(done)
		fatal(err.Error())
	}
	close(done)
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}

	// Filter according to flags
	filteredMissing := dr.missing
	filteredMismatch := make(map[string]mismatchEntry)
	if !*keepMismatched {
		filteredMismatch = dr.mismatch
	}
	filteredExtra := make(map[string]diffEntry)
	if *deleteExtra {
		filteredExtra = dr.extra
	}

	useColorTree := !*jsonOut && isTerminal(os.Stdout)

	// JSON output
	if *jsonOut {
		type jsonSync struct {
			Source      string              `json:"source"`
			Destination string              `json:"destination"`
			Preset      Preset              `json:"preset"`
			DryRun      bool                `json:"dryRun"`
			Added       []string            `json:"added,omitempty"`
			Replaced    []map[string]string `json:"replaced,omitempty"`
			Deleted     []string            `json:"deleted,omitempty"`
			KeptMismatch []map[string]string `json:"keptMismatch,omitempty"`
			KeptExtra   []string            `json:"keptExtra,omitempty"`
		}
		addedList := sortedKeys(filteredMissing)
		deletedList := sortedKeys(filteredExtra)
		replacedList := []map[string]string{}
		for exp, me := range filteredMismatch {
			replacedList = append(replacedList, map[string]string{"expected": exp, "actual": me.actualRel, "src": me.srcRel})
		}
		sort.Slice(replacedList, func(i, j int) bool { return replacedList[i]["expected"] < replacedList[j]["expected"] })
		keptMismatch := []map[string]string{}
		if *keepMismatched {
			for exp, me := range dr.mismatch {
				keptMismatch = append(keptMismatch, map[string]string{"expected": exp, "actual": me.actualRel, "src": me.srcRel})
			}
			sort.Slice(keptMismatch, func(i, j int) bool { return keptMismatch[i]["expected"] < keptMismatch[j]["expected"] })
		}
		keptExtra := []string{}
		if !*deleteExtra {
			keptExtra = sortedKeys(dr.extra)
		}
		js := jsonSync{
			Source:       srcRoot,
			Destination:  dstRoot,
			Preset:       defPreset,
			DryRun:       *dryRun,
			Added:        addedList,
			Replaced:     replacedList,
			Deleted:      deletedList,
			KeptMismatch: keptMismatch,
			KeptExtra:    keptExtra,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(js)
		if *dryRun {
			return
		}
		// for non-dry-run json, continue to perform sync but not print tree
	}

	// Dry-run preview
	if *dryRun {
		if len(filteredMissing) == 0 && len(filteredMismatch) == 0 && len(filteredExtra) == 0 {
			if !*jsonOut {
				fmt.Println("Already in sync — nothing to do")
				fmt.Printf("\nSummary: %s added, %s replaced, %s deleted\n",
					colorize("0", colorGreen, useColorTree),
					colorize("0", colorYellow, useColorTree),
					colorize("0", colorRed, useColorTree),
				)
			}
			return
		}
		if !*jsonOut {
			// Reuse sync tree for preview (same as post-sync view)
			printSyncTree(srcRoot, dstRoot, sr, expectedMap, actualMap, filteredMismatch, filteredMissing, filteredExtra, dr.upToDate, *full, *expand, *showAll, useColorTree, defPreset, true)
		}
		return
	}

	// Nothing to do?
	if len(filteredMissing) == 0 && len(filteredMismatch) == 0 && len(filteredExtra) == 0 {
		if !*jsonOut {
			fmt.Println("Already in sync — nothing to do")
		}
		return
	}

	// Perform deletions for extra
	var deletedCount int
	var deletedRels []string
	if len(filteredExtra) > 0 {
		// Sort by depth descending so files in deeper dirs removed first (not strictly needed)
		rels := make([]string, 0, len(filteredExtra))
		for r := range filteredExtra {
			rels = append(rels, r)
		}
		sort.Slice(rels, func(i, j int) bool {
			// deeper first, then lexical
			di := strings.Count(rels[i], "/")
			dj := strings.Count(rels[j], "/")
			if di != dj {
				return di > dj
			}
			return rels[i] < rels[j]
		})
		for _, rel := range rels {
			path := filepath.Join(dstRoot, filepath.FromSlash(rel))
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "warning: failed to delete extra %q: %v\n", rel, err)
				continue
			}
			deletedCount++
			deletedRels = append(deletedRels, rel)
			// prune empty parents
			dir := filepath.Dir(path)
			for {
				if dir == dstRoot || dir == "." || dir == "/" {
					break
				}
				// check if dir is inside dstRoot
				relDir, err := filepath.Rel(dstRoot, dir)
				if err != nil || strings.HasPrefix(relDir, "..") {
					break
				}
				entries, err := os.ReadDir(dir)
				if err != nil {
					break
				}
				if len(entries) != 0 {
					break
				}
				_ = os.Remove(dir)
				dir = filepath.Dir(dir)
			}
		}
	}

	// For mismatches, remove old file before conversion
	if !*keepMismatched {
		for _, me := range filteredMismatch {
			path := filepath.Join(dstRoot, filepath.FromSlash(me.actualRel))
			_ = os.Remove(path)
		}
	}

	// Build jobs for missing + mismatch
	var jobs []Job
	needAAC := false
	needMP3 := false
	for rel, entry := range filteredMissing {
		src := filepath.Join(srcRoot, filepath.FromSlash(entry.srcRel))
		dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
		jobs = append(jobs, Job{Src: src, Dst: dst, Preset: entry.preset})
		if entry.action == "convert" {
			if entry.preset == PresetMP3 {
				needMP3 = true
			} else {
				needAAC = true
			}
		}
	}
	for expRel, me := range filteredMismatch {
		entry, ok := expectedMap[expRel]
		if !ok {
			continue
		}
		src := filepath.Join(srcRoot, filepath.FromSlash(me.srcRel))
		dst := filepath.Join(dstRoot, filepath.FromSlash(expRel))
		jobs = append(jobs, Job{Src: src, Dst: dst, Preset: entry.preset})
		if entry.action == "convert" {
			if entry.preset == PresetMP3 {
				needMP3 = true
			} else {
				needAAC = true
			}
		}
	}

	// Validate encoders if needed
	if len(jobs) > 0 {
		if needAAC {
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
		if needMP3 {
			out, err := exec.Command("ffmpeg", "-encoders").Output()
			if err != nil || !strings.Contains(string(out), "libmp3lame") {
				fatal("ffmpeg libmp3lame encoder not available — install ffmpeg with --enable-libmp3lame")
			}
		}
	}

	// Run workers if there are conversion/copy jobs
	var failed int
	var failedFiles []string
	if len(jobs) > 0 {
		total := len(jobs)
		var numWorkers int
		if *workers > 0 {
			numWorkers = *workers
		} else {
			numWorkers = runtime.NumCPU() / 2
			if numWorkers < 1 {
				numWorkers = 1
			}
		}
		if !*jsonOut {
			fmt.Printf("Syncing %d file(s) with %d worker(s)\n", total, numWorkers)
		}
		jobChan := make(chan Job, numWorkers*2)
		doneChan := make(chan string, numWorkers*2)
		var wg sync.WaitGroup
		var mu sync.Mutex
		processed := 0
		var printerWg sync.WaitGroup
		printerWg.Add(1)
		go func() {
			defer printerWg.Done()
			for name := range doneChan {
				mu.Lock()
				processed++
				if !*jsonOut {
					percent := float64(processed) / float64(total) * 100
					barLen := 40
					filled := int(percent / 100 * float64(barLen))
					bar := strings.Repeat("█", filled) + strings.Repeat("-", barLen-filled)
					fmt.Printf("\r[%s] %.1f%%  %s", bar, percent, name)
				}
				mu.Unlock()
			}
			if !*jsonOut {
				fmt.Println()
				if failed > 0 {
					fmt.Printf("Done with %d error(s)\n", failed)
				} else {
					fmt.Println("Done!")
				}
			}
		}()

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
		for _, job := range jobs {
			jobChan <- job
		}
		close(jobChan)
		wg.Wait()
		close(doneChan)
		printerWg.Wait()
	}

	if !*jsonOut {
		// Print sync result tree
		printSyncTree(srcRoot, dstRoot, sr, expectedMap, actualMap, filteredMismatch, filteredMissing, filteredExtra, dr.upToDate, *full, *expand, *showAll, useColorTree, defPreset, false)
		if failed > 0 {
			fmt.Fprintf(os.Stderr, "\n%d file(s) failed:\n", failed)
			for _, f := range failedFiles {
				fmt.Fprintln(os.Stderr, " ", f)
			}
			os.Exit(1)
		}
	} else {
		if failed > 0 {
			os.Exit(1)
		}
	}
}

// printSyncTree prints the result of sync — only what was changed.
// If whole folder was replaced (all expected files in that subtree are among added/replaced), it shows just the folder line.
func printSyncTree(srcRoot, dstRoot string, sr *scanResult, expectedMap, actualMap map[string]diffEntry, mismatch map[string]mismatchEntry, missing, extra, upToDate map[string]diffEntry, full, expand, showAll bool, useColor bool, defPreset Preset, isDryRun bool) {
	// Build maps for sync view statuses
	// For sync we show: added (missing), replaced (mismatch), deleted (extra)
	// Compute per-dir counts for collapsing whole-folder case
	expectedPerDir := make(map[string]int) // subtree total expected files
	upToDatePerDir := make(map[string]int)
	syncedPerDir := make(map[string]int)
	for expRel := range expectedMap {
		dir := filepath.ToSlash(filepath.Dir(expRel))
		if dir == "" {
			dir = "."
		}
		for _, anc := range ancestorsOf(dir) {
			expectedPerDir[anc]++
		}
		if _, ok := upToDate[expRel]; ok {
			for _, anc := range ancestorsOf(dir) {
				upToDatePerDir[anc]++
			}
		}
		if _, ok := missing[expRel]; ok {
			for _, anc := range ancestorsOf(dir) {
				syncedPerDir[anc]++
			}
		} else if _, ok := mismatch[expRel]; ok {
			for _, anc := range ancestorsOf(dir) {
				syncedPerDir[anc]++
			}
		}
	}

	// Build set of all rels for tree structure: include expected and actual for context, plus sr.allDirs
	allRels := make(map[string]bool)
	for rel := range expectedMap {
		allRels[rel] = true
		dir := filepath.ToSlash(filepath.Dir(rel))
		for dir != "." && dir != "" {
			allRels[dir] = true
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
	}
	for rel := range actualMap {
		allRels[rel] = true
		dir := filepath.ToSlash(filepath.Dir(rel))
		for dir != "." && dir != "" {
			allRels[dir] = true
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
	}
	for _, dirRel := range sr.allDirs {
		allRels[dirRel] = true
		dir := dirRel
		for dir != "." && dir != "" {
			dir = filepath.ToSlash(filepath.Dir(dir))
			allRels[dir] = true
		}
	}
	// Also need to include dirs from extra/missing for tree
	for rel := range extra {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "" {
			dir = "."
		}
		allRels[dir] = true
		for dir != "." && dir != "" {
			dir = filepath.ToSlash(filepath.Dir(dir))
			allRels[dir] = true
		}
	}

	nodes := make(map[string]*diffNode)
	nodes["."] = &diffNode{rel: ".", children: make(map[string]*diffNode)}
	for rel := range allRels {
		if _, isFile := expectedMap[rel]; isFile {
			continue
		}
		if _, isFile := actualMap[rel]; isFile {
			continue
		}
		// also check if rel is in extra/deleted set that may not be in expected/actual? but extra already in actual
		isDir := false
		for _, d := range sr.allDirs {
			if d == rel {
				isDir = true
				break
			}
		}
		if !isDir && strings.Contains(filepath.Base(rel), ".") {
			continue
		}
		if _, ok := nodes[rel]; !ok {
			nodes[rel] = &diffNode{rel: rel, children: make(map[string]*diffNode)}
		}
	}
	for _, dirRel := range sr.allDirs {
		if _, ok := nodes[dirRel]; !ok {
			nodes[dirRel] = &diffNode{rel: dirRel, children: make(map[string]*diffNode)}
		}
	}
	for rel, n := range nodes {
		if rel == "." {
			continue
		}
		parentRel := filepath.ToSlash(filepath.Dir(rel))
		if parentRel == "" {
			parentRel = "."
		}
		if p, ok := nodes[parentRel]; ok {
			p.children[filepath.Base(rel)] = n
		} else {
			p = &diffNode{rel: parentRel, children: make(map[string]*diffNode)}
			nodes[parentRel] = p
			if root, ok2 := nodes["."]; ok2 {
				root.children[filepath.Base(parentRel)] = p
			}
		}
	}

	// Per-dir file lists for sync view
	dirAdded := make(map[string][]string)
	dirReplaced := make(map[string][]string)
	dirDeleted := make(map[string][]string)
	for rel := range missing {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "" {
			dir = "."
		}
		dirAdded[dir] = append(dirAdded[dir], rel)
	}
	for rel := range mismatch {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "" {
			dir = "."
		}
		dirReplaced[dir] = append(dirReplaced[dir], rel)
	}
	for rel := range extra {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "" {
			dir = "."
		}
		dirDeleted[dir] = append(dirDeleted[dir], rel)
	}
	dirUpToDateFiles := make(map[string][]string)
	if full {
		for rel := range upToDate {
			dir := filepath.ToSlash(filepath.Dir(rel))
			if dir == "" {
				dir = "."
			}
			dirUpToDateFiles[dir] = append(dirUpToDateFiles[dir], rel)
		}
	}

	// Determine statusW for sync view: [added], [replaced], [deleted]
	statuses := []string{"[added]", "[replaced]", "[deleted]"}
	if showAll {
		statuses = append(statuses, "[ignored]")
	}
	statusW := 0
	hasAdded := len(missing) > 0
	hasReplaced := len(mismatch) > 0
	hasDeleted := len(extra) > 0
	for _, s := range statuses {
		need := false
		switch s {
		case "[added]":
			need = hasAdded
		case "[replaced]":
			need = hasReplaced
		case "[deleted]":
			need = hasDeleted
		case "[ignored]":
			need = showAll
		}
		if !need && s != "[ignored]" {
			continue
		}
		if showAll && s == "[ignored]" {
			// reserve even if no ignored files when --all (like diff)
			need = true
		}
		if !need {
			continue
		}
		if w := visibleWidth(s); w > statusW {
			statusW = w
		}
	}
	if statusW == 0 {
		statusW = visibleWidth("[added]")
		if w := visibleWidth("[replaced]"); w > statusW {
			statusW = w
		}
	}
	width := termWidth()
	colW := (width - 3 - 1 - statusW) / 2
	if colW < 20 {
		colW = 20
		if width-3-1-colW*2 < statusW {
			statusW = width - 3 - 1 - colW*2
			if statusW < 7 {
				statusW = 7
			}
		}
	}
	headerLeft := fmt.Sprintf("SRC: %s", srcRoot)
	headerRight := fmt.Sprintf("DST: %s", dstRoot)
	leftHeader := padRightVisible(truncateVisible(headerLeft, colW), colW)
	rightHeader := padRightVisible(truncateVisible(headerRight, colW), colW)
	statusHeader := strings.Repeat(" ", statusW)
	fmt.Printf("%s | %s %s\n", leftHeader, rightHeader, statusHeader)
	fmt.Printf("%s-+-%s %s\n", strings.Repeat("-", colW), strings.Repeat("-", colW), strings.Repeat("-", statusW))

	var printDir func(n *diffNode, prefix string, isLast bool, depth int)
	printDir = func(n *diffNode, prefix string, isLast bool, depth int) {
		// Determine if this dir has any sync changes in subtree
		hasChangeDirect := len(dirAdded[n.rel]) > 0 || len(dirReplaced[n.rel]) > 0 || len(dirDeleted[n.rel]) > 0
		if showAll {
			for _, c := range sr.ignoredFiles {
				if c.dirRel == n.rel {
					hasChangeDirect = true
					break
				}
			}
		}
		hasChangeInSubtree := hasChangeDirect
		if !full {
			var checkSubtree func(*diffNode) bool
			checkSubtree = func(dn *diffNode) bool {
				if len(dirAdded[dn.rel]) > 0 || len(dirReplaced[dn.rel]) > 0 || len(dirDeleted[dn.rel]) > 0 {
					return true
				}
				if showAll {
					for _, c := range sr.ignoredFiles {
						if c.dirRel == dn.rel {
							return true
						}
					}
				}
				for _, child := range dn.children {
					if checkSubtree(child) {
						return true
					}
				}
				return false
			}
			if !hasChangeDirect {
				for _, child := range n.children {
					if checkSubtree(child) {
						hasChangeInSubtree = true
						break
					}
				}
			} else {
				hasChangeInSubtree = true
			}
			if !hasChangeInSubtree && n.rel != "." {
				return
			}
		} else {
			// in full mode, show all dirs that have expected content, but for sync view we still filter to only dirs with expected?
			// Keep same as above: if full, show all dirs
			hasChangeInSubtree = true
			if n.rel != "." && !hasChangeInSubtree && len(dirUpToDateFiles[n.rel]) == 0 {
				// still show dir if full
			}
		}

		// Whole-folder collapsed check: if all expected files under this dir are synced (added/replaced) and no upToDate remains
		collapsed := false
		if n.rel != "." && expectedPerDir[n.rel] > 0 && syncedPerDir[n.rel] == expectedPerDir[n.rel] && upToDatePerDir[n.rel] == 0 && len(dirDeleted[n.rel]) == 0 {
			// also ensure no extra remains in subtree that would not be synced (but extra is separate)
			// Count extra in subtree: if any extra files exist under this dir and deleteExtra is off, we still collapsed? Better not collapse if extra exists and not deleted
			hasExtraInSubtree := false
			var checkExtra func(*diffNode) bool
			checkExtra = func(dn *diffNode) bool {
				if len(dirDeleted[dn.rel]) > 0 {
					return true
				}
				for _, child := range dn.children {
					if checkExtra(child) {
						return true
					}
				}
				return false
			}
			for _, child := range n.children {
				if checkExtra(child) {
					hasExtraInSubtree = true
					break
				}
			}
			if len(dirDeleted[n.rel]) > 0 {
				hasExtraInSubtree = true
			}
			if !hasExtraInSubtree {
				collapsed = true
			}
		}

		if n.rel != "." {
			branch := "└── "
			if !isLast {
				branch = "├── "
			}
			pruned := false
			prunedReason := ""
			if sr != nil {
				if !sr.subtreeHasMusic[n.rel] {
					pruned = true
					hasIgnored := false
					for _, c := range sr.ignoredFiles {
						if c.dirRel == n.rel || strings.HasPrefix(c.dirRel, n.rel+"/") {
							hasIgnored = true
							break
						}
					}
					if hasIgnored {
						prunedReason = ".ozemignore"
					} else {
						prunedReason = "empty"
					}
				}
			}
			if pruned {
				line := fmt.Sprintf("%s%s%s/ [pruned (%s)]", prefix, branch, filepath.Base(n.rel), prunedReason)
				leftTrunc := truncateVisible(line, colW)
				leftPadded := padRightVisible(leftTrunc, colW)
				rightPadded := padRightVisible("", colW)
				statusBlank := strings.Repeat(" ", statusW)
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusBlank)
				return
			}

			preset := effectivePresetFor(n.rel, sr.dirPresets, sr.dirHasPreset, defPreset)
			presetStr := fmt.Sprintf("preset:%s", preset)
			var dirPlain string
			if collapsed {
				// Whole folder replaced
				count := expectedPerDir[n.rel]
				dirPlain = fmt.Sprintf("%s%s%s/ [%s, synced %d files]", prefix, branch, filepath.Base(n.rel), presetStr, count)
			} else {
				dirPlain = fmt.Sprintf("%s%s%s/ [%s]", prefix, branch, filepath.Base(n.rel), presetStr)
			}
			dirTrunc := truncateVisible(dirPlain, colW)
			leftPadded := padRightVisible(dirTrunc, colW)
			rightPadded := padRightVisible("", colW)
			statusBlank := strings.Repeat(" ", statusW)
			if collapsed {
				// For collapsed folder, show status in third column as [synced]
				syncStatus := "[synced]"
				if visibleWidth(syncStatus) > statusW {
					syncStatus = truncateVisible(syncStatus, statusW)
				}
				statusPadded := colorize(syncStatus, colorGreen, useColor) + strings.Repeat(" ", statusW-visibleWidth(syncStatus))
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
			} else {
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusBlank)
			}
			if collapsed {
				return
			}
			if !isLast {
				prefix = prefix + "│   "
			} else {
				prefix = prefix + "    "
			}
		}

		// Show files in this dir (only synced ones, unless full)
		addedFiles := dirAdded[n.rel]
		replacedFiles := dirReplaced[n.rel]
		deletedFiles := dirDeleted[n.rel]
		upToDateFiles := dirUpToDateFiles[n.rel]
		sort.Strings(addedFiles)
		sort.Strings(replacedFiles)
		sort.Strings(deletedFiles)
		sort.Strings(upToDateFiles)

		// Replaced first (like mismatch)
		for _, expRel := range replacedFiles {
			me := mismatch[expRel]
			srcBase := filepath.Base(me.srcRel)
			rightFile := filepath.Base(me.actualRel)
			dstExt := presetForRel(expRel, sr, defPreset).Ext()
			srcExt := strings.ToLower(filepath.Ext(srcBase))
			leftRaw := srcBase + " → " + dstExt
			if strings.EqualFold(srcExt, ".m4a") && strings.EqualFold(dstExt, ".m4a") {
				if entry, ok := expectedMap[expRel]; ok && entry.detail == "ALAC" {
					leftRaw = srcBase + " (ALAC) → " + dstExt
				}
			}
			fBranch := "├── "
			leftAvail := colW - visibleWidth(prefix+fBranch)
			if leftAvail < 10 {
				leftAvail = 10
			}
			leftPlainTrunc := truncateVisible(leftRaw, leftAvail)
			rightPlainTrunc := truncateVisible(rightFile, colW)
			leftColored := colorize(leftPlainTrunc, colorYellow, useColor)
			rightColored := colorize(rightPlainTrunc, colorYellow, useColor)
			leftPlainFull := prefix + fBranch + leftPlainTrunc
			leftPadded := prefix + fBranch + leftColored + strings.Repeat(" ", colW-visibleWidth(leftPlainFull))
			rightPadded := rightColored + strings.Repeat(" ", colW-visibleWidth(rightPlainTrunc))
			statusPlain := "[replaced]"
			statusPadded := colorize(statusPlain, colorYellow, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
			fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
		}
		for _, rel := range addedFiles {
			entry := missing[rel]
			srcBase := filepath.Base(entry.srcRel)
			dstExt := filepath.Ext(rel)
			leftRaw := srcBase
			if entry.action == "convert" {
				if entry.detail == "ALAC" && strings.EqualFold(filepath.Ext(srcBase), dstExt) {
					leftRaw = srcBase + " (ALAC) → " + dstExt
				} else {
					leftRaw = srcBase + " → " + dstExt
				}
			}
			fBranch := "├── "
			leftAvail := colW - visibleWidth(prefix+fBranch)
			if leftAvail < 10 {
				leftAvail = 10
			}
			leftPlainTrunc := truncateVisible(leftRaw, leftAvail)
			leftColored := colorize(leftPlainTrunc, colorGreen, useColor)
			leftPlainFull := prefix + fBranch + leftPlainTrunc
			leftPadded := prefix + fBranch + leftColored + strings.Repeat(" ", colW-visibleWidth(leftPlainFull))
			rightPadded := padRightVisible("", colW)
			statusPlain := "[added]"
			statusPadded := colorize(statusPlain, colorGreen, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
			fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
		}
		for _, rel := range deletedFiles {
			rightFile := filepath.Base(rel)
			rightPlainTrunc := truncateVisible(rightFile, colW)
			rightColored := colorize(rightPlainTrunc, colorRed, useColor)
			rightPadded := rightColored + strings.Repeat(" ", colW-visibleWidth(rightPlainTrunc))
			leftPadded := padRightVisible("", colW)
			statusPlain := "[deleted]"
			statusPadded := colorize(statusPlain, colorRed, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
			fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
		}
		if full {
			for _, rel := range upToDateFiles {
				leftFile := filepath.Base(rel)
				fBranch := "├── "
				leftAvail := colW - visibleWidth(prefix+fBranch)
				if leftAvail < 10 {
					leftAvail = 10
				}
				leftPlainTrunc := truncateVisible(leftFile, leftAvail)
				leftPlainFull := prefix + fBranch + leftPlainTrunc
				leftPadded := prefix + fBranch + leftPlainTrunc + strings.Repeat(" ", colW-visibleWidth(leftPlainFull))
				rightPlainTrunc := truncateVisible(leftFile, colW)
				rightPadded := padRightVisible(rightPlainTrunc, colW)
				statusBlank := strings.Repeat(" ", statusW)
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusBlank)
			}
		}
		if showAll {
			var ignoredInDir []string
			for _, c := range sr.ignoredFiles {
				if c.dirRel == n.rel {
					ignoredInDir = append(ignoredInDir, c.rel)
				}
			}
			sort.Strings(ignoredInDir)
			for _, rel := range ignoredInDir {
				fBranch := "├── "
				fileBase := filepath.Base(rel)
				leftAvail := colW - visibleWidth(prefix+fBranch)
				if leftAvail < 10 {
					leftAvail = 10
				}
				fileTrunc := truncateVisible(fileBase, leftAvail)
				fileColored := colorize(fileTrunc, colorDim, useColor)
				leftPlainFull := prefix + fBranch + fileTrunc
				leftPadded := prefix + fBranch + fileColored + strings.Repeat(" ", colW-visibleWidth(leftPlainFull))
				rightPadded := padRightVisible("", colW)
				statusPlain := "[ignored]"
				statusPadded := colorize(statusPlain, colorRed, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
			}
		}

		sorted := sortedDiffDirNames(n)
		for i, name := range sorted {
			child := n.children[name]
			isLastChild := i == len(sorted)-1
			printDir(child, prefix, isLastChild, depth+1)
		}
	}
	root, ok := nodes["."]
	if !ok {
		return
	}
	sortedRoot := sortedDiffDirNames(root)
	for i, name := range sortedRoot {
		child := nodes[name]
		isLast := i == len(sortedRoot)-1
		printDir(child, "", isLast, 1)
	}
	// Summary for sync
	totalAdded := len(missing)
	totalReplaced := len(mismatch)
	totalDeleted := len(extra)
	if isDryRun {
		fmt.Printf("\nPreview: %s to add, %s to replace, %s to delete\n",
			colorize(fmt.Sprintf("%d", totalAdded), colorGreen, useColor),
			colorize(fmt.Sprintf("%d", totalReplaced), colorYellow, useColor),
			colorize(fmt.Sprintf("%d", totalDeleted), colorRed, useColor),
		)
	} else {
		fmt.Printf("\nSynced: %s added, %s replaced, %s deleted\n",
			colorize(fmt.Sprintf("%d", totalAdded), colorGreen, useColor),
			colorize(fmt.Sprintf("%d", totalReplaced), colorYellow, useColor),
			colorize(fmt.Sprintf("%d", totalDeleted), colorRed, useColor),
		)
	}
}
