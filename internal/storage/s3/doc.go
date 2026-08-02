/*
Package s3 provides a high-performance AWS S3 backend with CargoShip optimization and advanced storage tier management.

This package implements the core object storage functionality for ObjectFS, featuring comprehensive S3 integration
with multi-tier storage support, cost optimization, and performance enhancements through CargoShip integration
that delivers up to 4.6x performance improvements over standard S3 operations.

# Architecture Overview

The S3 backend provides multiple layers of functionality:

	┌─────────────────────────────────────────────────────────────┐
	│                   ObjectFS Interface                       │
	│              (types.Backend Implementation)                │
	└─────────────────────────────────────────────────────────────┘
	                          │
	┌─────────────────────────────────────────────────────────────┐
	│                    S3 Backend Layer                        │
	│  ┌─────────────────┐ ┌──────────────┐ ┌─────────────────┐  │
	│  │  Cost Optimizer │ │ Tier Manager │ │ Pricing Manager │  │
	│  └─────────────────┘ └──────────────┘ └─────────────────┘  │
	└─────────────────────────────────────────────────────────────┘
	                          │
	┌─────────────────────────────────────────────────────────────┐
	│              CargoShip Transporter                          │
	│         (4.6x Performance Optimization)                    │
	└─────────────────────────────────────────────────────────────┘
	                          │
	┌─────────────────────────────────────────────────────────────┐
	│                 AWS S3 Service                             │
	│    Connection Pool  │  Multiple Regions  │  Storage Tiers  │
	└─────────────────────────────────────────────────────────────┘

# CargoShip Integration

The backend leverages CargoShip optimization for significant performance improvements:

Performance Benefits:
- 4.6x faster upload speeds through intelligent chunking
- Optimized connection pooling and reuse
- Advanced retry logic with exponential backoff
- Intelligent multipart upload optimization
- Reduced API call overhead through batching

CargoShip Features:
- Automatic optimal chunk size calculation
- Concurrent upload streams with load balancing
- Smart failure detection and recovery
- Regional endpoint optimization
- Bandwidth-aware throttling

# Storage Tier Management

Comprehensive support for all AWS S3 storage classes.

Rates are deliberately not restated here. This section used to carry a per-GB figure for each tier
and a Cost/GB column in the summary table below, which made it two more copies of the S3 rate card —
and a rate in a doc comment has no way to be told it is stale, so the only question is when it starts
lying rather than whether. internal/awsrates holds every rate, in one place, checked against the live
AWS Pricing API by a test. Read it there, or call PricingManager.GetTierPricing, which serves from it.

What stays here is the part that is S3 *behavior* rather than S3 *price*: minimum billable size,
minimum storage duration, and retrieval latency. Those change when AWS changes the product, not when
AWS changes a number.

Standard Tier (STANDARD):
- Instant access, no retrieval costs
- Recommended for frequently accessed data
- No minimum object size or storage duration

Standard-IA (STANDARD_IA):
- Instant access with retrieval costs
- 128KB minimum object size
- 30-day minimum storage duration
- Retrieval is charged per GB

One Zone-IA (ONEZONE_IA):
- Single availability zone storage
- Lower cost than Standard-IA
- Same constraints as Standard-IA
- Retrieval is charged per GB

Glacier Instant Retrieval (GLACIER_IR):
- Instant access for archive data
- 128KB minimum object size
- 90-day minimum storage duration
- Retrieval is charged per GB, at the highest rate of the instant-access tiers

Glacier Flexible Retrieval (GLACIER):
- Minutes to hours retrieval time
- 40KB minimum object size
- 90-day minimum storage duration
- Retrieval is charged per GB and varies by requested speed

Deep Archive (DEEP_ARCHIVE):
- Lowest cost, hours for retrieval
- 40KB minimum object size
- 180-day minimum storage duration
- Retrieval is charged per GB and varies by requested speed

Intelligent Tiering (INTELLIGENT_TIERING):
- Automatic tier optimization
- No retrieval charges
- Priced as Standard for the frequent-access tier, plus per-object monitoring charges

# Cost Optimization

Advanced cost optimization capabilities:

Intelligent Tier Selection:
The system analyzes access patterns and automatically recommends optimal storage tiers:

Access patterns are recorded by GetObject as reads happen; the report is derived from what has been
observed, so a freshly-opened backend has nothing to say yet.

	report := backend.GetCostOptimizationReport()

	for _, o := range report.OptimizationResults {
		fmt.Printf("%s: %s → %s (%s), $%.2f/month, confidence %.0f%%\n",
			o.ObjectKey, o.FromTier, o.ToTier, o.Reason,
			o.EstimatedMonthlySavings, o.ConfidenceLevel*100)
	}

	fmt.Printf("%d objects, $%.2f/month total\n",
		report.TotalObjects, report.TotalPotentialSavings)

Note that recording is gated on Config.MonitorAccessPatterns, which defaults false — with it off,
the report is empty rather than wrong.

Enterprise Pricing Support:
- Volume discount calculation
- Reserved capacity pricing
- Custom enterprise rates
- Multi-region cost analysis

# Configuration

Flexible configuration options:

	config := &s3.Config{
		Region:   "us-west-2",
		Endpoint: "", // empty: the AWS endpoint for the region

		// CargoShip upload path
		EnableCargoShipOptimization: true,

		// Connections and timeouts
		PoolSize:       8,
		ConnectTimeout: 10 * time.Second,
		RequestTimeout: 30 * time.Second,

		// Storage tier
		StorageTier: s3.TierStandard,
	}

Every field may be omitted. NewBackend fills in a default for each one whose zero value is not a
usable setting, so &s3.Config{Region: "us-west-2"} is a complete configuration — see NewBackend.
The exception is ParallelReadThreshold, where zero means "off" rather than "unset".

# Usage Examples

Basic backend initialization:

	backend, err := s3.NewBackend(ctx, "my-bucket", config)
	if err != nil {
		log.Fatal(err)
	}
	defer backend.Close()

Object operations with automatic optimization:

	// Put object with automatic tier selection. The final argument is user metadata, stored as
	// x-amz-meta-* headers; nil is fine. ObjectFS writes its own integrity keys there
	// (objectfs-sha256, objectfs-original-size) last, so those two names win over anything you
	// supply under them — they describe the bytes actually uploaded, which after compression are
	// not the bytes you handed in.
	err := backend.PutObject(ctx, "data/file.txt", data, nil)

	// Get object with CargoShip optimization
	data, err := backend.GetObject(ctx, "data/file.txt", 0, -1)

	// Head object for metadata
	info, err := backend.HeadObject(ctx, "data/file.txt")

Batch operations for improved performance:

	// Batch get operations
	keys := []string{"file1.txt", "file2.txt", "file3.txt"}
	results, err := backend.GetObjects(ctx, keys)

	// Batch put operations
	objects := map[string][]byte{
		"file1.txt": data1,
		"file2.txt": data2,
	}
	err = backend.PutObjects(ctx, objects)

# Performance Optimization

Multi-level performance optimizations:

CargoShip Integration:
- Automatically enabled for all operations
- Intelligent chunk size calculation
- Concurrent stream optimization
- Advanced retry mechanisms

Connection Pooling:
- Configurable pool size (default: 8 connections)
- Health monitoring and replacement
- Load balancing across connections
- Connection lifetime management

Tier-Aware Operations:
- Automatic tier detection
- Optimized operations based on storage class
- Retrieval cost prediction
- Access pattern learning

# Enterprise Features

Advanced enterprise capabilities:

Cost Management:
- Real-time cost tracking
- Budget alerts and controls
- Cost attribution by application/team
- Reserved capacity optimization

Multi-Region Support:
- Cross-region replication
- Regional failover
- Latency-based routing
- Cost-optimized regional storage

Security Integration:
- IAM role integration
- KMS encryption support
- VPC endpoint compatibility
- Access logging and monitoring

# Monitoring and Observability

Comprehensive monitoring integration:

Metrics Collection:
- Operation latency and throughput
- Error rates and retry statistics
- Cost tracking and attribution
- Storage tier utilization

Health Monitoring:
- Connection pool health
- Service availability checks
- Performance degradation detection
- Automatic recovery triggers

Alerting:
- Cost threshold violations
- Performance anomaly detection
- Tier optimization opportunities
- Error rate escalations

# Error Handling

Robust error handling and recovery:

Transient Error Recovery:
- Exponential backoff retry logic
- Circuit breaker patterns
- Connection pool failover
- Graceful degradation

Permanent Error Handling:
- Clear error categorization
- Detailed error context
- Recovery recommendations
- Operational guidance

# Thread Safety

The backend is designed for concurrent access:

- All public methods are thread-safe
- Internal state is protected with appropriate synchronization
- Connection pool handles concurrent requests
- Statistics collection is atomic

# Storage Classes Summary

Quick reference for S3 storage classes:

| Tier | Access | Min Size | Min Duration | Use Case |
|------|--------|----------|--------------|----------|
| Standard | Instant | None | None | Frequent |
| Standard-IA | Instant | 128KB | 30 days | Infrequent |
| One Zone-IA | Instant | 128KB | 30 days | Non-critical |
| Glacier IR | Instant | 128KB | 90 days | Archive + instant |
| Glacier | Minutes-Hours | 40KB | 90 days | Long-term archive |
| Deep Archive | Hours | 40KB | 180 days | Very long-term |
| Intelligent | Variable | 128KB | None | Auto-optimize |

This package provides enterprise-grade S3 integration with advanced optimization,
comprehensive cost management, and high-performance operation capabilities.
*/
package s3
