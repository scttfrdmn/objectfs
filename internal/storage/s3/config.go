package s3

import (
	"time"
)

// Config represents S3 backend configuration
type Config struct {
	Region          string `yaml:"region"`
	Endpoint        string `yaml:"endpoint"`
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`
	ForcePathStyle  bool   `yaml:"force_path_style"`

	// Performance settings
	MaxRetries     int           `yaml:"max_retries"`
	ConnectTimeout time.Duration `yaml:"connect_timeout"`
	RequestTimeout time.Duration `yaml:"request_timeout"`
	PoolSize       int           `yaml:"pool_size"`

	// Advanced settings
	UseAccelerate bool `yaml:"use_accelerate"`
	UseDualStack  bool `yaml:"use_dual_stack"`
	DisableSSL    bool `yaml:"disable_ssl"`

	// CargoShip optimization settings
	EnableCargoShipOptimization bool    `yaml:"enable_cargoship_optimization"`
	TargetThroughput            float64 `yaml:"target_throughput"`  // MB/s
	OptimizationLevel           string  `yaml:"optimization_level"` // "standard", "aggressive"

	// S3 Storage Tier Configuration
	StorageTier      string           `yaml:"storage_tier"`      // "STANDARD", "STANDARD_IA", "ONEZONE_IA", etc.
	TierConstraints  TierConstraints  `yaml:"tier_constraints"`  // Tier-specific constraints
	CostOptimization CostOptimization `yaml:"cost_optimization"` // Cost optimization settings
	PricingConfig    PricingConfig    `yaml:"pricing_config"`    // Custom pricing configuration
}

// TierConstraints defines tier-specific constraints and limitations
type TierConstraints struct {
	MinObjectSize      int64         `yaml:"min_object_size"`      // Minimum object size in bytes
	DeletionEmbargo    time.Duration `yaml:"deletion_embargo"`     // Minimum storage duration before deletion
	RetrievalLatency   string        `yaml:"retrieval_latency"`    // Expected retrieval latency ("instant", "minutes", "hours")
	RetrievalCost      bool          `yaml:"retrieval_cost"`       // Whether retrieval incurs additional charges
	MinimumStorageDays int           `yaml:"minimum_storage_days"` // Minimum billable storage period
	TransitionDelay    time.Duration `yaml:"transition_delay"`     // Delay before transitioning to this tier
}

// CostOptimization defines cost optimization settings
type CostOptimization struct {
	EnableAutoTiering     bool             `yaml:"enable_auto_tiering"`     // Automatically transition objects between tiers
	TransitionRules       []TransitionRule `yaml:"transition_rules"`        // Rules for automatic tier transitions
	LifecycleManagement   bool             `yaml:"lifecycle_management"`    // Enable S3 lifecycle management
	IntelligentTiering    bool             `yaml:"intelligent_tiering"`     // Use S3 Intelligent Tiering
	CostThreshold         float64          `yaml:"cost_threshold"`          // Cost threshold for optimization decisions ($/GB/month)
	MonitorAccessPatterns bool             `yaml:"monitor_access_patterns"` // Monitor and optimize based on access patterns
}

// TransitionRule defines automatic tier transition rules
type TransitionRule struct {
	FromTier         string       `yaml:"from_tier"`          // Source tier
	ToTier           string       `yaml:"to_tier"`            // Destination tier
	AfterDays        int          `yaml:"after_days"`         // Days after creation to transition
	AccessPattern    string       `yaml:"access_pattern"`     // "infrequent", "archive", "cold"
	ObjectSizeFilter ObjectFilter `yaml:"object_size_filter"` // Filter by object size
}

// ObjectFilter defines filters for transition rules
type ObjectFilter struct {
	MinSize int64 `yaml:"min_size"` // Minimum object size in bytes
	MaxSize int64 `yaml:"max_size"` // Maximum object size in bytes (-1 for unlimited)
}

// PricingConfig defines custom pricing configuration for S3 costs
type PricingConfig struct {
	UsePricingAPI      bool                   `yaml:"use_pricing_api"`      // Fetch current AWS pricing via API
	Region             string                 `yaml:"region"`               // Pricing region (may differ from bucket region)
	CustomPricing      map[string]TierPricing `yaml:"custom_pricing"`       // Override pricing per tier
	DiscountConfig     DiscountConfig         `yaml:"discount_config"`      // Volume discounts and enterprise rates
	DiscountConfigFile string                 `yaml:"discount_config_file"` // Path to external discount config file
	AdditionalCosts    AdditionalCosts        `yaml:"additional_costs"`     // Request costs, data transfer, etc.
	LastUpdated        string                 `yaml:"last_updated"`         // When pricing was last updated
	Currency           string                 `yaml:"currency"`             // USD, EUR, etc.
}

// TierPricing defines pricing for a specific storage tier
type TierPricing struct {
	StorageCostPerGBMonth float64            `yaml:"storage_cost_per_gb_month"` // $/GB/month for storage
	RetrievalCostPerGB    float64            `yaml:"retrieval_cost_per_gb"`     // $/GB for retrieval
	RequestCosts          RequestCosts       `yaml:"request_costs"`             // Per-request pricing
	MinimumBillableSize   int64              `yaml:"minimum_billable_size"`     // Minimum billable object size
	MinimumBillableDays   int                `yaml:"minimum_billable_days"`     // Minimum billable period
	TransitionCosts       map[string]float64 `yaml:"transition_costs"`          // Cost to transition to other tiers
}

// RequestCosts defines per-request pricing
type RequestCosts struct {
	PutRequestCost    float64 `yaml:"put_request_cost"`    // Cost per PUT request
	GetRequestCost    float64 `yaml:"get_request_cost"`    // Cost per GET request
	DeleteRequestCost float64 `yaml:"delete_request_cost"` // Cost per DELETE request
	ListRequestCost   float64 `yaml:"list_request_cost"`   // Cost per LIST request
	HeadRequestCost   float64 `yaml:"head_request_cost"`   // Cost per HEAD request
}

// DiscountConfig defines volume discounts and enterprise pricing
type DiscountConfig struct {
	EnableVolumeDiscounts    bool               `yaml:"enable_volume_discounts"`    // Enable volume-based discounts
	VolumeTiers              []VolumeTier       `yaml:"volume_tiers"`               // Volume discount tiers
	EnterpriseDiscount       float64            `yaml:"enterprise_discount"`        // Overall enterprise discount (%)
	ReservedCapacityDiscount float64            `yaml:"reserved_capacity_discount"` // Reserved capacity discount (%)
	SpotDiscount             float64            `yaml:"spot_discount"`              // Spot pricing discount (%)
	CustomDiscounts          map[string]float64 `yaml:"custom_discounts"`           // Custom discounts per tier
}

// VolumeTier defines volume-based discount tiers
type VolumeTier struct {
	MinSizeGB       float64  `yaml:"min_size_gb"`      // Minimum size for this tier
	MaxSizeGB       float64  `yaml:"max_size_gb"`      // Maximum size for this tier (-1 = unlimited)
	DiscountPercent float64  `yaml:"discount_percent"` // Discount percentage for this tier
	AppliesTo       []string `yaml:"applies_to"`       // Which storage tiers this applies to
}

// AdditionalCosts defines additional cost factors
type AdditionalCosts struct {
	DataTransferOut   DataTransferPricing `yaml:"data_transfer_out"`  // Data transfer out costs
	ReplicationCosts  ReplicationPricing  `yaml:"replication_costs"`  // Cross-region replication
	CloudWatchMetrics float64             `yaml:"cloudwatch_metrics"` // CloudWatch metrics cost per metric
	InventoryReports  float64             `yaml:"inventory_reports"`  // S3 Inventory cost per object
	AccessLogging     float64             `yaml:"access_logging"`     // Access logging cost per request
}

// DataTransferPricing defines data transfer cost structure
type DataTransferPricing struct {
	FirstTBPerGB  float64 `yaml:"first_tb_per_gb"`  // First TB pricing
	Next9TBPerGB  float64 `yaml:"next_9tb_per_gb"`  // 1-10 TB pricing
	Next40TBPerGB float64 `yaml:"next_40tb_per_gb"` // 10-50 TB pricing
	Over50TBPerGB float64 `yaml:"over_50tb_per_gb"` // >50 TB pricing
}

// ReplicationPricing defines cross-region replication costs
type ReplicationPricing struct {
	ReplicationPerGB       float64 `yaml:"replication_per_gb"`       // Cost per GB replicated
	DestinationPutRequests float64 `yaml:"destination_put_requests"` // PUT request cost at destination
}

// NewDefaultConfig returns a configuration with sensible defaults
func NewDefaultConfig() *Config {
	return &Config{
		MaxRetries:                  3,
		ConnectTimeout:              10 * time.Second,
		RequestTimeout:              30 * time.Second,
		PoolSize:                    8,
		EnableCargoShipOptimization: true,
		TargetThroughput:            800.0, // 800 MB/s target for ObjectFS
		OptimizationLevel:           "standard",
		StorageTier:                 TierStandard,      // Default to Standard tier
		TierConstraints:             TierConstraints{}, // Use tier defaults
		CostOptimization: CostOptimization{
			EnableAutoTiering:     false,
			LifecycleManagement:   false,
			IntelligentTiering:    false,
			MonitorAccessPatterns: false,
		},
		PricingConfig: PricingConfig{
			UsePricingAPI: false,
			Region:        "us-east-1",
			Currency:      "USD",
			CustomPricing: make(map[string]TierPricing),
			DiscountConfig: DiscountConfig{
				EnableVolumeDiscounts: false,
				VolumeTiers:           []VolumeTier{},
				CustomDiscounts:       make(map[string]float64),
			},
		},
	}
}
