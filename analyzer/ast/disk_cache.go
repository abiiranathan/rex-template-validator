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
	"sync"
	"sync/atomic"
	"time"
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

// hashCacheEntry caches a computed source hash with its computation time.
type hashCacheEntry struct {
	hash       string
	computedAt time.Time
}

// hashCache is an in-memory cache of source hashes to avoid redundant filesystem walks.
// The TTL is 2 seconds — fast enough for interactive use, long enough to amortise
// repeated calls within a single analysis cycle (ReadDiskCache + WriteDiskCache).
var (
	hashCacheMu    sync.RWMutex
	hashCacheStore = make(map[string]hashCacheEntry, 4)
	hashCacheTTL   = 2 * time.Second
)

// computeSourceHash produces a fast fingerprint of every Go source file under
// dir plus the optional contextFile.  We use path + mtime + size rather than
// content hashing – O(n) stat calls, no file reads, milliseconds for most
// projects.  Vendor and hidden directories are skipped.
//
// Results are cached in-process for hashCacheTTL to avoid redundant walks when
// ReadDiskCache and WriteDiskCache are called in the same analysis cycle.
func computeSourceHash(dir, contextFile string) string {
	cacheKey := dir + "\x00" + contextFile

	// Fast path: check in-memory cache under read lock.
	hashCacheMu.RLock()
	if entry, ok := hashCacheStore[cacheKey]; ok && time.Since(entry.computedAt) < hashCacheTTL {
		hashCacheMu.RUnlock()
		return entry.hash
	}
	hashCacheMu.RUnlock()

	hash := computeSourceHashSlow(dir, contextFile)

	hashCacheMu.Lock()
	hashCacheStore[cacheKey] = hashCacheEntry{hash: hash, computedAt: time.Now()}
	hashCacheMu.Unlock()

	return hash
}

// computeSourceHashSlow is the actual filesystem walk. Called only on cache miss.
func computeSourceHashSlow(dir, contextFile string) string {
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
			if name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".") {
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

// InvalidateHashCache removes the in-memory hash cache entry for dir so the
// next call to computeSourceHash performs a fresh filesystem walk.
func InvalidateHashCache(dir, contextFile string) {
	cacheKey := dir + "\x00" + contextFile
	hashCacheMu.Lock()
	delete(hashCacheStore, cacheKey)
	hashCacheMu.Unlock()
}

// diskCachePath returns the path for the gzip-compressed cache file.
func diskCachePath(dir, contextFile string) string {
	base, err := os.UserCacheDir()
	if err != nil {
		base = filepath.Join(os.TempDir(), "rex-tpl-analyzer-cache")
	}

	key := filepath.Clean(dir) + "\x00" + contextFile
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(base, "rex-template-analyzer", fmt.Sprintf("%x.gz", sum[:8]))
}

// inMemoryCache holds the last AnalysisResult per (dir, contextFile) key so that
// a second call within the same process (e.g. from a test) avoids deserializing
// the gzip+JSON blob entirely.
var (
	inMemCacheMu    sync.RWMutex
	inMemCacheStore = make(map[string]*AnalysisResult, 4)
	// inMemCacheHash tracks the source hash that was current when the entry was stored.
	inMemCacheHash = make(map[string]string, 4)
)

// inMemoryCacheHits is an atomic counter for observability / benchmarks.
var inMemoryCacheHits atomic.Int64

// ReadDiskCache attempts to load a previously cached AnalysisResult.
// Check order: (1) in-process memory cache, (2) gzip+JSON on disk.
func ReadDiskCache(dir, contextFile string) (*AnalysisResult, bool) {
	cacheKey := dir + "\x00" + contextFile
	currentHash := computeSourceHash(dir, contextFile)

	// 1. In-process cache: O(1) lookup, no I/O.
	inMemCacheMu.RLock()
	if result, ok := inMemCacheStore[cacheKey]; ok {
		if inMemCacheHash[cacheKey] == currentHash {
			inMemCacheMu.RUnlock()
			inMemoryCacheHits.Add(1)
			return result, true
		}
	}
	inMemCacheMu.RUnlock()

	// 2. Disk cache: decompress + decode.
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

	if entry.SourceHash != currentHash {
		return nil, false
	}

	result := &entry.Result

	// Populate in-memory cache so subsequent calls skip disk I/O.
	inMemCacheMu.Lock()
	inMemCacheStore[cacheKey] = result
	inMemCacheHash[cacheKey] = currentHash
	inMemCacheMu.Unlock()

	return result, true
}

// WriteDiskCache serializes result to disk using gzip+JSON.
// Also updates the in-memory cache atomically.
func WriteDiskCache(dir, contextFile string, result AnalysisResult) {
	hash := computeSourceHash(dir, contextFile)
	cacheKey := dir + "\x00" + contextFile

	// Update in-memory cache first (fast path for next ReadDiskCache call).
	resultCopy := result // shallow copy to avoid sharing mutable slices
	inMemCacheMu.Lock()
	inMemCacheStore[cacheKey] = &resultCopy
	inMemCacheHash[cacheKey] = hash
	inMemCacheMu.Unlock()

	path := diskCachePath(dir, contextFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	tmpPath := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	f, err := os.Create(tmpPath)
	if err != nil {
		return
	}

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

	os.Rename(tmpPath, path)

	// Remove stale cache siblings.
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

// ClearDiskCache removes the on-disk and in-memory cache entries for dir.
func ClearDiskCache(dir, contextFile string) {
	os.Remove(diskCachePath(dir, contextFile))

	cacheKey := dir + "\x00" + contextFile
	inMemCacheMu.Lock()
	delete(inMemCacheStore, cacheKey)
	delete(inMemCacheHash, cacheKey)
	inMemCacheMu.Unlock()

	InvalidateHashCache(dir, contextFile)
}
