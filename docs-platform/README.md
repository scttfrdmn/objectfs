# ObjectFS Documentation Platform

A comprehensive, interactive documentation platform for ObjectFS featuring API exploration,
code examples, tutorials, and real-time testing capabilities.

## Features

- 📚 **Interactive Documentation** - VitePress-powered docs with custom components
- 🧪 **API Playground** - Test ObjectFS API endpoints directly in the browser
- ⚡ **Code Runner** - Execute code examples in sandboxed containers
- 🎓 **Interactive Tutorials** - Step-by-step guides with real-time feedback
- 📊 **Performance Monitoring** - Live metrics and performance dashboards
- 🔧 **Configuration Builder** - Visual configuration generator
- 🌐 **Multi-Language SDKs** - Complete documentation for Python, JavaScript, and Java SDKs

## Quick Start

### Development

```bash
# Install dependencies
npm install

# Start documentation server
npm run serve:docs

# Start API server (in another terminal)
npm run serve:api

# Open browser to http://localhost:5173
```

### Production

```bash
# Build documentation
npm run build

# Start production servers
npm start
```

## Architecture

```
docs-platform/
├── .vitepress/                 # VitePress configuration
│   ├── config.js              # Main config
│   ├── theme/                 # Custom theme
│   └── components/            # Vue components
├── src/                       # Backend services
│   └── api-server.js         # API proxy and code execution
├── guide/                     # Documentation content
├── api/                      # API reference
├── tutorials/                # Interactive tutorials
├── playground/               # Code playground
└── examples/                 # Code examples
```

## Components

### Interactive Components

- **ApiPlayground** - Test API endpoints with live requests
- **CodeRunner** - Execute code examples in sandboxed environments
- **ConfigurationBuilder** - Visual configuration generator
- **PerformanceChart** - Real-time performance visualization
- **InteractiveExample** - Step-by-step tutorial components

### Backend Services

- **API Proxy** - Forwards requests to ObjectFS API
- **Code Execution** - Sandboxed code execution using Docker
- **Real-time Updates** - WebSocket-based live features

## Configuration

### Environment Variables

```bash
# API Server Configuration
API_PORT=3001
NODE_ENV=development
OBJECTFS_API_BASE=http://localhost:8081

# Docker Configuration (for code execution)
DOCKER_HOST=unix:///var/run/docker.sock
ENABLE_CODE_EXECUTION=true

# Security
CORS_ORIGIN=*
RATE_LIMIT_WINDOW_MS=900000
RATE_LIMIT_MAX=100
```

### Docker Setup

For code execution features, ensure Docker is available:

```bash
# Pull required images
docker pull alpine:latest
docker pull python:3.11-alpine
docker pull node:18-alpine
docker pull golang:1.21-alpine

# Test code execution
curl -X POST http://localhost:3001/api/code-runner/execute \
  -H "Content-Type: application/json" \
  -d '{"language": "python", "code": "print(\"Hello from sandbox!\")"}'
```

## Deployment

### Docker Deployment

```bash
# Build documentation image
docker build -t objectfs-docs .

# Run with docker-compose
docker-compose up -d
```

### Kubernetes Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: objectfs-docs
spec:
  replicas: 2
  selector:
    matchLabels:
      app: objectfs-docs
  template:
    metadata:
      labels:
        app: objectfs-docs
    spec:
      containers:
      - name: docs
        image: objectfs-docs:latest
        ports:
        - containerPort: 3001
        env:
        - name: NODE_ENV
          value: production
        - name: OBJECTFS_API_BASE
          value: http://objectfs-api:8081
```

## Security

### Code Execution Sandbox

Code execution uses Docker containers with security restrictions:

- **Resource limits**: 128MB memory, 50% CPU quota
- **Network isolation**: No network access
- **Read-only root filesystem**
- **Temporary storage**: Limited tmpfs volumes
- **Execution timeout**: 30 seconds default

### API Security

- **CORS protection** with configurable origins
- **Rate limiting** to prevent abuse
- **Input validation** for all endpoints
- **Request size limits**
- **Helmet.js** security headers

## Monitoring

### Health Checks

```bash
# API health
curl http://localhost:3001/api/health

# Documentation build status
curl http://localhost:5173/health
```

### Metrics

The platform exposes metrics compatible with Prometheus:

```bash
# API metrics
curl http://localhost:3001/metrics

# Code execution metrics
curl http://localhost:3001/api/code-runner/metrics
```

## Contributing

### Adding Documentation

1. Create markdown files in appropriate directories
2. Update `.vitepress/config.js` navigation
3. Use custom components for interactive features

### Adding Interactive Examples

```markdown
<CodeRunner language="python" :executable="true">
```python
# Your executable Python code here
print("This runs in a sandbox!")
```

</CodeRunner>

<ApiPlayground />
```

### Adding Custom Components

1. Create Vue components in `.vitepress/components/`
2. Register in `.vitepress/theme/index.js`
3. Use in markdown files

## API Reference

### Code Execution API

```javascript
// Execute code
POST /api/code-runner/execute
{
  "language": "python|javascript|bash|go",
  "code": "print('Hello World')",
  "timeout": 30
}

// Response
{
  "success": true,
  "output": "Hello World\n",
  "error": null
}
```

### Tutorial Progress API

```javascript
// Save progress
POST /api/tutorials/progress
{
  "tutorialId": "getting-started",
  "stepId": "first-mount",
  "completed": true,
  "data": {}
}
```

### Examples API

```javascript
// Get examples
GET /api/examples/python
{
  "category": "python",
  "examples": [...]
}
```

## Development

### Local Development

```bash
# Install dependencies
npm install

# Start all services
npm run dev

# Run tests
npm test

# Lint and format
npm run lint
npm run format
```

### Adding New Languages

To add support for new programming languages in CodeRunner:

1. Add Docker image configuration in `src/api-server.js`
2. Update language detection in frontend
3. Add syntax highlighting support
4. Test code execution sandbox

### Debugging

```bash
# Enable debug logging
DEBUG=objectfs:* npm run dev

# View container logs
docker logs $(docker ps -q --filter ancestor=python:3.11-alpine)

# Test API endpoints
curl -X POST http://localhost:3001/api/code-runner/execute \
  -H "Content-Type: application/json" \
  -d '{"language": "python", "code": "import sys; print(sys.version)"}'
```

## Performance

### Optimization Tips

1. **Static Generation**: Pre-build documentation for faster loading
2. **CDN Deployment**: Serve static assets from CDN
3. **Code Splitting**: Lazy load interactive components
4. **Caching**: Cache API responses and code execution results
5. **Container Optimization**: Pre-pull Docker images for faster execution

### Monitoring Performance

```bash
# Monitor API response times
curl -w "@curl-format.txt" http://localhost:3001/api/health

# Monitor memory usage
docker stats
```

## Troubleshooting

### Common Issues

**Documentation not building:**

```bash
# Clear cache and rebuild
rm -rf .vitepress/cache
npm run build:docs
```

**Code execution failing:**

```bash
# Check Docker daemon
docker info

# Test container creation
docker run --rm alpine:latest echo "test"
```

**API proxy errors:**

```bash
# Verify ObjectFS is running
curl http://localhost:8081/health

# Check network connectivity
netstat -tlnp | grep 8081
```

## License

MIT License - see [LICENSE](../LICENSE) for details.
