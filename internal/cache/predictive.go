package cache

import (
	"context"
	"math"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scttfrdmn/objectfs/pkg/types"
)

// PredictiveCache implements ML-based predictive caching with intelligent prefetching
type PredictiveCache struct {
	baseCache   types.Cache
	predictor   *AccessPredictor
	prefetcher  *IntelligentPrefetcher
	evictionMgr *IntelligentEvictionManager
	config      *PredictiveCacheConfig
	stats       *PredictiveStats

	// ledger remembers what a prefetch stored, so a later read can be attributed to it, and predictions
	// remembers what the predictor said would be read next. Two ledgers and not one: a prediction that
	// no worker acted on — because the rate limiter refused it, or the queue was full, or there is no
	// backend — is still a prediction whose accuracy is worth knowing, and conflating the two would score
	// the predictor on the prefetcher's throughput.
	ledger      *rangeLedger
	predictions *rangeLedger
}

// PredictiveCacheConfig configures predictive caching behavior
type PredictiveCacheConfig struct {
	// Base cache config
	BaseCache types.Cache
	Backend   types.Backend // Backend for prefetch operations

	// Prediction settings
	EnablePrediction    bool    `yaml:"enable_prediction"`
	PredictionWindow    int     `yaml:"prediction_window"`    // Number of accesses to consider
	ConfidenceThreshold float64 `yaml:"confidence_threshold"` // Min confidence to trigger prefetch
	LearningRate        float64 `yaml:"learning_rate"`        // ML model learning rate

	// Prefetch settings
	EnablePrefetch     bool  `yaml:"enable_prefetch"`
	MaxConcurrentFetch int   `yaml:"max_concurrent_fetch"`
	PrefetchAhead      int   `yaml:"prefetch_ahead"`     // Number of blocks to prefetch ahead
	PrefetchBandwidth  int64 `yaml:"prefetch_bandwidth"` // Max bandwidth for prefetching

	// Eviction settings
	EnableIntelligentEviction bool   `yaml:"enable_intelligent_eviction"`
	EvictionAlgorithm         string `yaml:"eviction_algorithm"` // "lru", "lfu", "arc", "ml"
	MLModelPath               string `yaml:"ml_model_path"`      // Path to trained model

	// Performance settings
	StatisticsInterval   time.Duration `yaml:"statistics_interval"`
	ModelUpdateInterval  time.Duration `yaml:"model_update_interval"`
	PatternAnalysisDepth int           `yaml:"pattern_analysis_depth"`
}

// PredictiveStats tracks predictive cache performance.
//
// Every field here is assigned by the cache on a path a mount takes. That is worth stating because it
// was not true: this struct declared seventeen fields, of which fourteen — PredictionAccuracy,
// PrefetchEfficiency, CacheHitImprovement, LatencyReduction, BandwidthSavings, ModelAccuracy and the
// rest — were written by nothing anywhere in the repository, and the three that were written included
// PrefetchHits, which required an AccessEvent with Prefetch set true, which nothing constructed. So a
// caller reaching these numbers would have read zeros and had no way to tell them from a cache that had
// prefetched nothing. #222's percentiles were the same defect in a different package, and the answer is
// the same: a statistic that cannot be computed should not be exported. The deleted fields described a
// trained model, an eviction-accuracy follow-up and a counterfactual hit-rate comparison, none of which
// this cache does; adding them means building those mechanisms, not adding a field back.
//
// The counters are the primary values and the ratios derive from them, so a reader that distrusts a
// ratio can recompute it. Access under mu, or through [PredictiveCache.GetPredictiveStats], which
// copies.
type PredictiveStats struct {
	mu sync.RWMutex

	// Prediction metrics. A prediction is one [types.PrefetchCandidate] the predictor emitted, and it is
	// counted correct when a later read hits the range it named — see PredictiveCache.Get.
	PredictionsTotal   uint64  `json:"predictions_total"`
	PredictionsCorrect uint64  `json:"predictions_correct"`
	PredictionAccuracy float64 `json:"prediction_accuracy"`
	AvgConfidence      float64 `json:"avg_confidence"`

	// Prefetch metrics. A request is a candidate a worker acted on; a hit is a subsequent read served
	// from bytes that worker stored. Waste is the remainder — fetched and evicted or never read — and is
	// the number that says whether prefetch is earning its bandwidth.
	PrefetchRequests   uint64  `json:"prefetch_requests"`
	PrefetchHits       uint64  `json:"prefetch_hits"`
	PrefetchBytes      int64   `json:"prefetch_bytes"`
	PrefetchWaste      uint64  `json:"prefetch_waste"`
	PrefetchEfficiency float64 `json:"prefetch_efficiency"`

	// Eviction metrics. Intelligent evictions are those the scoring path chose, as opposed to the base
	// cache's own LRU, which runs when the scorer produces no candidates.
	EvictionsTotal       uint64 `json:"evictions_total"`
	EvictionsIntelligent uint64 `json:"evictions_intelligent"`
}

// AccessPredictor implements machine learning-based access pattern prediction
type AccessPredictor struct {
	mu           sync.RWMutex
	patterns     map[string]*AccessPattern
	model        *PredictionModel
	config       *PredictiveCacheConfig
	recentAccess []AccessEvent
	windowSize   int
}

