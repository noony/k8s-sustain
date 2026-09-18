import { createApp } from 'vue'
import App from './App.vue'
import { router } from './router'
import '@fontsource-variable/inter'
import '@fontsource-variable/jetbrains-mono'
import './style.css'

createApp(App).use(router).mount('#app')
