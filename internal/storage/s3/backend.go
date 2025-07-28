package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	cargoships3 "github.com/scttfrdmn/cargoship/pkg/aws/s3"
	awsconfig "github.com/scttfrdmn/cargoship/pkg/aws/config"

	"github.com/objectfs/objectfs/pkg/types"
)

// S3 Storage Tier Constants
const (
	TierStandard          = "STANDARD"
	TierStandardIA        = "STANDARD_IA" 
	TierOneZoneIA         = "ONEZONE_IA"
	TierReducedRedundancy = "REDUCED_REDUNDANCY"
	TierGlacierIR         = "GLACIER_IR"
	TierGlacier           = "GLACIER"
	TierDeepArchive       = "DEEP_ARCHIVE"
	TierIntelligent       = "INTELLIGENT_TIERING"
)

// Predefined storage tier information with AWS constraints
var StorageTiers = map[string]StorageTierInfo{
	TierStandard: {
		Name:                "Standard",
		MinObjectSize:       0,
		DeletionEmbargo:     0,
		RetrievalLatency:    "instant",
		RetrievalCost:       false,
		MinimumStorageDays:  0,
		RecommendedUseCase:  "Frequently accessed data",
		CostPerGBMonth:      0.023, // Approximate USD
	},
	TierStandardIA: {
		Name:                "Standard-Infrequent Access",
		MinObjectSize:       128 * 1024, // 128 KB minimum
		DeletionEmbargo:     30 * 24 * time.Hour, // 30 days minimum storage
		RetrievalLatency:    "instant",
		RetrievalCost:       true, // $0.01 per GB retrieval cost
		MinimumStorageDays:  30,
		RecommendedUseCase:  "Infrequently accessed data that needs instant access",
		CostPerGBMonth:      0.0125,
	},
	TierOneZoneIA: {
		Name:                "One Zone-Infrequent Access",
		MinObjectSize:       128 * 1024, // 128 KB minimum
		DeletionEmbargo:     30 * 24 * time.Hour, // 30 days minimum storage
		RetrievalLatency:    "instant",
		RetrievalCost:       true, // $0.01 per GB retrieval cost
		MinimumStorageDays:  30,
		RecommendedUseCase:  "Infrequently accessed data in single AZ",
		CostPerGBMonth:      0.01,
	},
	TierReducedRedundancy: {
		Name:                "Reduced Redundancy",
		MinObjectSize:       0,
		DeletionEmbargo:     0,
		RetrievalLatency:    "instant",
		RetrievalCost:       false,
		MinimumStorageDays:  0,
		RecommendedUseCase:  "Non-critical, reproducible data (deprecated)",
		CostPerGBMonth:      0.024,
	},
	TierGlacierIR: {
		Name:                "Glacier Instant Retrieval",
		MinObjectSize:       128 * 1024, // 128 KB minimum
		DeletionEmbargo:     90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:    "instant",
		RetrievalCost:       true, // $0.03 per GB retrieval cost
		MinimumStorageDays:  90,
		RecommendedUseCase:  "Archive data needing instant access",
		CostPerGBMonth:      0.004,
	},
	TierGlacier: {
		Name:                "Glacier Flexible Retrieval",
		MinObjectSize:       40 * 1024, // 40 KB minimum
		DeletionEmbargo:     90 * 24 * time.Hour, // 90 days minimum storage
		RetrievalLatency:    "minutes-hours",
		RetrievalCost:       true, // Variable retrieval costs
		MinimumStorageDays:  90,
		RecommendedUseCase:  "Long-term archive with flexible retrieval",
		CostPerGBMonth:      0.0036,
	},
	TierDeepArchive: {
		Name:                "Glacier Deep Archive",
		MinObjectSize:       40 * 1024, // 40 KB minimum
		DeletionEmbargo:     180 * 24 * time.Hour, // 180 days minimum storage
		RetrievalLatency:    "hours",
		RetrievalCost:       true, // Variable retrieval costs
		MinimumStorageDays:  180,
		RecommendedUseCase:  "Long-term archive rarely accessed",
		CostPerGBMonth:      0.00099,
	},
	TierIntelligent: {
		Name:                "Intelligent Tiering",
		MinObjectSize:       128 * 1024, // 128 KB minimum for optimization
		DeletionEmbargo:     0,
		RetrievalLatency:    "variable",
		RetrievalCost:       false, // No retrieval charges
		MinimumStorageDays:  0,
		RecommendedUseCase:  "Automatic cost optimization for changing access patterns",
		CostPerGBMonth:      0.023, // Plus monitoring charges
	},
}

// Backend implements the S3 storage backend with CargoShip optimization
type Backend struct {
	client     *s3.Client
	bucket     string
	region     string
	endpoint   string
	pathStyle  bool
	
	// Connection pool
	pool       *ConnectionPool
	
	// Configuration
	config     *Config
	
	// CargoShip S3 Optimization (4.6x performance)
	transporter *cargoships3.Transporter
	logger      *slog.Logger
	
	// Storage Tier Management
	currentTier     string
	tierInfo        StorageTierInfo
	tierValidator   *TierValidator
	costOptimizer   *CostOptimizer
	pricingManager  *PricingManager
	
	// Metrics
	mu         sync.RWMutex
	metrics    BackendMetrics
}

