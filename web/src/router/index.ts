import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import Home from '@/pages/Home.vue'
import Placeholder from '@/pages/Placeholder.vue'
import { useAuthStore } from '@/stores/auth'
import { getAccessToken } from '@/api/token'

// meta.bare：无应用框架的整屏页（登录/首启/回调）。
// meta.public：无需登录。meta.admin：需管理员。
const routes: RouteRecordRaw[] = [
  { path: '/setup', name: 'setup', component: () => import('@/pages/Setup.vue'), meta: { bare: true, public: true, title: '初始化' } },
  { path: '/login', name: 'login', component: () => import('@/pages/Login.vue'), meta: { bare: true, public: true, title: '登录' } },
  { path: '/login/callback', name: 'oauth-callback', component: () => import('@/pages/OAuthCallback.vue'), meta: { bare: true, public: true, title: '登录中' } },

  { path: '/', name: 'home', component: Home, meta: { title: '市场首页' } },
  { path: '/watchlist', name: 'watchlist', component: () => import('@/pages/Watchlist.vue'), meta: { title: '自选股' } },
  { path: '/positions', name: 'positions', component: () => import('@/pages/Positions.vue'), meta: { title: '持仓' } },
  { path: '/analysis', name: 'analysis', component: Placeholder, meta: { title: 'AI 分析' } },
  { path: '/recommendations', name: 'recommendations', component: Placeholder, meta: { title: '推荐追踪' } },
  { path: '/settings', name: 'settings', component: () => import('@/pages/Settings.vue'), meta: { title: '设置' } },
  { path: '/admin', name: 'admin', component: () => import('@/pages/AdminSettings.vue'), meta: { title: '管理后台', admin: true } },
  { path: '/:pathMatch(.*)*', name: 'notfound', component: Placeholder, meta: { title: '未找到' } },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()

  // 首次导航先确认系统初始化状态（失败时放行，避免后端不可达卡死）。
  if (!auth.statusLoaded) {
    try {
      await auth.fetchSetupStatus()
    } catch {
      return true
    }
  }

  // 系统未初始化：强制去首启页。
  if (!auth.initialized) {
    return to.name === 'setup' ? true : { name: 'setup' }
  }
  // 已初始化却访问首启页：回首页。
  if (to.name === 'setup') return { name: 'home' }

  // 有 token 但内存无 user（刷新页面）：尝试恢复登录态。
  if (!auth.isLoggedIn && getAccessToken()) {
    await auth.loadSelf()
  }

  // 公开页：已登录则跳离登录页。
  if (to.meta.public) {
    if (auth.isLoggedIn && (to.name === 'login' || to.name === 'oauth-callback')) {
      return { name: 'home' }
    }
    return true
  }

  // 受保护页：需登录。
  if (!auth.isLoggedIn) {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  // 管理员页：需管理员。
  if (to.meta.admin && !auth.isAdmin) {
    return { name: 'home' }
  }
  return true
})

router.afterEach((to) => {
  document.title = `QuantVista · ${(to.meta.title as string) || ''}`
})

export default router
