// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/storage/tsdb/config.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package tsdb

import (
	"flag"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/alecthomas/units"
	"github.com/go-kit/log"
	"github.com/pkg/errors"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/wlog"

	"github.com/grafana/mimir/pkg/ingester/activeseries"
	"github.com/grafana/mimir/pkg/storage/bucket"
	"github.com/grafana/mimir/pkg/storegateway/indexheader"
	"github.com/grafana/mimir/pkg/util"
)

const (
	// DeprecatedTenantIDExternalLabel is the external label containing the tenant ID,
	// set when shipping blocks to the storage.
	//
	// Mimir no longer generates blocks with this label, however old blocks may still use this label.
	DeprecatedTenantIDExternalLabel = "__org_id__"

	// DeprecatedIngesterIDExternalLabel is the external label containing the ingester ID,
	// set when shipping blocks to the storage.
	//
	// Mimir no longer generates blocks with this label, however old blocks may still use this label.
	DeprecatedIngesterIDExternalLabel = "__ingester_id__"

	// CompactorShardIDExternalLabel is the external label used to store
	// the ID of a sharded block generated by the split-and-merge compactor. If a block hasn't
	// this label, it means the block hasn't been split.
	CompactorShardIDExternalLabel = "__compactor_shard_id__"

	// DeprecatedShardIDExternalLabel is deprecated.
	DeprecatedShardIDExternalLabel = "__shard_id__"

	// OutOfOrderExternalLabel is the external label used to mark blocks
	// containing out-of-order data.
	OutOfOrderExternalLabel = "__out_of_order__"

	// OutOfOrderExternalLabelValue is the value to be used for the OutOfOrderExternalLabel label
	OutOfOrderExternalLabelValue = "true"

	// DefaultCloseIdleTSDBInterval is how often are open TSDBs checked for being idle and closed.
	DefaultCloseIdleTSDBInterval = 5 * time.Minute

	// DeletionMarkCheckInterval is how often to check for tenant deletion mark.
	DeletionMarkCheckInterval = 1 * time.Hour

	// EstimatedMaxChunkSize is average max of chunk size. This can be exceeded though in very rare (valid) cases.
	EstimatedMaxChunkSize = 16000

	// EstimatedSeriesP99Size is the size in bytes of a single series in the TSDB index. This includes the symbol IDs in
	// the symbols table (not the actual label strings) and the chunk refs (min time, max time, offset).
	// This is an estimation that should cover >99% of series with less than 30 labels and around 50 chunks per series.
	EstimatedSeriesP99Size = 512

	// MaxSeriesSize is an estimated worst case for a series size in the index. A valid series may still be larger than this.
	// This was calculated assuming a rate of 1 sample/sec, in a 24h block we have 744 chunks per series.
	// The worst case scenario of each meta ref is 8*3=24 bytes, so 744*24 = 17856 bytes, which is 448 bytes away form 17 KiB.
	MaxSeriesSize = 17 * 1024

	// BytesPerPostingInAPostingList is the number of bytes that each posting (series ID) takes in a
	// posting list in the index. Each posting is 4 bytes (uint32) which are the offset of the series in the index file.
	BytesPerPostingInAPostingList = 4

	// ChunkPoolDefaultMinBucketSize is the default minimum bucket size (bytes) of the chunk pool.
	ChunkPoolDefaultMinBucketSize = EstimatedMaxChunkSize // Deprecated. TODO: Remove in Mimir 2.11.

	// ChunkPoolDefaultMaxBucketSize is the default maximum bucket size (bytes) of the chunk pool.
	ChunkPoolDefaultMaxBucketSize = 50e6 // Deprecated. TODO: Remove in Mimir 2.11.

	// DefaultPostingOffsetInMemorySampling represents default value for --store.index-header-posting-offsets-in-mem-sampling.
	// 32 value is chosen as it's a good balance for common setups. Sampling that is not too large (too many CPU cycles) and
	// not too small (too much memory).
	DefaultPostingOffsetInMemorySampling = 32

	// DefaultPartitionerMaxGapSize is the default max size - in bytes - of a gap for which the store-gateway
	// partitioner aggregates together two bucket GET object requests.
	DefaultPartitionerMaxGapSize = uint64(512 * 1024)

	headChunkWriterBufferSizeHelp = "The write buffer size used by the head chunks mapper. Lower values reduce memory utilisation on clusters with a large number of tenants at the cost of increased disk I/O operations."
	headChunksEndTimeVarianceHelp = "How much variance (as percentage between 0 and 1) should be applied to the chunk end time, to spread chunks writing across time. Doesn't apply to the last chunk of the chunk range. 0 means no variance."
	headStripeSizeHelp            = "The number of shards of series to use in TSDB (must be a power of 2). Reducing this will decrease memory footprint, but can negatively impact performance."
	headChunksWriteQueueSizeHelp  = "The size of the write queue used by the head chunks mapper. Lower values reduce memory utilisation at the cost of potentially higher ingest latency. Value of 0 switches chunks mapper to implementation without a queue."

	headCompactionIntervalFlag                = "blocks-storage.tsdb.head-compaction-interval"
	maxTSDBOpeningConcurrencyOnStartupFlag    = "blocks-storage.tsdb.max-tsdb-opening-concurrency-on-startup"
	defaultMaxTSDBOpeningConcurrencyOnStartup = 10

	maxChunksBytesPoolFlag      = "blocks-storage.bucket-store.max-chunk-pool-bytes"
	minBucketSizeBytesFlag      = "blocks-storage.bucket-store.chunk-pool-min-bucket-size-bytes"
	maxBucketSizeBytesFlag      = "blocks-storage.bucket-store.chunk-pool-max-bucket-size-bytes"
	seriesSelectionStrategyFlag = "blocks-storage.bucket-store.series-selection-strategy"
	bucketIndexFlagPrefix       = "blocks-storage.bucket-store.bucket-index."
)

