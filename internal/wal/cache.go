package wal

import (
	"container/list"
	"sync"
	"time"
)

// Cache provides intelligent caching for WAL operations
type Cache struct {
	// LRU cache for materialized states
	stateCache *LRUCache

	// Recent entries cache (for append optimizations)
	recentEntries *list.List
	recentMap     map[int64]*list.Element
	maxRecent     int

	// Query result cache
	queryCache    map[string]*CachedQuery
	queryCacheTTL time.Duration

	// Branch metadata cache
	branchCache    map[string]*CachedBranch
	branchCacheTTL time.Duration

	// Coordination
	mu            sync.RWMutex
	cleanupTicker *time.Ticker
	stopCleanup   chan struct{}
}

// CachedState represents a cached materialized state
type CachedState struct {
	Collection string
	LSN        int64
	State      map[string]interface{}
	AccessTime time.Time
	Size       int64
}

// CachedQuery represents a cached query result
type CachedQuery struct {
	Key        string
	Result     interface{}
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AccessTime time.Time
	HitCount   int64
}

// CachedBranch represents cached branch metadata
type CachedBranch struct {
	BranchID   string
	Metadata   interface{}
	CreatedAt  time.Time
	ExpiresAt  time.Time
	AccessTime time.Time
}

// LRUCache implements a Least Recently Used cache
type LRUCache struct {
	capacity int64
	size     int64
	items    map[string]*list.Element
	order    *list.List
	mu       sync.RWMutex
}

// LRUItem represents an item in the LRU cache
type LRUItem struct {
	key   string
	value *CachedState
}

// CacheConfig configures cache behavior
type CacheConfig struct {
	MaxStateMemory   int64         // Maximum memory for state cache (bytes)
	MaxRecentEntries int           // Maximum recent entries to keep
	QueryCacheTTL    time.Duration // Query cache time-to-live
	BranchCacheTTL   time.Duration // Branch cache time-to-live
	CleanupInterval  time.Duration // How often to run cleanup
	EnableMetrics    bool          // Enable cache metrics
}

// CacheStats provides cache performance statistics
type CacheStats struct {
	StateCache   LRUStats
	QueryCache   QueryCacheStats
	BranchCache  BranchCacheStats
	RecentHits   int64
	RecentMisses int64
}

type LRUStats struct {
	Size     int64
	Capacity int64
	Items    int
	Hits     int64
	Misses   int64
}

type QueryCacheStats struct {
	Items   int
	Hits    int64
	Misses  int64
	Expired int64
}

type BranchCacheStats struct {
	Items   int
	Hits    int64
	Misses  int64
	Expired int64
}

// NewCache creates a new WAL cache
func NewCache(config CacheConfig) *Cache {
	if config.MaxStateMemory == 0 {
		config.MaxStateMemory = 100 * 1024 * 1024 // 100MB default
	}
	if config.MaxRecentEntries == 0 {
		config.MaxRecentEntries = 1000
	}
	if config.QueryCacheTTL == 0 {
		config.QueryCacheTTL = 5 * time.Minute
	}
	if config.BranchCacheTTL == 0 {
		config.BranchCacheTTL = 30 * time.Minute
	}
	if config.CleanupInterval == 0 {
		config.CleanupInterval = 1 * time.Minute
	}

	cache := &Cache{
		stateCache:     NewLRUCache(config.MaxStateMemory),
		recentEntries:  list.New(),
		recentMap:      make(map[int64]*list.Element),
		maxRecent:      config.MaxRecentEntries,
		queryCache:     make(map[string]*CachedQuery),
		queryCacheTTL:  config.QueryCacheTTL,
		branchCache:    make(map[string]*CachedBranch),
		branchCacheTTL: config.BranchCacheTTL,
		stopCleanup:    make(chan struct{}),
	}

	// Start cleanup routine
	cache.cleanupTicker = time.NewTicker(config.CleanupInterval)
	go cache.cleanupLoop()

	return cache
}

// NewLRUCache creates a new LRU cache
func NewLRUCache(capacity int64) *LRUCache {
	return &LRUCache{
		capacity: capacity,
		size:     0,
		items:    make(map[string]*list.Element),
		order:    list.New(),
	}
}

// GetMaterializedState retrieves a cached materialized state
func (c *Cache) GetMaterializedState(collection string, lsn int64) (map[string]interface{}, bool) {
	key := c.stateKey(collection, lsn)
	return c.stateCache.Get(key)
}

