package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

func termWidth() int {
	if s := os.Getenv("COLUMNS"); s != "" {
		if w, err := strconv.Atoi(s); err == nil && w > 20 {
			return w
		}
	}
	if isTerminal(os.Stdout) {
		if ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ); err == nil && ws.Col > 20 {
			return int(ws.Col)
		}
	}
	return 80
}

type diffEntry struct {
	rel     string
	srcRel  string // for expected, src rel; for actual, dst rel (same)
	preset  Preset
	action  string
	isCover bool
	detail  string // e.g., "ALAC" for m4a ALAC
}

type diffResult struct {
	missing  map[string]diffEntry // expectedRel -> entry
	extra    map[string]diffEntry // actualRel -> entry
	mismatch map[string]mismatchEntry
	upToDate map[string]diffEntry
}

type mismatchEntry struct {
	expectedRel string
	actualRel   string
	preset      Preset
	srcRel      string
}

type diffNode struct {
	rel      string
	children map[string]*diffNode
}

// computeDiff performs the full diff between src and dst, reusing the same logic as diff.
// It handles scanning src (with isALAC probing), scanning dst, and building missing/mismatch/extra sets.
// The caller may provide atomic counters and phase for spinner progress (or nil to skip).
func computeDiff(srcRoot, dstRoot string, defPreset Preset, scannedSrc, scannedDst, probed *atomic.Int64, phase *atomic.Value) (*scanResult, map[string]diffEntry, map[string]diffEntry, *diffResult, error) {
	// Scan src
	sr, err := scanSource(srcRoot, defPreset, func(n int) {
		if scannedSrc != nil {
			scannedSrc.Store(int64(n))
		}
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if phase != nil {
		phase.Store("probing")
	}
	// Build expected map (need isALAC for .m4a)
	expectedMap := make(map[string]diffEntry)
	expectedByBase := make(map[string]string) // base without ext -> expectedRel
	for _, c := range sr.candidates {
		// Cover only if subtree has music (candidates includes covers even for pruned dirs, filter here)
		if c.isCover && !sr.subtreeHasMusic[c.dirRel] {
			continue
		}
		// Determine expected dst rel (with preset ext if convert)
		ext := strings.ToLower(filepath.Ext(c.src))
		lcName := strings.ToLower(filepath.Base(c.src))
		var dstRel string
		var action string
		var detail string
		if coverFiles[lcName] {
			dstRel = c.rel
			action = "copy"
			detail = "cover"
		} else if losslessExt[ext] {
			dstRel = strings.TrimSuffix(c.rel, filepath.Ext(c.rel)) + c.preset.Ext()
			action = "convert"
			detail = ext
		} else if ext == ".m4a" {
			isALAC, err := isALAC(c.src)
			if err != nil {
				dstRel = c.rel
				action = "copy"
				detail = "AAC"
			} else if isALAC {
				dstRel = strings.TrimSuffix(c.rel, filepath.Ext(c.rel)) + c.preset.Ext()
				action = "convert"
				detail = "ALAC"
			} else {
				dstRel = c.rel
				action = "copy"
				detail = "AAC"
			}
			if probed != nil {
				probed.Add(1)
			}
		} else if lossyExt[ext] {
			dstRel = c.rel
			action = "copy"
			detail = ext
		} else {
			continue
		}
		dstRel = filepath.ToSlash(dstRel)
		expectedMap[dstRel] = diffEntry{rel: dstRel, srcRel: c.rel, preset: c.preset, action: action, isCover: c.isCover, detail: detail}
		base := strings.TrimSuffix(dstRel, filepath.Ext(dstRel))
		// Also base without ext for mismatch detection, but need to handle multiple files with same base different ext? Use first
		if _, ok := expectedByBase[base]; !ok {
			expectedByBase[base] = dstRel
		}
	}
	if phase != nil {
		phase.Store("scanning dst")
	}
	// Scan dst
	actualMap := make(map[string]diffEntry)
	actualByBase := make(map[string]string)
	dstScanned := 0
	err = filepath.WalkDir(dstRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == ".DS_Store" || strings.HasPrefix(d.Name(), "._") {
			return nil
		}
		rel, err := filepath.Rel(dstRoot, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		dstScanned++
		if scannedDst != nil && dstScanned%50 == 0 {
			scannedDst.Store(int64(dstScanned))
		}
		actualMap[rel] = diffEntry{rel: rel, srcRel: rel}
		base := strings.TrimSuffix(rel, filepath.Ext(rel))
		if _, ok := actualByBase[base]; !ok {
			actualByBase[base] = rel
		}
		return nil
	})
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if scannedDst != nil {
		scannedDst.Store(int64(dstScanned))
	}

	// Diff
	missing := make(map[string]diffEntry)
	extra := make(map[string]diffEntry)
	mismatch := make(map[string]mismatchEntry)
	upToDate := make(map[string]diffEntry)

	for expRel, expEntry := range expectedMap {
		if _, ok := actualMap[expRel]; !ok {
			// Check preset mismatch: same base different ext exists in actual
			base := strings.TrimSuffix(expRel, filepath.Ext(expRel))
			if actRel, ok2 := actualByBase[base]; ok2 && actRel != expRel {
				// Mismatch
				mismatch[expRel] = mismatchEntry{expectedRel: expRel, actualRel: actRel, preset: expEntry.preset, srcRel: expEntry.srcRel}
			} else {
				missing[expRel] = expEntry
			}
		} else {
			upToDate[expRel] = expEntry
		}
	}
	for actRel, actEntry := range actualMap {
		if _, ok := expectedMap[actRel]; !ok {
			base := strings.TrimSuffix(actRel, filepath.Ext(actRel))
			if expRel, ok2 := expectedByBase[base]; ok2 {
				// Already counted as mismatch, skip extra
				if _, isMismatch := mismatch[expRel]; isMismatch {
					continue
				}
				// Check if this actual's base corresponds to expected with different ext -> already mismatch
				// Need to check if expectedByBase has this base with different ext
				// If so, skip
				continue
			}
			extra[actRel] = actEntry
		}
	}

	dr := &diffResult{
		missing:  missing,
		extra:    extra,
		mismatch: mismatch,
		upToDate: upToDate,
	}
	return sr, expectedMap, actualMap, dr, nil
}

func runDiff(args []string) {
	fs := flag.NewFlagSet("diff", flag.ExitOnError)
	presetFlag := fs.String("preset", "aac", "output preset: aac or mp3 (available presets: aac, mp3)")
	showAll := fs.Bool("all", false, "show ignored files")
	jsonOut := fs.Bool("json", false, "output JSON")
	full := fs.Bool("full", false, "show full tree (not only diffs)")
	expand := fs.Bool("expand", false, "expand file list even when uniform preset")
	fs.BoolVar(expand, "v", false, "alias for --expand")
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
	rest := fs.Args()
	if len(rest) != 2 {
		fatal("usage: ozemflac diff [--all] [--json] [--full] [--expand] [-preset aac|mp3] <source_dir> <destination_dir>")
	}
	srcRoot := filepath.Clean(rest[0])
	dstRoot := filepath.Clean(rest[1])

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

	// Single spinner for scan src + scan dst + probing
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

	// Use computeDiff for the heavy lifting (need to count m4a for spinner total before probing, so pre-scan? We'll count after but need m4aTotal for spinner text)
	// To keep spinner accurate, we still need m4aTotal before probing; computeDiff does probing internally, so we estimate m4aTotal via quick scan or just allow spinner to show probed only.
	// Instead, we call computeDiff which handles probing and phase updates.
	sr, expectedMap, actualMap, dr, err := computeDiff(srcRoot, dstRoot, defPreset, &scannedSrc, &scannedDst, &probed, &phase)
	if err != nil {
		close(done)
		fatal(err.Error())
	}
	// Count m4a for summary (for spinner display we already had probing)
	close(done)
	if !*jsonOut {
		fmt.Fprintf(os.Stderr, "\r\033[K")
	}
	missing := dr.missing
	extra := dr.extra
	mismatch := dr.mismatch
	upToDate := dr.upToDate

	useColorTree := !*jsonOut && isTerminal(os.Stdout)
	if *jsonOut {
		// JSON output
		type jsonDiff struct {
			Source      string                 `json:"source"`
			Destination string                 `json:"destination"`
			Preset      Preset                 `json:"preset"`
			Missing     []string               `json:"missing,omitempty"`
			Extra       []string               `json:"extra,omitempty"`
			Mismatch    []map[string]string    `json:"mismatch,omitempty"`
			UpToDate    []string               `json:"upToDate,omitempty"`
			Details     map[string]interface{} `json:"details,omitempty"`
		}
		// Build sorted lists
		missingList := sortedKeys(missing)
		extraList := sortedKeys(extra)
		mismatchList := []map[string]string{}
		for expRel, me := range mismatch {
			mismatchList = append(mismatchList, map[string]string{"expected": expRel, "actual": me.actualRel, "src": me.srcRel})
		}
		sort.Slice(mismatchList, func(i, j int) bool { return mismatchList[i]["expected"] < mismatchList[j]["expected"] })
		upToDateList := []string{}
		if *full {
			upToDateList = sortedKeys(upToDate)
		}
		jd := jsonDiff{
			Source:      srcRoot,
			Destination: dstRoot,
			Preset:      defPreset,
			Missing:     missingList,
			Extra:       extraList,
			Mismatch:    mismatchList,
			UpToDate:    upToDateList,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(jd)
		return
	}

	// Text two-column tree
	printDiffTree(srcRoot, dstRoot, sr, expectedMap, actualMap, mismatch, missing, extra, upToDate, *full, *expand, *showAll, useColorTree, defPreset)
}

func sortedKeys(m map[string]diffEntry) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// printDiffTree prints two-column adaptive tree
func printDiffTree(srcRoot, dstRoot string, sr *scanResult, expectedMap, actualMap map[string]diffEntry, mismatch map[string]mismatchEntry, missing, extra, upToDate map[string]diffEntry, full, expand, showAll bool, useColor bool, defPreset Preset) {
	// Build set of all rels for tree structure: union of expected and actual, plus dirs
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
	// Also include pruned dirs from scan (for pruned display)
	for _, dirRel := range sr.allDirs {
		allRels[dirRel] = true
		dir := dirRel
		for dir != "." && dir != "" {
			dir = filepath.ToSlash(filepath.Dir(dir))
			allRels[dir] = true
		}
	}
	// Build dir tree
	nodes := make(map[string]*diffNode)
	nodes["."] = &diffNode{rel: ".", children: make(map[string]*diffNode)}
	// Create nodes for all rels that are dirs
	for rel := range allRels {
		// Determine if rel is file or dir: if it contains dot and is in expected/actual as file, treat as file, not dir
		// For tree, we need dir nodes only
		if _, isFile := expectedMap[rel]; isFile {
			continue
		}
		if _, isFile := actualMap[rel]; isFile {
			continue
		}
		// Check if rel looks like file (has ext) but not in maps? Skip creating file nodes
		// Instead create dir node if rel has no ext or is known dir from allDirs
		isDir := false
		for _, d := range sr.allDirs {
			if d == rel {
				isDir = true
				break
			}
		}
		// Also if rel contains no dot after slash, treat as dir
		if !isDir && strings.Contains(filepath.Base(rel), ".") {
			// Might be file not in maps but still file, skip dir creation
			continue
		}
		if _, ok := nodes[rel]; !ok {
			nodes[rel] = &diffNode{rel: rel, children: make(map[string]*diffNode)}
		}
	}
	// Ensure all dirs from sr.allDirs exist
	for _, dirRel := range sr.allDirs {
		if _, ok := nodes[dirRel]; !ok {
			nodes[dirRel] = &diffNode{rel: dirRel, children: make(map[string]*diffNode)}
		}
	}
	// Link children
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
			// Create missing parent
			p = &diffNode{rel: parentRel, children: make(map[string]*diffNode)}
			nodes[parentRel] = p
			if root, ok2 := nodes["."]; ok2 {
				root.children[filepath.Base(parentRel)] = p
			}
		}
	}
	// Collect files per dir
	dirExtraFiles := make(map[string][]string)
	dirMissingFiles := make(map[string][]string)
	dirMismatchFiles := make(map[string][]string)
	for rel := range expectedMap {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if dir == "" {
			dir = "."
		}
		if _, ok := mismatch[rel]; ok {
			dirMismatchFiles[dir] = append(dirMismatchFiles[dir], rel)
		} else if _, ok := missing[rel]; ok {
			dirMissingFiles[dir] = append(dirMissingFiles[dir], rel)
		} else {
			// upToDate
			if full {
				// Will show if full
			}
		}
	}
	for rel := range actualMap {
		if _, ok := expectedMap[rel]; !ok {
			// Check if part of mismatch already
			base := strings.TrimSuffix(rel, filepath.Ext(rel))
			isMismatch := false
			for expRel := range mismatch {
				if strings.TrimSuffix(expRel, filepath.Ext(expRel)) == base {
					isMismatch = true
					break
				}
			}
			if isMismatch {
				continue
			}
			dir := filepath.ToSlash(filepath.Dir(rel))
			if dir == "" {
				dir = "."
			}
			dirExtraFiles[dir] = append(dirExtraFiles[dir], rel)
		}
	}
	// For full mode, also need upToDate files per dir
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

	statusW := 0
	// dynamic per actual diff set (saves width when only narrow statuses appear)
	if len(mismatch) > 0 {
		if w := visibleWidth("[mismatch]"); w > statusW {
			statusW = w
		}
	}
	if len(missing) > 0 {
		if w := visibleWidth("[missing]"); w > statusW {
			statusW = w
		}
	}
	if len(extra) > 0 {
		if w := visibleWidth("[extra]"); w > statusW {
			statusW = w
		}
	}
	if showAll {
		hasIgnored := len(sr.ignoredFiles) > 0
		if !hasIgnored {
			// also check pruned ignored not yet counted; still reserve for header alignment if --all
			hasIgnored = true // when --all we may show [ignored] even if currently empty, keep stable header
		}
		if hasIgnored {
			if w := visibleWidth("[ignored]"); w > statusW {
				statusW = w
			}
		}
	}
	if statusW == 0 {
		// no diffs (e.g. --full with only upToDate): still reserve max for stable layout
		statusW = visibleWidth("[mismatch]")
		if w := visibleWidth("[missing]"); w > statusW {
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
	// Header — third column blank but padded to statusW to keep alignment
	headerLeft := fmt.Sprintf("SRC: %s", srcRoot)
	headerRight := fmt.Sprintf("DST: %s", dstRoot)
	leftHeader := padRightVisible(truncateVisible(headerLeft, colW), colW)
	rightHeader := padRightVisible(truncateVisible(headerRight, colW), colW)
	statusHeader := strings.Repeat(" ", statusW)
	fmt.Printf("%s | %s %s\n", leftHeader, rightHeader, statusHeader)
	fmt.Printf("%s-+-%s %s\n", strings.Repeat("-", colW), strings.Repeat("-", colW), strings.Repeat("-", statusW))
	// Print tree recursively
	var printDir func(n *diffNode, prefix string, isLast bool, depth int)
	printDir = func(n *diffNode, prefix string, isLast bool, depth int) {
		// Determine if this dir should be shown in diff-only mode (no full)
		hasDiffInSubtree := false
		hasDiffDirect := len(dirMissingFiles[n.rel]) > 0 || len(dirExtraFiles[n.rel]) > 0 || len(dirMismatchFiles[n.rel]) > 0
		if showAll {
			// When --all, ignored files count as diff to show
			for _, c := range sr.ignoredFiles {
				if c.dirRel == n.rel {
					hasDiffDirect = true
					break
				}
			}
		}
		if !full {
			// Check subtree
			var checkSubtree func(*diffNode) bool
			checkSubtree = func(dn *diffNode) bool {
				if len(dirMissingFiles[dn.rel]) > 0 || len(dirExtraFiles[dn.rel]) > 0 || len(dirMismatchFiles[dn.rel]) > 0 {
					return true
				}
				if showAll {
					for _, c := range sr.ignoredFiles {
						if c.dirRel == dn.rel {
							return true
						}
					}
				}
				// Pruned handling: if pruned and has ignored, consider diff? For diff, pruned not relevant, but extra in pruned dst will be extra
				for _, child := range dn.children {
					if checkSubtree(child) {
						return true
					}
				}
				return false
			}
			hasDiffInSubtree = hasDiffDirect || checkSubtree(n)
			if !hasDiffInSubtree && n.rel != "." {
				return
			}
		}
		// For root, always show
		if n.rel != "." {
			branch := "└── "
			if !isLast {
				branch = "├── "
			}
			// Pruned check
			pruned := false
			prunedReason := ""
			if sr != nil {
				if !sr.subtreeHasMusic[n.rel] {
					pruned = true
					// Determine reason
					hasIgnored := false
					for _, c := range sr.ignoredFiles {
						if c.dirRel == n.rel || strings.HasPrefix(c.dirRel, n.rel+"/") || n.rel == "." && strings.HasPrefix(c.dirRel, "") {
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
				if showAll {
					// Show ignored files directly in this pruned dir when --all
					var ignoredInDir []string
					for _, c := range sr.ignoredFiles {
						if c.dirRel == n.rel {
							ignoredInDir = append(ignoredInDir, c.rel)
						}
					}
					sort.Strings(ignoredInDir)
					igPrefix := prefix
					if !isLast {
						igPrefix += "│   "
					} else {
						igPrefix += "    "
					}
					for _, rel := range ignoredInDir {
						fileBase := filepath.Base(rel)
						fileAvail := colW - visibleWidth(igPrefix+"├── ")
						if fileAvail < 10 {
							fileAvail = 10
						}
						fileTrunc := truncateVisible(fileBase, fileAvail)
						fileColored := colorize(fileTrunc, colorDim, useColor)
						leftPlain := igPrefix + "├── " + fileTrunc
						leftPadded := igPrefix + "├── " + fileColored + strings.Repeat(" ", colW-visibleWidth(leftPlain))
						rightPadded := padRightVisible("", colW)
						statusPlain := "[ignored]"
						statusPadded := colorize(statusPlain, colorRed, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
						fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
					}
				}
				return
			}
			// Dir line with preset info (like tree)
			preset := effectivePresetFor(n.rel, sr.dirPresets, sr.dirHasPreset, defPreset)
			presetStr := fmt.Sprintf("preset:%s", preset)
			dirPlain := fmt.Sprintf("%s%s%s/ [%s]", prefix, branch, filepath.Base(n.rel), presetStr)
			dirTrunc := truncateVisible(dirPlain, colW)
			leftPadded := padRightVisible(dirTrunc, colW)
			rightPadded := padRightVisible("", colW)
			statusBlank := strings.Repeat(" ", statusW)
			fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusBlank)
			if !isLast {
				prefix = prefix + "│   "
			} else {
				prefix = prefix + "    "
			}
		}
		// Show files in this dir
		// Collect files for this dir
		missingFiles := dirMissingFiles[n.rel]
		mismatchFiles := dirMismatchFiles[n.rel]
		extraFiles := dirExtraFiles[n.rel]
		upToDateFiles := dirUpToDateFiles[n.rel]

		// Sort
		sort.Strings(missingFiles)
		sort.Strings(extraFiles)
		sort.Strings(mismatchFiles)
		sort.Strings(upToDateFiles)

		// Decide expand: if uniform and not full/expand, collapsed? For diff, we always show diffs, so show all diff files
		// For full mode with uniform, respect expand logic similar to tree
		// For diff default (no full), we show diff files regardless of uniform
		showFiles := true
		if !full && !expand {
			// In diff-only mode, we show files that are diffs, so show
			showFiles = true
		}

		if showFiles {
			// Mismatch first (single line with both sides) — third column same line, dynamic statusW
			for _, expRel := range mismatchFiles {
				me := mismatch[expRel]
				srcBase := filepath.Base(me.srcRel)
				rightFile := filepath.Base(me.actualRel)
				// Build left: srcBase → dstExt, with ALAC detail if needed
				dstExt := presetForRel(expRel, sr, defPreset).Ext()
				srcExt := strings.ToLower(filepath.Ext(srcBase))
				leftRaw := srcBase + " → " + dstExt
				if me.preset == PresetAAC || me.preset == PresetMP3 {
					// Check if src was ALAC with same ext as dst (both .m4a) -> show ALAC detail
					if strings.EqualFold(srcExt, ".m4a") && strings.EqualFold(dstExt, ".m4a") {
						if entry, ok := expectedMap[expRel]; ok && entry.detail == "ALAC" {
							leftRaw = srcBase + " (ALAC) → " + dstExt
						}
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
				statusPlain := "[mismatch]"
				statusPadded := colorize(statusPlain, colorYellow, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
			}
			for _, rel := range missingFiles {
				entry := missing[rel]
				srcBase := filepath.Base(entry.srcRel)
				dstExt := filepath.Ext(rel) // dst ext from expectedRel
				// For convert, show src → dstExt, for copy just src
				leftRaw := srcBase
				if entry.action == "convert" {
					// Check ALAC same-ext case
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
				statusPlain := "[missing]"
				statusPadded := colorize(statusPlain, colorGreen, useColor) + strings.Repeat(" ", statusW-visibleWidth(statusPlain))
				fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusPadded)
			}
			for _, rel := range extraFiles {
				rightFile := filepath.Base(rel)
				rightPlainTrunc := truncateVisible(rightFile, colW)
				rightColored := colorize(rightPlainTrunc, colorRed, useColor)
				rightPadded := rightColored + strings.Repeat(" ", colW-visibleWidth(rightPlainTrunc))
				leftPadded := padRightVisible("", colW)
				statusPlain := "[extra]"
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
					leftColored := leftPlainTrunc // no color for up-to-date? Keep plain
					leftPlainFull := prefix + fBranch + leftPlainTrunc
					leftPadded := prefix + fBranch + leftColored + strings.Repeat(" ", colW-visibleWidth(leftPlainFull))
					rightPlainTrunc := truncateVisible(leftFile, colW)
					rightPadded := padRightVisible(rightPlainTrunc, colW)
					statusBlank := strings.Repeat(" ", statusW)
					fmt.Printf("%s | %s %s\n", leftPadded, rightPadded, statusBlank)
				}
			}
			if showAll {
				// Show ignored files in this dir
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
		}

		// Recurse children
		sorted := sortedDiffDirNames(n)
		for i, name := range sorted {
			child := n.children[name]
			isLastChild := i == len(sorted)-1
			printDir(child, prefix, isLastChild, depth+1)
		}
	}
	// Start from root children
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
	// Summary
	totalMissing := len(missing)
	totalExtra := len(extra)
	totalMismatch := len(mismatch)
	fmt.Printf("\nSummary: %s missing, %s mismatch, %s extra\n",
		colorize(fmt.Sprintf("%d", totalMissing), colorGreen, useColor),
		colorize(fmt.Sprintf("%d", totalMismatch), colorYellow, useColor),
		colorize(fmt.Sprintf("%d", totalExtra), colorRed, useColor),
	)
}

func sortedDiffDirNames(n *diffNode) []string {
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func presetForRel(rel string, sr *scanResult, defPreset Preset) Preset {
	dirRel := filepath.ToSlash(filepath.Dir(rel))
	if dirRel == "" {
		dirRel = "."
	}
	return effectivePresetFor(dirRel, sr.dirPresets, sr.dirHasPreset, defPreset)
}
