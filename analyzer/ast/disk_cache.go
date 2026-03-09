package ast

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// diskCacheVersion must be bumped whenever AnalysisResult's schema changes in a
// backwards-incompatible way so stale cache files are automatically rejected.
const diskCacheVersion = 4

// diskCacheEntry is the top-level envelope written to disk.
type diskCacheEntry struct {
	Version    int            `json:"v"`
	SourceHash string         `json:"h"`
	Result     AnalysisResult `json:"r"`
}

// computeSourceHash produces a fast fingerprint of every Go source file under
// dir plus the optional contextFile.  We use path + mtime + size rather than
// content hashing – O(n) stat calls, no file reads, milliseconds for most
// projects.  Vendor and hidden directories are skipped.
func computeSourceHash(dir, contextFile string) string {
	type fe struct {
		p    string
		mods int64 // mod-time nanoseconds
		size int64
	}

	entries := make([]fe, 0, 512)

	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "vendor" ||
				name == "node_modules" ||
				name == ".git" ||
				strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		base := filepath.Base(path)
		if ext == ".go" || base == "go.mod" || base == "go.sum" {
			info, err2 := d.Info()
			if err2 == nil {
				entries = append(entries, fe{path, info.ModTime().UnixNano(), info.Size()})
			}
		}
		return nil
	})

	sort.Slice(entries, func(i, j int) bool { return entries[i].p < entries[j].p })

	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s|%d|%d\n", e.p, e.mods, e.size)
	}

	if contextFile != "" {
		if info, err := os.Stat(contextFile); err == nil {
			fmt.Fprintf(h, "ctx:%s|%d|%d\n", contextFile, info.ModTime().UnixNano(), info.Size())
		}
	}

	return fmt.Sprintf("%x", h.Sum(nil))
}

// diskCachePath returns the path for the gzip-compressed cache file.
// The path is stable: it depends only on the canonical directory path and the
// contextFile argument (not on source content).
func diskCachePath(dir, contextFile string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "rex-tpl-analyzer-cache")
	}

	key := filepath.Clean(dir) + "\x00" + contextFile
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(base, "rex-template-analyzer", fmt.Sprintf("%x.gz", sum[:8]))
}

// ReadDiskCache attempts to load a previously cached AnalysisResult.
// It returns (result, true) on a valid cache hit, (nil, false) otherwise.
// Stale entries (wrong version or source hash mismatch) are silently discarded.
func ReadDiskCache(dir, contextFile string) (*AnalysisResult, bool) {
	path := diskCachePath(dir, contextFile)

	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	gr, err := gzip.NewReader(f)
	if err != nil {
		return nil, false
	}
	defer gr.Close()

	var entry diskCacheEntry
	if err := json.NewDecoder(gr).Decode(&entry); err != nil {
		return nil, false
	}

	if entry.Version != diskCacheVersion {
		return nil, false
	}

	if entry.SourceHash != computeSourceHash(dir, contextFile) {
		return nil, false
	}

	return &entry.Result, true
}

// WriteDiskCache serializes result to disk using gzip+JSON.
// It writes to a temporary file then renames atomically to avoid partial reads.
// The write is synchronous so callers can be sure the cache is ready before the
// process exits; the added latency (~150 ms for a 10 MB pre-flatten result) only
// applies to cache-miss runs.
func WriteDiskCache(dir, contextFile string, result AnalysisResult) {
	hash := computeSourceHash(dir, contextFile)
	path := diskCachePath(dir, contextFile)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	tmpPath := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.Create(tmpPath)
	if err != nil {
		return
	}

	// BestSpeed: fast write, still achieves 5-8x compression on repetitive JSON.
	gw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)

	entry := diskCacheEntry{
		Version:    diskCacheVersion,
		SourceHash: hash,
		Result:     result,
	}

	encErr := json.NewEncoder(gw).Encode(entry)
	gwErr := gw.Close()
	fErr := f.Close()

	if encErr != nil || gwErr != nil || fErr != nil {
		os.Remove(tmpPath)
		return
	}

	os.Rename(tmpPath, path) // atomic on POSIX
	// Remove all other cache entries in the directory so only one is kept.
	cacheDir := filepath.Dir(path)
	if entries, err := os.ReadDir(cacheDir); err == nil {
		for _, e := range entries {
			if e.Name() == filepath.Base(path) || e.IsDir() {
				continue
			}
			os.Remove(filepath.Join(cacheDir, e.Name()))
		}
	}
}

// ClearDiskCache removes the on-disk cache entry for the given directory.
func ClearDiskCache(dir, contextFile string) {
	os.Remove(diskCachePath(dir, contextFile))
}
