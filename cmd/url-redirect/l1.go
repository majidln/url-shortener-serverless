package main

import (
	"sync"
	"time"
)

// l1Cache is a tiny in-process TTL cache for key -> url. It is deliberately NOT an
// LRU: when it fills it drops everything and starts over. Each Lambda execution
// environment has its own copy and a cold container starts empty, so the hit
// rate tracks how concentrated the traffic is. The TTL is the only invalidation
// — the longest a repointed or deleted link can still be served from here.
type l1Cache struct {
	mu         sync.Mutex
	m          map[string]entry
	ttl        time.Duration
	maxEntries int
	now        func() time.Time // swapped in tests
}

type entry struct {
	url string
	exp time.Time
}

func newL1Cache(ttl time.Duration, maxEntries int) *l1Cache {
	return &l1Cache{
		m:          make(map[string]entry),
		ttl:        ttl,
		maxEntries: maxEntries,
		now:        time.Now,
	}
}

// Get returns the url for key and true on a live hit. An expired entry is
// dropped and reported as a miss.
func (c *l1Cache) Get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.m[key]
	if !ok {
		return "", false
	}
	if !c.now().Before(e.exp) {
		delete(c.m, key)
		return "", false
	}
	return e.url, true
}

// Put stores key -> url with a fresh TTL. If the map is at capacity it is
// cleared first (see the type comment).
func (c *l1Cache) Put(key, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxEntries <= 0 || c.ttl <= 0 {
		return // caching disabled by config
	}
	if len(c.m) >= c.maxEntries {
		c.m = make(map[string]entry, c.maxEntries)
	}
	c.m[key] = entry{url: url, exp: c.now().Add(c.ttl)}
}
