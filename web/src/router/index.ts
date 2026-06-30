import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import Home from '@/pages/Home.vue'
import Placeholder from '@/pages/Placeholder.vue'

// 路由对齐 docs/ARCHITECTURE.md 第 9 节页面结构。
// 骨架阶段仅首页可用，其余为占位，随阶段推进逐步实现。
const routes: RouteRecordRaw[] = [
  { path: '/', name: 'home', component: Home, meta: { title: '市场首页' } },
  { path: '/watchlist', name: 'watchlist', component: Placeholder, meta: { title: '自选股' } },
  { path: '/positions', name: 'positions', component: Placeholder, meta: { title: '持仓' } },
  { path: '/analysis', name: 'analysis', component: Placeholder, meta: { title: 'AI 分析' } },
  { path: '/recommendations', name: 'recommendations', component: Placeholder, meta: { title: '推荐追踪' } },
  { path: '/settings', name: 'settings', component: Placeholder, meta: { title: '设置' } },
  { path: '/:pathMatch(.*)*', name: 'notfound', component: Placeholder, meta: { title: '未找到' } },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.afterEach((to) => {
  document.title = `QuantVista · ${(to.meta.title as string) || ''}`
})

export default router