// Validation errors
var (
	errInvalidShipConcurrency                       = errors.New("invalid TSDB ship concurrency")
	errInvalidOpeningConcurrency                    = errors.New("invalid TSDB opening concurrency")
	errInvalidCompactionInterval                    = errors.New("invalid TSDB compaction interval")
	errInvalidCompactionConcurrency                 = errors.New("invalid TSDB compaction concurrency")
	errInvalidWALSegmentSizeBytes                   = errors.New("invalid TSDB WAL segment size bytes")
	errInvalidWALReplayConcurrency                  = errors.New("invalid TSDB WAL replay concurrency")
	errInvalidStripeSize                            = errors.New("invalid TSDB stripe size")
	errInvalidStreamingBatchSize                    = errors.New("invalid store-gateway streaming batch size")
	errInvalidEarlyHeadCompactionMinSeriesReduction = errors.New("early compaction minimum series reduction percentage must be a value between 0 and 100 (included)")
	errEarlyCompactionRequiresActiveSeries          = fmt.Errorf("early compaction requires -%s to be enabled", activeseries.EnabledFlag)
	errEmptyBlockranges                             = errors.New("empty block ranges for TSDB")
	errInvalidIndexHeaderLazyLoadingConcurrency     = errors.New("invalid index-header lazy loading max concurrency; must be non-negative")
)

// BlocksStorageConfig holds the config information for the blocks storage.
type BlocksStorageConfig struct {
	Bucket      bucket.Config     `yaml:",inline"`
	BucketStore BucketStoreConfig `yaml:"bucket_store" doc:"description=This configures how the querier and store-gateway discover and synchronize blocks stored in the bucket."`
	TSDB        TSDBConfig        `yaml:"tsdb"`
}

// DurationList is the block ranges for a tsdb
type DurationList []time.Duration

// String implements the flag.Value interface
func (d *DurationList) String() string {
	values := make([]string, 0, len(*d))
	for _, v := range *d {
		values = append(values, v.String())
	}

	return strings.Join(values, ",")
}

// Set implements the flag.Value interface
func (d *DurationList) Set(s string) error {
	values := strings.Split(s, ",")
	*d = make([]time.Duration, 0, len(values)) // flag.Parse may be called twice, so overwrite instead of append
	for _, v := range values {
		t, err := time.ParseDuration(v)
		if err != nil {
			return err
		}
		*d = append(*d, t)
	}
	return nil
}

// ToMilliseconds returns the duration list in milliseconds
func (d *DurationList) ToMilliseconds() []int64 {
	values := make([]int64, 0, len(*d))
	for _, t := range *d {
		values = append(values, t.Milliseconds())
	}

	return values
}

// RegisterFlags registers the TSDB flags
func (cfg *BlocksStorageConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.Bucket.RegisterFlagsWithPrefixAndDefaultDirectory("blocks-storage.", "blocks", f)
	cfg.BucketStore.RegisterFlags(f)
	cfg.TSDB.RegisterFlags(f)
}

// Validate the config.
func (cfg *BlocksStorageConfig) Validate(activeSeriesCfg activeseries.Config, logger log.Logger) error {
	if err := cfg.Bucket.Validate(); err != nil {
		return err
	}

	if err := cfg.TSDB.Validate(activeSeriesCfg, logger); err != nil {
		return err
	}

	return cfg.BucketStore.Validate(logger)
}

