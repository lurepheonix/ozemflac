package main

import (
	"fmt"
	"io"
	"os"
)

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return true
		}
	}
	return false
}

func printTopUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac [command] [options] <source_dir> [destination_dir]")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Available Commands:")
	fmt.Fprintln(w, "  convert    Convert lossless audio (FLAC/ALAC/WAV/AIFF/APE/WV/TTA) to aac/mp3 (default when no command is given)")
	fmt.Fprintln(w, "  tree       Display and inspect the source directory tree")
	fmt.Fprintln(w, "  covers     Inspect or extract cover art from audio files")
	fmt.Fprintln(w, "  short      Find audio tracks shorter or longer than a duration")
	fmt.Fprintln(w, "  diff       Compare differences between source and destination directories")
	fmt.Fprintln(w, "  sync       Synchronize source files to the destination directory")
	fmt.Fprintln(w, "  version    Print version information")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Global Options:")
	fmt.Fprintln(w, "  --version, -V               Print version information")
	fmt.Fprintln(w, "  -preset <aac|mp3>         Encoding format (default: aac)")
	fmt.Fprintln(w, "  -workers <N>              Number of parallel workers")
	fmt.Fprintln(w, "  --no-extract-cover        Skip extracting cover art")
	fmt.Fprintln(w, "  --preserve-embedded-art   Keep embedded art inside target files")
	fmt.Fprintln(w, "  --nomedia                 Create .nomedia flag in destination root")
	fmt.Fprintln(w, "  --json                    Output results in JSON format")
	fmt.Fprintln(w, "  --all                     Process all items")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Run 'ozemflac <command> --help' for more information on a specific command.")
}

func printTopError() {
	fmt.Fprintln(os.Stderr, "error: missing required arguments or subcommand.")
	fmt.Fprintln(os.Stderr, "")
	printTopUsage(os.Stderr)
}

func printUnknownCommand(cmd string) {
	fmt.Fprintf(os.Stderr, "error: unknown command %q.\n", cmd)
	fmt.Fprintln(os.Stderr, "")
	printTopUsage(os.Stderr)
}

// printCommandUsage writes minimal per-command help to w. Returns false for unknown commands.
func printCommandUsage(cmd string, w io.Writer) bool {
	switch cmd {
	case "convert", "(default)", "":
		printConvertUsage(w)
		return true
	case "tree":
		printTreeUsage(w)
		return true
	case "covers":
		printCoversUsage(w)
		return true
	case "short":
		printShortUsage(w)
		return true
	case "diff":
		printDiffUsage(w)
		return true
	case "sync":
		printSyncUsage(w)
		return true
	default:
		return false
	}
}

func printConvertUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac [convert] [-workers N] [-preset aac|mp3] [--no-extract-cover] [--preserve-embedded-art] [--nomedia] <source_dir> <destination_dir>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Convert lossless audio (FLAC/ALAC/WAV/AIFF/APE/WV/TTA) to aac/mp3.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Arguments:")
	fmt.Fprintln(w, "  <source_dir>              Source music directory")
	fmt.Fprintln(w, "  <destination_dir>         Destination directory (must not exist or be empty)")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  -preset <aac|mp3>         Encoding format (default: aac)")
	fmt.Fprintln(w, "  -workers <N>              Number of parallel workers")
	fmt.Fprintln(w, "  --no-extract-cover        Skip extracting cover art")
	fmt.Fprintln(w, "  --preserve-embedded-art   Keep embedded art inside target files")
	fmt.Fprintln(w, "  --nomedia                 Create .nomedia flag in destination root")
}

func printTreeUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac tree [--all] [--json] [--expand] [-preset aac|mp3] [--no-extract-cover] <source_dir>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Display and inspect the source directory tree.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Arguments:")
	fmt.Fprintln(w, "  <source_dir>              Source music directory")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  --all                     Process all items")
	fmt.Fprintln(w, "  --json                    Output results in JSON format")
	fmt.Fprintln(w, "  --expand, -v              Expand file list even when uniform preset")
	fmt.Fprintln(w, "  -preset <aac|mp3>         Encoding format (default: aac)")
	fmt.Fprintln(w, "  --no-extract-cover        Skip extracting cover art")
}

func printCoversUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac covers [--all] [--json] [--no-extract-cover] [--preserve-embedded-art] <source_dir>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Inspect or extract cover art from audio files.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Arguments:")
	fmt.Fprintln(w, "  <source_dir>              Source music directory")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  --all                     Process all items")
	fmt.Fprintln(w, "  --json                    Output results in JSON format")
	fmt.Fprintln(w, "  --no-extract-cover        Skip extracting cover art")
	fmt.Fprintln(w, "  --preserve-embedded-art   Keep embedded art inside target files")
}

func printShortUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac short [--all] [--json] --under <seconds> [--over <seconds>] <source_dir>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Find audio tracks shorter or longer than a duration.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Arguments:")
	fmt.Fprintln(w, "  <source_dir>              Source music directory")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  --all                     Process all items")
	fmt.Fprintln(w, "  --json                    Output results in JSON format")
	fmt.Fprintln(w, "  --under <seconds>         Upper duration bound in seconds (required)")
	fmt.Fprintln(w, "  --over <seconds>          Lower duration bound in seconds")
}

func printDiffUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac diff [--all] [--json] [--full] [--expand] [-preset aac|mp3] [--no-extract-cover] [--nomedia] <source_dir> <destination_dir>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Compare differences between source and destination directories.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Arguments:")
	fmt.Fprintln(w, "  <source_dir>              Source music directory")
	fmt.Fprintln(w, "  <destination_dir>         Destination directory")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  --all                     Process all items")
	fmt.Fprintln(w, "  --json                    Output results in JSON format")
	fmt.Fprintln(w, "  --full                    Show full tree (not only changes)")
	fmt.Fprintln(w, "  --expand, -v              Expand file list even when uniform preset")
	fmt.Fprintln(w, "  -preset <aac|mp3>         Encoding format (default: aac)")
	fmt.Fprintln(w, "  --no-extract-cover        Skip extracting cover art")
	fmt.Fprintln(w, "  --nomedia                 Create .nomedia flag in destination root")
}

func printSyncUsage(w io.Writer) {
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintln(w, "  ozemflac sync [--all] [--json] [--full] [--expand] [--dry-run] [--keep-mismatched] [--delete-extra] [-workers N] [-preset aac|mp3] [--no-extract-cover] [--preserve-embedded-art] [--nomedia] <source_dir> <destination_dir>")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Description:")
	fmt.Fprintln(w, "  Synchronize source files to the destination directory.")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Arguments:")
	fmt.Fprintln(w, "  <source_dir>              Source music directory")
	fmt.Fprintln(w, "  <destination_dir>         Destination directory")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "Options:")
	fmt.Fprintln(w, "  --all                     Process all items")
	fmt.Fprintln(w, "  --json                    Output results in JSON format")
	fmt.Fprintln(w, "  --full                    Show full tree (not only changes)")
	fmt.Fprintln(w, "  --expand, -v              Expand file list even when uniform preset")
	fmt.Fprintln(w, "  --dry-run                 Preview changes without applying")
	fmt.Fprintln(w, "  --keep-mismatched         Keep mismatched files")
	fmt.Fprintln(w, "  --delete-extra            Delete extra files in destination")
	fmt.Fprintln(w, "  -workers <N>              Number of parallel workers")
	fmt.Fprintln(w, "  -preset <aac|mp3>         Encoding format (default: aac)")
	fmt.Fprintln(w, "  --no-extract-cover        Skip extracting cover art")
	fmt.Fprintln(w, "  --preserve-embedded-art   Keep embedded art inside target files")
	fmt.Fprintln(w, "  --nomedia                 Create .nomedia flag in destination root")
}

func printUsageErrorAndExit(cmd string, msg string) {
	fmt.Fprintf(os.Stderr, "error: %s\n", msg)
	fmt.Fprintln(os.Stderr, "")
	_ = printCommandUsage(cmd, os.Stderr)
	os.Exit(1)
}
