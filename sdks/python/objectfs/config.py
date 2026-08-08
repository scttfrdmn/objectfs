"""
ObjectFS Configuration Classes

Comprehensive configuration management for ObjectFS Python SDK.
"""

import os
import yaml
from dataclasses import dataclass, field, asdict
from pathlib import Path
from typing import Dict, List, Optional, Union, Any

from .exceptions import ConfigurationError


@dataclass
class GlobalConfig:
    """Global configuration settings."""
    log_level: str = "INFO"
    log_file: str = ""
    pid_file: str = ""
    daemon: bool = False


@dataclass
class S3Config:
    """S3 storage configuration."""
    region: str = "us-east-1"
    endpoint: str = ""
    profile: str = ""
    use_acceleration: bool = False
    force_path_style: bool = False
    max_retries: int = 3
    timeout: int = 30

    # Cost optimization. One key, matching the Go schema exactly, because ``to_yaml``
    # writes this dict straight into a document the Go loader decodes strictly: a key
    # it does not define fails the mount naming the key.
    #
    # There were five here -- ``enabled``, ``tiering_enabled``, ``lifecycle_enabled``,
    # ``transition_to_ia``, ``transition_to_glacier`` -- and they were removed from the
    # schema in v0.11.0 rather than renamed, having never reached the S3 backend at all.
    # Three of them also defaulted to True, so this SDK's default configuration wrote a
    # document asking for automatic tiering and lifecycle management, neither of which
    # ObjectFS implements. Default False here for the same reason the Go side does: it
    # changes the storage class objects are written with.
    cost_optimization: Dict[str, Any] = field(default_factory=lambda: {
        "small_objects_on_standard": False
    })


@dataclass
class StorageConfig:
    """Storage backend configuration."""
    s3: S3Config = field(default_factory=S3Config)

    def validate(self):
        """Validate storage configuration."""
        if not self.s3.region:
            raise ConfigurationError("S3 region is required")


@dataclass
class PerformanceConfig:
    """Performance and caching configuration."""
    cache_size: str = "4GB"
    max_concurrency: int = 200
    multilevel_caching: bool = True
    predictive_caching: bool = False
    ml_model_path: str = ""

    # I/O optimization
    read_ahead_size: str = "4MB"
    write_buffer_size: str = "4MB"
    max_write_buffer: str = "64MB"

    def validate(self):
        """Validate performance configuration."""
        if self.max_concurrency <= 0:
            raise ConfigurationError("max_concurrency must be positive")


@dataclass
class ClusterConfig:
    """Distributed cluster configuration.

    ``consistency_level`` is gone as of v0.12.0. It took ``"eventual"``, ``"strong"`` or
    ``"session"``, all three of which issued the same unconditional PUT and differed only in how
    many nodes issued it, so the setting selected a request count rather than a guarantee. The Go
    loader rejects unknown keys, so a document written by an older SDK now fails at load naming the
    key -- which is the intended outcome: what replaced it is a per-write precondition evaluated by
    S3, not a per-mount level, and no key here can express that.

    The three timeouts below are a separate, older problem and are left as they are: the Go
    ``cluster`` schema has never had ``election_timeout``, ``heartbeat_interval`` or
    ``join_timeout`` -- those live on the disjoint ``internal/distributed.ClusterConfig`` (#139) --
    so ``to_yaml`` emits a cluster block the loader refuses on the first of them. Verified against
    the loader rather than inferred. Fixing that is not #284's to do; it is filed as #385.
    """
    enabled: bool = False
    node_id: str = ""
    listen_addr: str = "0.0.0.0:8080"
    advertise_addr: str = "127.0.0.1:8080"
    seed_nodes: List[str] = field(default_factory=list)
    replication_factor: int = 3

    # Timeouts
    election_timeout: str = "5s"
    heartbeat_interval: str = "1s"
    join_timeout: str = "30s"

    def validate(self):
        """Validate cluster configuration."""
        if self.enabled and not self.listen_addr:
            raise ConfigurationError("listen_addr required when cluster is enabled")


@dataclass
class SecurityConfig:
    """Security configuration."""
    enabled: bool = False
    auth_method: str = "none"
    tls_enabled: bool = False
    tls_cert_path: str = ""
    tls_key_path: str = ""
    tls_ca_path: str = ""

    def validate(self):
        """Validate security configuration."""
        if self.tls_enabled:
            if not self.tls_cert_path or not self.tls_key_path:
                raise ConfigurationError("TLS certificate and key paths required")


