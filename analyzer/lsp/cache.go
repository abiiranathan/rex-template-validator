package lsp

import (
	"strings"
	"sync"

	"github.com/rex-template-analyzer/ast"
	"github.com/rex-template-analyzer/validator"
)

// cachedAnalysis holds the memoised output of a full AnalyzeDir call plus any
// lazily-computed named-block registry for the same workspace.
type cachedAnalysis struct {
	result ast.AnalysisResult

	// namedBlocks is populated the first time a validate or getNamedBlocks
	// request is served. It is nil until then to avoid the directory walk on
	// every getTemplateContext call.
	nbOnce      sync.Once
	namedBlocks map[string][]validator.NamedBlockEntry
	nbErrors    []validator.NamedBlockDuplicateError
}

// loadNamedBlocks ensures namedBlocks is populated, computing it at most once.
func (c *cachedAnalysis) loadNamedBlocks(baseDir, templateRoot string) (
	map[string][]validator.NamedBlockEntry,
	[]validator.NamedBlockDuplicateError,
) {
	c.nbOnce.Do(func() {
		c.namedBlocks, c.nbErrors = validator.ParseAllNamedTemplates(baseDir, templateRoot)
	})
	return c.namedBlocks, c.nbErrors
}

// analysisCache is a concurrent-safe store of per-directory analysis results.
// The cache key is "dir|contextFile" so that different context files for the
// same directory are cached independently.
type analysisCache struct {
	mu    sync.RWMutex
	store map[string]*cachedAnalysis
}

func newAnalysisCache() *analysisCache {
	return &analysisCache{
		store: make(map[string]*cachedAnalysis),
	}
}

// key builds the cache lookup key from the two identifying dimensions.
func (c *analysisCache) key(dir, contextFile string) string {
	return dir + "|" + contextFile
}

// get returns the cached entry for the given (dir, contextFile) pair, if any.
func (c *analysisCache) get(dir, contextFile string) (*cachedAnalysis, bool) {
	c.mu.RLock()
	v, ok := c.store[c.key(dir, contextFile)]
	c.mu.RUnlock()
	return v, ok
}

// set stores an analysis result in the cache.
func (c *analysisCache) set(dir, contextFile string, v *cachedAnalysis) {
	c.mu.Lock()
	c.store[c.key(dir, contextFile)] = v
	c.mu.Unlock()
}

// invalidate removes all entries whose key starts with dir (covers all
// contextFile variants for that directory) and also clears the underlying
// ast package-level cache so that the next analysis re-parses Go sources.
func (c *analysisCache) invalidate(dir string) {
	c.mu.Lock()
	for k := range c.store {
		// Match "dir|..." — avoids accidentally evicting a parent directory
		// whose path is a prefix of another.
		if strings.HasPrefix(k, dir+"|") || k == dir {
			delete(c.store, k)
		}
	}
	c.mu.Unlock()

	// Also evict the package-level Go AST cache inside the ast package so that
	// modified .go files are re-parsed on the next request.
	ast.ClearCache()
}
