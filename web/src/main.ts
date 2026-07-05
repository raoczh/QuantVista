import { createApp } from 'vue'
import { createPinia } from 'pinia'
import router from './router'
import App from './App.vue'
import './styles/global.css'

const app = createApp(App)
app.use(createPinia())
app.use(router)
// 等首次路由解析完成再挂载：否则 route.meta 尚为空，App.vue 的 isBare 误判为 false，
// AppShell 会在登录/回调等裸页上闪现并发出需登录的请求（如 /api/todos 401），
// 其 401 整页跳登录会掐死回调页飞行中的 OAuth 换令牌请求，GitHub 登录必失败。
router.isReady().then(() => app.mount('#app'))
