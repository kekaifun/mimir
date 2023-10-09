// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/thanos-io/thanos/blob/main/pkg/store/cache/memcached.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Thanos Authors.

package indexcache

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"sync"
	"time"
	"unsafe"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/cache"
	"github.com/oklog/ulid/v2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"golang.org/x/crypto/blake2b"

	"github.com/grafana/mimir/pkg/storage/sharding"
)

const (
	remoteDefaultTTL = 7 * 24 * time.Hour
)

var (
	postingsCacheKeyLabelHashBufferPool = sync.Pool{New: func() any {
		// We assume the label name/value pair is typically not longer than 1KB.
		b := make([]byte, 1024)
		return &b
	}}
)

// RemoteIndexCache is a memcached or redis based index cache.
type RemoteIndexCache struct {
	logger log.Logger
	remote cache.RemoteCacheClient

	// Metrics.
	requests *prometheus.CounterVec
	hits     *prometheus.CounterVec
}

// NewRemoteIndexCache makes a new RemoteIndexCache.
func NewRemoteIndexCache(logger log.Logger, remote cache.RemoteCacheClient, reg prometheus.Registerer) (*RemoteIndexCache, error) {
	c := &RemoteIndexCache{
		logger: logger,
		remote: remote,
	}

	c.requests = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "thanos_store_index_cache_requests_total",
		Help: "Total number of items requests to the cache.",
	}, []string{"item_type"})
	initLabelValuesForAllCacheTypes(c.requests.MetricVec)

	c.hits = promauto.With(reg).NewCounterVec(prometheus.CounterOpts{
		Name: "thanos_store_index_cache_hits_total",
		Help: "Total number of items requests to the cache that were a hit.",
	}, []string{"item_type"})
	initLabelValuesForAllCacheTypes(c.hits.MetricVec)

	level.Info(logger).Log("msg", "created remote index cache")

	return c, nil
}

// set stores a value for the given key in the remote cache.
func (c *RemoteIndexCache) set(typ string, key string, val []byte) {
	if err := c.remote.SetAsync(key, val, remoteDefaultTTL); err != nil {
		level.Error(c.logger).Log("msg", "failed to set item in remote cache", "type", typ, "err", err)
	}
}

// get retrieves a single value from the remote cache, returned bool value indicates whether the value was found or not.
func (c *RemoteIndexCache) get(ctx context.Context, typ string, key string) ([]byte, bool) {
	c.requests.WithLabelValues(typ).Inc()
	results := c.remote.GetMulti(ctx, []string{key})
	data, ok := results[key]
	if ok {
		c.hits.WithLabelValues(typ).Inc()
	}
	return data, ok
}

// StorePostings sets the postings identified by the ulid and label to the value v.
// The function enqueues the request and returns immediately: the entry will be
// asynchronously stored in the cache.
func (c *RemoteIndexCache) StorePostings(userID string, blockID ulid.ULID, l labels.Label, v []byte) {
	c.set(cacheTypePostings, postingsCacheKey(userID, blockID.String(), l), v)
}

// FetchMultiPostings fetches multiple postings - each identified by a label.
// In case of error, it logs and return an empty result.
func (c *RemoteIndexCache) FetchMultiPostings(ctx context.Context, userID string, blockID ulid.ULID, lbls []labels.Label) BytesResult {
	blockIDStr := blockID.String()

	keys := make([]string, 0, len(lbls))
	for _, lbl := range lbls {
		keys = append(keys, postingsCacheKey(userID, blockIDStr, lbl))
	}

	// Fetch the keys from the remote cache in a single request.
	c.requests.WithLabelValues(cacheTypePostings).Add(float64(len(keys)))
	results := c.remote.GetMulti(ctx, keys)
	c.hits.WithLabelValues(cacheTypePostings).Add(float64(len(results)))

	return &MapIterator[string]{
		Keys: keys,
		M:    results,
	}
}

// postingsCacheKey returns the cache key used to store postings matching the input
// label name/value pair in the given block.
func postingsCacheKey(userID, blockID string, l labels.Label) string {
	const (
		prefix    = "P2:"
		separator = ":"
	)

	// Compute the label hash.
	lblHash, hashLen := postingsCacheKeyLabelID(l)

	// Preallocate the byte slice used to store the cache key.
	encodedHashLen := base64.RawURLEncoding.EncodedLen(hashLen)
	expectedLen := len(prefix) + len(userID) + 1 + ulid.EncodedSize + 1 + encodedHashLen
	key := make([]byte, expectedLen)
	offset := 0

	offset += copy(key[offset:], prefix)
	offset += copy(key[offset:], userID)
	offset += copy(key[offset:], separator)
	offset += copy(key[offset:], blockID)
	offset += copy(key[offset:], separator)
	base64.RawURLEncoding.Encode(key[offset:], lblHash[:hashLen])
	offset += encodedHashLen

	sizedKey := key[:offset]
	// Convert []byte to string with no extra allocation.
	return *(*string)(unsafe.Pointer(&sizedKey))
}

