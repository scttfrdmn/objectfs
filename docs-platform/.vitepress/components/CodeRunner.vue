<template>
  <div class="code-runner">
    <div class="code-runner-controls">
      <button
        @click="runCode"
        :disabled="loading"
        class="code-runner-btn"
      >
        {{ loading ? 'Running...' : 'Run' }}
      </button>
      <button
        @click="copyCode"
        class="code-runner-btn"
      >
        Copy
      </button>
    </div>

    <div class="code-block">
      <slot></slot>
    </div>

    <div v-if="output" class="code-runner-output">
      <div class="text-xs text-gray-500 mb-2">Output:</div>
      <pre>{{ output }}</pre>
    </div>

    <div v-if="error" class="code-runner-output text-red-600">
      <div class="text-xs text-red-400 mb-2">Error:</div>
      <pre>{{ error }}</pre>
    </div>
  </div>
</template>

<script>
export default {
  name: 'CodeRunner',
  props: {
    language: {
      type: String,
      default: 'bash'
    },
    executable: {
      type: Boolean,
      default: true
    }
  },
  data() {
    return {
      loading: false,
      output: '',
      error: ''
    }
  },
  methods: {
    async runCode() {
      if (!this.executable) return

      this.loading = true
      this.output = ''
      this.error = ''

      try {
        // Extract code from the slot
        const codeElement = this.$el.querySelector('pre code')
        const code = codeElement ? codeElement.textContent : ''

        if (!code.trim()) {
          this.error = 'No code to execute'
          return
        }

        // Send code to execution server
        const response = await fetch('/api/code-runner/execute', {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json'
          },
          body: JSON.stringify({
            language: this.language,
            code: code.trim()
          })
        })

        const result = await response.json()

        if (result.success) {
          this.output = result.output
        } else {
          this.error = result.error || 'Execution failed'
        }
      } catch (err) {
        this.error = `Failed to execute: ${err.message}`
      } finally {
        this.loading = false
      }
    },

    async copyCode() {
      try {
        const codeElement = this.$el.querySelector('pre code')
        const code = codeElement ? codeElement.textContent : ''

        if (navigator.clipboard) {
          await navigator.clipboard.writeText(code)
        } else {
          // Fallback for older browsers
          const textarea = document.createElement('textarea')
          textarea.value = code
          document.body.appendChild(textarea)
          textarea.select()
          document.execCommand('copy')
          document.body.removeChild(textarea)
        }

        // Visual feedback
        const btn = this.$el.querySelector('.code-runner-btn:last-child')
        const originalText = btn.textContent
        btn.textContent = 'Copied!'
        setTimeout(() => {
          btn.textContent = originalText
        }, 1000)
      } catch (err) {
        console.error('Failed to copy code:', err)
      }
    }
  }
}
</script>

<style scoped>
.code-runner {
  position: relative;
  margin: 16px 0;
}

.code-runner-controls {
  position: absolute;
  top: 8px;
  right: 8px;
  display: flex;
  gap: 8px;
  z-index: 10;
}

.code-runner-btn {
  padding: 4px 8px;
  font-size: 12px;
  background: var(--vp-c-brand-1);
  color: white;
  border: none;
  border-radius: 4px;
  cursor: pointer;
  transition: background 0.2s;
}

.code-runner-btn:hover {
  background: var(--vp-c-brand-2);
}

.code-runner-btn:disabled {
  opacity: 0.5;
  cursor: not-allowed;
}

.code-runner-output {
  margin-top: 8px;
  padding: 16px;
  background: var(--vp-c-bg-alt);
  border-radius: 8px;
  font-family: var(--vp-font-family-mono);
  font-size: 14px;
  line-height: 1.4;
  border: 1px solid var(--vp-c-border);
}

.code-block {
  position: relative;
}

.code-block:deep(div[class*="language-"]) {
  margin: 0;
}
</style>