// TSDBConfig holds the config for TSDB opened in the ingesters.
//
//nolint:revive
type TSDBConfig struct {
	Dir                       string        `yaml:"dir"`
	BlockRanges               DurationList  `yaml:"block_ranges_period" category:"experimental" doc:"hidden"`
	Retention                 time.Duration `yaml:"retention_period"`
	ShipInterval              time.Duration `yaml:"ship_interval" category:"advanced"`
	ShipConcurrency           int           `yaml:"ship_concurrency" category:"advanced"`
	HeadCompactionInterval    time.Duration `yaml:"head_compaction_interval" category:"advanced"`
	HeadCompactionConcurrency int           `yaml:"head_compaction_concurrency" category:"advanced"`
	HeadCompactionIdleTimeout time.Duration `yaml:"head_compaction_idle_timeout" category:"advanced"`
	HeadChunksWriteBufferSize int           `yaml:"head_chunks_write_buffer_size_bytes" category:"advanced"`
	HeadChunksEndTimeVariance float64       `yaml:"head_chunks_end_time_variance" category:"experimental"`
	StripeSize                int           `yaml:"stripe_size" category:"advanced"`
	WALCompressionEnabled     bool          `yaml:"wal_compression_enabled" category:"advanced"`
	WALSegmentSizeBytes       int           `yaml:"wal_segment_size_bytes" category:"advanced"`
	WALReplayConcurrency      int           `yaml:"wal_replay_concurrency" category:"advanced"`
	FlushBlocksOnShutdown     bool          `yaml:"flush_blocks_on_shutdown" category:"advanced"`
	CloseIdleTSDBTimeout      time.Duration `yaml:"close_idle_tsdb_timeout" category:"advanced"`
	MemorySnapshotOnShutdown  bool          `yaml:"memory_snapshot_on_shutdown" category:"experimental"`
	HeadChunksWriteQueueSize  int           `yaml:"head_chunks_write_queue_size" category:"advanced"`

	// Series hash cache.
	SeriesHashCacheMaxBytes uint64 `yaml:"series_hash_cache_max_size_bytes" category:"advanced"`

	// DeprecatedMaxTSDBOpeningConcurrencyOnStartup limits the number of concurrently opening TSDB's during startup.
	DeprecatedMaxTSDBOpeningConcurrencyOnStartup int `yaml:"max_tsdb_opening_concurrency_on_startup" category:"deprecated"` // Deprecated. Remove in Mimir 2.10.

	// If true, user TSDBs are not closed on shutdown. Only for testing.
	// If false (default), user TSDBs are closed to make sure all resources are released and closed properly.
	KeepUserTSDBOpenOnShutdown bool `yaml:"-"`

	// How often to check for idle TSDBs for closing. DefaultCloseIdleTSDBInterval is not suitable for testing, so tests can override.
	CloseIdleTSDBInterval time.Duration `yaml:"-"`

	// For experimental out of order metrics support.
	OutOfOrderCapacityMax int `yaml:"out_of_order_capacity_max" category:"experimental"`

	// HeadPostingsForMatchersCacheTTL is the TTL of the postings for matchers cache in the Head.
	// If it's 0, the cache will only deduplicate in-flight requests, deleting the results once the first request has finished.
	HeadPostingsForMatchersCacheTTL time.Duration `yaml:"head_postings_for_matchers_cache_ttl" category:"experimental"`

	// HeadPostingsForMatchersCacheSize is the maximum size of cached postings for matchers elements in the Head.
	// It's ignored used when HeadPostingsForMatchersCacheTTL is 0.
	HeadPostingsForMatchersCacheSize int `yaml:"head_postings_for_matchers_cache_size" category:"experimental"`

	// HeadPostingsForMatchersCacheForce forces the usage of postings for matchers cache for all calls on Head and OOOHead regardless of the `concurrent` param.
	HeadPostingsForMatchersCacheForce bool `yaml:"head_postings_for_matchers_cache_force" category:"experimental"`

	// BlockPostingsForMatchersCacheTTL is the TTL of the postings for matchers cache in each compacted block.
	// If it's 0, the cache will only deduplicate in-flight requests, deleting the results once the first request has finished.
	BlockPostingsForMatchersCacheTTL time.Duration `yaml:"block_postings_for_matchers_cache_ttl" category:"experimental"`

	// BlockPostingsForMatchersCacheSize is the maximum size of cached postings for matchers elements in each compcated block.
	// It's ignored used when BlockPostingsForMatchersCacheTTL is 0.
	BlockPostingsForMatchersCacheSize int `yaml:"block_postings_for_matchers_cache_size" category:"experimental"`

	// BlockPostingsForMatchersCacheForce forces the usage of postings for matchers cache for all calls compacted blocks
	// regardless of the `concurrent` param.
	BlockPostingsForMatchersCacheForce bool `yaml:"block_postings_for_matchers_cache_force" category:"experimental"`

	EarlyHeadCompactionMinInMemorySeries                     int64 `yaml:"early_head_compaction_min_in_memory_series" category:"experimental"`
	EarlyHeadCompactionMinEstimatedSeriesReductionPercentage int   `yaml:"early_head_compaction_min_estimated_series_reduction_percentage" category:"experimental"`

	// HeadCompactionIntervalJitterEnabled is enabled by default, but allows to disable it in tests.
	HeadCompactionIntervalJitterEnabled bool `yaml:"-"`
}