// SetMaterializedState caches a materialized state
func (c *Cache) SetMaterializedState(collection string, lsn int64, state map[string]interface{}) {
	key := c.stateKey(collection, lsn)

	// Estimate size (rough approximation)
	size := c.estimateStateSize(state)

	cachedState := &CachedState{
		Collection: collection,
		LSN:        lsn,
		State:      state,
		AccessTime: time.Now(),
		Size:       size,
	}

	c.stateCache.Set(key, cachedState)
}

// GetQueryResult retrieves a cached query result
func (c *Cache) GetQueryResult(queryKey string) (interface{}, bool) {
	c.mu.RLock()
	cached, exists := c.queryCache[queryKey]
	c.mu.RUnlock()

	if !exists {
		return nil, false
	}

	// Check if expired
	if time.Now().After(cached.ExpiresAt) {
		c.mu.Lock()
		delete(c.queryCache, queryKey)
		c.mu.Unlock()
		return nil, false
	}

	// Update access time and hit count
	c.mu.Lock()
	cached.AccessTime = time.Now()
	cached.HitCount++
	c.mu.Unlock()

	return cached.Result, true
}

// SetQueryResult caches a query result
func (c *Cache) SetQueryResult(queryKey string, result interface{}) {
	now := time.Now()

	cached := &CachedQuery{
		Key:        queryKey,
		Result:     result,
		CreatedAt:  now,
		ExpiresAt:  now.Add(c.queryCacheTTL),
		AccessTime: now,
		HitCount:   0,
	}

	c.mu.Lock()
	c.queryCache[queryKey] = cached
	c.mu.Unlock()
}

// GetBranchMetadata retrieves cached branch metadata
func (c *Cache) GetBranchMetadata(branchID string) (interface{}, bool) {
	c.mu.RLock()
	cached, exists := c.branchCache[branchID]
	c.mu.RUnlock()

	if !exists {
		return nil, false
	}

	// Check if expired
	if time.Now().After(cached.ExpiresAt) {
		c.mu.Lock()
		delete(c.branchCache, branchID)
		c.mu.Unlock()
		return nil, false
	}

	// Update access time
	c.mu.Lock()
	cached.AccessTime = time.Now()
	c.mu.Unlock()

	return cached.Metadata, true
}

// SetBranchMetadata caches branch metadata
func (c *Cache) SetBranchMetadata(branchID string, metadata interface{}) {
	now := time.Now()

	cached := &CachedBranch{
		BranchID:   branchID,
		Metadata:   metadata,
		CreatedAt:  now,
		ExpiresAt:  now.Add(c.branchCacheTTL),
		AccessTime: now,
	}

	c.mu.Lock()
	c.branchCache[branchID] = cached
	c.mu.Unlock()
}

// AddRecentEntry adds an entry to the recent entries cache
func (c *Cache) AddRecentEntry(lsn int64, entry interface{}) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Remove if already exists
	if elem, exists := c.recentMap[lsn]; exists {
		c.recentEntries.Remove(elem)
		delete(c.recentMap, lsn)
	}

	// Add to front
	elem := c.recentEntries.PushFront(map[string]interface{}{
		"lsn":   lsn,
		"entry": entry,
	})
	c.recentMap[lsn] = elem

	// Evict oldest if over capacity
	for c.recentEntries.Len() > c.maxRecent {
		oldest := c.recentEntries.Back()
		if oldest != nil {
			data := oldest.Value.(map[string]interface{})
			oldLSN := data["lsn"].(int64)
			c.recentEntries.Remove(oldest)
			delete(c.recentMap, oldLSN)
		}
	}
}

// GetRecentEntry retrieves a recent entry
func (c *Cache) GetRecentEntry(lsn int64) (interface{}, bool) {
	c.mu.RLock()
	elem, exists := c.recentMap[lsn]
	c.mu.RUnlock()

	if !exists {
		return nil, false
	}

	data := elem.Value.(map[string]interface{})
	return data["entry"], true
}

// InvalidateState removes cached states for a collection
func (c *Cache) InvalidateState(collection string) {
	c.stateCache.InvalidateByPrefix(collection + ":")
}

// InvalidateBranch removes cached data for a branch
func (c *Cache) InvalidateBranch(branchID string) {
	c.mu.Lock()
	delete(c.branchCache, branchID)
	c.mu.Unlock()
}