// AccessPattern represents learned access patterns for a file/key
type AccessPattern struct {
	Key             string                    `json:"key"`
	AccessHistory   []AccessEvent             `json:"access_history"`
	SequentialScore float64                   `json:"sequential_score"` // 0-1, how sequential accesses are
	FrequencyScore  float64                   `json:"frequency_score"`  // Access frequency
	RecencyScore    float64                   `json:"recency_score"`    // Recent access score
	SizePattern     []int64                   `json:"size_pattern"`     // Common access sizes
	TimePattern     []time.Duration           `json:"time_pattern"`     // Access intervals
	Confidence      float64                   `json:"confidence"`       // Model confidence
	LastAccess      time.Time                 `json:"last_access"`
	PredictedNext   []types.PrefetchCandidate `json:"predicted_next"`
	Features        map[string]float64        `json:"features"` // ML features
}

// AccessEvent represents a single access event
type AccessEvent struct {
	Key       string    `json:"key"`
	Offset    int64     `json:"offset"`
	Size      int64     `json:"size"`
	Timestamp time.Time `json:"timestamp"`
	Hit       bool      `json:"hit"`      // Was it a cache hit?
	Prefetch  bool      `json:"prefetch"` // Was it prefetched?
}

// PredictionModel implements the ML model for access prediction
type PredictionModel struct {
	mu           sync.RWMutex
	weights      map[string]float64 // Feature weights
	bias         float64
	learningRate float64
	trainingData []TrainingExample
}

// TrainingExample represents a training data point
type TrainingExample struct {
	Features []float64 `json:"features"`
	Target   float64   `json:"target"` // 1.0 if access occurred, 0.0 if not
	Weight   float64   `json:"weight"` // Importance weight
}

// IntelligentPrefetcher handles predictive prefetching
type IntelligentPrefetcher struct {
	backend       types.Backend
	prefetchQueue chan *PrefetchJob
	activeJobs    map[string]*PrefetchJob
	workerPool    chan struct{}
	stats         PrefetchStats
	rateLimiter   *RateLimiter
	config        *PredictiveCacheConfig

	// stopCh is closed once, by stopOnce, to retire the workers.
	//
	// prefetchQueue is deliberately never closed. Closing a channel is the sender's prerogative and
	// the senders here are cache reads: triggerPrefetch is reached from every L1 Get, from arbitrary
	// goroutines, with no way to know when the last one has returned. A send on a closed channel
	// panics, and it panics inside a select with a default arm too — the default covers a full
	// channel, not a closed one — so closing the queue turned a shutdown racing a read into a crash
	// of the whole process, which on a mount means the filesystem disappearing under every open
	// descriptor. The workers select on stopCh instead, and the queued jobs are simply abandoned;
	// they are speculative prefetches, so dropping them costs a future cache miss and nothing else.
	stopCh   chan struct{}
	stopOnce sync.Once
}

// PrefetchJob represents a prefetch operation
type PrefetchJob struct {
	Key          string
	Candidates   []types.PrefetchCandidate
	Priority     int
	Confidence   float64
	CreatedAt    time.Time
	StartedAt    time.Time
	CompletedAt  time.Time
	Error        error
	BytesFetched int64
}

// PrefetchStats tracks prefetch performance
type PrefetchStats struct {
	JobsQueued        uint64        `json:"jobs_queued"`
	JobsCompleted     uint64        `json:"jobs_completed"`
	JobsFailed        uint64        `json:"jobs_failed"`
	BytesPrefetched   int64         `json:"bytes_prefetched"`
	AverageLatency    time.Duration `json:"average_latency"`
	QueueDepth        int           `json:"queue_depth"`
	WorkerUtilization float64       `json:"worker_utilization"`
}

// IntelligentEvictionManager handles ML-driven cache eviction
type IntelligentEvictionManager struct {
	cache         types.Cache
	predictor     *AccessPredictor
	evictionModel *EvictionModel
	config        *PredictiveCacheConfig
}

// EvictionCandidate represents an item that could be evicted
type EvictionCandidate struct {
	Key            string    `json:"key"`
	Size           int64     `json:"size"`
	LastAccess     time.Time `json:"last_access"`
	AccessCount    int       `json:"access_count"`
	PredictedReuse float64   `json:"predicted_reuse"` // Probability of future access
	EvictionScore  float64   `json:"eviction_score"`  // Higher = more likely to evict
	CacheLevel     string    `json:"cache_level"`
}

// EvictionModel implements ML-based eviction decisions
type EvictionModel struct {
	weights   map[string]float64
	threshold float64
}

// RateLimiter controls prefetch bandwidth usage
type RateLimiter struct {
	mu         sync.Mutex
	capacity   int64 // bytes per second
	tokens     int64 // current tokens
	lastRefill time.Time
	refillRate int64 // tokens per second
}