// RegisterFlags registers the TSDBConfig flags.
func (cfg *TSDBConfig) RegisterFlags(f *flag.FlagSet) {
	if len(cfg.BlockRanges) == 0 {
		cfg.BlockRanges = []time.Duration{2 * time.Hour} // Default 2h block
	}

	f.StringVar(&cfg.Dir, "blocks-storage.tsdb.dir", "./tsdb/", "Directory to store TSDBs (including WAL) in the ingesters. This directory is required to be persisted between restarts.")
	f.Var(&cfg.BlockRanges, "blocks-storage.tsdb.block-ranges-period", "TSDB blocks range period.")
	f.DurationVar(&cfg.Retention, "blocks-storage.tsdb.retention-period", 13*time.Hour, "TSDB blocks retention in the ingester before a block is removed. If shipping is enabled, the retention will be relative to the time when the block was uploaded to storage. If shipping is disabled then its relative to the creation time of the block. This should be larger than the -blocks-storage.tsdb.block-ranges-period, -querier.query-store-after and large enough to give store-gateways and queriers enough time to discover newly uploaded blocks.")
	f.DurationVar(&cfg.ShipInterval, "blocks-storage.tsdb.ship-interval", 1*time.Minute, "How frequently the TSDB blocks are scanned and new ones are shipped to the storage. 0 means shipping is disabled.")
	f.IntVar(&cfg.ShipConcurrency, "blocks-storage.tsdb.ship-concurrency", 10, "Maximum number of tenants concurrently shipping blocks to the storage.")
	f.Uint64Var(&cfg.SeriesHashCacheMaxBytes, "blocks-storage.tsdb.series-hash-cache-max-size-bytes", uint64(1*units.Gibibyte), "Max size - in bytes - of the in-memory series hash cache. The cache is shared across all tenants and it's used only when query sharding is enabled.")
	f.IntVar(&cfg.DeprecatedMaxTSDBOpeningConcurrencyOnStartup, maxTSDBOpeningConcurrencyOnStartupFlag, defaultMaxTSDBOpeningConcurrencyOnStartup, "limit the number of concurrently opening TSDB's on startup")
	f.DurationVar(&cfg.HeadCompactionInterval, headCompactionIntervalFlag, 1*time.Minute, "How frequently the ingester checks whether the TSDB head should be compacted and, if so, triggers the compaction. Mimir applies a jitter to the first check, and subsequent checks will happen at the configured interval. A block is only created if data covers the smallest block range. The configured interval must be between 0 and 15 minutes.")
	f.IntVar(&cfg.HeadCompactionConcurrency, "blocks-storage.tsdb.head-compaction-concurrency", 1, "Maximum number of tenants concurrently compacting TSDB head into a new block")
	f.DurationVar(&cfg.HeadCompactionIdleTimeout, "blocks-storage.tsdb.head-compaction-idle-timeout", 1*time.Hour, "If TSDB head is idle for this duration, it is compacted. Note that up to 25% jitter is added to the value to avoid ingesters compacting concurrently. 0 means disabled.")
	f.IntVar(&cfg.HeadChunksWriteBufferSize, "blocks-storage.tsdb.head-chunks-write-buffer-size-bytes", chunks.DefaultWriteBufferSize, headChunkWriterBufferSizeHelp)
	f.Float64Var(&cfg.HeadChunksEndTimeVariance, "blocks-storage.tsdb.head-chunks-end-time-variance", 0, headChunksEndTimeVarianceHelp)
	f.IntVar(&cfg.StripeSize, "blocks-storage.tsdb.stripe-size", 16384, headStripeSizeHelp)
	f.BoolVar(&cfg.WALCompressionEnabled, "blocks-storage.tsdb.wal-compression-enabled", false, "True to enable TSDB WAL compression.")
	f.IntVar(&cfg.WALSegmentSizeBytes, "blocks-storage.tsdb.wal-segment-size-bytes", wlog.DefaultSegmentSize, "TSDB WAL segments files max size (bytes).")
	f.IntVar(&cfg.WALReplayConcurrency, "blocks-storage.tsdb.wal-replay-concurrency", 0, "Maximum number of CPUs that can simultaneously processes WAL replay. If it is set to 0, then each TSDB is replayed with a concurrency equal to the number of CPU cores available on the machine. If set to a positive value it overrides the deprecated -"+maxTSDBOpeningConcurrencyOnStartupFlag+" option.")
	f.BoolVar(&cfg.FlushBlocksOnShutdown, "blocks-storage.tsdb.flush-blocks-on-shutdown", false, "True to flush blocks to storage on shutdown. If false, incomplete blocks will be reused after restart.")
	f.DurationVar(&cfg.CloseIdleTSDBTimeout, "blocks-storage.tsdb.close-idle-tsdb-timeout", 13*time.Hour, "If TSDB has not received any data for this duration, and all blocks from TSDB have been shipped, TSDB is closed and deleted from local disk. If set to positive value, this value should be equal or higher than -querier.query-ingesters-within flag to make sure that TSDB is not closed prematurely, which could cause partial query results. 0 or negative value disables closing of idle TSDB.")
	f.BoolVar(&cfg.MemorySnapshotOnShutdown, "blocks-storage.tsdb.memory-snapshot-on-shutdown", false, "True to enable snapshotting of in-memory TSDB data on disk when shutting down.")
	f.IntVar(&cfg.HeadChunksWriteQueueSize, "blocks-storage.tsdb.head-chunks-write-queue-size", 1000000, headChunksWriteQueueSizeHelp)
	f.IntVar(&cfg.OutOfOrderCapacityMax, "blocks-storage.tsdb.out-of-order-capacity-max", 32, "Maximum capacity for out of order chunks, in samples between 1 and 255.")
	f.DurationVar(&cfg.HeadPostingsForMatchersCacheTTL, "blocks-storage.tsdb.head-postings-for-matchers-cache-ttl", 10*time.Second, "How long to cache postings for matchers in the Head and OOOHead. 0 disables the cache and just deduplicates the in-flight calls.")
	f.IntVar(&cfg.HeadPostingsForMatchersCacheSize, "blocks-storage.tsdb.head-postings-for-matchers-cache-size", 100, "Maximum number of entries in the cache for postings for matchers in the Head and OOOHead when TTL is greater than 0.")
	f.BoolVar(&cfg.HeadPostingsForMatchersCacheForce, "blocks-storage.tsdb.head-postings-for-matchers-cache-force", false, "Force the cache to be used for postings for matchers in the Head and OOOHead, even if it's not a concurrent (query-sharding) call.")
	f.DurationVar(&cfg.BlockPostingsForMatchersCacheTTL, "blocks-storage.tsdb.block-postings-for-matchers-cache-ttl", 10*time.Second, "How long to cache postings for matchers in each compacted block queried from the ingester. 0 disables the cache and just deduplicates the in-flight calls.")
	f.IntVar(&cfg.BlockPostingsForMatchersCacheSize, "blocks-storage.tsdb.block-postings-for-matchers-cache-size", 100, "Maximum number of entries in the cache for postings for matchers in each compacted block when TTL is greater than 0.")
	f.BoolVar(&cfg.BlockPostingsForMatchersCacheForce, "blocks-storage.tsdb.block-postings-for-matchers-cache-force", false, "Force the cache to be used for postings for matchers in compacted blocks, even if it's not a concurrent (query-sharding) call.")
	f.Int64Var(&cfg.EarlyHeadCompactionMinInMemorySeries, "blocks-storage.tsdb.early-head-compaction-min-in-memory-series", 0, fmt.Sprintf("When the number of in-memory series in the ingester is equal to or greater than this setting, the ingester tries to compact the TSDB Head. The early compaction removes from the memory all samples and inactive series up until -%s time ago. After an early compaction, the ingester will not accept any sample with a timestamp older than -%s time ago (unless out of order ingestion is enabled). The ingester checks every -%s whether an early compaction is required. Use 0 to disable it.", activeseries.IdleTimeoutFlag, activeseries.IdleTimeoutFlag, headCompactionIntervalFlag))
	f.IntVar(&cfg.EarlyHeadCompactionMinEstimatedSeriesReductionPercentage, "blocks-storage.tsdb.early-head-compaction-min-estimated-series-reduction-percentage", 10, "When the early compaction is enabled, the early compaction is triggered only if the estimated series reduction is at least the configured percentage (0-100).")

	cfg.HeadCompactionIntervalJitterEnabled = true
}

