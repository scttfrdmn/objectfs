<template>
  <div class="api-playground">
    <div class="api-playground-header">
      <h3>API Playground</h3>
      <p>Try ObjectFS API endpoints interactively</p>
    </div>
    <div class="api-playground-content">
      <div class="api-playground-input">
        <div class="mb-4">
          <label class="block text-sm font-medium mb-2">Endpoint</label>
          <select v-model="selectedEndpoint" class="w-full p-2 border rounded">
            <option v-for="endpoint in endpoints" :key="endpoint.path" :value="endpoint">
              {{ endpoint.method }} {{ endpoint.path }}
            </option>
          </select>
        </div>

        <div class="mb-4" v-if="selectedEndpoint.params">
          <label class="block text-sm font-medium mb-2">Parameters</label>
          <div v-for="param in selectedEndpoint.params" :key="param.name" class="mb-2">
            <label class="block text-xs text-gray-600 mb-1">{{ param.name }} ({{ param.type }})</label>
            <input
              v-model="requestParams[param.name]"
              :type="param.type === 'number' ? 'number' : 'text'"
              class="w-full p-2 border rounded text-sm"
              :placeholder="param.description"
            >
          </div>
        </div>

        <div class="mb-4" v-if="selectedEndpoint.body">
          <label class="block text-sm font-medium mb-2">Request Body</label>
          <textarea
            v-model="requestBody"
            class="w-full p-2 border rounded font-mono text-sm"
            rows="6"
            :placeholder="selectedEndpoint.bodyExample"
          ></textarea>
        </div>

        <div class="mb-4">
          <label class="block text-sm font-medium mb-2">Headers</label>
          <textarea
            v-model="requestHeaders"
            class="w-full p-2 border rounded font-mono text-sm"
            rows="3"
            placeholder="Content-Type: application/json"
          ></textarea>
        </div>

        <button
          @click="executeRequest"
          :disabled="loading"
          class="w-full bg-blue-600 text-white p-2 rounded hover:bg-blue-700 disabled:opacity-50"
        >
          {{ loading ? 'Sending...' : 'Send Request' }}
        </button>
      </div>

      <div class="api-playground-output">
        <div class="mb-4">
          <h4 class="text-sm font-medium mb-2">Response</h4>
          <div class="bg-gray-100 p-2 rounded text-sm">
            Status: {{ response.status || 'Not sent' }}
          </div>
        </div>

        <div class="mb-4" v-if="response.headers">
          <h4 class="text-sm font-medium mb-2">Headers</h4>
          <pre class="bg-gray-100 p-2 rounded text-xs overflow-auto">{{ formatHeaders(response.headers) }}</pre>
        </div>

        <div v-if="response.body">
          <h4 class="text-sm font-medium mb-2">Body</h4>
          <pre class="bg-gray-100 p-2 rounded text-xs overflow-auto">{{ formatJson(response.body) }}</pre>
        </div>

        <div v-if="response.error" class="text-red-600 text-sm">
          Error: {{ response.error }}
        </div>

        <div v-if="!response.status && !response.error" class="text-gray-500 text-sm">
          No response yet. Try sending a request.
        </div>
      </div>
    </div>
  </div>
</template>