// NewPredictiveCache creates a new predictive cache
func NewPredictiveCache(config *PredictiveCacheConfig) (*PredictiveCache, error) {
	if config == nil {
		config = &PredictiveCacheConfig{
			EnablePrediction:          true,
			PredictionWindow:          100,
			ConfidenceThreshold:       0.7,
			LearningRate:              0.01,
			EnablePrefetch:            true,
			MaxConcurrentFetch:        4,
			PrefetchAhead:             3,
			PrefetchBandwidth:         10 * 1024 * 1024, // 10 MB/s
			EnableIntelligentEviction: true,
			EvictionAlgorithm:         "ml",
			StatisticsInterval:        30 * time.Second,
			ModelUpdateInterval:       5 * time.Minute,
			PatternAnalysisDepth:      1000,
		}
	}

	predictor := &AccessPredictor{
		patterns:     make(map[string]*AccessPattern),
		windowSize:   config.PredictionWindow,
		config:       config,
		recentAccess: make([]AccessEvent, 0, config.PredictionWindow),
		model: &PredictionModel{
			weights:      make(map[string]float64),
			learningRate: config.LearningRate,
			trainingData: make([]TrainingExample, 0, 10000),
		},
	}

	prefetcher := &IntelligentPrefetcher{
		backend:       config.Backend,
		prefetchQueue: make(chan *PrefetchJob, 1000),
		activeJobs:    make(map[string]*PrefetchJob),
		workerPool:    make(chan struct{}, config.MaxConcurrentFetch),
		config:        config,
		stopCh:        make(chan struct{}),
		rateLimiter: &RateLimiter{
			capacity:   config.PrefetchBandwidth,
			refillRate: config.PrefetchBandwidth,

			// Start full, which is the usual token-bucket convention and not merely a convenience:
			// the zero value is an empty bucket, so the first prefetch of a mount's life was refused
			// and the budget had to be earned by a second of idleness that a busy filesystem never
			// provides.
			tokens:     config.PrefetchBandwidth,
			lastRefill: time.Now(),
		},
	}

	evictionMgr := &IntelligentEvictionManager{
		cache:     config.BaseCache,
		predictor: predictor,
		config:    config,
		evictionModel: &EvictionModel{
			weights:   make(map[string]float64),
			threshold: 0.5,
		},
	}

	pc := &PredictiveCache{
		baseCache:   config.BaseCache,
		predictor:   predictor,
		prefetcher:  prefetcher,
		evictionMgr: evictionMgr,
		config:      config,
		stats:       &PredictiveStats{},
		ledger:      newRangeLedger(),
		predictions: newRangeLedger(),
	}

	// Initialize feature weights with reasonable defaults
	pc.initializeModel()

	// Start background workers
	if config.EnablePrefetch {
		pc.startPrefetchWorkers()
	}

	return pc, nil
}

// Get retrieves data with predictive intelligence
func (pc *PredictiveCache) Get(key string, offset, size int64) []byte {
	start := time.Now()

	// Record access event
	event := AccessEvent{
		Key:       key,
		Offset:    offset,
		Size:      size,
		Timestamp: start,
		Hit:       false,
		Prefetch:  false,
	}

	// Try base cache first
	data := pc.baseCache.Get(key, offset, size)
	event.Hit = data != nil

	// Whether this read was served by something a prefetch worker stored. Consulted before the predictor
	// runs, so a prediction made *by* this read cannot be credited *to* it — which is how a prefetcher
	// scores 100% against itself.
	//
	// event.Prefetch was declared for this and set false at both of its two construction sites, so
	// PrefetchHits — guarded by `event.Hit && event.Prefetch` — could never be incremented. That was the
	// defect: the field existed, the guard read it, and nothing ever made it true.
	if event.Hit {
		event.Prefetch = pc.ledger.claim(key, offset, size)
	}
	pc.recordPrediction(key, offset, size)

	// Update predictor with access pattern
	if pc.config.EnablePrediction {
		pc.predictor.RecordAccess(event)

		// Trigger predictions and prefetching
		if predictions := pc.predictor.PredictNextAccess(key); len(predictions) > 0 {
			pc.recordPredictions(predictions)
			pc.triggerPrefetch(predictions)
		}
	}

	// Update statistics
	pc.updateStats(event, time.Since(start))

	return data
}

// Put stores data with intelligent cache management
func (pc *PredictiveCache) Put(key string, offset int64, data []byte) {
	// Check if we need to evict before putting
	if pc.config.EnableIntelligentEviction {
		pc.intelligentEvict(int64(len(data)))
	}

	// Store in base cache
	pc.baseCache.Put(key, offset, data)

	// Update access patterns
	if pc.config.EnablePrediction {
		event := AccessEvent{
			Key:       key,
			Offset:    offset,
			Size:      int64(len(data)),
			Timestamp: time.Now(),
			Hit:       false,
			Prefetch:  false,
		}
		pc.predictor.RecordAccess(event)
	}
}

// Delete removes data from cache
func (pc *PredictiveCache) Delete(key string) {
	pc.baseCache.Delete(key)

	// Clean up prediction data
	if pc.config.EnablePrediction {
		pc.predictor.mu.Lock()
		delete(pc.predictor.patterns, key)
		pc.predictor.mu.Unlock()
	}
}

// Evict performs intelligent eviction
func (pc *PredictiveCache) Evict(size int64) bool {
	if pc.config.EnableIntelligentEviction {
		return pc.intelligentEvict(size)
	}
	return pc.baseCache.Evict(size)
}

// Size returns cache size
func (pc *PredictiveCache) Size() int64 {
	return pc.baseCache.Size()
}

// Stats returns comprehensive statistics
func (pc *PredictiveCache) Stats() types.CacheStats {
	baseStats := pc.baseCache.Stats()

	pc.stats.mu.RLock()
	defer pc.stats.mu.RUnlock()

	// Enhance base stats with predictive metrics
	// Note: In a full implementation, you'd merge these properly
	return baseStats
}