// Validate the config.
func (cfg *TSDBConfig) Validate(activeSeriesCfg activeseries.Config, logger log.Logger) error {
	if cfg.ShipInterval > 0 && cfg.ShipConcurrency <= 0 {
		return errInvalidShipConcurrency
	}

	if cfg.DeprecatedMaxTSDBOpeningConcurrencyOnStartup <= 0 {
		return errInvalidOpeningConcurrency
	}
	if cfg.DeprecatedMaxTSDBOpeningConcurrencyOnStartup != defaultMaxTSDBOpeningConcurrencyOnStartup {
		util.WarnDeprecatedConfig(maxTSDBOpeningConcurrencyOnStartupFlag, logger)
	}

	if cfg.HeadCompactionInterval <= 0 || cfg.HeadCompactionInterval > 15*time.Minute {
		return errInvalidCompactionInterval
	}

	if cfg.HeadCompactionConcurrency <= 0 {
		return errInvalidCompactionConcurrency
	}

	if cfg.HeadChunksWriteBufferSize < chunks.MinWriteBufferSize || cfg.HeadChunksWriteBufferSize > chunks.MaxWriteBufferSize || cfg.HeadChunksWriteBufferSize%1024 != 0 {
		return errors.Errorf("head chunks write buffer size must be a multiple of 1024 between %d and %d", chunks.MinWriteBufferSize, chunks.MaxWriteBufferSize)
	}

	if cfg.StripeSize <= 1 || (cfg.StripeSize&(cfg.StripeSize-1)) != 0 { // ensure stripe size is a positive power of 2
		return errInvalidStripeSize
	}

	if len(cfg.BlockRanges) == 0 {
		return errEmptyBlockranges
	}

	if cfg.WALSegmentSizeBytes <= 0 {
		return errInvalidWALSegmentSizeBytes
	}

	if cfg.WALReplayConcurrency < 0 {
		return errInvalidWALReplayConcurrency
	}

	if cfg.EarlyHeadCompactionMinInMemorySeries > 0 && !activeSeriesCfg.Enabled {
		return errEarlyCompactionRequiresActiveSeries
	}

	if cfg.EarlyHeadCompactionMinEstimatedSeriesReductionPercentage < 0 || cfg.EarlyHeadCompactionMinEstimatedSeriesReductionPercentage > 100 {
		return errInvalidEarlyHeadCompactionMinSeriesReduction
	}

	return nil
}

func (cfg *TSDBConfig) WALCompressionType() wlog.CompressionType {
	if cfg.WALCompressionEnabled {
		return wlog.CompressionSnappy
	}

	return wlog.CompressionNone
}

// BlocksDir returns the directory path where TSDB blocks and wal should be
// stored by the ingester
func (cfg *TSDBConfig) BlocksDir(userID string) string {
	return filepath.Join(cfg.Dir, userID)
}

// IsShippingEnabled returns whether blocks shipping is enabled.
func (cfg *TSDBConfig) IsBlocksShippingEnabled() bool {
	return cfg.ShipInterval > 0
}

