// The default VitePress theme plus a stylesheet. No components.
//
// This file has carried two separate findings, and they are the same mistake at different scales.
//
// It used to import five .vue components. Three — InteractiveExample, PerformanceChart,
// ConfigurationBuilder — had never existed; not deleted, never written (`git log --all` finds no
// history for any of the files). A missing import is a hard rollup error, so that alone made every
// page in this tree unbuildable, and it went unnoticed because no CI job built the site (#214).
//
// The other two, ApiPlayground and CodeRunner, did exist, and are gone now as well (#336). They
// were not broken the way the phantom three were. They were worse, because they rendered. Each drew
// a button that fetched an endpoint — `/api-playground/...` and `/api/code-runner/execute` — served
// only by `docs-platform/src/api-server.js`, a 549-line Express app nothing in this repository
// built, tested, deployed, or documented how to start correctly. A reader clicking "Run" got a
// request to a path with nothing behind it and no error to explain why. A phantom import fails
// loudly at build; an interactive control wired to nothing fails silently, at the reader.
//
// So this directory is a documentation site and nothing else. The blocks that were wrapped in
// <CodeRunner> are ordinary fenced blocks now, which is also what a reader can use: VitePress puts
// a copy button on every fenced block without any of this.
import DefaultTheme from 'vitepress/theme'
import './custom.css'

export default DefaultTheme
