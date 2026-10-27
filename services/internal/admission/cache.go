package admission

import (
	"sync"
	"time"
)

// Cache defaults. Each is a trade someone should be able to argue with, so
// each says what it is trading.
const (
	// defaultTTL is the staleness budget for a positive answer: an operator
	// edit takes up to this long to affect a live call. A minute is short
	// enough that nobody waits on it and long enough that a steady call rate
	// mostly misses the database.
	defaultTTL = time.Minute

	// defaultNegativeTTL is shorter, because the cost of the two mistakes is
	// not symmetric. A stale positive routes a call that should have been
	// refused; a stale negative refuses a number that was just provisioned,
	// and provisioning a number is exactly when someone is watching and
	// testing it.
	defaultNegativeTTL = 15 * time.Second

	// defaultStaleWindow is how long an expired entry may still be served
	// when the database is unreachable. Fifteen minutes covers an RDS failover
	// or a maintenance blip. Beyond it, refusing is more honest than routing
	// on configuration that may have been changed twice since.
	defaultStaleWindow = 15 * time.Minute

	// defaultMaxEntries bounds memory. Every number a carrier can deliver can
	// reach this cache, not only the ones we own, so an unbounded map is a
	// slow memory leak driven by whoever is scanning us today.
	defaultMaxEntries = 10_000
)

type entry struct {
	route     Route
	notFound  bool
	expiresAt time.Time
	staleAt   time.Time
}

// cache is a TTL map with a stale window. A plain map plus a mutex, not an LRU:
// at beta volume the entry count is small, evictions are rare, and an LRU's
// bookkeeping would be more code than the thing it optimises.
type cache struct {
	mu      sync.RWMutex
	entries map[string]entry

	ttl         time.Duration
	negativeTTL time.Duration
	staleWindow time.Duration
	maxEntries  int

	// now is swappable so the tests can advance time instead of sleeping.
	now func() time.Time
}

func newCache(opts Options) *cache {
	c := &cache{
		entries:     make(map[string]entry),
		ttl:         opts.TTL,
		negativeTTL: opts.NegativeTTL,
		staleWindow: opts.StaleWindow,
		maxEntries:  opts.MaxEntries,
		now:         time.Now,
	}

	if c.ttl <= 0 {
		c.ttl = defaultTTL
	}
	if c.negativeTTL <= 0 {
		c.negativeTTL = defaultNegativeTTL
	}
	if c.staleWindow <= 0 {
		c.staleWindow = defaultStaleWindow
	}
	if c.maxEntries <= 0 {
		c.maxEntries = defaultMaxEntries
	}

	return c
}

// fresh returns an entry that has not expired.
func (c *cache) fresh(number string) (entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[number]
	if !ok || c.now().After(e.expiresAt) {
		return entry{}, false
	}
	return e, true
}

// stale returns an expired entry that is still inside the stale window. Only
// used when the database is unreachable.
func (c *cache) stale(number string) (entry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	e, ok := c.entries[number]
	if !ok || c.now().After(e.staleAt) {
		return entry{}, false
	}
	return e, true
}

func (c *cache) put(number string, route Route, notFound bool) {
	if number == "" {
		return
	}

	ttl := c.ttl
	if notFound {
		ttl = c.negativeTTL
	}

	now := c.now()

	c.mu.Lock()
	defer c.mu.Unlock()

	// Evict before inserting a NEW key, so the map cannot exceed the bound.
	// Refreshing an existing key is not growth and must not trigger this.
	if _, exists := c.entries[number]; !exists && len(c.entries) >= c.maxEntries {
		c.evictExpiredLocked(now)

		// Still full: every entry is live. Drop one arbitrary entry rather
		// than refusing to cache, which would silently turn the cache off
		// under exactly the load it exists for. Map iteration order makes the
		// victim effectively random, which is fine — a wrongly evicted entry
		// costs one query.
		if len(c.entries) >= c.maxEntries {
			for k := range c.entries {
				delete(c.entries, k)
				break
			}
		}
	}

	c.entries[number] = entry{
		route:     route,
		notFound:  notFound,
		expiresAt: now.Add(ttl),
		staleAt:   now.Add(ttl + c.staleWindow),
	}
}

func (c *cache) drop(number string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, number)
}

func (c *cache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[string]entry)
}

// evictExpiredLocked removes entries past their stale window. Anything inside
// it is still useful — that is the entry that keeps calls routing when the
// database is down — so expiry alone is not grounds for eviction.
func (c *cache) evictExpiredLocked(now time.Time) {
	for k, e := range c.entries {
		if now.After(e.staleAt) {
			delete(c.entries, k)
		}
	}
}

func (c *cache) len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}