@dataclass
class MonitoringConfig:
    """Monitoring and observability configuration.

    Each listener's address lives beside the ``enabled`` flag that governs it, matching the Go
    schema this class serializes to. It previously declared flat ``metrics_addr`` and
    ``health_check_addr`` keys defaulting to ``:9090``/``:8081``: the Go loader read neither --
    it read ``global.metrics_port``/``global.health_port`` -- so ``to_yaml`` produced a document
    that set no listener address, and the empty host meant every interface rather than loopback
    anyway. Both keys are gone from the Go schema as of v0.11.0, and the loader now rejects
    unknown keys, so a file written by an older SDK fails at load and names the key to fix.

    ``enable_pprof`` is gone with them. Nothing ever started a pprof listener, and the server it
    would have started serves mutating ``/memory/gc`` and ``/memory/free`` handlers with no
    authentication.

    Both endpoints below are unauthenticated -- ``/metrics`` reports per-operation counts and
    timings, ``/health`` reports component names and error strings -- which is why the defaults
    are loopback. Publishing them further is a choice you write down.
    """
    enabled: bool = False

    metrics: Dict[str, Any] = field(default_factory=lambda: {
        "enabled": True,
        "addr": "127.0.0.1:8080",
        "prometheus": True,
    })

    health_checks: Dict[str, Any] = field(default_factory=lambda: {
        "enabled": True,
        "addr": "127.0.0.1:8081",
        "interval": "30s",
        "timeout": "5s",
    })

    # OpenTelemetry
    opentelemetry: Dict[str, Any] = field(default_factory=lambda: {
        "enabled": False,
        "endpoint": "localhost:4317",
        "service_name": "objectfs",
        "headers": {}
    })


@dataclass
class FUSEConfig:
    """FUSE filesystem configuration."""
    allow_other: bool = False
    allow_root: bool = False
    default_permissions: bool = False
    uid: int = -1
    gid: int = -1
    umask: int = 0o022


