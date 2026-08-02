# ObjectFS Playground

Welcome to the ObjectFS Playground! This interactive environment lets you explore ObjectFS
S3 integration, test API endpoints, and experiment with configurations without needing a local
installation.

> **The shell commands on this page were written against a CLI ObjectFS does not have.** The binary
> takes exactly two positional arguments — bucket URI and mount point — and has **no subcommands**,
> so `objectfs mount`, `objectfs unmount`, `objectfs list-mounts`, `objectfs status`, and
> `objectfs metrics --watch` all exit with an argument error. `list-mounts` and `status` are not
> planned anywhere; `mount`, `unmount`, and `version` are proposed in
> [#134](https://github.com/scttfrdmn/objectfs/issues/134). The commands below are corrected to what
> `objectfs --help` accepts.
>
> The Python, JavaScript, and Go blocks are unchanged and are **not** corrected. Marking each
> divergence individually would be noise when the containing page cannot render: the Go example
> imports `pkg/client`, which does not exist, and this whole documentation tree fails to build for
> four independent reasons recorded in
> [#214](https://github.com/scttfrdmn/objectfs/issues/214). Whether it is repaired or deleted is that
> issue's decision. The shell commands are fixed here because they are read on GitHub whether or not
> the site builds, and a wrong command is something a reader will type.

## Interactive API Explorer

Try ObjectFS API endpoints directly in your browser:

<ApiPlayground />

## Code Examples

### Basic Mount Operations

<CodeRunner language="bash" :executable="true">

```bash
# Mount an S3 bucket — two positional arguments, no subcommand
objectfs s3://demo-bucket /mnt/demo

# List mounted filesystems: the platform's own tool, not an objectfs command
mount | grep objectfs

# Check mount status: the health endpoint the mount process serves
curl http://localhost:8081/health
```

</CodeRunner>

### Python SDK Example

<CodeRunner language="python" :executable="true">

```python
import asyncio
from objectfs import ObjectFSClient, Configuration

async def demo():
    # Create client with configuration
    config = Configuration.from_preset('high-performance')
    client = ObjectFSClient(config=config)

    # Mount filesystem
    mount_id = await client.mount(
        's3://demo-bucket',
        '/mnt/demo',
        config_overrides={
            'performance': {'cache_size': '4GB'}
        }
    )
    print(f"Mounted: {mount_id}")

    # Get health status
    health = await client.get_health()
    print(f"Health: {health['status']}")

    # List objects
    objects = await client.list_objects('s3://demo-bucket', max_keys=10)
    print(f"Objects: {len(objects['objects'])}")

    # Cleanup
    await client.unmount('/mnt/demo')
    await client.close()

# Run the demo
asyncio.run(demo())
```

</CodeRunner>

### JavaScript SDK Example

<CodeRunner language="javascript" :executable="true">

```javascript
const { ObjectFSClient, Configuration } = require('@objectfs/sdk');

async function demo() {
  // Create client
  const client = new ObjectFSClient({
    config: Configuration.fromPreset('production')
  });

  // Mount filesystem
  const mountId = await client.mount('s3://demo-bucket', '/mnt/demo', {
    foreground: false,
    configOverrides: {
      performance: {
        cacheSize: '4GB',
        maxConcurrency: 200
      }
    }
  });

  console.log(`Mounted: ${mountId}`);

  // Set up event listeners
  client.on('health_change', (health) => {
    console.log(`Health status: ${health.status}`);
  });

  // Start monitoring
  await client.startMonitoring(5000);

  // Storage operations
  const objects = await client.listObjects('s3://demo-bucket', {
    prefix: 'data/',
    maxKeys: 10
  });

  console.log(`Found ${objects.objects.length} objects`);

  // Cleanup
  await client.unmount('/mnt/demo');
  await client.close();
}

demo().catch(console.error);
```

</CodeRunner>

### Go API Example

<CodeRunner language="go" :executable="true">

```go
package main

import (
    "fmt"
    "context"
    "time"

    "github.com/scttfrdmn/objectfs/pkg/client"
    "github.com/scttfrdmn/objectfs/internal/config"
)

func main() {
    // Create configuration
    cfg := &config.Config{
        Performance: config.PerformanceConfig{
            CacheSize:      "4GB",
            MaxConcurrency: 200,
        },
        Storage: config.StorageConfig{
            S3: config.S3Config{
                Region: "us-east-1",
            },
        },
    }

    // Create client
    client, err := client.New(cfg)
    if err != nil {
        panic(err)
    }
    defer client.Close()

    ctx := context.Background()

    // Mount filesystem
    mountID, err := client.Mount(ctx, "s3://demo-bucket", "/mnt/demo", nil)
    if err != nil {
        panic(err)
    }
    fmt.Printf("Mounted: %s\n", mountID)

    // Get health
    health, err := client.GetHealth(ctx)
    if err != nil {
        panic(err)
    }
    fmt.Printf("Health: %s\n", health.Status)

    // List objects
    objects, err := client.ListObjects(ctx, "s3://demo-bucket", &client.ListObjectsOptions{
        Prefix:  "data/",
        MaxKeys: 10,
    })
    if err != nil {
        panic(err)
    }
    fmt.Printf("Objects: %d\n", len(objects.Objects))

    // Wait a bit
    time.Sleep(2 * time.Second)

    // Unmount
    err = client.Unmount(ctx, "/mnt/demo", nil)
    if err != nil {
        panic(err)
    }
    fmt.Println("Unmounted successfully")
}
```

</CodeRunner>

## Configuration Builder

Build and test different ObjectFS configurations:

<ConfigurationBuilder />

## Performance Testing

### Benchmark Different Cache Sizes

<CodeRunner language="bash" :executable="true">

```bash
#!/bin/bash

echo "=== ObjectFS Performance Benchmark ==="
echo

# Test different cache sizes
for cache_size in "1GB" "4GB" "8GB" "16GB"; do
    echo "Testing cache size: $cache_size"

    # Mount with a specific cache size, in the background
    objectfs --cache-size "$cache_size" --log-level WARN \
        s3://benchmark-bucket /mnt/test &
    objectfs_pid=$!
    sleep 2

    # Run benchmark
    time dd if=/dev/zero of=/mnt/test/testfile bs=1M count=100 2>/dev/null
    sync
    time dd if=/mnt/test/testfile of=/dev/null bs=1M 2>/dev/null

    # Cleanup. rm returns EROFS on a mount today, so remove the object at the
    # source; unmount by signalling the process, which flushes on the way out.
    aws s3 rm s3://benchmark-bucket/testfile
    kill -TERM "$objectfs_pid"
    wait "$objectfs_pid" 2>/dev/null

    echo "---"
done

echo "Benchmark complete!"
```

</CodeRunner>

### Cache Hit Rate Analysis

<CodeRunner language="python" :executable="true">

```python
import asyncio
import json
import time
from objectfs import ObjectFSClient

async def analyze_cache_performance():
    client = ObjectFSClient()

    # Mount with monitoring
    await client.mount('s3://demo-bucket', '/mnt/demo')

    print("Cache Performance Analysis")
    print("=" * 40)

    for i in range(10):
        # Get current metrics
        metrics = await client.get_metrics()
        cache_metrics = metrics.get('cache', {})

        hit_rate = cache_metrics.get('hit_rate', 0) * 100
        total_requests = cache_metrics.get('total_requests', 0)
        cache_size = cache_metrics.get('current_size_mb', 0)

        print(f"Iteration {i+1:2d}: "
              f"Hit Rate: {hit_rate:5.1f}% | "
              f"Requests: {total_requests:6d} | "
              f"Cache: {cache_size:5.1f}MB")

        # Simulate some file operations to generate cache activity
        if i < 5:
            # This would trigger cache misses initially
            pass

        await asyncio.sleep(2)

    await client.unmount('/mnt/demo')
    await client.close()

# Run analysis
asyncio.run(analyze_cache_performance())
```

</CodeRunner>

## Interactive Tutorials

### Tutorial 1: Basic Operations

<InteractiveExample>
<div slot="description">
Learn the fundamentals of ObjectFS by following this step-by-step tutorial.
</div>

1. **Mount a filesystem**

   ```bash
   objectfs s3://tutorial-bucket /mnt/tutorial &
   ```

2. **Create and write to a file**

   ```bash
   echo "Hello ObjectFS!" > /mnt/tutorial/hello.txt
   ```

3. **Read the file**

   ```bash
   cat /mnt/tutorial/hello.txt
   ```

4. **List files**

   ```bash
   ls -la /mnt/tutorial/
   ```

5. **Unmount** — the platform's own tool, or signal the mount process

   ```bash
   fusermount -u /mnt/tutorial   # Linux; on macOS, umount /mnt/tutorial
   ```

</InteractiveExample>

### Tutorial 2: Performance Optimization

<InteractiveExample>
<div slot="description">
Optimize ObjectFS for your specific workload patterns.
</div>

1. **Identify your workload pattern**
   - Sequential reads (streaming)
   - Random reads (database-like)
   - Write-heavy (logs, backups)
   - Mixed workload

2. **Configure cache appropriately**

   ```yaml
   performance:
     cache_size: 8GB
     multilevel_caching: true
   ```

   `predictive_caching: true` was a third key here. It is a valid schema key that changes nothing:
   the predictor it selects is never installed on the mount path.

3. **Monitor and adjust**

   ```bash
   watch -n2 'curl -s http://localhost:9090/metrics | grep objectfs_cache'
   ```

</InteractiveExample>

## Real-time Metrics Dashboard

<div id="metrics-dashboard" class="metrics-dashboard">
  <h3>Live ObjectFS Metrics</h3>
  <p>Connect to a running ObjectFS instance to see real-time metrics.</p>

  <div class="metrics-grid">
    <div class="metric-card">
      <h4>Cache Hit Rate</h4>
      <div class="metric-value">---%</div>
    </div>

    <div class="metric-card">
      <h4>Throughput</h4>
      <div class="metric-value">--- MB/s</div>
    </div>

    <div class="metric-card">
      <h4>Active Mounts</h4>
      <div class="metric-value">---</div>
    </div>

    <div class="metric-card">
      <h4>Cache Size</h4>
      <div class="metric-value">--- MB</div>
    </div>
  </div>
</div>

## Debugging Tools

### Log Analysis

<CodeRunner language="bash" :executable="true">

```bash
# View ObjectFS logs in real-time
tail -f /var/log/objectfs.log | grep -E "(ERROR|WARN|mount|unmount)"

# Analyze performance logs
grep "cache_hit" /var/log/objectfs.log | tail -10

# Check for errors
grep "ERROR" /var/log/objectfs.log | head -20
```

</CodeRunner>

### Network Tracing

<CodeRunner language="bash" :executable="true">

```bash
# Trace network calls to object storage
# This shows the actual HTTP requests ObjectFS makes

strace -e trace=network objectfs s3://bucket /mnt/test 2>&1 | \
    grep -E "(connect|send|recv)"
```

</CodeRunner>

## Community Examples

Browse examples contributed by the ObjectFS community:

<div class="example-gallery">
  <div class="example-card">
    <h4>🔬 Machine Learning Pipeline</h4>
    <p>Process training data directly from S3</p>
    <a href="#ml-example">View Example →</a>
  </div>

  <div class="example-card">
    <h4>🎬 Video Processing</h4>
    <p>Transcode videos stored in object storage</p>
    <a href="#video-example">View Example →</a>
  </div>

  <div class="example-card">
    <h4>📊 Data Analytics</h4>
    <p>Query large datasets with familiar tools</p>
    <a href="#analytics-example">View Example →</a>
  </div>

  <div class="example-card">
    <h4>🐳 Container Integration</h4>
    <p>Use ObjectFS in Kubernetes pods</p>
    <a href="#k8s-example">View Example →</a>
  </div>
</div>

<style>
.metrics-dashboard {
  border: 1px solid var(--vp-c-border);
  border-radius: 8px;
  padding: 20px;
  margin: 20px 0;
  background: var(--vp-c-bg-soft);
}

.metrics-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 16px;
  margin-top: 16px;
}

.metric-card {
  padding: 16px;
  background: var(--vp-c-bg);
  border: 1px solid var(--vp-c-border);
  border-radius: 6px;
  text-align: center;
}

.metric-card h4 {
  margin: 0 0 8px 0;
  font-size: 14px;
  color: var(--vp-c-text-2);
}

.metric-value {
  font-size: 24px;
  font-weight: bold;
  color: var(--vp-c-brand-1);
}

.example-gallery {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
  gap: 16px;
  margin: 20px 0;
}

.example-card {
  padding: 20px;
  border: 1px solid var(--vp-c-border);
  border-radius: 8px;
  background: var(--vp-c-bg-soft);
}

.example-card h4 {
  margin: 0 0 8px 0;
}

.example-card p {
  margin: 0 0 12px 0;
  color: var(--vp-c-text-2);
  font-size: 14px;
}

.example-card a {
  color: var(--vp-c-brand-1);
  text-decoration: none;
  font-size: 14px;
}
</style>

<script setup>
import { onMounted, ref } from 'vue'

// Simulated metrics data
const metrics = ref({
  cacheHitRate: 0,
  throughput: 0,
  activeMounts: 0,
  cacheSize: 0
})

onMounted(() => {
  // Simulate real-time metrics updates
  setInterval(() => {
    metrics.value = {
      cacheHitRate: Math.floor(Math.random() *100),
      throughput: Math.floor(Math.random()* 1000),
      activeMounts: Math.floor(Math.random() *5) + 1,
      cacheSize: Math.floor(Math.random()* 8192)
    }
  }, 2000)
})
</script>
