// Package reasoning provides an in-memory cache for reasoning_content so it can
// be replayed to the upstream on later turns.
//
// Thinking models (DeepSeek-R1, Kimi) require the assistant's reasoning_content
// to be passed back on subsequent turns. Codex does replay reasoning items in
// its input history, but unreliably, so the gateway keeps its own copy keyed
// two ways:
//
//   - by tool call_id: recovered when a function_call is replayed.
//   - by a hash of the assistant text: recovered when a plain assistant message
//     is replayed (Codex replays the whole conversation in input[]).
//
// Entries are bounded by an LRU capacity and a TTL. The cache is process-local,
// safe for concurrent use, has no external dependency, and is always on —
// independent of the optional SQLite metrics store. A nil *Cache is a no-op.
package reasoning

import (
	"container/list"
	"hash/fnv"
	"sync"
	"time"
)

// Default cache bounds. Reasoning blobs are small and only useful for the life
// of an in-flight conversation, so a modest capacity and a few hours of TTL are
// plenty.
const (
	DefaultCapacity = 4096
	DefaultTTL      = 6 * time.Hour
)

// Cache caches reasoning_content keyed by tool call_id and by assistant-text
// fingerprint. Use New to build one; a nil *Cache behaves as an empty no-op.
type Cache struct {
	byCall *lru[string]
	byHash *lru[uint64]
}

// New returns a Cache bounded by capacity entries (per index) and ttl.
func New(capacity int, ttl time.Duration) *Cache {
	return &Cache{
		byCall: newLRU[string](capacity, ttl),
		byHash: newLRU[uint64](capacity, ttl),
	}
}

// SaveCall stores reasoning under a tool call_id. Empty inputs are ignored.
func (c *Cache) SaveCall(callID, reasoning string) {
	if c == nil || callID == "" || reasoning == "" {
		return
	}
	c.byCall.set(callID, reasoning)
}

// SaveContent stores reasoning under a hash of the assistant text. Empty inputs
// are ignored.
func (c *Cache) SaveContent(content, reasoning string) {
	if c == nil || content == "" || reasoning == "" {
		return
	}
	c.byHash.set(hash(content), reasoning)
}

// ByCallID returns the reasoning stored for a tool call_id, or "" if absent.
func (c *Cache) ByCallID(callID string) string {
	if c == nil || callID == "" {
		return ""
	}
	return c.byCall.get(callID)
}

// ByContent returns the reasoning stored for the assistant text, or "" if absent.
func (c *Cache) ByContent(content string) string {
	if c == nil || content == "" {
		return ""
	}
	return c.byHash.get(hash(content))
}

func hash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}

// lru is a small TTL + capacity bounded LRU map, safe for concurrent use.
// Expiry is lazy (checked on read); capacity is enforced on write.
type lru[K comparable] struct {
	mu  sync.Mutex
	ll  *list.List // front = most recently used
	m   map[K]*list.Element
	cap int
	ttl time.Duration
}

type entry[K comparable] struct {
	key K
	val string
	exp time.Time
}

func newLRU[K comparable](capacity int, ttl time.Duration) *lru[K] {
	if capacity < 1 {
		capacity = 1
	}
	return &lru[K]{
		ll:  list.New(),
		m:   make(map[K]*list.Element, capacity),
		cap: capacity,
		ttl: ttl,
	}
}

func (l *lru[K]) get(key K) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	el, ok := l.m[key]
	if !ok {
		return ""
	}
	en := el.Value.(*entry[K])
	if time.Now().After(en.exp) {
		l.remove(el)
		return ""
	}
	l.ll.MoveToFront(el)
	return en.val
}

func (l *lru[K]) set(key K, val string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.m[key]; ok {
		en := el.Value.(*entry[K])
		en.val = val
		en.exp = time.Now().Add(l.ttl)
		l.ll.MoveToFront(el)
		return
	}
	el := l.ll.PushFront(&entry[K]{key: key, val: val, exp: time.Now().Add(l.ttl)})
	l.m[key] = el
	if l.ll.Len() > l.cap {
		if back := l.ll.Back(); back != nil {
			l.remove(back)
		}
	}
}

func (l *lru[K]) remove(el *list.Element) {
	l.ll.Remove(el)
	delete(l.m, el.Value.(*entry[K]).key)
}
