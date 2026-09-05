package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

const (
	colorReset  = "\033[0m"
	colorDim    = "\033[2m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

func colorize(s, code string, useColor bool) string {
	if !useColor {
		return s
	}
	return code + s + colorReset
}

func runTree(args []string) {
	fs := flag.NewFlagSet("tree", flag.ExitOnError)
	presetFlag := fs.String("preset", "aac", "output preset: aac or mp3 (available presets: aac, mp3)")
	showAll := fs.Bool("all", false, "show ignored files")
	jsonOut := fs.Bool("json", false, "output JSON")
	expand := fs.Bool("expand", false, "expand file list even when uniform preset")
	fs.BoolVar(expand, "v", false, "alias for --expand")
	// Parse
	if err := fs.Parse(args); err != nil {
		fatal(err.Error())
	}
	// Strict lower-case validation
	var defPreset Preset
	switch Preset(*presetFlag) {
	case PresetAAC, PresetMP3:
		defPreset = Preset(*presetFlag)
	default:
		fatal("invalid -preset \"" + *presetFlag + "\": available presets: aac, mp3")
	}

	rest := fs.Args()
	if len(rest) != 1 {
		fatal("usage: ozemflac tree [--all] [--json] [--expand] [-preset aac|mp3] <source_dir>")
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
	m4aTotal := 0
	done := make(chan struct{})
	useColorSpinner := isTerminal(os.Stderr)
	useColorTree := !*jsonOut && isTerminal(os.Stdout)
	if !*jsonOut {
		go withSpinnerAfter(1*time.Second, func() string {
			if p, ok := phase.Load().(string); ok && p == "probing" {
				return fmt.Sprintf("Probing… %d/%d .m4a files", probed.Load(), m4aTotal)
			}
			return fmt.Sprintf("Scanning… %d files", scanned.Load())
		}, done, useColorSpinner)
	}

	sr, err := scanSource(srcRoot, defPreset, func(n int) { scanned.Store(int64(n)) })
	if err != nil {
		close(done)
		fatal(err.Error())
	}
	// Count m4a for probing phase
	for _, c := range sr.candidates {
		if strings.ToLower(filepath.Ext(c.src)) == ".m4a" {
			m4aTotal++
		}
	}
	if m4aTotal > 0 {
		phase.Store("probing")
	}

	// Precompute per-candidate action (needs isALAC for .m4a)
	candidateActions := make(map[string]fileInfo) // key src
	for _, c := range sr.candidates {
		ext := strings.ToLower(filepath.Ext(c.src))
		lcName := strings.ToLower(filepath.Base(c.src))
		action := "copy"
		detail := ""
		if coverFiles[lcName] {
			action = "copy"
			detail = "cover"
		} else if losslessExt[ext] {
			action = "convert"
			detail = ext
		} else if ext == ".m4a" {
			// Probe ALAC
			isALAC, err := isALAC(c.src)
			if err != nil {
				action = "error"
				detail = "probe failed"
			} else if isALAC {
				action = "convert"
				detail = "ALAC"
			} else {
				action = "copy"
				detail = "AAC"
			}
			probed.Add(1)
		} else if lossyExt[ext] {
			action = "copy"
			detail = ext
		} else {
			action = "skip"
		}
		candidateActions[c.src] = fileInfo{cand: c, action: action, detail: detail}
		if ext == ".m4a" {
			// already counted via probed.Add above, but ensure count for non-m4a not double
		}
	}
	close(done)
	// withSpinnerAfter already clears line if it was shown; no extra clear needed

	if *jsonOut {
		// JSON output: build structured tree (no colors, no spinner already suppressed)
		root := buildJSONTree(srcRoot, sr, candidateActions, *showAll, defPreset)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		_ = enc.Encode(root)
		return
	}

	// Text tree
	printTextTree(srcRoot, sr, candidateActions, *showAll, *expand, useColorTree, defPreset)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func withSpinnerAfter(delay time.Duration, msg func() string, done chan struct{}, useColor bool) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	}
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	i := 0
	// Print immediately after delay, then ticker
	fmt.Fprintf(os.Stderr, "\r%s %s", colorize(frames[i%len(frames)], colorDim, useColor), msg())
	i++
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	printed := true
	for {
		select {
		case <-done:
			if printed {
				fmt.Fprintf(os.Stderr, "\r\033[K")
			}
			return
		case <-ticker.C:
			fmt.Fprintf(os.Stderr, "\r%s %s", colorize(frames[i%len(frames)], colorDim, useColor), msg())
			i++
			printed = true
		}
	}
}

// dirNode for tree building
type dirNode struct {
	rel             string
	name            string
	depth           int
	children        map[string]*dirNode
	files           []candidate
	ignoredFiles    []candidate
	hasOzemrc       bool
	rawPreset       Preset
	effectivePreset Preset
	subtreeHasMusic bool
	pruned          bool
	presetSource    string // "default", "via .ozemrc", "inherited via X/.ozemrc"
	prunedReason    string // ".ozemignore" or "empty"
}

func buildTreeNodes(sr *scanResult, defPreset Preset) map[string]*dirNode {
	nodes := make(map[string]*dirNode)
	// Ensure root
	nodes["."] = &dirNode{rel: ".", name: ".", depth: 0, children: make(map[string]*dirNode)}
	for _, dirRel := range sr.allDirs {
		if _, ok := nodes[dirRel]; !ok {
			nodes[dirRel] = &dirNode{rel: dirRel, name: filepath.Base(dirRel), depth: strings.Count(dirRel, "/"), children: make(map[string]*dirNode)}
			if dirRel == "." {
				nodes[dirRel].name = "."
			}
		}
	}
	// Fill parent links
	for rel, n := range nodes {
		if rel == "." {
			continue
		}
		parentRel := filepath.ToSlash(filepath.Dir(rel))
		if parentRel == "" {
			parentRel = "."
		}
		if parentRel == "." && filepath.Dir(rel) == "." {
			parentRel = "."
		}
		if p, ok := nodes[parentRel]; ok {
			p.children[n.name] = n
		} else {
			// Create missing parent (should not happen as allDirs includes ancestors, but handle)
			p = &dirNode{rel: parentRel, name: filepath.Base(parentRel), children: make(map[string]*dirNode)}
			nodes[parentRel] = p
			// Link to grandparent recursively? Simplified: attach to root
			if root, ok2 := nodes["."]; ok2 {
				root.children[parentRel] = p
			}
		}
		// Set preset info
		if sr.dirHasPreset[rel] {
			n.hasOzemrc = true
			n.rawPreset = sr.dirPresets[rel]
			n.effectivePreset = sr.dirPresets[rel]
			n.presetSource = "via .ozemrc"
		} else {
			n.effectivePreset = effectivePresetFor(rel, sr.dirPresets, sr.dirHasPreset, defPreset)
			if n.effectivePreset == defPreset {
				// Check if inherited
				anc := ancestorsOf(rel)
				found := false
				for i := len(anc) - 1; i >= 0; i-- {
					if sr.dirHasPreset[anc[i]] {
						n.presetSource = fmt.Sprintf("inherited via %s/.ozemrc", anc[i])
						found = true
						break
					}
				}
				if !found {
					n.presetSource = "default"
				}
			} else {
				n.presetSource = "inherited"
			}
		}
		n.subtreeHasMusic = sr.subtreeHasMusic[rel]
		n.pruned = !sr.subtreeHasMusic[rel]
		// Root special
		if rel == "." {
			if sr.dirHasPreset["."] {
				n.hasOzemrc = true
				n.rawPreset = sr.dirPresets["."]
				n.presetSource = "via .ozemrc"
			} else {
				n.effectivePreset = defPreset
				n.presetSource = "default"
			}
			n.subtreeHasMusic = sr.subtreeHasMusic["."]
			if len(sr.subtreeHasMusic) == 0 {
				// No music at all -> root pruned
				n.pruned = !sr.subtreeHasMusic["."]
				// Actually if no music anywhere, root pruned should be true
				if !n.pruned && len(sr.candidates) == 0 {
					// Check if any music candidate exists
					hasMusic := false
					for _, c := range sr.candidates {
						if c.isMusic {
							hasMusic = true
							break
						}
					}
					if !hasMusic {
						n.pruned = true
					}
				}
			}
		}
	}
	// Assign files to nodes
	for _, c := range sr.candidates {
		if n, ok := nodes[c.dirRel]; ok {
			n.files = append(n.files, c)
		}
	}
	for _, c := range sr.ignoredFiles {
		if n, ok := nodes[c.dirRel]; ok {
			n.ignoredFiles = append(n.ignoredFiles, c)
		}
	}
	// Ensure root's preset source correct when root not in allDirs? Already.
	if root, ok := nodes["."]; ok {
		if _, has := sr.dirHasPreset["."]; !has {
			root.effectivePreset = defPreset
			root.presetSource = "default"
		} else {
			root.effectivePreset = sr.dirPresets["."]
			root.presetSource = "via .ozemrc"
		}
		root.subtreeHasMusic = sr.subtreeHasMusic["."]
		if len(sr.candidates) == 0 {
			// No candidates -> check if root should be pruned
			hasMusic := false
			for _, c := range sr.candidates {
				if c.isMusic {
					hasMusic = true
					break
				}
			}
			if !hasMusic && len(sr.candidates) == 0 {
				// If no music at all, root pruned if no subtree music
				if !root.subtreeHasMusic {
					root.pruned = true
				}
			}
		}
	}
	// Compute pruned reasons: .ozemignore vs empty
	subtreeHasIgnored := make(map[string]bool)
	for _, c := range sr.ignoredFiles {
		subtreeHasIgnored[c.dirRel] = true
		for _, anc := range ancestorsOf(c.dirRel) {
			subtreeHasIgnored[anc] = true
		}
	}
	for _, n := range nodes {
		if n.pruned {
			if subtreeHasIgnored[n.rel] {
				n.prunedReason = ".ozemignore"
			} else {
				n.prunedReason = "empty"
			}
		}
	}
	return nodes
}

func printTextTree(srcRoot string, sr *scanResult, candidateActions map[string]fileInfo, showAll, expand, useColor bool, defPreset Preset) {
	nodes := buildTreeNodes(sr, defPreset)
	root, ok := nodes["."]
	if !ok {
		fmt.Printf("%s [empty]\n", srcRoot)
		return
	}
	// Print root
	fmt.Printf("%s", colorize(srcRoot, colorBold, useColor))
	// Root preset summary
	presetInfo := fmt.Sprintf("preset: %s → %s (%s)", root.effectivePreset, root.effectivePreset.Ext(), root.presetSource)
	fmt.Printf(" [%s]", colorize(presetInfo, colorCyan, useColor))
	if len(sr.dirPatterns) > 0 {
		fmt.Printf(" %s", colorize(fmt.Sprintf("ignores:%d files", len(sr.ignoredFiles)), colorDim, useColor))
	}
	fmt.Println()
	// Recursively print children
	// Sort root children names
	sortedChildren := sortedDirNames(root)
	for i, name := range sortedChildren {
		child := root.children[name]
		isLast := i == len(sortedChildren)-1
		printDirRecursive(child, nodes, candidateActions, "", isLast, showAll, expand, useColor, 1)
	}
	// Summary footer
	totalMusic := 0
	totalCovers := 0
	totalIgnored := len(sr.ignoredFiles)
	for _, c := range sr.candidates {
		if c.isMusic {
			totalMusic++
		} else if c.isCover {
			totalCovers++
		}
	}
	fmt.Printf("\nSummary: %s music, %s covers, %s ignored, %s pruned dirs\n",
		colorize(fmt.Sprintf("%d", totalMusic), colorGreen, useColor),
		colorize(fmt.Sprintf("%d", totalCovers), colorCyan, useColor),
		colorize(fmt.Sprintf("%d", totalIgnored), colorDim, useColor),
		colorize(fmt.Sprintf("%d", countPruned(nodes)), colorRed, useColor),
	)
}

func sortedDirNames(n *dirNode) []string {
	names := make([]string, 0, len(n.children))
	for k := range n.children {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

func countPruned(nodes map[string]*dirNode) int {
	c := 0
	for _, n := range nodes {
		if n.pruned && n.rel != "." {
			// Only count topmost pruned (whose parent not pruned)
			parentRel := filepath.ToSlash(filepath.Dir(n.rel))
			if parentRel == "" {
				parentRel = "."
			}
			if p, ok := nodes[parentRel]; ok && p.pruned {
				continue
			}
			c++
		}
	}
	return c
}

type fileInfo struct {
	cand   candidate
	action string
	detail string
}

func printDirRecursive(n *dirNode, allNodes map[string]*dirNode, actions map[string]fileInfo, prefix string, isLast bool, showAll, expand, useColor bool, depth int) {
	// If pruned and has no subtree music, show pruned and don't recurse (but show ignored if --all)
	if n.pruned {
		branch := "└── "
		if !isLast {
			branch = "├── "
		}
		reason := n.prunedReason
		if reason == "" {
			reason = "empty"
		}
		line := fmt.Sprintf("%s%s%s/ [pruned, %s]", prefix, branch, n.name, reason)
		fmt.Println(colorize(line, colorRed, useColor))
		if showAll && len(n.ignoredFiles) > 0 {
			// Show ignored files inside pruned dir when --all
			sort.Slice(n.ignoredFiles, func(i, j int) bool {
				return strings.ToLower(filepath.Base(n.ignoredFiles[i].src)) < strings.ToLower(filepath.Base(n.ignoredFiles[j].src))
			})
			igPrefix := prefix
			if !isLast {
				igPrefix += "│   "
			} else {
				igPrefix += "    "
			}
			for _, c := range n.ignoredFiles {
				fmt.Printf("%s├── %s %s\n", igPrefix, filepath.Base(c.src), colorize("[ignored]", colorRed, useColor))
			}
		}
		return
	}

	branch := "└── "
	nextPrefix := prefix + "    "
	if !isLast {
		branch = "├── "
		nextPrefix = prefix + "│   "
	}

	// Directory line
	presetStr := fmt.Sprintf("preset: %s → %s (%s)", n.effectivePreset, n.effectivePreset.Ext(), n.presetSource)
	// Stats for this dir (direct files only, not subtree)
	musicInDir := 0
	coversInDir := 0
	ignoredInDir := 0
	// Count direct files
	for _, c := range n.files {
		if c.isMusic {
			musicInDir++
		} else if c.isCover {
			coversInDir++
		}
	}
	// Ignored direct (from all ignoredFiles that belong to this dir)
	for _, c := range n.ignoredFiles {
		_ = c
		ignoredInDir++
	}
	// Also count ignored via ignoredCount map? Use n.ignoredFiles length
	stats := fmt.Sprintf("music:%d covers:%d", musicInDir, coversInDir)
	if ignoredInDir > 0 {
		stats += fmt.Sprintf(" ignored:%d", ignoredInDir)
	}
	// Pruned already handled

	dirLine := fmt.Sprintf("%s%s%s/ [%s, %s]", prefix, branch, n.name, presetStr, stats)
	// Color preset part
	dirLine = colorize(dirLine, "", useColor) // keep plain, but preset cyan already
	fmt.Println(dirLine)

	// Decide whether to show files: expand if heterogeneous, else collapsed
	presets2 := make(map[Preset]bool)
	actionTypes := make(map[string]bool)
	var fileActions []fileInfo
	for _, c := range n.files {
		if c.isMusic || c.isCover {
			if fi, ok := actions[c.src]; ok {
				fileActions = append(fileActions, fi)
				if c.isMusic {
					presets2[c.preset] = true
					actionTypes[fi.action] = true
				}
			}
		}
	}
	showFiles := expand
	if !showFiles {
		if len(presets2) > 1 || len(actionTypes) > 1 {
			showFiles = true
		}
	}
	// If still not showing and there are ignored files and --all, should we show ignored?
	// We'll handle ignored separately

	fileBranchPrefix := nextPrefix
	// Collect files to show
	var filesToShow []candidate
	for _, c := range n.files {
		filesToShow = append(filesToShow, c)
	}
	// Sort files by base name
	sort.Slice(filesToShow, func(i, j int) bool {
		return strings.ToLower(filepath.Base(filesToShow[i].src)) < strings.ToLower(filepath.Base(filesToShow[j].src))
	})

	// Determine file listing
	if showFiles {
		for i, c := range filesToShow {
			isLastFile := i == len(filesToShow)-1 && len(n.children) == 0 && (!showAll || len(n.ignoredFiles) == 0)
			fBranch := "├── "
			if isLastFile {
				fBranch = "└── "
			}
			ext := strings.ToLower(filepath.Ext(c.src))
			lcName := strings.ToLower(filepath.Base(c.src))
			fi, ok := actions[c.src]
			actionStr := ""
			if ok {
				if coverFiles[lcName] {
					actionStr = colorize("copy (cover)", colorCyan, useColor)
				} else if losslessExt[ext] {
					actionStr = colorize(fmt.Sprintf("convert → %s (%s)", c.preset.Ext(), c.preset), colorGreen, useColor)
				} else if ext == ".m4a" {
					if fi.action == "convert" {
						actionStr = colorize(fmt.Sprintf("convert → %s (%s, %s)", c.preset.Ext(), c.preset, fi.detail), colorGreen, useColor)
					} else {
						actionStr = colorize("copy (AAC)", colorDim, useColor)
					}
				} else if lossyExt[ext] {
					actionStr = colorize("copy", colorDim, useColor)
				} else {
					actionStr = fi.action
				}
			} else {
				actionStr = "copy"
			}
			fmt.Printf("%s%s%s [%s]\n", fileBranchPrefix, fBranch, filepath.Base(c.src), actionStr)
		}
	} else {
		// Collapsed: summary line already shows counts, no extra hint
	}

	// Show ignored files if --all
	if showAll && len(n.ignoredFiles) > 0 {
		sort.Slice(n.ignoredFiles, func(i, j int) bool {
			return strings.ToLower(filepath.Base(n.ignoredFiles[i].src)) < strings.ToLower(filepath.Base(n.ignoredFiles[j].src))
		})
		for _, c := range n.ignoredFiles {
			// Find if this is last in combined files+children
			fBranch := "├── "
			// Simple
			fmt.Printf("%s%s%s %s\n", fileBranchPrefix, fBranch, filepath.Base(c.src), colorize("[ignored]", colorRed, useColor))
		}
	}

	// Recurse children (sorted) unless pruned (already returned)
	sorted := sortedDirNames(n)
	for i, name := range sorted {
		child := n.children[name]
		// Don't show children of pruned dirs (already handled by early return at top, but still need skip)
		if child.pruned {
			// Show pruned child as pruned line, not its subtree
			isLastChild := i == len(sorted)-1
			printDirRecursive(child, allNodes, actions, nextPrefix, isLastChild, showAll, expand, useColor, depth+1)
			continue
		}
		isLastChild := i == len(sorted)-1
		printDirRecursive(child, allNodes, actions, nextPrefix, isLastChild, showAll, expand, useColor, depth+1)
	}
}

// JSON structures
type jsonDir struct {
	Path            string     `json:"path"`
	Rel             string     `json:"rel"`
	Preset          Preset     `json:"preset"`
	PresetSource    string     `json:"presetSource"`
	HasOzemrc       bool       `json:"hasOzemrc"`
	Pruned          bool       `json:"pruned"`
	PrunedReason    string     `json:"prunedReason,omitempty"`
	SubtreeHasMusic bool       `json:"subtreeHasMusic"`
	Stats           jsonStats  `json:"stats"`
	Files           []jsonFile `json:"files,omitempty"`
	IgnoredFiles    []jsonFile `json:"ignoredFiles,omitempty"`
	Children        []*jsonDir `json:"children,omitempty"`
}

type jsonStats struct {
	Music   int            `json:"music"`
	Covers  int            `json:"covers"`
	Ignored int            `json:"ignored"`
	ByExt   map[string]int `json:"byExt"`
}

type jsonFile struct {
	Name   string `json:"name"`
	Rel    string `json:"rel"`
	Ext    string `json:"ext"`
	Preset Preset `json:"preset"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

func buildJSONTree(srcRoot string, sr *scanResult, actions map[string]fileInfo, showAll bool, defPreset Preset) *jsonDir {
	nodes := buildTreeNodes(sr, defPreset)
	root, ok := nodes["."]
	if !ok {
		return &jsonDir{Path: srcRoot, Rel: ".", Preset: Preset(""), Stats: jsonStats{}}
	}
	return jsonDirFromNode(root, nodes, actions, sr, showAll)
}

func jsonDirFromNode(n *dirNode, all map[string]*dirNode, actions map[string]fileInfo, sr *scanResult, showAll bool) *jsonDir {
	jd := &jsonDir{
		Path:            n.rel,
		Rel:             n.rel,
		Preset:          n.effectivePreset,
		PresetSource:    n.presetSource,
		HasOzemrc:       n.hasOzemrc,
		Pruned:          n.pruned,
		PrunedReason:    n.prunedReason,
		SubtreeHasMusic: n.subtreeHasMusic,
		Stats: jsonStats{
			ByExt: make(map[string]int),
		},
	}
	// Fill stats - but for pruned dirs, don't list files (they won't appear in dst)
	if !n.pruned {
		for _, c := range n.files {
			if c.isMusic {
				jd.Stats.Music++
			} else if c.isCover {
				jd.Stats.Covers++
			}
			ext := strings.ToLower(filepath.Ext(c.src))
			jd.Stats.ByExt[ext]++
			fi, ok := actions[c.src]
			action := "copy"
			detail := ""
			if ok {
				action = fi.action
				detail = fi.detail
			}
			jf := jsonFile{Name: filepath.Base(c.src), Rel: c.rel, Ext: ext, Preset: c.preset, Action: action, Detail: detail}
			jd.Files = append(jd.Files, jf)
		}
		// Count byExt for stats already done; for pruned we keep stats empty (0)
	} else {
		// Pruned: keep stats empty, no files
		jd.Stats.ByExt = map[string]int{}
	}
	jd.Stats.Ignored = len(n.ignoredFiles)
	if showAll {
		for _, c := range n.ignoredFiles {
			ext := strings.ToLower(filepath.Ext(c.src))
			jf := jsonFile{Name: filepath.Base(c.src), Rel: c.rel, Ext: ext, Preset: c.preset, Action: "ignored"}
			jd.IgnoredFiles = append(jd.IgnoredFiles, jf)
		}
	}
	// Children - pruned dirs have no children shown
	if n.pruned {
		return jd
	}
	for _, name := range sortedDirNames(n) {
		child := n.children[name]
		jd.Children = append(jd.Children, jsonDirFromNode(child, all, actions, sr, showAll))
	}
	return jd
}