// GetPredictiveStats returns a snapshot of this cache's predictive statistics.
//
// Field by field rather than by struct copy, because PredictiveStats holds the mutex guarding it and
// copying it would copy the lock — `go vet`'s copylocks catches that, and the caller would be locking a
// copy of a lock that protects nothing.
//
// Reachable on a mount through [MultiLevelCache.PredictiveStats]. It was not: the mount's instance is
// held as an opaque types.Cache inside a CacheLevel with no accessor reaching past it, so these numbers
// were computed and discarded at unmount (#223).
func (pc *PredictiveCache) GetPredictiveStats() PredictiveStats {
	pc.stats.mu.RLock()
	defer pc.stats.mu.RUnlock()

	return PredictiveStats{
		PredictionsTotal:     pc.stats.PredictionsTotal,
		PredictionsCorrect:   pc.stats.PredictionsCorrect,
		PredictionAccuracy:   pc.stats.PredictionAccuracy,
		AvgConfidence:        pc.stats.AvgConfidence,
		PrefetchRequests:     pc.stats.PrefetchRequests,
		PrefetchHits:         pc.stats.PrefetchHits,
		PrefetchBytes:        pc.stats.PrefetchBytes,
		PrefetchWaste:        pc.stats.PrefetchWaste,
		PrefetchEfficiency:   pc.stats.PrefetchEfficiency,
		EvictionsTotal:       pc.stats.EvictionsTotal,
		EvictionsIntelligent: pc.stats.EvictionsIntelligent,
	}
}

// Access Prediction Implementation

// RecordAccess updates the prediction model with new access data
func (ap *AccessPredictor) RecordAccess(event AccessEvent) {
	ap.mu.Lock()
	defer ap.mu.Unlock()

	// Add to recent access window
	ap.recentAccess = append(ap.recentAccess, event)
	if len(ap.recentAccess) > ap.windowSize {
		ap.recentAccess = ap.recentAccess[1:]
	}

	// Update or create pattern for this key
	pattern, exists := ap.patterns[event.Key]
	if !exists {
		pattern = &AccessPattern{
			Key:           event.Key,
			AccessHistory: make([]AccessEvent, 0, 100),
			Features:      make(map[string]float64),
		}
		ap.patterns[event.Key] = pattern
	}

	// Update pattern
	pattern.AccessHistory = append(pattern.AccessHistory, event)
	if len(pattern.AccessHistory) > 100 {
		pattern.AccessHistory = pattern.AccessHistory[1:]
	}
	pattern.LastAccess = event.Timestamp

	// Recalculate pattern features
	ap.calculatePatternFeatures(pattern)

	// Update ML model if we have enough data
	if len(ap.recentAccess) >= ap.windowSize/2 {
		ap.updateModel()
	}
}

// PredictNextAccess uses ML to predict future access patterns
func (ap *AccessPredictor) PredictNextAccess(key string) []types.PrefetchCandidate {
	ap.mu.RLock()
	defer ap.mu.RUnlock()

	pattern, exists := ap.patterns[key]
	if !exists || len(pattern.AccessHistory) < 3 {
		return nil
	}

	var candidates []types.PrefetchCandidate

	// Sequential prediction
	if pattern.SequentialScore > 0.7 {
		candidates = append(candidates, ap.predictSequential(pattern)...)
	}

	// Temporal prediction
	if pattern.FrequencyScore > 0.5 {
		candidates = append(candidates, ap.predictTemporal(pattern)...)
	}

	// ML-based prediction
	if ap.model != nil {
		candidates = append(candidates, ap.predictML(pattern)...)
	}

	// Sort by confidence and return top candidates
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].Priority > candidates[j].Priority
	})

	if len(candidates) > ap.config.PrefetchAhead {
		candidates = candidates[:ap.config.PrefetchAhead]
	}

	return candidates
}

// Helper methods for prediction algorithms

func (ap *AccessPredictor) calculatePatternFeatures(pattern *AccessPattern) {
	if len(pattern.AccessHistory) < 2 {
		return
	}

	// Calculate sequential score
	sequential := 0
	total := 0
	for i := 1; i < len(pattern.AccessHistory); i++ {
		prev := pattern.AccessHistory[i-1]
		curr := pattern.AccessHistory[i]

		if curr.Offset == prev.Offset+prev.Size {
			sequential++
		}
		total++
	}
	if total > 0 {
		pattern.SequentialScore = float64(sequential) / float64(total)
	}

	// Calculate frequency score
	now := time.Now()
	recent := 0
	for _, event := range pattern.AccessHistory {
		if now.Sub(event.Timestamp) < time.Hour {
			recent++
		}
	}
	pattern.FrequencyScore = float64(recent) / float64(len(pattern.AccessHistory))

	// Calculate recency score
	if !pattern.LastAccess.IsZero() {
		age := now.Sub(pattern.LastAccess)
		pattern.RecencyScore = math.Exp(-age.Hours() / 24) // Exponential decay over days
	}

	// Update ML features
	pattern.Features["sequential_score"] = pattern.SequentialScore
	pattern.Features["frequency_score"] = pattern.FrequencyScore
	pattern.Features["recency_score"] = pattern.RecencyScore
	pattern.Features["access_count"] = float64(len(pattern.AccessHistory))
	pattern.Features["avg_size"] = ap.calculateAverageSize(pattern.AccessHistory)
	pattern.Features["time_variance"] = ap.calculateTimeVariance(pattern.AccessHistory)
}

