# ObjectFS

[![Go Reference](https://pkg.go.dev/badge/github.com/scttfrdmn/objectfs.svg)](https://pkg.go.dev/github.com/scttfrdmn/objectfs)
[![Go Report Card](https://goreportcard.com/badge/github.com/scttfrdmn/objectfs)](https://goreportcard.com/report/github.com/scttfrdmn/objectfs)
[![codecov](https://codecov.io/gh/scttfrdmn/objectfs/graph/badge.svg)](https://codecov.io/gh/scttfrdmn/objectfs)
[![CI Status](https://github.com/scttfrdmn/objectfs/workflows/CI/badge.svg)](https://github.com/scttfrdmn/objectfs/actions)

[![Release](https://img.shields.io/github/v/release/scttfrdmn/objectfs)](https://github.com/scttfrdmn/objectfs/releases)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/scttfrdmn/objectfs)](go.mod)
[![GitHub issues](https://img.shields.io/github/issues/scttfrdmn/objectfs)](https://github.com/scttfrdmn/objectfs/issues)
[![GitHub stars](https://img.shields.io/github/stars/scttfrdmn/objectfs)](https://github.com/scttfrdmn/objectfs/stargazers)

**Enterprise-grade POSIX-compliant filesystem for AWS S3 with intelligent cost optimization**

ObjectFS provides a high-performance, cross-platform FUSE filesystem that makes AWS S3 buckets accessible as local directories, specifically optimized for research workloads and enterprise deployments with comprehensive cost management.

---

## 🎯 Key Features

### 🚀 **High-Performance S3 Access**
- **POSIX-compliant** filesystem operations on S3 objects
- **Cross-platform FUSE support** (Linux, macOS, Windows)
- **CargoShip integration** for 4.6x S3 performance optimization
- **Intelligent caching** with multi-level cache hierarchy
- **Concurrent operations** with configurable parallelism

### 💰 **Enterprise Cost Management** ⭐
- **Complete S3 storage tier support** with automatic constraint validation
- **Enterprise pricing system** with multi-layered discount calculations
- **Institutional configuration management** for standardized enterprise deployments
- **Intelligent cost optimization** with access pattern analysis
- **Volume discount tiers** and custom enterprise agreements support

### 🔧 **Production Ready**
- **Zero-downtime deployments** with graceful configuration reloading
- **Comprehensive monitoring** with metrics and health checks
- **Security-first design** with credential management best practices
- **Extensive logging** for debugging and performance analysis
- **Pre-commit hooks** ensuring code quality and test coverage

---

## 🚀 Quick Start

### Installation

```bash
# Clone and build
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs
go build -o objectfs ./cmd/objectfs

# Or install directly
go install github.com/scttfrdmn/objectfs/cmd/objectfs@latest
```

### Basic Usage

```bash
# Create configuration
cp examples/config.yaml ~/.objectfs/config.yaml
# Edit config.yaml with your AWS credentials and S3 bucket

# Mount S3 bucket as local filesystem
./objectfs mount --config ~/.objectfs/config.yaml --mount-point /mnt/s3-data

# Use as normal filesystem
ls /mnt/s3-data
cp local-file.txt /mnt/s3-data/
cat /mnt/s3-data/remote-file.txt
```

### Enterprise Configuration

For institutions with AWS Enterprise Agreements:

```yaml
backends:
  s3:
    bucket: "your-enterprise-bucket"
    region: "us-west-2"
    
    # Reference external discount configuration distributed by IT
    pricing_config:
      discount_config_file: "/shared/aws/institutional-discounts.yaml"
      
    # Intelligent cost optimization
    cost_optimization:
      enabled: true
      cost_threshold: 0.01  # $0.01 minimum for optimization recommendations
```

See [examples/DISCOUNT_CONFIG_README.md](examples/DISCOUNT_CONFIG_README.md) for complete institutional setup guide.

---

## 📊 Use Cases

### 🔬 **Research & Academia**
- **Genomics data analysis** with seamless S3 access
- **Large dataset processing** without local storage limitations
- **Collaborative research** with shared S3 bucket access
- **Cost-effective archival** with automatic tier optimization

### 🏢 **Enterprise & Organizations**
- **Multi-department data sharing** with centralized S3 storage
- **Compliance and governance** with audit trails and access controls
- **Cost optimization** across multiple research groups and projects
- **Hybrid cloud workflows** integrating S3 with local compute

### 🧬 **Computational Biology**
- **Reference genome access** without local downloads
- **Pipeline data staging** with automatic caching
- **Result archival** with intelligent tier selection
- **Collaborative analysis** with shared intermediate results

---

## 🏗 Architecture

ObjectFS combines multiple optimization layers for maximum performance:

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────┐
│   Applications  │    │   FUSE Layer     │    │  S3 Backend     │
│                 │◄──►│                  │◄──►│                 │
│ cp, ls, grep... │    │ POSIX Operations │    │ CargoShip 4.6x  │
└─────────────────┘    └──────────────────┘    └─────────────────┘
                                │                         │
                       ┌──────────────────┐    ┌─────────────────┐
                       │  Cache System    │    │ Cost Optimizer  │
                       │                  │    │                 │
                       │ Multi-level LRU  │    │ Tier Analysis   │
                       └──────────────────┘    └─────────────────┘
```

---

## 📈 Performance

ObjectFS delivers exceptional performance for S3-based workloads:

- **4.6x throughput improvement** with CargoShip optimization
- **Sub-millisecond cache hits** for frequently accessed data
- **Concurrent operations** with configurable parallelism
- **Intelligent prefetching** based on access patterns
- **Write buffering** with configurable flush strategies

**Benchmark Results** (1GB genomics dataset):
- First access: ~45s (S3 download + cache)
- Subsequent access: ~2s (cache hit)
- Write operations: ~12s (buffered + async upload)

---

## 🛠 Configuration

ObjectFS supports comprehensive configuration for various deployment scenarios:

### Basic Research Setup
```yaml
backends:
  s3:
    bucket: "research-data"
    region: "us-east-1"
    storage_tier: "STANDARD"
```

### Enterprise Cost Optimization
```yaml
backends:
  s3:
    pricing_config:
      discount_config_file: "enterprise-discounts.yaml"
      use_pricing_api: true
    
    cost_optimization:
      enabled: true
      monitor_access_patterns: true
      optimization_interval: "24h"
```

See [examples/config.yaml](examples/config.yaml) for complete configuration options.

---

## 🧪 Development

ObjectFS uses **pre-commit hooks** for comprehensive development workflow:

### Setup Development Environment

```bash
# Clone and setup
git clone https://github.com/scttfrdmn/objectfs.git
cd objectfs

# Install and setup pre-commit hooks
./scripts/setup-hooks.sh
```

### Development Workflow

Every commit automatically runs:
- 🔧 **Code formatting** (gofmt, goimports)
- 🔍 **Linting** (golangci-lint)
- 🧪 **Full test suite** (go test -race with coverage)
- 🔒 **Security scanning** (gosec)
- ⚡ **Performance benchmarks**
- 📊 **Integration tests** (if LocalStack available)

### Manual Testing

```bash
# Run all checks manually
pre-commit run --all-files

# Run specific checks
pre-commit run go-test
pre-commit run gosec

# Skip hooks for emergency commits (not recommended)
git commit --no-verify
```

---

## 📦 Releases

### Latest Release: [v0.2.0](https://github.com/scttfrdmn/objectfs/releases/tag/v0.2.0) - Enterprise S3 Storage Tier Management

**Major enterprise-focused release** adding comprehensive AWS S3 storage tier support with intelligent cost optimization and institutional configuration management.

#### 🎯 New in v0.2.0
- **Complete S3 storage tier support** (Standard, Standard-IA, One Zone-IA, Glacier IR, Glacier, Deep Archive, Intelligent Tiering)
- **Enterprise pricing system** with multi-layered discount calculations
- **Institutional configuration management** for standardized enterprise deployments
- **Intelligent cost optimization engine** with access pattern analysis
- **External discount configuration files** for IT department distribution

#### Migration from v0.1.0
- **Fully backward compatible** - existing configurations work unchanged
- Optional adoption of new pricing and tier management features

### Previous Releases
- **[v0.1.0](https://github.com/scttfrdmn/objectfs/releases/tag/v0.1.0)** - Cross-Platform Research-Focused S3 Filesystem

---

## 🤝 Contributing

We welcome contributions! Please see our [contributing guidelines](CONTRIBUTING.md) for details.

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/amazing-feature`)
3. Make changes with comprehensive tests
4. Run pre-commit checks (`pre-commit run --all-files`)
5. Commit changes (`git commit -m 'Add amazing feature'`)
6. Push to branch (`git push origin feature/amazing-feature`)
7. Open a Pull Request

---

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

---

## 🆘 Support

- **Documentation**: [examples/](examples/) directory
- **Issues**: [GitHub Issues](https://github.com/scttfrdmn/objectfs/issues)
- **Discussions**: [GitHub Discussions](https://github.com/scttfrdmn/objectfs/discussions)

---

## 🏷 Keywords

`golang` `s3` `fuse` `filesystem` `aws` `storage` `genomics` `research` `enterprise` `cost-optimization` `posix` `cross-platform` `high-performance`

---

<div align="center">

**Built for the enterprise, optimized for research** 🚀

[⭐ Star this repository](https://github.com/scttfrdmn/objectfs) if ObjectFS helps your research or organization!

</div>