// Config represents S3 backend configuration
type Config struct {
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`
	ForcePathStyle  bool   `yaml:"force_path_style"`
	
	// Performance settings
	MaxRetries      int           `yaml:"max_retries"`
	ConnectTimeout  time.Duration `yaml:"connect_timeout"`
	RequestTimeout  time.Duration `yaml:"request_timeout"`
	PoolSize        int           `yaml:"pool_size"`
	
	// Advanced settings
	UseAccelerate   bool   `yaml:"use_accelerate"`
	UseDualStack    bool   `yaml:"use_dual_stack"`
	DisableSSL      bool   `yaml:"disable_ssl"`
	
	// CargoShip optimization settings
	EnableCargoShipOptimization bool `yaml:"enable_cargoship_optimization"`
	TargetThroughput           float64 `yaml:"target_throughput"`          // MB/s
	OptimizationLevel         string   `yaml:"optimization_level"`        // "standard", "aggressive"
	
	// S3 Storage Tier Configuration
	StorageTier               string            `yaml:"storage_tier"`               // "STANDARD", "STANDARD_IA", "ONEZONE_IA", "REDUCED_REDUNDANCY", "GLACIER_IR"
	TierConstraints           TierConstraints   `yaml:"tier_constraints"`           // Tier-specific constraints
	CostOptimization          CostOptimization  `yaml:"cost_optimization"`          // Cost optimization settings
	PricingConfig             PricingConfig     `yaml:"pricing_config"`             // Custom pricing configuration
}

// PricingConfig defines custom pricing configuration for S3 costs
type PricingConfig struct {
	UsePricingAPI           bool                      `yaml:"use_pricing_api"`            // Fetch current AWS pricing via API
	Region                  string                    `yaml:"region"`                     // Pricing region (may differ from bucket region)
	CustomPricing           map[string]TierPricing    `yaml:"custom_pricing"`             // Override pricing per tier
	DiscountConfig          DiscountConfig            `yaml:"discount_config"`            // Volume discounts and enterprise rates
	DiscountConfigFile      string                    `yaml:"discount_config_file"`       // Path to external discount config file
	AdditionalCosts         AdditionalCosts           `yaml:"additional_costs"`           // Request costs, data transfer, etc.
	LastUpdated             string                    `yaml:"last_updated"`               // When pricing was last updated
	Currency                string                    `yaml:"currency"`                   // USD, EUR, etc.
}

// TierPricing defines pricing for a specific storage tier
type TierPricing struct {
	StorageCostPerGBMonth   float64   `yaml:"storage_cost_per_gb_month"`    // $/GB/month for storage
	RetrievalCostPerGB      float64   `yaml:"retrieval_cost_per_gb"`        // $/GB for retrieval
	RequestCosts            RequestCosts `yaml:"request_costs"`             // Per-request pricing
	MinimumBillableSize     int64     `yaml:"minimum_billable_size"`        // Minimum billable object size
	MinimumBillableDays     int       `yaml:"minimum_billable_days"`        // Minimum billable period
	TransitionCosts         map[string]float64 `yaml:"transition_costs"`    // Cost to transition to other tiers
}

// RequestCosts defines per-request pricing
type RequestCosts struct {
	PutRequestCost          float64   `yaml:"put_request_cost"`             // Cost per PUT request
	GetRequestCost          float64   `yaml:"get_request_cost"`             // Cost per GET request
	DeleteRequestCost       float64   `yaml:"delete_request_cost"`          // Cost per DELETE request
	ListRequestCost         float64   `yaml:"list_request_cost"`            // Cost per LIST request
	HeadRequestCost         float64   `yaml:"head_request_cost"`            // Cost per HEAD request
}

// DiscountConfig defines volume discounts and enterprise pricing
type DiscountConfig struct {
	EnableVolumeDiscounts   bool                      `yaml:"enable_volume_discounts"`    // Enable volume-based discounts
	VolumeTiers             []VolumeTier              `yaml:"volume_tiers"`               // Volume discount tiers
	EnterpriseDiscount      float64                   `yaml:"enterprise_discount"`        // Overall enterprise discount (%)
	ReservedCapacityDiscount float64                  `yaml:"reserved_capacity_discount"` // Reserved capacity discount (%)
	SpotDiscount            float64                   `yaml:"spot_discount"`              // Spot pricing discount (%)
	CustomDiscounts         map[string]float64        `yaml:"custom_discounts"`           // Custom discounts per tier
}

// VolumeTier defines volume-based discount tiers
type VolumeTier struct {
	MinSizeGB               float64   `yaml:"min_size_gb"`                  // Minimum size for this tier
	MaxSizeGB               float64   `yaml:"max_size_gb"`                  // Maximum size for this tier (-1 = unlimited)
	DiscountPercent         float64   `yaml:"discount_percent"`             // Discount percentage for this tier
	AppliesTo               []string  `yaml:"applies_to"`                   // Which storage tiers this applies to
}

// AdditionalCosts defines additional cost factors
type AdditionalCosts struct {
	DataTransferOut         DataTransferPricing       `yaml:"data_transfer_out"`          // Data transfer out costs
	ReplicationCosts        ReplicationPricing        `yaml:"replication_costs"`          // Cross-region replication
	CloudWatchMetrics       float64                   `yaml:"cloudwatch_metrics"`        // CloudWatch metrics cost per metric
	InventoryReports        float64                   `yaml:"inventory_reports"`         // S3 Inventory cost per object
	AccessLogging           float64                   `yaml:"access_logging"`            // Access logging cost per request
}

// DataTransferPricing defines data transfer cost structure
type DataTransferPricing struct {
	FirstTBPerGB            float64   `yaml:"first_tb_per_gb"`              // First TB pricing
	Next9TBPerGB            float64   `yaml:"next_9tb_per_gb"`              // 1-10 TB pricing
	Next40TBPerGB           float64   `yaml:"next_40tb_per_gb"`             // 10-50 TB pricing
	Over50TBPerGB           float64   `yaml:"over_50tb_per_gb"`             // >50 TB pricing
}

// ReplicationPricing defines cross-region replication costs
type ReplicationPricing struct {
	ReplicationPerGB        float64   `yaml:"replication_per_gb"`           // Cost per GB replicated
	DestinationPutRequests  float64   `yaml:"destination_put_requests"`     // PUT request cost at destination
}

// TierConstraints defines tier-specific constraints and limitations
type TierConstraints struct {
	MinObjectSize       int64         `yaml:"min_object_size"`        // Minimum object size in bytes
	DeletionEmbargo     time.Duration `yaml:"deletion_embargo"`       // Minimum storage duration before deletion
	RetrievalLatency    string        `yaml:"retrieval_latency"`      // Expected retrieval latency ("instant", "minutes", "hours")
	RetrievalCost       bool          `yaml:"retrieval_cost"`         // Whether retrieval incurs additional charges
	MinimumStorageDays  int           `yaml:"minimum_storage_days"`   // Minimum billable storage period
	TransitionDelay     time.Duration `yaml:"transition_delay"`       // Delay before transitioning to this tier
}

// CostOptimization defines cost optimization settings
type CostOptimization struct {
	EnableAutoTiering       bool              `yaml:"enable_auto_tiering"`        // Automatically transition objects between tiers
	TransitionRules         []TransitionRule  `yaml:"transition_rules"`           // Rules for automatic tier transitions
	LifecycleManagement     bool              `yaml:"lifecycle_management"`       // Enable S3 lifecycle management
	IntelligentTiering      bool              `yaml:"intelligent_tiering"`        // Use S3 Intelligent Tiering
	CostThreshold           float64           `yaml:"cost_threshold"`             // Cost threshold for optimization decisions ($/GB/month)
	MonitorAccessPatterns   bool              `yaml:"monitor_access_patterns"`    // Monitor and optimize based on access patterns
}

// TransitionRule defines automatic tier transition rules
type TransitionRule struct {
	FromTier          string        `yaml:"from_tier"`           // Source tier
	ToTier            string        `yaml:"to_tier"`             // Destination tier
	AfterDays         int           `yaml:"after_days"`          // Days after creation to transition
	AccessPattern     string        `yaml:"access_pattern"`      // "infrequent", "archive", "cold"
	ObjectSizeFilter  ObjectFilter  `yaml:"object_size_filter"`  // Filter by object size
}

// ObjectFilter defines filters for transition rules
type ObjectFilter struct {
	MinSize int64 `yaml:"min_size"` // Minimum object size in bytes
	MaxSize int64 `yaml:"max_size"` // Maximum object size in bytes (-1 for unlimited)
}

// StorageTierInfo contains tier-specific information and constraints
type StorageTierInfo struct {
	Name                string        `json:"name"`
	MinObjectSize       int64         `json:"min_object_size"`
	DeletionEmbargo     time.Duration `json:"deletion_embargo"`
	RetrievalLatency    string        `json:"retrieval_latency"`
	RetrievalCost       bool          `json:"retrieval_cost"`
	MinimumStorageDays  int           `json:"minimum_storage_days"`
	RecommendedUseCase  string        `json:"recommended_use_case"`
	CostPerGBMonth      float64       `json:"cost_per_gb_month"`      // Approximate cost in USD
}

// TierValidator validates operations against storage tier constraints
type TierValidator struct {
	tier        string
	constraints TierConstraints
	tierInfo    StorageTierInfo
	logger      *slog.Logger
}

// NewTierValidator creates a new tier validator
func NewTierValidator(tier string, constraints TierConstraints, logger *slog.Logger) *TierValidator {
	tierInfo, exists := StorageTiers[tier]
	if !exists {
		// Default to Standard tier if unknown
		tierInfo = StorageTiers[TierStandard]
		tier = TierStandard
	}
	
	return &TierValidator{
		tier:        tier,
		constraints: constraints,
		tierInfo:    tierInfo,
		logger:      logger,
	}
}

// ValidateWrite validates a write operation against tier constraints
func (tv *TierValidator) ValidateWrite(key string, dataSize int64) error {
	// Check minimum object size constraint
	minSize := tv.tierInfo.MinObjectSize
	if tv.constraints.MinObjectSize > 0 {
		minSize = tv.constraints.MinObjectSize
	}
	
	if dataSize < minSize {
		return fmt.Errorf("object size %d bytes is below minimum %d bytes for %s tier", 
			dataSize, minSize, tv.tier)
	}
	
	// Log tier-specific warnings
	if tv.tierInfo.RetrievalCost {
		tv.logger.Debug("Writing to tier with retrieval costs", 
			"tier", tv.tier, 
			"key", key, 
			"size", dataSize)
	}
	
	return nil
}

// ValidateDelete validates a delete operation against tier constraints
func (tv *TierValidator) ValidateDelete(key string, objectAge time.Duration) error {
	// Check deletion embargo
	embargo := tv.tierInfo.DeletionEmbargo
	if tv.constraints.DeletionEmbargo > 0 {
		embargo = tv.constraints.DeletionEmbargo
	}
	
	if embargo > 0 && objectAge < embargo {
		return fmt.Errorf("object %s cannot be deleted before %v (current age: %v) due to %s tier constraints",
			key, embargo, objectAge, tv.tier)
	}
	
	// Warn about minimum storage charges
	if tv.tierInfo.MinimumStorageDays > 0 && objectAge < time.Duration(tv.tierInfo.MinimumStorageDays)*24*time.Hour {
		tv.logger.Warn("Deleting object before minimum storage period - charges may still apply",
			"tier", tv.tier,
			"key", key,
			"age", objectAge,
			"minimum_days", tv.tierInfo.MinimumStorageDays)
	}
	
	return nil
}

// GetTierInfo returns information about the current tier
func (tv *TierValidator) GetTierInfo() StorageTierInfo {
	return tv.tierInfo
}

// GetRecommendations returns tier recommendations based on access patterns
func (tv *TierValidator) GetRecommendations(objectSize int64, accessFrequency string) []string {
	recommendations := make([]string, 0, 3)
	
	// Size-based recommendations
	if objectSize < 128*1024 {
		recommendations = append(recommendations, "Consider Standard tier for small objects to avoid IA minimum charges")
	}
	
	// Access pattern recommendations
	switch accessFrequency {
	case "frequent":
		if tv.tier != TierStandard {
			recommendations = append(recommendations, "Consider Standard tier for frequently accessed data")
		}
	case "infrequent":
		if tv.tier == TierStandard {
			recommendations = append(recommendations, "Consider Standard-IA or One Zone-IA for cost savings")
		}
	case "archive":
		if tv.tier != TierGlacierIR && tv.tier != TierGlacier {
			recommendations = append(recommendations, "Consider Glacier tiers for archive data")
		}
	case "unknown":
		if tv.tier != TierIntelligent {
			recommendations = append(recommendations, "Consider Intelligent Tiering for unknown access patterns")
		}
	}
	
	return recommendations
}

// convertTierToStorageClass converts our tier constants to AWS SDK storage class types
func convertTierToStorageClass(tier string) s3types.StorageClass {
	switch tier {
	case TierStandard:
		return s3types.StorageClassStandard
	case TierStandardIA:
		return s3types.StorageClassStandardIa
	case TierOneZoneIA:
		return s3types.StorageClassOnezoneIa
	case TierReducedRedundancy:
		return s3types.StorageClassReducedRedundancy
	case TierGlacierIR:
		return s3types.StorageClassGlacierIr
	case TierGlacier:
		return s3types.StorageClassGlacier
	case TierDeepArchive:
		return s3types.StorageClassDeepArchive
	case TierIntelligent:
		return s3types.StorageClassIntelligentTiering
	default:
		return s3types.StorageClassStandard
	}
}

// convertTierToCargoShipStorageClass converts our tier constants to CargoShip storage class types
func convertTierToCargoShipStorageClass(tier string) awsconfig.StorageClass {
	switch tier {
	case TierStandard:
		return awsconfig.StorageClassStandard
	case TierStandardIA:
		return awsconfig.StorageClassStandardIA
	case TierOneZoneIA:
		return awsconfig.StorageClassOneZoneIA
	case TierReducedRedundancy:
		return awsconfig.StorageClassStandard // Fallback to Standard (deprecated tier)
	case TierGlacierIR:
		return awsconfig.StorageClassGlacier // Use Glacier for instant retrieval (CargoShip limitation)
	case TierGlacier:
		return awsconfig.StorageClassGlacier
	case TierDeepArchive:
		return awsconfig.StorageClassDeepArchive
	case TierIntelligent:
		return awsconfig.StorageClassIntelligentTiering
	default:
		return awsconfig.StorageClassStandard
	}
}

// BackendMetrics tracks S3 backend performance metrics
type BackendMetrics struct {
	Requests        int64         `json:"requests"`
	Errors          int64         `json:"errors"`
	BytesUploaded   int64         `json:"bytes_uploaded"`
	BytesDownloaded int64         `json:"bytes_downloaded"`
	AverageLatency  time.Duration `json:"average_latency"`
	LastError       string        `json:"last_error"`
	LastErrorTime   time.Time     `json:"last_error_time"`
}

// NewBackend creates a new S3 backend instance
func NewBackend(ctx context.Context, bucket string, cfg *Config) (*Backend, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket name cannot be empty")
	}

	if cfg == nil {
		cfg = &Config{
			MaxRetries:     3,
			ConnectTimeout: 10 * time.Second,
			RequestTimeout: 30 * time.Second,
			PoolSize:       8,
			EnableCargoShipOptimization: true,
			TargetThroughput: 800.0, // 800 MB/s target for ObjectFS
			OptimizationLevel: "standard",
			StorageTier: TierStandard, // Default to Standard tier
			TierConstraints: TierConstraints{}, // Use tier defaults
			CostOptimization: CostOptimization{
				EnableAutoTiering: false,
				LifecycleManagement: false,
				IntelligentTiering: false,
				MonitorAccessPatterns: false,
			},
			PricingConfig: PricingConfig{
				UsePricingAPI: false,
				Region: "us-east-1",
				Currency: "USD", 
				CustomPricing: make(map[string]TierPricing),
				DiscountConfig: DiscountConfig{
					EnableVolumeDiscounts: false,
					VolumeTiers: []VolumeTier{},
					CustomDiscounts: make(map[string]float64),
				},
			},
		}
	}
	
	// Set default storage tier if not specified
	if cfg.StorageTier == "" {
		cfg.StorageTier = TierStandard
	}

	// Load AWS configuration
	awsCfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(cfg.Region),
		config.WithRetryMaxAttempts(cfg.MaxRetries),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to load AWS config: %w", err)
	}

	// Create S3 client with custom options
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		}
		if cfg.ForcePathStyle {
			o.UsePathStyle = true
		}
		if cfg.UseAccelerate {
			o.UseAccelerate = true
		}
		if cfg.UseDualStack {
			o.UseDualstack = true
		}
	})

	// Create connection pool
	pool, err := NewConnectionPool(cfg.PoolSize, func() (*s3.Client, error) {
		return s3.NewFromConfig(awsCfg), nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create connection pool: %w", err)
	}

	// Initialize logger
	logger := slog.Default().With("component", "s3-backend", "bucket", bucket)
	
	// Initialize CargoShip S3 transporter if enabled
	var transporter *cargoships3.Transporter
	if cfg.EnableCargoShipOptimization {
		// Create CargoShip S3 config with optimization settings
		cargoConfig := awsconfig.S3Config{
			Bucket:             bucket,
			StorageClass:       awsconfig.StorageClassIntelligentTiering, // Intelligent tiering
			MultipartThreshold: 32 * 1024 * 1024,    // 32MB threshold
			MultipartChunkSize: 16 * 1024 * 1024,    // 16MB chunks for optimization
			Concurrency:        cfg.PoolSize,         // Match pool size
		}
		
		// Use CargoShip's optimized transporter with BBR/CUBIC algorithms
		transporter = cargoships3.NewTransporter(client, cargoConfig)
		logger.Info("CargoShip S3 optimization enabled", "target_throughput", cfg.TargetThroughput, "chunk_size", "16MB", "concurrency", cfg.PoolSize)
	}
	
	// Initialize tier validator
	tierValidator := NewTierValidator(cfg.StorageTier, cfg.TierConstraints, logger)
	tierInfo := tierValidator.GetTierInfo()
	
	backend := &Backend{
		client:        client,
		bucket:        bucket,
		region:        cfg.Region,
		endpoint:      cfg.Endpoint,
		pathStyle:     cfg.ForcePathStyle,
		pool:          pool,
		config:        cfg,
		transporter:   transporter,
		logger:        logger,
		currentTier:   cfg.StorageTier,
		tierInfo:      tierInfo,
		tierValidator: tierValidator,
		metrics:       BackendMetrics{},
	}
	
	// Initialize pricing manager
	backend.pricingManager = NewPricingManager(cfg.PricingConfig, logger)
	
	// Initialize cost optimizer
	backend.costOptimizer = NewCostOptimizer(backend, cfg.CostOptimization, logger)
	
	// Log tier configuration
	logger.Info("S3 storage tier configured", 
		"tier", cfg.StorageTier,
		"tier_name", tierInfo.Name,
		"min_object_size", tierInfo.MinObjectSize,
		"deletion_embargo", tierInfo.DeletionEmbargo,
		"retrieval_cost", tierInfo.RetrievalCost,
		"cost_per_gb_month", tierInfo.CostPerGBMonth)

	// Test connection
	if err := backend.HealthCheck(ctx); err != nil {
		return nil, fmt.Errorf("S3 backend health check failed: %w", err)
	}

	return backend, nil
}

// GetObject retrieves an object or part of an object from S3 with CargoShip optimization
func (b *Backend) GetObject(ctx context.Context, key string, offset, size int64) ([]byte, error) {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	// Build range header if needed
	var rangeHeader *string
	if offset > 0 || size > 0 {
		if size > 0 {
			rangeHeader = aws.String(fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))
		} else {
			rangeHeader = aws.String(fmt.Sprintf("bytes=%d-", offset))
		}
	}

	input := &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
		Range:  rangeHeader,
	}

	// Use standard S3 client for reads (CargoShip optimizes uploads)
	client := b.pool.Get()
	defer b.pool.Put(client)
	
	result, err := client.GetObject(ctx, input)
	
	if err != nil {
		b.recordError(err)
		return nil, b.translateError(err, "GetObject", key)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		b.recordError(err)
		return nil, fmt.Errorf("failed to read object body: %w", err)
	}

	b.mu.Lock()
	b.metrics.BytesDownloaded += int64(len(data))
	b.mu.Unlock()

	// Record access pattern for cost optimization
	b.costOptimizer.RecordAccess(key, int64(len(data)))

	return data, nil
}

// PutObject stores an object in S3 with CargoShip optimization
func (b *Backend) PutObject(ctx context.Context, key string, data []byte) error {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	// Validate write operation against tier constraints
	if err := b.tierValidator.ValidateWrite(key, int64(len(data))); err != nil {
		b.recordError(err)
		return fmt.Errorf("tier validation failed: %w", err)
	}

	// Handle Standard tier overhead for cost optimization
	effectiveTier := b.currentTier
	if b.config.CostOptimization.MonitorAccessPatterns {
		effectiveTier = b.costOptimizer.HandleStandardTierOverhead(key, int64(len(data)))
		if effectiveTier != b.currentTier {
			b.logger.Debug("Using Standard tier to avoid IA overhead",
				"object", key,
				"size", len(data),
				"configured_tier", b.currentTier,
				"effective_tier", effectiveTier)
		}
	}

	// Get storage class for effective tier
	storageClass := convertTierToStorageClass(effectiveTier)

	input := &s3.PutObjectInput{
		Bucket:        aws.String(b.bucket),
		Key:           aws.String(key),
		Body:          bytes.NewReader(data),
		ContentLength: aws.Int64(int64(len(data))),
		ContentType:   aws.String(b.detectContentType(key)),
		StorageClass:  storageClass,
	}

	// Use CargoShip transporter if available for optimized uploads (4.6x performance)
	var err error
	
	if b.transporter != nil {
		// Use CargoShip's optimized upload with BBR/CUBIC algorithms
		cargoStorageClass := convertTierToCargoShipStorageClass(effectiveTier)
		archive := cargoships3.Archive{
			Key:    key,
			Reader: bytes.NewReader(data),
			Size:   int64(len(data)),
			StorageClass: cargoStorageClass,
			Metadata: map[string]string{
				"objectfs-upload": "true",
				"content-type":    b.detectContentType(key),
				"storage-tier":    effectiveTier,
				"configured-tier": b.currentTier,
			},
		}
		
		result, uploadErr := b.transporter.Upload(ctx, archive)
		if uploadErr == nil {
			b.logger.Debug("CargoShip optimized upload completed", 
				"key", key, 
				"size", len(data), 
				"throughput", result.Throughput,
				"duration", result.Duration)
			return nil
		}
		
		b.logger.Warn("CargoShip optimization failed, falling back to standard S3", "key", key, "error", uploadErr)
	}
	
	// Fallback to standard S3 client
	client := b.pool.Get()
	defer b.pool.Put(client)
	_, err = client.PutObject(ctx, input)
	
	if err != nil {
		b.recordError(err)
		return b.translateError(err, "PutObject", key)
	}

	b.mu.Lock()
	b.metrics.BytesUploaded += int64(len(data))
	b.mu.Unlock()

	return nil
}

// DeleteObject removes an object from S3
func (b *Backend) DeleteObject(ctx context.Context, key string) error {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()
	
	// Get object metadata to check creation time for tier validation
	objectInfo, err := b.HeadObject(ctx, key)
	if err != nil {
		// If object doesn't exist, that's ok for delete operation
		var notFound *s3types.NoSuchKey
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("failed to get object metadata for deletion validation: %w", err)
	}
	
	// Validate deletion against tier constraints
	objectAge := time.Since(objectInfo.LastModified)
	if err := b.tierValidator.ValidateDelete(key, objectAge); err != nil {
		b.recordError(err)
		return fmt.Errorf("tier validation failed: %w", err)
	}

	client := b.pool.Get()
	defer b.pool.Put(client)

	input := &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}

	_, err = client.DeleteObject(ctx, input)
	if err != nil {
		b.recordError(err)
		return b.translateError(err, "DeleteObject", key)
	}

	return nil
}

// HeadObject retrieves metadata about an object
func (b *Backend) HeadObject(ctx context.Context, key string) (*types.ObjectInfo, error) {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	client := b.pool.Get()
	defer b.pool.Put(client)

	input := &s3.HeadObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	}

	result, err := client.HeadObject(ctx, input)
	if err != nil {
		b.recordError(err)
		return nil, b.translateError(err, "HeadObject", key)
	}

	info := &types.ObjectInfo{
		Key:          key,
		Size:         aws.ToInt64(result.ContentLength),
		LastModified: aws.ToTime(result.LastModified),
		ETag:         aws.ToString(result.ETag),
		ContentType:  aws.ToString(result.ContentType),
		Metadata:     make(map[string]string),
	}

	// Copy metadata
	for k, v := range result.Metadata {
		info.Metadata[k] = v
	}

	return info, nil
}

// GetObjects retrieves multiple objects in batch with CargoShip optimization
func (b *Backend) GetObjects(ctx context.Context, keys []string) (map[string][]byte, error) {
	if len(keys) == 0 {
		return make(map[string][]byte), nil
	}

	// Use parallel individual requests (CargoShip focuses on upload optimization)
	results := make(map[string][]byte, len(keys))
	
	type result struct {
		key  string
		data []byte
		err  error
	}

	resultCh := make(chan result, len(keys))
	semaphore := make(chan struct{}, b.config.PoolSize)

	for _, key := range keys {
		go func(k string) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			data, err := b.GetObject(ctx, k, 0, 0)
			resultCh <- result{key: k, data: data, err: err}
		}(key)
	}

	var firstError error
	for i := 0; i < len(keys); i++ {
		res := <-resultCh
		if res.err != nil {
			if firstError == nil {
				firstError = res.err
			}
			continue
		}
		results[res.key] = res.data
	}

	if firstError != nil && len(results) == 0 {
		return nil, firstError
	}

	return results, nil
}

// PutObjects stores multiple objects in batch with CargoShip optimization
func (b *Backend) PutObjects(ctx context.Context, objects map[string][]byte) error {
	if len(objects) == 0 {
		return nil
	}

	// Use parallel individual requests (each will use CargoShip if available)
	type result struct {
		key string
		err error
	}

	resultCh := make(chan result, len(objects))
	semaphore := make(chan struct{}, b.config.PoolSize)

	for key, data := range objects {
		go func(k string, d []byte) {
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			err := b.PutObject(ctx, k, d)
			resultCh <- result{key: k, err: err}
		}(key, data)
	}

	var errors []string
	for i := 0; i < len(objects); i++ {
		res := <-resultCh
		if res.err != nil {
			errors = append(errors, fmt.Sprintf("%s: %v", res.key, res.err))
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("batch put failed for %d objects: %s", len(errors), strings.Join(errors, "; "))
	}

	return nil
}

// ListObjects lists objects in the bucket with the given prefix
func (b *Backend) ListObjects(ctx context.Context, prefix string, limit int) ([]types.ObjectInfo, error) {
	start := time.Now()
	defer func() {
		b.recordMetrics(time.Since(start), false)
	}()

	client := b.pool.Get()
	defer b.pool.Put(client)

	var maxKeys *int32
	if limit > 0 {
		// Safe conversion to prevent overflow
		if limit > 0x7FFFFFFF {
			maxKeys = aws.Int32(0x7FFFFFFF)
		} else {
			maxKeys = aws.Int32(int32(limit))
		}
	}

	input := &s3.ListObjectsV2Input{
		Bucket:  aws.String(b.bucket),
		Prefix:  aws.String(prefix),
		MaxKeys: maxKeys,
	}

	result, err := client.ListObjectsV2(ctx, input)
	if err != nil {
		b.recordError(err)
		return nil, b.translateError(err, "ListObjects", prefix)
	}

	objects := make([]types.ObjectInfo, 0, len(result.Contents))
	for _, obj := range result.Contents {
		info := types.ObjectInfo{
			Key:          aws.ToString(obj.Key),
			Size:         aws.ToInt64(obj.Size),
			LastModified: aws.ToTime(obj.LastModified),
			ETag:         aws.ToString(obj.ETag),
			Metadata:     make(map[string]string),
		}
		objects = append(objects, info)
	}

	return objects, nil
}

// HealthCheck verifies the backend connection
func (b *Backend) HealthCheck(ctx context.Context) error {
	client := b.pool.Get()
	defer b.pool.Put(client)

	// Try to head the bucket
	input := &s3.HeadBucketInput{
		Bucket: aws.String(b.bucket),
	}

	_, err := client.HeadBucket(ctx, input)
	if err != nil {
		return fmt.Errorf("S3 health check failed: %w", err)
	}

	return nil
}

// GetMetrics returns current backend metrics
func (b *Backend) GetMetrics() BackendMetrics {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.metrics
}

// Close closes the backend and releases resources
func (b *Backend) Close() error {
	// CargoShip transporter doesn't require explicit cleanup
	
	return b.pool.Close()
}

// Helper methods

func (b *Backend) recordMetrics(duration time.Duration, isError bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	b.metrics.Requests++
	if isError {
		b.metrics.Errors++
	}
	
	// Calculate rolling average latency
	if b.metrics.Requests == 1 {
		b.metrics.AverageLatency = duration
	} else {
		b.metrics.AverageLatency = time.Duration(
			(int64(b.metrics.AverageLatency)*9 + int64(duration)) / 10,
		)
	}
}

func (b *Backend) recordError(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	
	b.metrics.LastError = err.Error()
	b.metrics.LastErrorTime = time.Now()
}

func (b *Backend) translateError(err error, operation, key string) error {
	switch {
	case isErrorType[*s3types.NoSuchKey](err):
		return fmt.Errorf("object not found: %s", key)
	case isErrorType[*s3types.NoSuchBucket](err):
		return fmt.Errorf("bucket not found: %s", b.bucket)
	default:
		return fmt.Errorf("%s failed for %s: %w", operation, key, err)
	}
}

func (b *Backend) detectContentType(key string) string {
	switch {
	case strings.HasSuffix(key, ".json"):
		return "application/json"
	case strings.HasSuffix(key, ".xml"):
		return "application/xml"
	case strings.HasSuffix(key, ".html"):
		return "text/html"
	case strings.HasSuffix(key, ".txt"):
		return "text/plain"
	case strings.HasSuffix(key, ".jpg"), strings.HasSuffix(key, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(key, ".png"):
		return "image/png"
	case strings.HasSuffix(key, ".pdf"):
		return "application/pdf"
	default:
		return "application/octet-stream"
	}
}

// isErrorType checks if an error is of a specific type
func isErrorType[T error](err error) bool {
	var target T
	return errors.As(err, &target)
}

// GetCurrentTier returns the current storage tier information
func (b *Backend) GetCurrentTier() StorageTierInfo {
	return b.tierInfo
}

// GetAllTiers returns information about all available storage tiers
func (b *Backend) GetAllTiers() map[string]StorageTierInfo {
	return StorageTiers
}

// GetTierRecommendations returns tier recommendations for an object
func (b *Backend) GetTierRecommendations(objectSize int64, accessFrequency string) []string {
	return b.tierValidator.GetRecommendations(objectSize, accessFrequency)
}

// SetStorageTier changes the storage tier (requires restarting backend for full effect)
func (b *Backend) SetStorageTier(tier string, constraints TierConstraints) error {
	tierInfo, exists := StorageTiers[tier]
	if !exists {
		return fmt.Errorf("unsupported storage tier: %s", tier)
	}
	
	// Update tier validator
	b.tierValidator = NewTierValidator(tier, constraints, b.logger)
	
	// Update backend state
	b.currentTier = tier
	b.tierInfo = tierInfo
	b.config.StorageTier = tier
	b.config.TierConstraints = constraints
	
	b.logger.Info("Storage tier changed", 
		"tier", tier,
		"tier_name", tierInfo.Name,
		"min_object_size", tierInfo.MinObjectSize,
		"deletion_embargo", tierInfo.DeletionEmbargo,
		"cost_per_gb_month", tierInfo.CostPerGBMonth)
	
	return nil
}

// ValidateObjectForTier validates if an object meets current tier requirements
func (b *Backend) ValidateObjectForTier(key string, size int64) error {
	return b.tierValidator.ValidateWrite(key, size)
}

// GetTierConstraints returns the current tier constraints
func (b *Backend) GetTierConstraints() TierConstraints {
	return b.config.TierConstraints
}

// GetTierCostEstimate estimates monthly storage cost for given data size
func (b *Backend) GetTierCostEstimate(sizeGB float64) float64 {
	return sizeGB * b.tierInfo.CostPerGBMonth
}

// GetCostOptimizationReport generates a cost optimization analysis report
func (b *Backend) GetCostOptimizationReport() OptimizationReport {
	report := b.costOptimizer.GetOptimizationReport()
	report.GeneratedAt = time.Now()
	return report
}

// OptimizeStorageCosts analyzes and applies cost optimizations
func (b *Backend) OptimizeStorageCosts(ctx context.Context) error {
	return b.costOptimizer.AnalyzeAndOptimize(ctx)
}

// EstimateStandardTierOverhead calculates potential overhead from Standard tier usage
func (b *Backend) EstimateStandardTierOverhead(objectSize int64, targetTier string) float64 {
	return b.costOptimizer.EstimateStandardTierOverhead(objectSize, targetTier)
}

// GetAccessPatterns returns access pattern data for cost analysis
func (b *Backend) GetAccessPatternCount() int {
	return len(b.costOptimizer.accessPatterns)
}

// GetPricingSummary returns current pricing configuration and rates
func (b *Backend) GetPricingSummary() PricingSummary {
	return b.pricingManager.GetPricingSummary()
}

// RefreshPricing forces a refresh of pricing data from AWS API
func (b *Backend) RefreshPricing(ctx context.Context) error {
	return b.pricingManager.RefreshPricing(ctx)
}

// GetTierPricingWithDiscounts returns pricing for a tier with all discounts applied
func (b *Backend) GetTierPricingWithDiscounts(tier string) (TierPricing, error) {
	return b.pricingManager.GetTierPricing(tier)
}

// CalculateCostWithVolume calculates cost for a specific volume and tier
func (b *Backend) CalculateCostWithVolume(tier string, sizeGB float64) (float64, error) {
	tierPricing, err := b.pricingManager.GetTierPricing(tier)
	if err != nil {
		return 0, err
	}
	
	baseCost := sizeGB * tierPricing.StorageCostPerGBMonth
	return b.pricingManager.CalculateVolumeDiscount(tier, sizeGB, baseCost), nil
}