// BucketStoreConfig holds the config information for Bucket Stores used by the querier and store-gateway.
type BucketStoreConfig struct {
	SyncDir                  string              `yaml:"sync_dir"`
	SyncInterval             time.Duration       `yaml:"sync_interval" category:"advanced"`
	MaxConcurrent            int                 `yaml:"max_concurrent" category:"advanced"`
	TenantSyncConcurrency    int                 `yaml:"tenant_sync_concurrency" category:"advanced"`
	BlockSyncConcurrency     int                 `yaml:"block_sync_concurrency" category:"advanced"`
	MetaSyncConcurrency      int                 `yaml:"meta_sync_concurrency" category:"advanced"`
	IndexCache               IndexCacheConfig    `yaml:"index_cache"`
	ChunksCache              ChunksCacheConfig   `yaml:"chunks_cache"`
	MetadataCache            MetadataCacheConfig `yaml:"metadata_cache"`
	IgnoreDeletionMarksDelay time.Duration       `yaml:"ignore_deletion_mark_delay" category:"advanced"`
	BucketIndex              BucketIndexConfig   `yaml:"bucket_index"`
	IgnoreBlocksWithin       time.Duration       `yaml:"ignore_blocks_within" category:"advanced"`

	// Chunk pool.
	DeprecatedMaxChunkPoolBytes           uint64 `yaml:"max_chunk_pool_bytes" category:"deprecated"`             // Deprecated. TODO: Remove in Mimir 2.11.
	DeprecatedChunkPoolMinBucketSizeBytes int    `yaml:"chunk_pool_min_bucket_size_bytes" category:"deprecated"` // Deprecated. TODO: Remove in Mimir 2.11.
	DeprecatedChunkPoolMaxBucketSizeBytes int    `yaml:"chunk_pool_max_bucket_size_bytes" category:"deprecated"` // Deprecated. TODO: Remove in Mimir 2.11.

	// Series hash cache.
	SeriesHashCacheMaxBytes uint64 `yaml:"series_hash_cache_max_size_bytes" category:"advanced"`

	// Controls whether index-header lazy loading is enabled.
	IndexHeaderLazyLoadingEnabled     bool          `yaml:"index_header_lazy_loading_enabled" category:"advanced"`
	IndexHeaderLazyLoadingIdleTimeout time.Duration `yaml:"index_header_lazy_loading_idle_timeout" category:"advanced"`

	// Maximum index-headers loaded into store-gateway concurrently
	IndexHeaderLazyLoadingConcurrency int `yaml:"index_header_lazy_loading_concurrency" category:"experimental"`

	// Controls whether persisting a sparse version of the index-header to disk is enabled.
	IndexHeaderSparsePersistenceEnabled bool `yaml:"index_header_sparse_persistence_enabled" category:"experimental"`

	// Controls the partitioner, used to aggregate multiple GET object API requests.
	PartitionerMaxGapBytes uint64 `yaml:"partitioner_max_gap_bytes" category:"advanced"`

	// Controls what is the ratio of postings offsets store will hold in memory.
	// Larger value will keep less offsets, which will increase CPU cycles needed for query touching those postings.
	// It's meant for setups that want low baseline memory pressure and where less traffic is expected.
	// On the contrary, smaller value will increase baseline memory usage, but improve latency slightly.
	// 1 will keep all in memory. Default value is the same as in Prometheus which gives a good balance.
	PostingOffsetsInMemSampling int `yaml:"postings_offsets_in_mem_sampling" category:"advanced"`

	// Controls experimental options for index-header file reading.
	IndexHeader indexheader.Config `yaml:"index_header" category:"experimental"`

	StreamingBatchSize          int    `yaml:"streaming_series_batch_size" category:"advanced"`
	ChunkRangesPerSeries        int    `yaml:"fine_grained_chunks_caching_ranges_per_series" category:"experimental"`
	SeriesSelectionStrategyName string `yaml:"series_selection_strategy" category:"experimental"`
	SelectionStrategies         struct {
		WorstCaseSeriesPreference float64 `yaml:"worst_case_series_preference" category:"experimental"`
	} `yaml:"series_selection_strategies"`
}

const (
	SpeculativePostingsStrategy                = "speculative"
	WorstCasePostingsStrategy                  = "worst-case"
	WorstCaseSmallPostingListsPostingsStrategy = "worst-case-small-posting-lists"
	AllPostingsStrategy                        = "all"
)

var validSeriesSelectionStrategies = []string{
	SpeculativePostingsStrategy,
	WorstCasePostingsStrategy,
	WorstCaseSmallPostingListsPostingsStrategy,
	AllPostingsStrategy,
}

