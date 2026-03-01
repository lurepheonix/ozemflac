package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

	if len(os.Args) != 3 {
		fatal("usage: converter <source_dir> <destination_dir>")
	}

	srcRoot := filepath.Clean(os.Args[1])
	dstRoot := filepath.Clean(os.Args[2])

	if _, err := os.Stat(dstRoot); err == nil {
		fatal("destination directory already exists")
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
	fmt.Printf("Found %d files to process\n", total)

	// Channels for worker pool
	jobChan := make(chan Job)
	doneChan := make(chan struct{})
	var wg sync.WaitGroup
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

	// Progress state
	var mu sync.Mutex
	processed := 0
	currentFile := ""

	// Progress printer
	go func() {
		for range doneChan {
			mu.Lock()
			percent := float64(processed) / float64(total) * 100
			barLen := 40
			filled := int(percent / 100 * float64(barLen))
			bar := strings.Repeat("█", filled) + strings.Repeat("-", barLen-filled)
			fmt.Printf("\r[%s] %.1f%%  %s", bar, percent, currentFile)
			mu.Unlock()
		}
		fmt.Println("\nDone!")
	}()

	// Launch workers
	for i := 0; i < numWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobChan {
				mu.Lock()
				currentFile = filepath.Base(job.Src)
				mu.Unlock()

				processFile(job.Src, job.Dst)

				mu.Lock()
				processed++
				mu.Unlock()

				doneChan <- struct{}{}
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
}

// processFile decides how to handle each file
func processFile(src, dst string) {
	ext := strings.ToLower(filepath.Ext(src))
	lcName := strings.ToLower(filepath.Base(src))

	// Ignore macOS metadata files (extra safety)
	if lcName == ".ds_store" {
		return
	}

	// Ignore macOS AppleDouble files (extra safety)
	if strings.HasPrefix(lcName, "._") {
		return
	}

	switch {
	case coverFiles[lcName]:
		_ = copyFile(src, dst)
	case losslessExt[ext]:
		_ = convertToAAC(src, dst)
	case ext == ".m4a":
		isALAC, err := isALAC(src)
		if err != nil {
			fmt.Println("warning: could not determine codec for", src, ":", err)
			return
		}
		if isALAC {
			_ = convertToAAC(src, dst)
		} else {
			_ = copyFile(src, dst)
		}
	case lossyExt[ext]:
		_ = copyFile(src, dst)
	default:
		// skip
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
		return err
	}

	return os.Rename(tmp, dst)
}

// isALAC uses afinfo to determine if a .m4a file is ALAC
func isALAC(path string) (bool, error) {
	cmd := exec.Command("/usr/bin/afinfo", path)
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}

	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		if strings.Contains(strings.ToLower(scanner.Text()), "alac") {
			return true, nil
		}
	}
	return false, nil
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
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	return out.Sync()
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