func (ap *AccessPredictor) predictSequential(pattern *AccessPattern) []types.PrefetchCandidate {
	if len(pattern.AccessHistory) == 0 {
		return nil
	}

	lastAccess := pattern.AccessHistory[len(pattern.AccessHistory)-1]
	var candidates []types.PrefetchCandidate

	// Predict next sequential blocks
	for i := 1; i <= ap.config.PrefetchAhead; i++ {
		offset := lastAccess.Offset + int64(i)*lastAccess.Size
		candidates = append(candidates, types.PrefetchCandidate{
			Path:     pattern.Key,
			Offset:   offset,
			Size:     lastAccess.Size,
			Priority: int(pattern.SequentialScore*100) - i, // Decreasing priority
			Deadline: time.Now().Add(time.Minute),
		})
	}

	return candidates
}

func (ap *AccessPredictor) predictTemporal(pattern *AccessPattern) []types.PrefetchCandidate {
	// Predict based on temporal patterns (simplified)
	// In practice, this would analyze time-based access patterns
	return nil
}

func (ap *AccessPredictor) predictML(pattern *AccessPattern) []types.PrefetchCandidate {
	if ap.model == nil {
		return nil
	}

	// Use ML model to predict next access
	features := ap.extractFeatures(pattern)
	confidence := ap.model.predict(features)

	if confidence < ap.config.ConfidenceThreshold {
		return nil
	}

	// Generate candidates based on ML prediction
	// This is a simplified version - real ML would be more sophisticated
	return []types.PrefetchCandidate{
		{
			Path:     pattern.Key,
			Offset:   pattern.AccessHistory[len(pattern.AccessHistory)-1].Offset,
			Size:     pattern.AccessHistory[len(pattern.AccessHistory)-1].Size,
			Priority: int(confidence * 100),
			Deadline: time.Now().Add(30 * time.Second),
		},
	}
}

func (ap *AccessPredictor) updateModel() {
	// Update ML model with recent training data
	// This is a simplified online learning approach

	if len(ap.recentAccess) < ap.windowSize {
		return
	}

	// Create training examples from recent access patterns
	examples := ap.createTrainingExamples()

	// Update model weights using gradient descent
	for _, example := range examples {
		// Calculate prediction without holding lock
		prediction := ap.model.predict(example.Features)
		residual := example.Target - prediction

		// Now acquire lock to update weights
		ap.model.mu.Lock()
		// Update weights
		for i, feature := range example.Features {
			featureName := ap.getFeatureName(i)
			if _, exists := ap.model.weights[featureName]; !exists {
				ap.model.weights[featureName] = 0.0
			}
			ap.model.weights[featureName] += ap.model.learningRate * residual * feature * example.Weight
		}

		// Update bias
		ap.model.bias += ap.model.learningRate * residual * example.Weight
		ap.model.mu.Unlock()
	}
}

// Helper methods

func (ap *AccessPredictor) calculateAverageSize(history []AccessEvent) float64 {
	if len(history) == 0 {
		return 0
	}

	total := int64(0)
	for _, event := range history {
		total += event.Size
	}
	return float64(total) / float64(len(history))
}

func (ap *AccessPredictor) calculateTimeVariance(history []AccessEvent) float64 {
	if len(history) < 2 {
		return 0
	}

	intervals := make([]time.Duration, 0, len(history)-1)
	for i := 1; i < len(history); i++ {
		interval := history[i].Timestamp.Sub(history[i-1].Timestamp)
		intervals = append(intervals, interval)
	}

	// Calculate variance of intervals
	mean := time.Duration(0)
	for _, interval := range intervals {
		mean += interval
	}
	mean /= time.Duration(len(intervals))

	variance := float64(0)
	for _, interval := range intervals {
		diff := float64(interval - mean)
		variance += diff * diff
	}
	variance /= float64(len(intervals))

	return variance
}

func (ap *AccessPredictor) extractFeatures(pattern *AccessPattern) []float64 {
	features := make([]float64, 0, 10)

	features = append(features, pattern.SequentialScore)
	features = append(features, pattern.FrequencyScore)
	features = append(features, pattern.RecencyScore)
	features = append(features, float64(len(pattern.AccessHistory)))

	if len(pattern.AccessHistory) > 0 {
		features = append(features, float64(pattern.AccessHistory[len(pattern.AccessHistory)-1].Size))
	} else {
		features = append(features, 0)
	}

	// Add more sophisticated features as needed
	return features
}

func (ap *AccessPredictor) createTrainingExamples() []TrainingExample {
	// Create training examples from access patterns
	// This is simplified - real implementation would be more sophisticated
	examples := make([]TrainingExample, 0, len(ap.recentAccess))

	for i := 1; i < len(ap.recentAccess); i++ {
		prev := ap.recentAccess[i-1]
		curr := ap.recentAccess[i]

		// Create features from previous access
		features := []float64{
			float64(prev.Size),
			float64(prev.Offset),
			float64(prev.Timestamp.Unix()),
		}

		// Target: 1 if next access was sequential, 0 otherwise
		target := 0.0
		if curr.Offset == prev.Offset+prev.Size && curr.Key == prev.Key {
			target = 1.0
		}

		examples = append(examples, TrainingExample{
			Features: features,
			Target:   target,
			Weight:   1.0,
		})
	}

	return examples
}

