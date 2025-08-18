---
layout: home

hero:
  name: ObjectFS
  text: High-Performance POSIX Filesystem for AWS S3
  tagline: Mount AWS S3 as a local filesystem with enterprise-grade performance, caching, and reliability optimized for S3.
  image:
    src: /logo.svg
    alt: ObjectFS
  actions:
    - theme: brand
      text: Get Started
      link: /guide/getting-started
    - theme: alt
      text: View on GitHub
      link: https://github.com/objectfs/objectfs
    - theme: alt
      text: Try the Playground
      link: /playground/

features:
  - icon: ⚡
    title: High Performance
    details: Advanced caching with ML-based predictive prefetching, multi-level cache hierarchies, and intelligent eviction strategies.

  - icon: 🔧
    title: Easy Integration
    details: Drop-in replacement for traditional filesystems. Works with any existing application without code changes.

  - icon: 🌐
    title: S3 Optimization
    details: Deep integration with AWS S3 including Intelligent Tiering, storage classes, and automatic cost optimization.

  - icon: 🏗️
    title: Enterprise Ready
    details: Distributed clusters, high availability, monitoring, authentication, and compliance frameworks built-in.

  - icon: 📊
    title: Observable
    details: Comprehensive metrics, health monitoring, distributed tracing, and real-time performance dashboards.

  - icon: 🛡️
    title: Secure & Compliant
    details: End-to-end encryption, RBAC, audit logging, and compliance frameworks for GDPR, HIPAA, and SOC2.
---

## Quick Start

Get ObjectFS running in minutes:

<CodeRunner language="bash">

```bash
# Install ObjectFS
curl -sSL https://get.objectfs.io | sh

# Mount your first filesystem
objectfs mount s3://my-bucket /mnt/data

# Use it like any local directory
ls /mnt/data
echo "Hello ObjectFS!" > /mnt/data/test.txt
cat /mnt/data/test.txt
```

</CodeRunner>

## Interactive Examples

### Configuration Builder

<ConfigurationBuilder />

### API Playground

<ApiPlayground />

## Performance Comparison

<PerformanceChart
  :data="performanceData"
  title="ObjectFS vs Traditional Storage"
/>

## Use Cases

<div class="use-cases">
  <div class="use-case">
    <h3>🔬 Data Science & ML</h3>
    <p>Access massive datasets stored in object storage as if they were local files.
    Perfect for training ML models, data analysis, and research workflows.</p>
    <a href="/tutorials/ml-models">Learn more →</a>
  </div>

  <div class="use-case">
    <h3>📦 Container Storage</h3>
    <p>Provide persistent, scalable storage for containerized applications.
    Seamlessly integrate with Kubernetes and container orchestration platforms.</p>
    <a href="/tutorials/containers">Learn more →</a>
  </div>

  <div class="use-case">
    <h3>🎬 Media & Content</h3>
    <p>Stream and process large media files directly from object storage.
    Ideal for video processing, content delivery, and digital asset management.</p>
    <a href="/tutorials/media">Learn more →</a>
  </div>

  <div class="use-case">
    <h3>💾 Backup & Archive</h3>
    <p>Cost-effective long-term storage with instant access. Automated tiering,
    compression, and lifecycle management for enterprise backup solutions.</p>
    <a href="/tutorials/backup">Learn more →</a>
  </div>
</div>

## SDKs & Integrations

ObjectFS provides native SDKs for popular programming languages:

<div class="sdk-grid">
  <div class="sdk-card">
    <h3>🐍 Python</h3>
    <p>Full async/await support with comprehensive configuration management.</p>
    <code>pip install objectfs</code>
    <a href="/sdks/python">Documentation →</a>
  </div>

  <div class="sdk-card">
    <h3>🟨 JavaScript/TypeScript</h3>
    <p>Event-driven architecture with complete TypeScript definitions.</p>
    <code>npm install @objectfs/sdk</code>
    <a href="/sdks/javascript">Documentation →</a>
  </div>

  <div class="sdk-card">
    <h3>☕ Java</h3>
    <p>Enterprise-ready with CompletableFuture async API.</p>
    <code>io.objectfs:objectfs-java-sdk</code>
    <a href="/sdks/java">Documentation →</a>
  </div>
</div>

## Community & Support

<div class="community-links">
  <a href="https://github.com/objectfs/objectfs" class="community-link">
    <h4>📚 GitHub</h4>
    <p>Source code, issues, and contributions</p>
  </a>

  <a href="https://community.objectfs.io" class="community-link">
    <h4>💬 Community Forum</h4>
    <p>Ask questions and share experiences</p>
  </a>

  <a href="https://docs.objectfs.io/api" class="community-link">
    <h4>🔧 API Reference</h4>
    <p>Complete API documentation</p>
  </a>
</div>

<style>
.use-cases {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(280px, 1fr));
  gap: 24px;
  margin: 32px 0;
}

.use-case {
  padding: 24px;
  border: 1px solid var(--vp-c-border);
  border-radius: 12px;
  background: var(--vp-c-bg-soft);
  transition: transform 0.2s, box-shadow 0.2s;
}

.use-case:hover {
  transform: translateY(-2px);
  box-shadow: 0 8px 24px rgba(0, 0, 0, 0.1);
}

.use-case h3 {
  margin: 0 0 12px 0;
  color: var(--vp-c-text-1);
}

.use-case p {
  margin: 0 0 16px 0;
  color: var(--vp-c-text-2);
  line-height: 1.6;
}

.use-case a {
  color: var(--vp-c-brand-1);
  text-decoration: none;
  font-weight: 500;
}

.sdk-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(250px, 1fr));
  gap: 20px;
  margin: 32px 0;
}

.sdk-card {
  padding: 20px;
  border: 1px solid var(--vp-c-border);
  border-radius: 8px;
  background: var(--vp-c-bg-soft);
}

.sdk-card h3 {
  margin: 0 0 12px 0;
}

.sdk-card p {
  margin: 0 0 12px 0;
  color: var(--vp-c-text-2);
  font-size: 14px;
}

.sdk-card code {
  display: block;
  background: var(--vp-c-bg-alt);
  padding: 8px 12px;
  border-radius: 4px;
  font-size: 12px;
  margin: 8px 0;
}

.sdk-card a {
  color: var(--vp-c-brand-1);
  text-decoration: none;
  font-size: 14px;
}

.community-links {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 16px;
  margin: 32px 0;
}

.community-link {
  display: block;
  padding: 20px;
  border: 1px solid var(--vp-c-border);
  border-radius: 8px;
  text-decoration: none;
  color: inherit;
  transition: transform 0.2s, box-shadow 0.2s;
}

.community-link:hover {
  transform: translateY(-2px);
  box-shadow: 0 4px 12px rgba(0, 0, 0, 0.1);
}

.community-link h4 {
  margin: 0 0 8px 0;
  color: var(--vp-c-text-1);
}

.community-link p {
  margin: 0;
  color: var(--vp-c-text-2);
  font-size: 14px;
}
</style>

<script setup>
import { ref } from 'vue'

const performanceData = ref({
  categories: ['Sequential Read', 'Sequential Write', 'Random Read', 'Random Write', 'Metadata Ops'],
  series: [
    {
      name: 'Traditional NFS',
      data: [100, 100, 100, 100, 100]
    },
    {
      name: 'ObjectFS (Cold)',
      data: [80, 85, 60, 70, 150]
    },
    {
      name: 'ObjectFS (Warm Cache)',
      data: [300, 250, 400, 300, 500]
    }
  ]
})
</script>