@dataclass
class Configuration:
    """Main ObjectFS configuration."""
    global_config: GlobalConfig = field(default_factory=GlobalConfig)
    storage: StorageConfig = field(default_factory=StorageConfig)
    performance: PerformanceConfig = field(default_factory=PerformanceConfig)
    cluster: ClusterConfig = field(default_factory=ClusterConfig)
    security: SecurityConfig = field(default_factory=SecurityConfig)
    monitoring: MonitoringConfig = field(default_factory=MonitoringConfig)
    fuse: FUSEConfig = field(default_factory=FUSEConfig)

    @classmethod
    def from_file(cls, path: Union[str, Path]) -> 'Configuration':
        """Load configuration from YAML file."""
        path = Path(path)
        if not path.exists():
            raise ConfigurationError(f"Configuration file not found: {path}")

        try:
            with open(path, 'r') as f:
                data = yaml.safe_load(f)
            return cls.from_dict(data or {})
        except yaml.YAMLError as e:
            raise ConfigurationError(f"Invalid YAML in {path}: {e}") from e
        except Exception as e:
            raise ConfigurationError(f"Error loading config from {path}: {e}") from e

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> 'Configuration':
        """Create configuration from dictionary."""
        config = cls()

        if 'global' in data:
            config.global_config = GlobalConfig(**data['global'])

        if 'storage' in data:
            storage_data = data['storage']
            s3_config = S3Config(**(storage_data.get('s3', {})))
            config.storage = StorageConfig(s3=s3_config)

        if 'performance' in data:
            config.performance = PerformanceConfig(**data['performance'])

        if 'cluster' in data:
            config.cluster = ClusterConfig(**data['cluster'])

        if 'security' in data:
            config.security = SecurityConfig(**data['security'])

        if 'monitoring' in data:
            monitoring_data = data['monitoring']
            config.monitoring = MonitoringConfig(**monitoring_data)

        if 'fuse' in data:
            config.fuse = FUSEConfig(**data['fuse'])

        return config

    @classmethod
    def from_preset(cls, preset: str) -> 'Configuration':
        """Create configuration from preset."""
        presets = {
            'development': cls._development_preset(),
            'production': cls._production_preset(),
            'high-performance': cls._high_performance_preset(),
            'cost-optimized': cls._cost_optimized_preset(),
            'cluster': cls._cluster_preset(),
        }

        if preset not in presets:
            raise ConfigurationError(f"Unknown preset: {preset}")

        return presets[preset]

    @classmethod
    def from_env(cls, prefix: str = "OBJECTFS_") -> 'Configuration':
        """Create configuration from environment variables."""
        config = cls()

        # Map environment variables to configuration
        env_mappings = {
            f"{prefix}LOG_LEVEL": ("global_config", "log_level"),
            f"{prefix}CACHE_SIZE": ("performance", "cache_size"),
            f"{prefix}MAX_CONCURRENCY": ("performance", "max_concurrency"),
            f"{prefix}S3_REGION": ("storage.s3", "region"),
            f"{prefix}S3_ENDPOINT": ("storage.s3", "endpoint"),
            f"{prefix}CLUSTER_ENABLED": ("cluster", "enabled"),
            f"{prefix}CLUSTER_LISTEN_ADDR": ("cluster", "listen_addr"),
        }

        for env_var, (section, key) in env_mappings.items():
            value = os.getenv(env_var)
            if value is not None:
                # Navigate to nested config section and set value
                obj = config
                parts = section.split('.')
                for part in parts[:-1]:
                    obj = getattr(obj, part)

                # Convert string values to appropriate types
                if key in ['enabled']:
                    value = value.lower() in ('true', '1', 'yes')
                elif key in ['max_concurrency']:
                    value = int(value)

                setattr(getattr(obj, parts[-1]) if parts else obj, key, value)

        return config

    def merge(self, overrides: Dict[str, Any]) -> 'Configuration':
        """Create new configuration with overrides applied."""
        # Convert to dict, apply overrides, then convert back
        config_dict = self.to_dict()

        def deep_merge(base: Dict, overlay: Dict) -> Dict:
            result = base.copy()
            for key, value in overlay.items():
                if key in result and isinstance(result[key], dict) and isinstance(value, dict):
                    result[key] = deep_merge(result[key], value)
                else:
                    result[key] = value
            return result

        merged_dict = deep_merge(config_dict, overrides)
        return self.from_dict(merged_dict)

    def to_dict(self) -> Dict[str, Any]:
        """Convert configuration to dictionary."""
        return {
            'global': asdict(self.global_config),
            'storage': {
                's3': asdict(self.storage.s3)
            },
            'performance': asdict(self.performance),
            'cluster': asdict(self.cluster),
            'security': asdict(self.security),
            'monitoring': asdict(self.monitoring),
            'fuse': asdict(self.fuse)
        }

    def to_yaml(self) -> str:
        """Convert configuration to YAML string."""
        return yaml.dump(self.to_dict(), default_flow_style=False, sort_keys=False)

    def save_to_file(self, path: Union[str, Path]):
        """Save configuration to YAML file."""
        path = Path(path)
        path.parent.mkdir(parents=True, exist_ok=True)

        with open(path, 'w') as f:
            f.write(self.to_yaml())

    def validate(self):
        """Validate entire configuration."""
        self.storage.validate()
        self.performance.validate()
        self.cluster.validate()
        self.security.validate()

    # Preset factory methods

    @classmethod
    def _development_preset(cls) -> 'Configuration':
        """Development environment preset."""
        config = cls()
        config.global_config.log_level = "DEBUG"
        config.performance.cache_size = "1GB"
        config.performance.max_concurrency = 50
        return config

    @classmethod
    def _production_preset(cls) -> 'Configuration':
        """Production environment preset."""
        config = cls()
        config.global_config.log_level = "INFO"
        config.performance.cache_size = "8GB"
        config.performance.max_concurrency = 500
        config.performance.multilevel_caching = True
        config.storage.s3.use_acceleration = True
        config.monitoring.enabled = True
        return config

    @classmethod
    def _high_performance_preset(cls) -> 'Configuration':
        """High performance preset."""
        config = cls()
        config.global_config.log_level = "WARN"
        config.performance.cache_size = "16GB"
        config.performance.max_concurrency = 1000
        config.performance.predictive_caching = True
        config.performance.multilevel_caching = True
        config.storage.s3.use_acceleration = True
        return config

    @classmethod
    def _cost_optimized_preset(cls) -> 'Configuration':
        """Cost optimized preset.

        This preset used to set five cost-optimization keys, including a
        seven-day transition to STANDARD_IA and a thirty-day transition to
        GLACIER. ObjectFS has never performed either: automatic tier
        transitions exist in the S3 backend but nothing on the mount path
        invokes them, and lifecycle rules are a bucket-level configuration it
        does not write. The keys were removed from the schema in v0.11.0, so
        setting them now fails the mount.

        What remains is one real saving and it is opt-in for a reason: writing
        small objects to STANDARD rather than to a tier that would bill them at
        its 128 KB minimum. Pair it with ``storage_tier`` set to STANDARD_IA or
        ONEZONE_IA -- on STANDARD it has nothing to do.
        """
        config = cls()
        config.performance.cache_size = "2GB"
        config.performance.max_concurrency = 100
        config.storage.s3.cost_optimization.update({
            "small_objects_on_standard": True
        })
        return config

    @classmethod
    def _cluster_preset(cls) -> 'Configuration':
        """Distributed cluster preset."""
        config = cls()
        config.global_config.log_level = "INFO"
        config.performance.cache_size = "4GB"
        config.performance.max_concurrency = 200
        config.cluster.enabled = True
        config.cluster.replication_factor = 3
        config.monitoring.enabled = True
        config.security.enabled = True
        # No `tls_enabled = True` here, deliberately. SecurityConfig.validate() requires
        # tls_cert_path and tls_key_path whenever TLS is on, and a preset cannot know where a
        # deployment keeps its certificates -- so setting the flag made this the one preset that
        # could not pass its own validate(). Verified: from_preset('cluster').validate() raised
        # "TLS certificate and key paths required". TLS is the caller's to enable, with the paths:
        #
        #     Configuration.from_preset('cluster').merge({'security': {
        #         'tls_enabled': True,
        #         'tls_cert_path': '/etc/objectfs/tls.crt',
        #         'tls_key_path': '/etc/objectfs/tls.key',
        #     }})
        return config
