package main

import (
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
	Src string
	Dst string
}

func main() {
	workers := flag.Int("workers", 0, "number of parallel workers")
	flag.Parse()

	args := flag.Args()
	if len(args) != 2 {
		fatal("usage: converter [-workers N] <source_dir> <destination_dir>")
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

	// Fail fast if required external binaries are missing
	if _, err := exec.LookPath("ffprobe"); err != nil {
		fatal("ffprobe not found in $PATH — install ffmpeg (brew install ffmpeg)")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		fatal("ffmpeg not found in $PATH — install ffmpeg (brew install ffmpeg)")
	}

	// Gather all files first
	var jobs []Job
	err := filepath.WalkDir(srcRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() {
			return nil
		}

		// Ignore macOS metadata files
		if d.Name() == ".DS_Store" {
			return nil
		}

		// Ignore macOS AppleDouble files
		if strings.HasPrefix(d.Name(), "._") {
			return nil
		}

		rel, err := filepath.Rel(srcRoot, path)
		if err != nil {
			return err
		}
		dstPath := filepath.Join(dstRoot, rel)
		jobs = append(jobs, Job{Src: path, Dst: dstPath})
		return nil
	})
	if err != nil {
		fatal(err.Error())
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
				err := processFile(job.Src, job.Dst)
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

// processFile decides how to handle each file
func processFile(src, dst string) error {
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
		return convertToAAC(src, dst)
	case ext == ".m4a":
		isALAC, err := isALAC(src)
		if err != nil {
			return fmt.Errorf("could not determine codec: %w", err)
		}
		if isALAC {
			return convertToAAC(src, dst)
		}
		return copyFile(src, dst)
	case lossyExt[ext]:
		return copyFile(src, dst)
	default:
		// skip unknown types
		return nil
	}
}

// convertToAAC uses afconvert to encode a lossless file to high-quality AAC
func convertToAAC(src, dst string) error {
	dst = strings.TrimSuffix(dst, filepath.Ext(dst)) + ".m4a"
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}

	cmd := exec.Command(
		"/usr/bin/afconvert",
		"-f", "m4af",
		"-d", "aac",
		"-b", "256000",
		src,
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
	metaCmd := exec.Command(
		"ffmpeg",
		"-y",
		"-i", src,
		"-i", dst,
		"-map", "1:a",
		"-map_metadata", "0",
		"-c", "copy",
		tmp,
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
