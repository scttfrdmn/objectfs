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
            // No 'REST API' entry: there is no /api/rest page and never was, and as of #367 there is
            // no pkg/api either. A running mount serves /metrics and /health; both are under
            // 'Health & Metrics' below.
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
      { icon: 'github', link: 'https://github.com/scttfrdmn/objectfs' }
    ],

    footer: {
      message: 'Released under the MIT License.',
      copyright: 'Copyright © 2024 ObjectFS Team'
    },

    editLink: {
      pattern: 'https://github.com/scttfrdmn/objectfs/edit/main/docs-platform/:path',
      text: 'Edit this page on GitHub'
    },

    search: {
      provider: 'local'
    }
  },

  // No `config` hook. It used to register `markdown-it-container` for 'tip', 'warning' and
  // 'danger', which was three kinds of redundant: VitePress ships those three containers built in,
  // no page in this tree uses `:::` syntax at all, and the `md` handed to this hook is VitePress's
  // own bundled markdown-it instance — so the top-level `markdown-it` dependency the hook appeared
  // to justify was never the one being extended.
  //
  // Verified by removing all three packages from node_modules and rebuilding: the site builds, and
  // the rendered HTML is byte-identical once asset content hashes are normalized. The dependencies
  // are gone from package.json with the hook.
  markdown: {
    lineNumbers: true
  },

  vite: {
    plugins: [
      // Custom plugins for API playground
    ]
  }
})
