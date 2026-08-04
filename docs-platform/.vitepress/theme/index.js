// Two components, not five.
//
// This file used to import InteractiveExample.vue, PerformanceChart.vue and
// ConfigurationBuilder.vue as well. None of the three has ever existed — not deleted, never
// written; `git log --all -- .vitepress/components/InteractiveExample.vue` returns nothing. A
// missing import is a hard rollup error ("Could not resolve"), so this alone made every page in
// this tree unbuildable, and it went unnoticed because no CI job builds the site (#214).
//
// The registrations are gone with them. `app.component()` with an undefined second argument is
// not an error at registration time, so leaving them would trade a build failure for a page that
// renders a blank where the component should be — worse, because it looks like it worked.
//
// Where the pages used them, they now say what the component would have done and what to run
// instead. That is deliberate rather than lazy: a placeholder that promises "try the interactive
// example above" above nothing is the same class of claim as the hardcoded performance chart
// removed from index.md, and it fails the same way — silently, for the reader.
import DefaultTheme from 'vitepress/theme'
import ApiPlayground from '../components/ApiPlayground.vue'
import CodeRunner from '../components/CodeRunner.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('ApiPlayground', ApiPlayground)
    app.component('CodeRunner', CodeRunner)
  }
}