// RegisterFlags registers the BucketStore flags
func (cfg *BucketStoreConfig) RegisterFlags(f *flag.FlagSet) {
	cfg.IndexCache.RegisterFlagsWithPrefix(f, "blocks-storage.bucket-store.index-cache.")
	cfg.ChunksCache.RegisterFlagsWithPrefix(f, "blocks-storage.bucket-store.chunks-cache.")
	cfg.MetadataCache.RegisterFlagsWithPrefix(f, "blocks-storage.bucket-store.metadata-cache.")
	cfg.BucketIndex.RegisterFlagsWithPrefix(f, bucketIndexFlagPrefix)
	cfg.IndexHeader.RegisterFlagsWithPrefix(f, "blocks-storage.bucket-store.index-header.")

	f.StringVar(&cfg.SyncDir, "blocks-storage.bucket-store.sync-dir", "./tsdb-sync/", "Directory to store synchronized TSDB index headers. This directory is not required to be persisted between restarts, but it's highly recommended in order to improve the store-gateway startup time.")
	f.DurationVar(&cfg.SyncInterval, "blocks-storage.bucket-store.sync-interval", 15*time.Minute, "How frequently to scan the bucket, or to refresh the bucket index (if enabled), in order to look for changes (new blocks shipped by ingesters and blocks deleted by retention or compaction).")
	f.Uint64Var(&cfg.DeprecatedMaxChunkPoolBytes, maxChunksBytesPoolFlag, uint64(2*units.Gibibyte), "Max size - in bytes - of a chunks pool, used to reduce memory allocations. The pool is shared across all tenants. 0 to disable the limit.")
	f.IntVar(&cfg.DeprecatedChunkPoolMinBucketSizeBytes, minBucketSizeBytesFlag, ChunkPoolDefaultMinBucketSize, "Size - in bytes - of the smallest chunks pool bucket.")
	f.IntVar(&cfg.DeprecatedChunkPoolMaxBucketSizeBytes, maxBucketSizeBytesFlag, ChunkPoolDefaultMaxBucketSize, "Size - in bytes - of the largest chunks pool bucket.")
	f.Uint64Var(&cfg.SeriesHashCacheMaxBytes, "blocks-storage.bucket-store.series-hash-cache-max-size-bytes", uint64(1*units.Gibibyte), "Max size - in bytes - of the in-memory series hash cache. The cache is shared across all tenants and it's used only when query sharding is enabled.")
	f.IntVar(&cfg.MaxConcurrent, "blocks-storage.bucket-store.max-concurrent", 100, "Max number of concurrent queries to execute against the long-term storage. The limit is shared across all tenants.")
	f.IntVar(&cfg.TenantSyncConcurrency, "blocks-storage.bucket-store.tenant-sync-concurrency", 10, "Maximum number of concurrent tenants synching blocks.")
	f.IntVar(&cfg.BlockSyncConcurrency, "blocks-storage.bucket-store.block-sync-concurrency", 20, "Maximum number of concurrent blocks synching per tenant.")
	f.IntVar(&cfg.MetaSyncConcurrency, "blocks-storage.bucket-store.meta-sync-concurrency", 20, "Number of Go routines to use when syncing block meta files from object storage per tenant.")
	f.DurationVar(&cfg.IgnoreDeletionMarksDelay, "blocks-storage.bucket-store.ignore-deletion-marks-delay", time.Hour*1, "Duration after which the blocks marked for deletion will be filtered out while fetching blocks. "+
		"The idea of ignore-deletion-marks-delay is to ignore blocks that are marked for deletion with some delay. This ensures store can still serve blocks that are meant to be deleted but do not have a replacement yet.")
	f.DurationVar(&cfg.IgnoreBlocksWithin, "blocks-storage.bucket-store.ignore-blocks-within", 10*time.Hour, "Blocks with minimum time within this duration are ignored, and not loaded by store-gateway. Useful when used together with -querier.query-store-after to prevent loading young blocks, because there are usually many of them (depending on number of ingesters) and they are not yet compacted. Negative values or 0 disable the filter.")
	f.IntVar(&cfg.PostingOffsetsInMemSampling, "blocks-storage.bucket-store.posting-offsets-in-mem-sampling", DefaultPostingOffsetInMemorySampling, "Controls what is the ratio of postings offsets that the store will hold in memory.")
	f.BoolVar(&cfg.IndexHeaderLazyLoadingEnabled, "blocks-storage.bucket-store.index-header-lazy-loading-enabled", true, "If enabled, store-gateway will lazy load an index-header only once required by a query.")
	f.DurationVar(&cfg.IndexHeaderLazyLoadingIdleTimeout, "blocks-storage.bucket-store.index-header-lazy-loading-idle-timeout", 60*time.Minute, "If index-header lazy loading is enabled and this setting is > 0, the store-gateway will offload unused index-headers after 'idle timeout' inactivity.")
	f.IntVar(&cfg.IndexHeaderLazyLoadingConcurrency, "blocks-storage.bucket-store.index-header-lazy-loading-concurrency", 0, "Maximum number of concurrent index header loads across all tenants. If set to 0, concurrency is unlimited.")
	f.BoolVar(&cfg.IndexHeaderSparsePersistenceEnabled, "blocks-storage.bucket-store.index-header-sparse-persistence-enabled", false, "If enabled, store-gateway will persist a sparse version of the index-header to disk on construction and load sparse index-headers from disk instead of the whole index-header.")
	f.Uint64Var(&cfg.PartitionerMaxGapBytes, "blocks-storage.bucket-store.partitioner-max-gap-bytes", DefaultPartitionerMaxGapSize, "Max size - in bytes - of a gap for which the partitioner aggregates together two bucket GET object requests.")
	f.IntVar(&cfg.StreamingBatchSize, "blocks-storage.bucket-store.batch-series-size", 5000, "This option controls how many series to fetch per batch. The batch size must be greater than 0.")
	f.IntVar(&cfg.ChunkRangesPerSeries, "blocks-storage.bucket-store.fine-grained-chunks-caching-ranges-per-series", 1, "This option controls into how many ranges the chunks of each series from each block are split. This value is effectively the number of chunks cache items per series per block when -blocks-storage.bucket-store.chunks-cache.fine-grained-chunks-caching-enabled is enabled.")
	f.StringVar(&cfg.SeriesSelectionStrategyName, seriesSelectionStrategyFlag, WorstCasePostingsStrategy, "This option controls the strategy to selection of series and deferring application of matchers. A more aggressive strategy will fetch less posting lists at the cost of more series. This is useful when querying large blocks in which many series share the same label name and value. Supported values (most aggressive to least aggressive): "+strings.Join(validSeriesSelectionStrategies, ", ")+".")
	f.Float64Var(&cfg.SelectionStrategies.WorstCaseSeriesPreference, "blocks-storage.bucket-store.series-selection-strategies.worst-case-series-preference", 0.75, "This option is only used when "+seriesSelectionStrategyFlag+"="+WorstCasePostingsStrategy+". Increasing the series preference results in fetching more series than postings. Must be a positive floating point number.")
}

