import DefaultTheme from 'vitepress/theme'
import ApiPlayground from '../components/ApiPlayground.vue'
import CodeRunner from '../components/CodeRunner.vue'
import InteractiveExample from '../components/InteractiveExample.vue'
import PerformanceChart from '../components/PerformanceChart.vue'
import ConfigurationBuilder from '../components/ConfigurationBuilder.vue'
import './custom.css'

export default {
  extends: DefaultTheme,
  enhanceApp({ app }) {
    app.component('ApiPlayground', ApiPlayground)
    app.component('CodeRunner', CodeRunner)
    app.component('InteractiveExample', InteractiveExample)
    app.component('PerformanceChart', PerformanceChart)
    app.component('ConfigurationBuilder', ConfigurationBuilder)
  }
}
