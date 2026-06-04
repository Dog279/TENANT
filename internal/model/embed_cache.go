package model

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"sync"
)

// EmbedCache is an LRU cache for embeddings keyed by (embedder_id, text
// hash). The cache key uses sha256 of the text to keep keys small and
// avoid retaining the source strings; collisions at this hash size are
// not a practical concern for embedding identity.
//
// Persistent (cross-restart) caching is a v1.1 TODO — see TODOS.md.
type EmbedCache struct {
	cap int

	mu    sync.Mutex
	ll    *list.List
	index map[cacheKey]*list.Element
}

type cacheKey struct {
	embedder string
	hashLow  uint64
	hashHigh uint64
}

type cacheEntry struct {
	key cacheKey
	vec []float32
}

// NewEmbedCache constructs an LRU with the given capacity. A reasonable
// default is 4096 entries; at 1024d float32 that's ~16MB.
func NewEmbedCache(capacity int) *EmbedCache {
	if capacity <= 0 {
		capacity = 4096
	}
	return &EmbedCache{
		cap:   capacity,
		ll:    list.New(),
		index: make(map[cacheKey]*list.Element, capacity),
	}
}

// Get returns the cached vector for (embedderID, text), or false.
// The returned slice is shared with the cache; callers MUST NOT mutate.
func (c *EmbedCache) Get(embedderID, text string) ([]float32, bool) {
	k := makeKey(embedderID, text)
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[k]
	if !ok {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheEntry).vec, true
}

// Put inserts vec for (embedderID, text). Evicts the LRU entry if at
// capacity. The cache takes ownership of vec; callers should not mutate.
func (c *EmbedCache) Put(embedderID, text string, vec []float32) {
	k := makeKey(embedderID, text)
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[k]; ok {
		el.Value.(*cacheEntry).vec = vec
		c.ll.MoveToFront(el)
		return
	}
	el := c.ll.PushFront(&cacheEntry{key: k, vec: vec})
	c.index[k] = el
	if c.ll.Len() > c.cap {
		c.evict()
	}
}

// Len reports the current entry count, for tests and metrics.
func (c *EmbedCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len()
}

func (c *EmbedCache) evict() {
	el := c.ll.Back()
	if el == nil {
		return
	}
	c.ll.Remove(el)
	delete(c.index, el.Value.(*cacheEntry).key)
}

func makeKey(embedder, text string) cacheKey {
	h := sha256.Sum256([]byte(text))
	return cacheKey{
		embedder: embedder,
		hashLow:  binary.LittleEndian.Uint64(h[:8]),
		hashHigh: binary.LittleEndian.Uint64(h[8:16]),
	}
}