// GetStats returns cache performance statistics
func (c *Cache) GetStats() CacheStats {
	return CacheStats{
		StateCache:  c.stateCache.GetStats(),
		QueryCache:  c.getQueryCacheStats(),
		BranchCache: c.getBranchCacheStats(),
	}
}

// Clear removes all cached data
func (c *Cache) Clear() {
	c.stateCache.Clear()

	c.mu.Lock()
	c.queryCache = make(map[string]*CachedQuery)
	c.branchCache = make(map[string]*CachedBranch)
	c.recentEntries = list.New()
	c.recentMap = make(map[int64]*list.Element)
	c.mu.Unlock()
}

// Close stops the cache and cleanup routines
func (c *Cache) Close() {
	close(c.stopCleanup)
	c.cleanupTicker.Stop()
}

// LRUCache methods
func (lru *LRUCache) Get(key string) (map[string]interface{}, bool) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	elem, exists := lru.items[key]
	if !exists {
		return nil, false
	}

	// Move to front (most recently used)
	lru.order.MoveToFront(elem)

	item := elem.Value.(*LRUItem)
	item.value.AccessTime = time.Now()

	return item.value.State, true
}

func (lru *LRUCache) Set(key string, state *CachedState) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	if elem, exists := lru.items[key]; exists {
		// Update existing item
		lru.order.MoveToFront(elem)
		item := elem.Value.(*LRUItem)
		oldSize := item.value.Size
		item.value = state
		lru.size = lru.size - oldSize + state.Size
	} else {
		// Add new item
		item := &LRUItem{key: key, value: state}
		elem := lru.order.PushFront(item)
		lru.items[key] = elem
		lru.size += state.Size
	}

	// Evict items if over capacity
	for lru.size > lru.capacity && lru.order.Len() > 0 {
		lru.evictOldest()
	}
}

func (lru *LRUCache) InvalidateByPrefix(prefix string) {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	keysToDelete := make([]string, 0)
	for key := range lru.items {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			keysToDelete = append(keysToDelete, key)
		}
	}

	for _, key := range keysToDelete {
		if elem, exists := lru.items[key]; exists {
			lru.order.Remove(elem)
			item := elem.Value.(*LRUItem)
			lru.size -= item.value.Size
			delete(lru.items, key)
		}
	}
}

func (lru *LRUCache) Clear() {
	lru.mu.Lock()
	defer lru.mu.Unlock()

	lru.items = make(map[string]*list.Element)
	lru.order = list.New()
	lru.size = 0
}

func (lru *LRUCache) GetStats() LRUStats {
	lru.mu.RLock()
	defer lru.mu.RUnlock()

	return LRUStats{
		Size:     lru.size,
		Capacity: lru.capacity,
		Items:    len(lru.items),
	}
}

func (lru *LRUCache) evictOldest() {
	elem := lru.order.Back()
	if elem != nil {
		lru.order.Remove(elem)
		item := elem.Value.(*LRUItem)
		delete(lru.items, item.key)
		lru.size -= item.value.Size
	}
}

// Helper methods
func (c *Cache) stateKey(collection string, lsn int64) string {
	return collection + ":" + string(rune(lsn))
}

func (c *Cache) estimateStateSize(state map[string]interface{}) int64 {
	// Rough estimation - in production, might use more sophisticated calculation
	return int64(len(state) * 100) // Assume 100 bytes per document on average
}

func (c *Cache) cleanupLoop() {
	for {
		select {
		case <-c.stopCleanup:
			return
		case <-c.cleanupTicker.C:
			c.cleanup()
		}
	}
}

func (c *Cache) cleanup() {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Clean up expired query cache entries
	for key, cached := range c.queryCache {
		if now.After(cached.ExpiresAt) {
			delete(c.queryCache, key)
		}
	}

	// Clean up expired branch cache entries
	for key, cached := range c.branchCache {
		if now.After(cached.ExpiresAt) {
			delete(c.branchCache, key)
		}
	}
}

func (c *Cache) getQueryCacheStats() QueryCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return QueryCacheStats{
		Items: len(c.queryCache),
	}
}

func (c *Cache) getBranchCacheStats() BranchCacheStats {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return BranchCacheStats{
		Items: len(c.branchCache),
	}
}
