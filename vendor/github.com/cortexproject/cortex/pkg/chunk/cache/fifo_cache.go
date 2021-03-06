package cache

import (
	"context"
	"flag"
	"sync"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"github.com/cortexproject/cortex/pkg/util"
)

var (
	cacheEntriesAdded = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "added_total",
		Help:      "The total number of Put calls on the cache",
	}, []string{"cache"})

	cacheEntriesAddedNew = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "added_new_total",
		Help:      "The total number of new entries added to the cache",
	}, []string{"cache"})

	cacheEntriesEvicted = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "evicted_total",
		Help:      "The total number of evicted entries",
	}, []string{"cache"})

	cacheEntriesCurrent = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "entries",
		Help:      "The total number of entries",
	}, []string{"cache"})

	cacheTotalGets = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "gets_total",
		Help:      "The total number of Get calls",
	}, []string{"cache"})

	cacheTotalMisses = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "misses_total",
		Help:      "The total number of Get calls that had no valid entry",
	}, []string{"cache"})

	cacheStaleGets = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "stale_gets_total",
		Help:      "The total number of Get calls that had an entry which expired",
	}, []string{"cache"})

	cacheMemoryBytes = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "querier",
		Subsystem: "cache",
		Name:      "memory_bytes",
		Help:      "The current cache size in bytes",
	}, []string{"cache"})
)

// FifoCacheConfig holds config for the FifoCache.
type FifoCacheConfig struct {
	Size     int           `yaml:"size"`
	Validity time.Duration `yaml:"validity"`
}

// RegisterFlagsWithPrefix adds the flags required to config this to the given FlagSet
func (cfg *FifoCacheConfig) RegisterFlagsWithPrefix(prefix, description string, f *flag.FlagSet) {
	f.IntVar(&cfg.Size, prefix+"fifocache.size", 0, description+"The number of entries to cache.")
	f.DurationVar(&cfg.Validity, prefix+"fifocache.duration", 0, description+"The expiry duration for the cache.")
}

// FifoCache is a simple string -> interface{} cache which uses a fifo slide to
// manage evictions.  O(1) inserts and updates, O(1) gets.
type FifoCache struct {
	lock     sync.RWMutex
	size     int
	validity time.Duration
	entries  []cacheEntry
	index    map[string]int

	// indexes into entries to identify the most recent and least recent entry.
	first, last int

	entriesAdded    prometheus.Counter
	entriesAddedNew prometheus.Counter
	entriesEvicted  prometheus.Counter
	entriesCurrent  prometheus.Gauge
	totalGets       prometheus.Counter
	totalMisses     prometheus.Counter
	staleGets       prometheus.Counter
	memoryBytes     prometheus.Gauge
}

type cacheEntry struct {
	updated    time.Time
	key        string
	value      interface{}
	prev, next int
}

// NewFifoCache returns a new initialised FifoCache of size.
// TODO(bwplotka): Fix metrics, get them out of globals, separate or allow prefixing.
func NewFifoCache(name string, cfg FifoCacheConfig) *FifoCache {
	util.WarnExperimentalUse("In-memory (FIFO) cache")

	cache := &FifoCache{
		size:     cfg.Size,
		validity: cfg.Validity,
		entries:  make([]cacheEntry, 0, cfg.Size),
		index:    make(map[string]int, cfg.Size),

		// TODO(bwplotka): There might be simple cache.Cache wrapper for those.
		entriesAdded:    cacheEntriesAdded.WithLabelValues(name),
		entriesAddedNew: cacheEntriesAddedNew.WithLabelValues(name),
		entriesEvicted:  cacheEntriesEvicted.WithLabelValues(name),
		entriesCurrent:  cacheEntriesCurrent.WithLabelValues(name),
		totalGets:       cacheTotalGets.WithLabelValues(name),
		totalMisses:     cacheTotalMisses.WithLabelValues(name),
		staleGets:       cacheStaleGets.WithLabelValues(name),
		memoryBytes:     cacheMemoryBytes.WithLabelValues(name),
	}
	// set initial memory allocation
	cache.memoryBytes.Set(float64(int(unsafe.Sizeof(cacheEntry{})) * cache.size))
	return cache
}

