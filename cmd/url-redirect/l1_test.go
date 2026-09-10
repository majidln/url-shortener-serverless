package main

import (
	"testing"
	"time"
)

// fixedClock returns a now() the test can advance by hand.
func fixedClock(start time.Time) (func() time.Time, func(d time.Duration)) {
	t := start
	return func() time.Time { return t }, func(d time.Duration) { t = t.Add(d) }
}

func TestCacheHitAndMiss(t *testing.T) {
	c := newL1Cache(time.Minute, 10)

	if _, ok := c.Get("k"); ok {
		t.Fatal("Get on empty cache = hit, want miss")
	}
	c.Put("k", "https://example.com/x")
	got, ok := c.Get("k")
	if !ok || got != "https://example.com/x" {
		t.Fatalf("Get after Put = (%q, %v), want (%q, true)", got, ok, "https://example.com/x")
	}
}

func TestCacheExpiry(t *testing.T) {
	c := newL1Cache(time.Minute, 10)
	now, advance := fixedClock(time.Unix(0, 0))
	c.now = now

	c.Put("k", "u")
	advance(59 * time.Second)
	if _, ok := c.Get("k"); !ok {
		t.Fatal("entry expired early")
	}
	advance(2 * time.Second) // now 61s, past the 60s TTL
	if _, ok := c.Get("k"); ok {
		t.Fatal("expired entry still returned")
	}
	// expired entry should have been dropped, not just skipped
	c.mu.Lock()
	_, present := c.m["k"]
	c.mu.Unlock()
	if present {
		t.Error("expired entry left in the map")
	}
}

func TestCacheClearsWhenFull(t *testing.T) {
	c := newL1Cache(time.Minute, 2)
	c.Put("a", "1")
	c.Put("b", "2")
	c.Put("c", "3") // at capacity -> whole map dropped, then c added

	if _, ok := c.Get("a"); ok {
		t.Error("a survived the capacity clear")
	}
	if _, ok := c.Get("b"); ok {
		t.Error("b survived the capacity clear")
	}
	if got, ok := c.Get("c"); !ok || got != "3" {
		t.Errorf("c = (%q, %v), want (%q, true)", got, ok, "3")
	}
}

func TestCacheDisabledByConfig(t *testing.T) {
	for _, c := range []*l1Cache{
		newL1Cache(0, 10),          // ttl 0
		newL1Cache(time.Minute, 0), // no capacity
	} {
		c.Put("k", "u")
		if _, ok := c.Get("k"); ok {
			t.Errorf("disabled cache (%v ttl, %d max) returned a hit", c.ttl, c.maxEntries)
		}
	}
}

func TestCacheConcurrentAccess(t *testing.T) {
	c := newL1Cache(time.Minute, 100)
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 1000; j++ {
				c.Put("k", "u")
				c.Get("k")
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
