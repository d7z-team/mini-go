package runtime

import (
	"maps"
	"sync"
)

// metadataCache publishes immutable metadata. Resolution runs outside the lock
// because resolving one type can recursively query another module's cache.
type metadataCache[K comparable, V any] struct {
	mu     sync.Mutex
	values map[K]V
}

func (cache *metadataCache[K, V]) load(key K) (V, bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	value, found := cache.values[key]
	return value, found
}

func (cache *metadataCache[K, V]) store(key K, value V) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cache.values == nil {
		cache.values = make(map[K]V)
	}
	cache.values[key] = value
}

func (cache *metadataCache[K, V]) loadOrStore(key K, value V) V {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if previous, found := cache.values[key]; found {
		return previous
	}
	if cache.values == nil {
		cache.values = make(map[K]V)
	}
	cache.values[key] = value
	return value
}

func (cache *metadataCache[K, V]) snapshot() map[K]V {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	return maps.Clone(cache.values)
}

func (cache *metadataCache[K, V]) clear() {
	cache.mu.Lock()
	cache.values = nil
	cache.mu.Unlock()
}