func (ap *AccessPredictor) getFeatureName(index int) string {
	names := []string{"size", "offset", "timestamp", "sequential", "frequency", "recency"}
	if index < len(names) {
		return names[index]
	}
	return "feature_" + string(rune(index))
}

// ML Model Implementation

func (pm *PredictionModel) predict(features []float64) float64 {
	pm.mu.RLock()
	defer pm.mu.RUnlock()

	prediction := pm.bias
	featureNames := []string{"size", "offset", "timestamp", "sequential", "frequency", "recency"}

	for i, feature := range features {
		var featureName string
		if i < len(featureNames) {
			featureName = featureNames[i]
		} else {
			featureName = "feature_" + string(rune(i))
		}

		if weight, exists := pm.weights[featureName]; exists {
			prediction += weight * feature
		}
	}

	// Apply sigmoid activation
	return 1.0 / (1.0 + math.Exp(-prediction))
}

// Prefetch Implementation

func (pc *PredictiveCache) triggerPrefetch(candidates []types.PrefetchCandidate) {
	if !pc.config.EnablePrefetch || len(candidates) == 0 {
		return
	}

	job := &PrefetchJob{
		Candidates: candidates,
		CreatedAt:  time.Now(),
		Priority:   candidates[0].Priority,
		Confidence: float64(candidates[0].Priority) / 100.0,
	}

	// Refuse work once the cache is closed. Without this a read arriving after Close leaves a job in
	// the buffer that no worker will ever take, and enough of them fill the queue — so the visible
	// symptom of a stale cache reference is prefetch silently ceasing, arbitrarily far from the cause.
	//
	// It is a separate check and not another arm of the select below, which is the version this
	// started as. A select chooses uniformly at random among the arms that are ready, so a closed
	// stopCh alongside a ready send wins only about half the time: measured, 25 of 64 reads after
	// Close still queued. The guard has to run before the send is offered, not beside it.
	select {
	case <-pc.prefetcher.stopCh:
		return
	default:
	}

	select {
	case pc.prefetcher.prefetchQueue <- job:
		atomic.AddUint64(&pc.prefetcher.stats.JobsQueued, 1)
	default:
		// Queue full, drop job
	}
}

func (pc *PredictiveCache) startPrefetchWorkers() {
	for range pc.config.MaxConcurrentFetch {
		go pc.prefetchWorker()
	}
}

func (pc *PredictiveCache) prefetchWorker() {
	for {
		select {
		case job := <-pc.prefetcher.prefetchQueue:
			pc.processPrefetchJob(job)
		case <-pc.prefetcher.stopCh:
			return
		}
	}
}

func (pc *PredictiveCache) processPrefetchJob(job *PrefetchJob) {
	job.StartedAt = time.Now()

	for _, candidate := range job.Candidates {
		// Check rate limiter
		if !pc.prefetcher.rateLimiter.Allow(candidate.Size) {
			continue
		}

		// Check if already in cache
		if existing := pc.baseCache.Get(candidate.Path, candidate.Offset, candidate.Size); existing != nil {
			continue
		}

		// Fetch from backend if available
		if pc.prefetcher.backend != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			data, err := pc.prefetcher.backend.GetObject(ctx, candidate.Path, candidate.Offset, candidate.Size)
			cancel()

			if err == nil {
				pc.baseCache.Put(candidate.Path, candidate.Offset, data)
				job.BytesFetched += int64(len(data))

				// Recorded against the bytes actually returned, not against candidate.Size: a ranged GET at
				// the end of an object returns short, and a ledger entry claiming more than was stored would
				// never be claimed by any read — scoring a correct prefetch as waste.
				pc.recordPrefetch(candidate.Path, candidate.Offset, int64(len(data)))
			}
		}
	}

	job.CompletedAt = time.Now()

	atomic.AddUint64(&pc.prefetcher.stats.JobsCompleted, 1)
}

// Intelligent Eviction Implementation

func (pc *PredictiveCache) intelligentEvict(sizeNeeded int64) bool {
	candidates := pc.evictionMgr.generateEvictionCandidates()
	if len(candidates) == 0 {
		return pc.baseCache.Evict(sizeNeeded)
	}

	// Sort by eviction score (higher score = more likely to evict)
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].EvictionScore > candidates[j].EvictionScore
	})

	totalEvicted := int64(0)
	for _, candidate := range candidates {
		if totalEvicted >= sizeNeeded {
			break
		}

		pc.baseCache.Delete(candidate.Key)
		totalEvicted += candidate.Size

		pc.stats.mu.Lock()
		pc.stats.EvictionsTotal++
		pc.stats.EvictionsIntelligent++
		pc.stats.recomputeRatiosLocked()
		pc.stats.mu.Unlock()
	}

	return totalEvicted >= sizeNeeded
}

