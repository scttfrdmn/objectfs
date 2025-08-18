import { defineConfig } from 'vitepress'

export default defineConfig({
  title: 'ObjectFS Documentation',
  description: 'High-performance POSIX filesystem for object storage',
  base: '/docs/',

  head: [
    ['link', { rel: 'icon', href: '/favicon.ico' }],
    ['meta', { name: 'theme-color', content: '#3c82f6' }],
    ['meta', { name: 'og:type', content: 'website' }],
    ['meta', { name: 'og:locale', content: 'en' }],
    ['meta', { name: 'og:site_name', content: 'ObjectFS Documentation' }],
  ],

  themeConfig: {
    logo: '/logo.svg',

    nav: [
      { text: 'Home', link: '/' },
      { text: 'Guide', link: '/guide/getting-started' },
      { text: 'API Reference', link: '/api/' },
      { text: 'Tutorials', link: '/tutorials/' },
      { text: 'SDKs', link: '/sdks/' },
      { text: 'Playground', link: '/playground/' },
    ],

    sidebar: {
      '/guide/': [
        {
          text: 'Getting Started',
          items: [
            { text: 'Introduction', link: '/guide/' },
            { text: 'Installation', link: '/guide/installation' },
            { text: 'Quick Start', link: '/guide/getting-started' },
            { text: 'Configuration', link: '/guide/configuration' },
          ]
        },
        {
          text: 'Core Concepts',
          items: [
            { text: 'Architecture', link: '/guide/architecture' },
            { text: 'Storage Backends', link: '/guide/storage-backends' },
            { text: 'Caching System', link: '/guide/caching' },
            { text: 'Performance', link: '/guide/performance' },
          ]
        },
        {
          text: 'Advanced Features',
          items: [
            { text: 'Distributed Clusters', link: '/guide/distributed' },
            { text: 'Monitoring', link: '/guide/monitoring' },
            { text: 'Security', link: '/guide/security' },
            { text: 'Troubleshooting', link: '/guide/troubleshooting' },
          ]
        }
      ],
      '/api/': [
        {
          text: 'API Reference',
          items: [
            { text: 'Overview', link: '/api/' },
            { text: 'REST API', link: '/api/rest' },
            { text: 'CLI Reference', link: '/api/cli' },
            { text: 'Configuration', link: '/api/configuration' },
          ]
        },
        {
          text: 'Endpoints',
          items: [
            { text: 'Mount Operations', link: '/api/mount' },
            { text: 'Storage Operations', link: '/api/storage' },
            { text: 'Health & Metrics', link: '/api/health' },
            { text: 'Cluster Management', link: '/api/cluster' },
          ]
        }
      ],
      '/tutorials/': [
        {
          text: 'Tutorials',
          items: [
            { text: 'Overview', link: '/tutorials/' },
            { text: 'First Mount', link: '/tutorials/first-mount' },
            { text: 'Performance Tuning', link: '/tutorials/performance-tuning' },
            { text: 'Multi-Node Setup', link: '/tutorials/multi-node' },
            { text: 'Container Integration', link: '/tutorials/containers' },
          ]
        },
        {
          text: 'Use Cases',
          items: [
            { text: 'Data Lake Access', link: '/tutorials/data-lake' },
            { text: 'ML Model Storage', link: '/tutorials/ml-models' },
            { text: 'Backup Solutions', link: '/tutorials/backup' },
            { text: 'Media Processing', link: '/tutorials/media' },
          ]
        }
      ],
      '/sdks/': [
        {
          text: 'SDK Documentation',
          items: [
            { text: 'Overview', link: '/sdks/' },
            { text: 'Python SDK', link: '/sdks/python' },
            { text: 'JavaScript SDK', link: '/sdks/javascript' },
            { text: 'Java SDK', link: '/sdks/java' },
          ]
        },
        {
          text: 'Examples',
          items: [
            { text: 'Python Examples', link: '/sdks/python-examples' },
            { text: 'JavaScript Examples', link: '/sdks/javascript-examples' },
            { text: 'Java Examples', link: '/sdks/java-examples' },
          ]
        }
      ]
    },

    socialLinks: [
      { icon: 'github', link: 'https://github.com/objectfs/objectfs' }
    ],

    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Copyright © 2024 ObjectFS Team'
    },

    editLink: {
      pattern: 'https://github.com/objectfs/objectfs/edit/main/docs-platform/:path',
      text: 'Edit this page on GitHub'
    },

    search: {
      provider: 'local'
    }
  },

  markdown: {
    lineNumbers: true,
    config: (md) => {
      // Add custom markdown plugins
      md.use(require('markdown-it-container'), 'tip')
      md.use(require('markdown-it-container'), 'warning')
      md.use(require('markdown-it-container'), 'danger')
    }
  },

  vite: {
    plugins: [
      // Custom plugins for API playground
    ]
  }
})