// postingsCacheKeyLabelID returns the hash of the input label or the label itself if it is shorter than the size of the hash.
// This is used as part of the cache key generated by postingsCacheKey().
func postingsCacheKeyLabelID(l labels.Label) (out [blake2b.Size256]byte, outLen int) {
	const separator = ":"

	// Compute the expected length.
	expectedLen := len(l.Name) + len(separator) + len(l.Value)

	// If the whole label is smaller than the hash, then shortcut hashing and directly write out the result.
	if expectedLen <= blake2b.Size256 {
		offset := 0
		offset += copy(out[offset:], l.Name)
		offset += copy(out[offset:], separator)
		offset += copy(out[offset:], l.Value)
		return out, offset
	}

	// Get a buffer from the pool and fill it with the label name/value pair to hash.
	bp := postingsCacheKeyLabelHashBufferPool.Get().(*[]byte)
	buf := *bp

	if cap(buf) < expectedLen {
		buf = make([]byte, expectedLen)
	} else {
		buf = buf[:expectedLen]
	}

	offset := 0
	offset += copy(buf[offset:], l.Name)
	offset += copy(buf[offset:], separator)
	offset += copy(buf[offset:], l.Value)

	// This is expected to be always equal. If it's not, then it's a severe bug.
	if offset != expectedLen {
		panic(fmt.Sprintf("postingsCacheKeyLabelID() computed an invalid expected length (expected: %d, actual: %d)", expectedLen, offset))
	}

	// Use cryptographically hash functions to avoid hash collisions
	// which would end up in wrong query results.
	hash := blake2b.Sum256(buf)

	// Reuse the same pointer to put the buffer back into the pool.
	*bp = buf
	postingsCacheKeyLabelHashBufferPool.Put(bp)

	return hash, len(hash)
}

// StoreSeriesForRef sets the series identified by the ulid and id to the value v.
// The function enqueues the request and returns immediately: the entry will be
// asynchronously stored in the cache.
func (c *RemoteIndexCache) StoreSeriesForRef(userID string, blockID ulid.ULID, id storage.SeriesRef, v []byte) {
	c.set(cacheTypeSeriesForRef, seriesForRefCacheKey(userID, blockID, id), v)
}

// FetchMultiSeriesForRefs fetches multiple series - each identified by ID - from the cache
// and returns a map containing cache hits, along with a list of missing IDs.
// In case of error, it logs and return an empty cache hits map.
func (c *RemoteIndexCache) FetchMultiSeriesForRefs(ctx context.Context, userID string, blockID ulid.ULID, ids []storage.SeriesRef) (hits map[storage.SeriesRef][]byte, misses []storage.SeriesRef) {
	// Build the cache keys, while keeping a map between input id and the cache key
	// so that we can easily reverse it back after the GetMulti().
	keys := make([]string, 0, len(ids))
	keysMapping := make(map[storage.SeriesRef]string, len(ids))

	for _, id := range ids {
		key := seriesForRefCacheKey(userID, blockID, id)

		keys = append(keys, key)
		keysMapping[id] = key
	}

	// Fetch the keys from the remote cache in a single request.
	c.requests.WithLabelValues(cacheTypeSeriesForRef).Add(float64(len(ids)))
	results := c.remote.GetMulti(ctx, keys)
	if len(results) == 0 {
		return nil, ids
	}

	// Construct the resulting hits map and list of missing keys. We iterate on the input
	// list of ids to be able to easily create the list of ones in a single iteration.
	hits = make(map[storage.SeriesRef][]byte, len(results))
	if numMisses := len(ids) - len(results); numMisses > 0 {
		misses = make([]storage.SeriesRef, 0, numMisses)
	}

	for _, id := range ids {
		key, ok := keysMapping[id]
		if !ok {
			_ = level.Error(c.logger).Log("msg", "keys mapping inconsistency found in remote index cache client", "type", "series", "id", id)
			continue
		}

		// Check if the key has been found in the remote cache. If not, we add it to the list
		// of missing keys.
		value, ok := results[key]
		if !ok {
			misses = append(misses, id)
			continue
		}

		hits[id] = value
	}

	c.hits.WithLabelValues(cacheTypeSeriesForRef).Add(float64(len(hits)))
	return hits, misses
}

func seriesForRefCacheKey(userID string, blockID ulid.ULID, id storage.SeriesRef) string {
	// Max uint64 string representation is no longer than 20 characters.
	b := make([]byte, 0, 20)
	return "S:" + userID + ":" + blockID.String() + ":" + string(strconv.AppendUint(b, uint64(id), 10))
}