func (em *IntelligentEvictionManager) generateEvictionCandidates() []*EvictionCandidate {
	em.predictor.mu.RLock()
	defer em.predictor.mu.RUnlock()

	now := time.Now()
	oneHourAgo := now.Add(-time.Hour)

	candidates := make([]*EvictionCandidate, 0, len(em.predictor.patterns))
	for _, pattern := range em.predictor.patterns {
		avgSize := int64(em.predictor.calculateAverageSize(pattern.AccessHistory))

		// Compute recency directly so future timestamps and patterns with
		// fewer than 2 accesses (where calculatePatternFeatures is a no-op)
		// are handled correctly.
		var recencyScore float64
		if !pattern.LastAccess.IsZero() {
			age := max(now.Sub(pattern.LastAccess),
				// clamp: treat future-timestamped entries as just-accessed
				0)
			recencyScore = math.Exp(-age.Hours() / 24)
		}

		// Compute frequency: fraction of history within the last hour.
		var frequencyScore float64
		if n := len(pattern.AccessHistory); n > 0 {
			recent := 0
			for _, ev := range pattern.AccessHistory {
				if ev.Timestamp.After(oneHourAgo) {
					recent++
				}
			}
			frequencyScore = float64(recent) / float64(n)
		}

		// Objects with low recency and low frequency should be evicted first.
		evictionScore := (1.0 - recencyScore) * (1.0 - frequencyScore*0.5)

		candidates = append(candidates, &EvictionCandidate{
			Key:            pattern.Key,
			Size:           avgSize,
			LastAccess:     pattern.LastAccess,
			AccessCount:    len(pattern.AccessHistory),
			PredictedReuse: recencyScore,
			EvictionScore:  evictionScore,
		})
	}
	return candidates
}

// Rate Limiter Implementation

// Allow reports whether a transfer of the given size fits in the remaining budget, consuming it if
// so.
//
// The refill arithmetic is where this used to fail, and it failed in the direction that is hardest to
// notice: `int64(elapsed.Seconds())` truncates, so every call made less than a second after the last
// one refilled *zero* tokens — while `lastRefill = now` was assigned unconditionally, discarding the
// elapsed time along with it. Called at 1 Hz or faster, which is what a cache under load does, the
// bucket never refilled at all. With `tokens` starting at the zero value the bucket also started
// empty, so the first call was refused too and the whole prefetcher — the predictor, the pattern
// analysis, four workers — was dead weight that only came alive under light enough load for a whole
// second to elapse between reads. A rate limiter that works when idle and not when busy is
// backwards.
//
// Two changes fix it. The refill is computed in nanoseconds so a fractional second is worth
// fractional tokens, and lastRefill only advances by the time actually converted into tokens, so a
// remainder too small to be a token is carried rather than dropped. Integer division still truncates
// the last few bytes, which is why the remainder has to be carried and not merely rounded.
func (rl *RateLimiter) Allow(bytes int64) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	if rl.refillRate > 0 {
		elapsed := time.Since(rl.lastRefill)

		if newTokens := elapsed.Nanoseconds() * rl.refillRate / int64(time.Second); newTokens > 0 {
			rl.tokens = min(rl.capacity, rl.tokens+newTokens)

			// Advance only by the span that became tokens. Assigning now would discard the
			// sub-token remainder on every call, which is the truncation above in a subtler form.
			rl.lastRefill = rl.lastRefill.Add(time.Duration(newTokens * int64(time.Second) / rl.refillRate))
		}
	}

	if rl.tokens >= bytes {
		rl.tokens -= bytes
		return true
	}

	return false
}

// Statistics and monitoring

// recordPredictions notes what the predictor just claimed would be read next, and counts the claims.
//
// Called before triggerPrefetch, and separately from it, so a prediction the prefetcher declined to act
// on still counts against accuracy. The two are different questions — "was the predictor right" and "did
// prefetching help" — and the second is not a fair proxy for the first on a mount, where the prefetcher's
// backend is nil and it fetches nothing at all.
func (pc *PredictiveCache) recordPredictions(candidates []types.PrefetchCandidate) {
	confidence := 0.0
	for _, candidate := range candidates {
		confidence += float64(candidate.Priority) / 100.0
	}

	// Counted before the ranges are published, for the reason recordPrefetch's comment gives: a
	// prediction is claimable the moment it is in the ledger, and PredictionsCorrect must not be able to
	// run ahead of PredictionsTotal.
	func() {
		pc.stats.mu.Lock()
		defer pc.stats.mu.Unlock()

		n := uint64(len(candidates))
		pc.stats.PredictionsTotal += n

		// A running mean over every prediction ever made, kept incrementally because the individual
		// confidences are not retained. Weighted by n so a burst of candidates does not count as one
		// sample.
		if total := pc.stats.PredictionsTotal; total > 0 {
			pc.stats.AvgConfidence += (confidence - float64(n)*pc.stats.AvgConfidence) / float64(total)
		}

		pc.stats.recomputeRatiosLocked()
	}()

	for _, candidate := range candidates {
		pc.predictions.record(candidate.Path, candidate.Offset, candidate.Size)
	}
}

// recordPrediction credits the predictor when a read lands in a range it named.
//
// Consulted for every read, hit or miss: a prediction is correct if the *application* read those bytes,
// which is the thing being predicted, and whether the cache happened to hold them is a separate question
// that PrefetchHits answers. Scoring only hits would make accuracy a function of cache capacity.
func (pc *PredictiveCache) recordPrediction(key string, offset, size int64) {
	if !pc.predictions.claim(key, offset, size) {
		return
	}

	pc.stats.mu.Lock()
	defer pc.stats.mu.Unlock()

	pc.stats.PredictionsCorrect++
	pc.stats.recomputeRatiosLocked()
}

