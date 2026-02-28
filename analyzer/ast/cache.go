package ast

import "sync"

// cachedFields stores pre-extracted field information to avoid redundant work.
// Each struct type's fields are computed once and reused throughout analysis.
type cachedFields struct {
	fields []FieldInfo // Exported fields and methods
	doc    string      // Struct-level documentation
}

// fieldCache provides concurrent-safe caching for struct field extraction.
// This is critical for performance when analyzing large codebases with
// many references to the same types.
type fieldCache struct {
	mu    sync.RWMutex            // Protects concurrent map access
	cache map[string]cachedFields // Cache storage (keyed by full type string)
}

// newFieldCache initializes a fieldCache with reasonable default capacity.
func newFieldCache() *fieldCache {
	return &fieldCache{
		cache: make(map[string]cachedFields, 256),
	}
}

// get retrieves cached field data with read lock for concurrent safety.
// Returns the cached data and a boolean indicating cache hit/miss.
func (fc *fieldCache) get(k string) (cachedFields, bool) {
	fc.mu.RLock()
	v, ok := fc.cache[k]
	fc.mu.RUnlock()
	return v, ok
}

// set stores field data in cache with write lock for concurrent safety.
func (fc *fieldCache) set(k string, v cachedFields) {
	fc.mu.Lock()
	fc.cache[k] = v
	fc.mu.Unlock()
}

// seenMapPool manages a pool of maps used to track visited types during
// recursive traversals. Pooling prevents excessive allocations, especially
// important when processing deeply nested type hierarchies.
type seenMapPool struct {
	pool sync.Pool
}

// newSeenMapPool creates a pool that generates fresh seen maps on demand.
func newSeenMapPool() *seenMapPool {
	return &seenMapPool{
		pool: sync.Pool{
			New: func() any {
				return make(map[string]bool, 16)
			},
		},
	}
}

// get retrieves a cleared seen map from the pool, ready for use.
// Maps are cleared to ensure no stale state from previous uses.
func (smp *seenMapPool) get() map[string]bool {
	m := smp.pool.Get().(map[string]bool)
	clear(m) // Go 1.21+ clear built-in
	return m
}

// put returns a seen map to the pool for later reuse.
func (smp *seenMapPool) put(m map[string]bool) {
	smp.pool.Put(m)
}