// StoreExpandedPostings stores the encoded result of ExpandedPostings for specified matchers identified by the provided LabelMatchersKey.
func (c *RemoteIndexCache) StoreExpandedPostings(userID string, blockID ulid.ULID, lmKey LabelMatchersKey, postingsSelectionStrategy string, v []byte) {
	c.set(cacheTypeExpandedPostings, expandedPostingsCacheKey(userID, blockID, lmKey, postingsSelectionStrategy), v)
}

// FetchExpandedPostings fetches the encoded result of ExpandedPostings for specified matchers identified by the provided LabelMatchersKey.
func (c *RemoteIndexCache) FetchExpandedPostings(ctx context.Context, userID string, blockID ulid.ULID, lmKey LabelMatchersKey, postingsSelectionStrategy string) ([]byte, bool) {
	return c.get(ctx, cacheTypeExpandedPostings, expandedPostingsCacheKey(userID, blockID, lmKey, postingsSelectionStrategy))
}

func expandedPostingsCacheKey(userID string, blockID ulid.ULID, lmKey LabelMatchersKey, postingsSelectionStrategy string) string {
	hash := blake2b.Sum256([]byte(lmKey))
	return "E2:" + userID + ":" + blockID.String() + ":" + base64.RawURLEncoding.EncodeToString(hash[0:]) + ":" + postingsSelectionStrategy
}

// StoreSeriesForPostings stores a series set for the provided postings.
func (c *RemoteIndexCache) StoreSeriesForPostings(userID string, blockID ulid.ULID, shard *sharding.ShardSelector, postingsKey PostingsKey, v []byte) {
	c.set(cacheTypeSeriesForPostings, seriesForPostingsCacheKey(userID, blockID, shard, postingsKey), v)
}

// FetchSeriesForPostings fetches a series set for the provided postings.
func (c *RemoteIndexCache) FetchSeriesForPostings(ctx context.Context, userID string, blockID ulid.ULID, shard *sharding.ShardSelector, postingsKey PostingsKey) ([]byte, bool) {
	return c.get(ctx, cacheTypeSeriesForPostings, seriesForPostingsCacheKey(userID, blockID, shard, postingsKey))
}

func seriesForPostingsCacheKey(userID string, blockID ulid.ULID, shard *sharding.ShardSelector, postingsKey PostingsKey) string {
	// We use SP2: as
	// * S: is already used for SeriesForRef
	// * SS: is already used for Series
	// * SP: was in use when using gob encoding
	//
	// "SP2" (3) + userID (150) + blockID (26) + shard (10 with up to 1000 shards) + ":" (4) = 193
	// Memcached limits key length to 250, so we're left with 57 bytes for the postings key.
	return "SP2:" + userID + ":" + blockID.String() + ":" + shardKey(shard) + ":" + string(postingsKey)
}

// StoreLabelNames stores the result of a LabelNames() call.
func (c *RemoteIndexCache) StoreLabelNames(userID string, blockID ulid.ULID, matchersKey LabelMatchersKey, v []byte) {
	c.set(cacheTypeLabelNames, labelNamesCacheKey(userID, blockID, matchersKey), v)
}

// FetchLabelNames fetches the result of a LabelNames() call.
func (c *RemoteIndexCache) FetchLabelNames(ctx context.Context, userID string, blockID ulid.ULID, matchersKey LabelMatchersKey) ([]byte, bool) {
	return c.get(ctx, cacheTypeLabelNames, labelNamesCacheKey(userID, blockID, matchersKey))
}

func labelNamesCacheKey(userID string, blockID ulid.ULID, matchersKey LabelMatchersKey) string {
	hash := blake2b.Sum256([]byte(matchersKey))
	return "LN:" + userID + ":" + blockID.String() + ":" + base64.RawURLEncoding.EncodeToString(hash[0:])
}

// StoreLabelValues stores the result of a LabelValues() call.
func (c *RemoteIndexCache) StoreLabelValues(userID string, blockID ulid.ULID, labelName string, matchersKey LabelMatchersKey, v []byte) {
	c.set(cacheTypeLabelValues, labelValuesCacheKey(userID, blockID, labelName, matchersKey), v)
}

// FetchLabelValues fetches the result of a LabelValues() call.
func (c *RemoteIndexCache) FetchLabelValues(ctx context.Context, userID string, blockID ulid.ULID, labelName string, matchersKey LabelMatchersKey) ([]byte, bool) {
	return c.get(ctx, cacheTypeLabelValues, labelValuesCacheKey(userID, blockID, labelName, matchersKey))
}

func labelValuesCacheKey(userID string, blockID ulid.ULID, labelName string, matchersKey LabelMatchersKey) string {
	hash := blake2b.Sum256([]byte(matchersKey))
	return "LV2:" + userID + ":" + blockID.String() + ":" + labelName + ":" + base64.RawURLEncoding.EncodeToString(hash[0:])
}