<script>
export default {
  name: 'ApiPlayground',
  data() {
    return {
      loading: false,
      selectedEndpoint: {
        method: 'GET',
        path: '/api/v1/health',
        description: 'Get health status',
        params: [],
        body: false,
        bodyExample: ''
      },
      requestParams: {},
      requestBody: '',
      requestHeaders: 'Content-Type: application/json',
      response: {},
      endpoints: [
        {
          method: 'GET',
          path: '/api/v1/health',
          description: 'Get health status',
          params: [],
          body: false
        },
        {
          method: 'GET',
          path: '/api/v1/metrics',
          description: 'Get metrics',
          params: [],
          body: false
        },
        {
          method: 'POST',
          path: '/api/v1/mount',
          description: 'Mount filesystem',
          params: [],
          body: true,
          bodyExample: JSON.stringify({
            storage_uri: 's3://my-bucket',
            mount_point: '/mnt/objectfs',
            config: {
              performance: {
                cache_size: '8GB'
              }
            }
          }, null, 2)
        },
        {
          method: 'DELETE',
          path: '/api/v1/mount/{mount_point}',
          description: 'Unmount filesystem',
          params: [
            { name: 'mount_point', type: 'string', description: 'Path to unmount' }
          ],
          body: false
        },
        {
          method: 'GET',
          path: '/api/v1/mounts',
          description: 'List active mounts',
          params: [],
          body: false
        },
        {
          method: 'GET',
          path: '/api/v1/storage/objects',
          description: 'List storage objects',
          params: [
            { name: 'storage_uri', type: 'string', description: 'Storage URI (e.g., s3://bucket)' },
            { name: 'prefix', type: 'string', description: 'Object prefix filter' },
            { name: 'max_keys', type: 'number', description: 'Maximum objects to return' }
          ],
          body: false
        }
      ]
    }
  },
  methods: {
    async executeRequest() {
      this.loading = true
      this.response = {}

      try {
        // Build URL with parameters
        let url = this.selectedEndpoint.path
        for (const [key, value] of Object.entries(this.requestParams)) {
          if (value) {
            url = url.replace(`{${key}}`, encodeURIComponent(value))
          }
        }

        // Add query parameters for GET requests
        if (this.selectedEndpoint.method === 'GET' && Object.keys(this.requestParams).length > 0) {
          const params = new URLSearchParams()
          for (const [key, value] of Object.entries(this.requestParams)) {
            if (value) params.set(key, value)
          }
          if (params.toString()) {
            url += '?' + params.toString()
          }
        }

        // Parse headers
        const headers = {}
        if (this.requestHeaders) {
          this.requestHeaders.split('\n').forEach(line => {
            const [key, ...valueParts] = line.split(':')
            if (key && valueParts.length > 0) {
              headers[key.trim()] = valueParts.join(':').trim()
            }
          })
        }

        // Prepare request options
        const options = {
          method: this.selectedEndpoint.method,
          headers
        }

        if (this.selectedEndpoint.body && this.requestBody) {
          options.body = this.requestBody
        }

        // Make request to our API server
        const response = await fetch(`/api-playground${url}`, options)

        const responseHeaders = {}
        for (const [key, value] of response.headers.entries()) {
          responseHeaders[key] = value
        }

        const responseText = await response.text()
        let responseBody = responseText
        try {
          responseBody = JSON.parse(responseText)
        } catch (e) {
          // Keep as text if not JSON
        }

        this.response = {
          status: response.status,
          statusText: response.statusText,
          headers: responseHeaders,
          body: responseBody
        }
      } catch (error) {
        this.response = {
          error: error.message
        }
      } finally {
        this.loading = false
      }
    },

    formatJson(data) {
      if (typeof data === 'object') {
        return JSON.stringify(data, null, 2)
      }
      return data
    },

    formatHeaders(headers) {
      return Object.entries(headers)
        .map(([key, value]) => `${key}: ${value}`)
        .join('\n')
    }
  },

  watch: {
    selectedEndpoint: {
      handler(newEndpoint) {
        // Reset params when endpoint changes
        this.requestParams = {}
        this.requestBody = newEndpoint.bodyExample || ''
      },
      deep: true
    }
  }
}
</script>

<style scoped>
.api-playground {
  @apply border border-gray-200 rounded-lg overflow-hidden my-4;
}

.api-playground-header {
  @apply bg-gray-50 p-4 border-b border-gray-200;
}

.api-playground-header h3 {
  @apply text-lg font-semibold mb-1;
}

.api-playground-header p {
  @apply text-sm text-gray-600 mb-0;
}

.api-playground-content {
  @apply flex h-96;
}

.api-playground-input {
  @apply flex-1 p-4 border-r border-gray-200 overflow-y-auto;
}

.api-playground-output {
  @apply flex-1 p-4 bg-gray-50 overflow-y-auto;
}

@media (max-width: 768px) {
  .api-playground-content {
    @apply flex-col h-auto;
  }

  .api-playground-input {
    @apply border-r-0 border-b border-gray-200;
  }
}
</style>