// Validate the config.
func (cfg *BucketStoreConfig) Validate(logger log.Logger) error {
	if cfg.StreamingBatchSize <= 0 {
		return errInvalidStreamingBatchSize
	}
	if err := cfg.IndexCache.Validate(); err != nil {
		return errors.Wrap(err, "index-cache configuration")
	}
	if err := cfg.ChunksCache.Validate(); err != nil {
		return errors.Wrap(err, "chunks-cache configuration")
	}
	if err := cfg.MetadataCache.Validate(); err != nil {
		return errors.Wrap(err, "metadata-cache configuration")
	}
	if err := cfg.BucketIndex.Validate(logger); err != nil {
		return errors.Wrap(err, "bucket-index configuration")
	}
	if cfg.DeprecatedMaxChunkPoolBytes != uint64(2*units.Gibibyte) {
		util.WarnDeprecatedConfig(maxChunksBytesPoolFlag, logger)
	}
	if cfg.DeprecatedChunkPoolMinBucketSizeBytes != ChunkPoolDefaultMinBucketSize {
		util.WarnDeprecatedConfig(minBucketSizeBytesFlag, logger)
	}
	if cfg.DeprecatedChunkPoolMaxBucketSizeBytes != ChunkPoolDefaultMaxBucketSize {
		util.WarnDeprecatedConfig(maxBucketSizeBytesFlag, logger)
	}
	if !util.StringsContain(validSeriesSelectionStrategies, cfg.SeriesSelectionStrategyName) {
		return errors.New("invalid series-selection-strategy, set one of " + strings.Join(validSeriesSelectionStrategies, ", "))
	}
	if cfg.SeriesSelectionStrategyName == WorstCasePostingsStrategy && cfg.SelectionStrategies.WorstCaseSeriesPreference <= 0 {
		return errors.New("invalid worst-case series preference; must be positive")
	}
	if err := cfg.IndexHeader.Validate(cfg.IndexHeaderLazyLoadingEnabled); err != nil {
		return errors.Wrap(err, "index-header configuration")
	}
	if cfg.IndexHeaderLazyLoadingConcurrency < 0 {
		return errInvalidIndexHeaderLazyLoadingConcurrency
	}
	return nil
}

type BucketIndexConfig struct {
	DeprecatedEnabled     bool          `yaml:"enabled" category:"deprecated"` // Deprecated. TODO: Remove in Mimir 2.11.
	UpdateOnErrorInterval time.Duration `yaml:"update_on_error_interval" category:"advanced"`
	IdleTimeout           time.Duration `yaml:"idle_timeout" category:"advanced"`
	MaxStalePeriod        time.Duration `yaml:"max_stale_period" category:"advanced"`
}

func (cfg *BucketIndexConfig) RegisterFlagsWithPrefix(f *flag.FlagSet, prefix string) {
	f.BoolVar(&cfg.DeprecatedEnabled, prefix+"enabled", true, "If enabled, queriers and store-gateways discover blocks by reading a bucket index (created and updated by the compactor) instead of periodically scanning the bucket.")
	f.DurationVar(&cfg.UpdateOnErrorInterval, prefix+"update-on-error-interval", time.Minute, "How frequently a bucket index, which previously failed to load, should be tried to load again. This option is used only by querier.")
	f.DurationVar(&cfg.IdleTimeout, prefix+"idle-timeout", time.Hour, "How long a unused bucket index should be cached. Once this timeout expires, the unused bucket index is removed from the in-memory cache. This option is used only by querier.")
	f.DurationVar(&cfg.MaxStalePeriod, prefix+"max-stale-period", time.Hour, "The maximum allowed age of a bucket index (last updated) before queries start failing because the bucket index is too old. The bucket index is periodically updated by the compactor, and this check is enforced in the querier (at query time).")
}

// Validate the config.
func (cfg *BucketIndexConfig) Validate(logger log.Logger) error {
	if !cfg.DeprecatedEnabled {
		util.WarnDeprecatedConfig(bucketIndexFlagPrefix+"enabled", logger)
	}
	return nil
}