// Fetch implements Cache.
func (c *FifoCache) Fetch(ctx context.Context, keys []string) (found []string, bufs [][]byte, missing []string) {
	found, missing, bufs = make([]string, 0, len(keys)), make([]string, 0, len(keys)), make([][]byte, 0, len(keys))
	for _, key := range keys {
		val, ok := c.Get(ctx, key)
		if !ok {
			missing = append(missing, key)
			continue
		}

		found = append(found, key)
		bufs = append(bufs, val.([]byte))
	}

	return
}

// Store implements Cache.
func (c *FifoCache) Store(ctx context.Context, keys []string, bufs [][]byte) {
	values := make([]interface{}, 0, len(bufs))
	for _, buf := range bufs {
		values = append(values, buf)
	}
	c.Put(ctx, keys, values)
}

// Stop implements Cache.
func (c *FifoCache) Stop() {
}

// Put stores the value against the key.
func (c *FifoCache) Put(ctx context.Context, keys []string, values []interface{}) {
	c.entriesAdded.Inc()
	if c.size == 0 {
		return
	}

	c.lock.Lock()
	defer c.lock.Unlock()

	for i := range keys {
		c.put(ctx, keys[i], values[i])
	}
}

func (c *FifoCache) put(ctx context.Context, key string, value interface{}) {
	// See if we already have the entry
	index, ok := c.index[key]
	if ok {
		entry := c.entries[index]
		deltaSize := sizeOf(value) - sizeOf(entry.value)

		entry.updated = time.Now()
		entry.value = value

		// Remove this entry from the FIFO linked-list.
		c.entries[entry.prev].next = entry.next
		c.entries[entry.next].prev = entry.prev

		// Corner case: updating last element
		if c.last == index {
			c.last = entry.prev
		}

		// Insert it at the beginning
		entry.next = c.first
		entry.prev = c.last
		c.entries[entry.next].prev = index
		c.entries[entry.prev].next = index
		c.first = index

		c.entries[index] = entry
		c.memoryBytes.Add(float64(deltaSize))
		return
	}
	c.entriesAddedNew.Inc()

	// Otherwise, see if we need to evict an entry.
	if len(c.entries) >= c.size {
		c.entriesEvicted.Inc()
		index = c.last
		entry := c.entries[index]
		deltaSize := sizeOf(key) + sizeOf(value) - sizeOf(entry.key) - sizeOf(entry.value)

		c.last = entry.prev
		c.first = index
		delete(c.index, entry.key)
		c.index[key] = index

		entry.updated = time.Now()
		entry.value = value
		entry.key = key
		c.entries[index] = entry
		c.memoryBytes.Add(float64(deltaSize))
		return
	}

	// Finally, no hit and we have space.
	index = len(c.entries)
	c.entries = append(c.entries, cacheEntry{
		updated: time.Now(),
		key:     key,
		value:   value,
		prev:    c.last,
		next:    c.first,
	})
	c.entries[c.first].prev = index
	c.entries[c.last].next = index
	c.first = index
	c.index[key] = index

	c.memoryBytes.Add(float64(sizeOf(key) + sizeOf(value)))
	c.entriesCurrent.Inc()
}

// Get returns the stored value against the key and when the key was last updated.
func (c *FifoCache) Get(ctx context.Context, key string) (interface{}, bool) {
	c.totalGets.Inc()
	if c.size == 0 {
		return nil, false
	}

	c.lock.RLock()
	defer c.lock.RUnlock()

	index, ok := c.index[key]
	if ok {
		updated := c.entries[index].updated
		if c.validity == 0 || time.Since(updated) < c.validity {
			return c.entries[index].value, true
		}

		c.totalMisses.Inc()
		c.staleGets.Inc()
		return nil, false
	}

	c.totalMisses.Inc()
	return nil, false
}

func sizeOf(i interface{}) int {
	switch v := i.(type) {
	case string:
		return len(v)
	case []int8:
		return len(v)
	case []uint8:
		return len(v)
	case []int32:
		return len(v) * 4
	case []uint32:
		return len(v) * 4
	case []float32:
		return len(v) * 4
	case []int64:
		return len(v) * 8
	case []uint64:
		return len(v) * 8
	case []float64:
		return len(v) * 8
	// next 2 cases are machine dependent
	case []int:
		if l := len(v); l > 0 {
			return int(unsafe.Sizeof(v[0])) * l
		}
		return 0
	case []uint:
		if l := len(v); l > 0 {
			return int(unsafe.Sizeof(v[0])) * l
		}
		return 0
	default:
		return int(unsafe.Sizeof(i))
	}
}