// recordPrefetch notes bytes a worker stored, so a later read can be attributed to the prefetch.
//
// # The denominator is counted before the range is published
//
// A range in a ledger is claimable the instant it is there, by any reader, on any goroutine. Recording
// the range first — which is what this did — opens a window in which the numerator can be incremented
// while the denominator has not been: a read claims the range, updateStats counts a PrefetchHit, and
// PrefetchRequests is still what it was. With several readers against one prefetch worker that window
// is not theoretical. CI caught PrefetchHits at 4 against PrefetchRequests at 1, reported as
// "efficiency 4" by TestGetPredictiveStatsIsSafeUnderConcurrentReads, which 40 local runs of the same
// test did not reproduce.
//
// Neither lock can be held across both halves. The ledger is written by prefetch workers while a read
// holds the stats lock, and the reverse order exists too, so a function taking both would deadlock —
// the same constraint that made takeUnclaimed a drained counter rather than a direct read. Ordering
// the two uncontended sections is what is available, and it is sufficient: a hit can only be counted
// after the range is published, which is after the request that published it was counted. The reverse
// window, where the denominator leads, makes the ratio momentarily *low* rather than impossible.
//
// A ratio above 1 is worse than one that is briefly low. It is not a value the statistic can take, so
// it tells an operator reading a dashboard that the instrumentation is broken — and it says so for the
// life of the mount, because these are cumulative counters and nothing subtracts. recordPredictions
// orders itself the same way and for the same reason.
func (pc *PredictiveCache) recordPrefetch(key string, offset, length int64) {
	func() {
		pc.stats.mu.Lock()
		defer pc.stats.mu.Unlock()

		pc.stats.PrefetchRequests++
		pc.stats.PrefetchBytes += length
		pc.stats.recomputeRatiosLocked()
	}()

	pc.ledger.record(key, offset, length)
}

func (pc *PredictiveCache) updateStats(event AccessEvent, latency time.Duration) {
	// Waste is collected here rather than at eviction because the ledger cannot take the stats lock: it is
	// written from the prefetch workers while a read holds that lock, and the reverse order exists too, so
	// having either reach into the other invites a deadlock. takeUnclaimed drains a counter instead.
	wasted := pc.ledger.takeUnclaimed()

	pc.stats.mu.Lock()
	defer pc.stats.mu.Unlock()

	pc.stats.PrefetchWaste += wasted

	// Bytes served from a range a prefetch worker stored. event.Prefetch is set by Get from the ledger;
	// before #223 nothing set it, so this branch was unreachable and PrefetchHits stayed zero on every
	// mount.
	if event.Hit && event.Prefetch {
		pc.stats.PrefetchHits++
	}

	pc.stats.recomputeRatiosLocked()

	// latency is accepted and unused. It was the input to LatencyReduction, which would need the latency
	// of the read that *did not happen* to be a reduction of anything — a counterfactual this cache has no
	// way to observe. The field is gone; the parameter stays because the measurement is genuinely taken at
	// the call site and a future comparison against the backend's own latency (internal/storage/s3 already
	// tracks it) is the shape that could use it.
	_ = latency
}

// recomputeRatiosLocked derives the ratio fields from the counters. Caller holds stats.mu.
//
// Derived on write rather than on read so that [PredictiveCache.GetPredictiveStats] cannot return a
// struct whose ratios disagree with its counters — and because every one of these was zero for the
// lifetime of this type, which is what #223 is about. A ratio with no denominator stays zero rather than
// becoming NaN: NaN serializes to invalid JSON, and `null` in a metrics field reads as an outage.
func (s *PredictiveStats) recomputeRatiosLocked() {
	if s.PredictionsTotal > 0 {
		s.PredictionAccuracy = float64(s.PredictionsCorrect) / float64(s.PredictionsTotal)
	}

	// Against requests, not against hits plus waste: a prefetch that has been fetched but not yet read or
	// evicted is neither, and excluding it would make efficiency jump around as the ledger fills.
	if s.PrefetchRequests > 0 {
		s.PrefetchEfficiency = float64(s.PrefetchHits) / float64(s.PrefetchRequests)
	}
}

// Initialize model with reasonable defaults
func (pc *PredictiveCache) initializeModel() {
	pc.predictor.model.weights["sequential_score"] = 2.0
	pc.predictor.model.weights["frequency_score"] = 1.5
	pc.predictor.model.weights["recency_score"] = 1.0
	pc.predictor.model.weights["size"] = 0.1
	pc.predictor.model.bias = -0.5
}

// Close shuts down the predictive cache and stops all background workers.
//
// It is idempotent and safe to call while reads are in flight, both of which it previously was not:
// it closed prefetchQueue, whose senders are cache reads that can arrive at any moment, and closed
// stopCh unconditionally so a second call panicked on its own. See [IntelligentPrefetcher.stopCh]
// for why the queue is left open.
//
// It does not wait for the workers to finish the job each is running. A prefetch is a GET the caller
// is not waiting on, so blocking an unmount behind one — potentially a multi-megabyte transfer over
// a slow link — would trade a real delay for no benefit.
func (pc *PredictiveCache) Close() error {
	if pc.prefetcher == nil {
		return nil
	}

	pc.prefetcher.stopOnce.Do(func() {
		close(pc.prefetcher.stopCh)
	})

	return nil
}
