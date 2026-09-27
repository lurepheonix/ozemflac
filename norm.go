package main

import (
	"path/filepath"

	"golang.org/x/text/unicode/norm"
)

// normName canonicalizes file/folder names to NFC so that
// precomposed (NFC, e.g. U+010D) and decomposed (NFD, e.g. U+0063 U+030C)
// spellings of the same name compare, group, and display identically.
//
// Apply it to the ORIGINAL name value at creation (right after
// filepath.Rel/filepath.ToSlash/filepath.Dir/filepath.Base), never only
// to comparison keys, so scanSource, diff, sync, and tree all share
// the same canonical form.
func normName(s string) string {
	return norm.NFC.String(s)
}

// dstFsPath resolves a normalized (NFC) dst rel to a filesystem path,
// preferring the real on-disk spelling (e.g. NFD) when known so that
// removals hit the actual file on normalization-sensitive filesystems.
// New files are created at the normalized NFC path.
func dstFsPath(dstRoot, rel string, onDisk map[string]string) string {
	if onDisk != nil {
		if real, ok := onDisk[rel]; ok {
			return filepath.Join(dstRoot, filepath.FromSlash(real))
		}
	}
	return filepath.Join(dstRoot, filepath.FromSlash(rel))
}
