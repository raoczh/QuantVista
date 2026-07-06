import { createRouter, createWebHistory, type RouteRecordRaw } from 'vue-router'
import Home from '@/pages/Home.vue'
import Placeholder from '@/pages/Placeholder.vue'
import { useAuthStore } from '@/stores/auth'
import { getAccessToken } from '@/api/token'
import { setPageTitle } from '@/lib/pageTitle'

// meta.bare：无应用框架的整屏页（登录/首启/回调）。
// meta.public：无需登录。meta.admin：需管理员。
const routes: RouteRecordRaw[] = [
  { path: '/setup', name: 'setup', component: () => import('@/pages/Setup.vue'), meta: { bare: true, public: true, title: '初始化' } },
  { path: '/login', name: 'login', component: () => import('@/pages/Login.vue'), meta: { bare: true, public: true, title: '登录' } },
  { path: '/login/callback', name: 'oauth-callback', component: () => import('@/pages/OAuthCallback.vue'), meta: { bare: true, public: true, title: '登录中' } },

  { path: '/', name: 'home', component: Home, meta: { title: '市场首页' } },
  { path: '/news', name: 'news', component: () => import('@/pages/News.vue'), meta: { title: '市场快讯' } },
  { path: '/stocks/:market/:symbol', name: 'stock-detail', component: () => import('@/pages/StockDetail.vue'), meta: { title: '个股详情' } },
  { path: '/today', name: 'today', component: () => import('@/pages/Today.vue'), meta: { title: '今日待办' } },
  { path: '/daily-report', name: 'daily-report', component: () => import('@/pages/DailyReport.vue'), meta: { title: '收盘日报' } },
  { path: '/watchlist', name: 'watchlist', component: () => import('@/pages/Watchlist.vue'), meta: { title: '自选股' } },
  { path: '/positions', name: 'positions', component: () => import('@/pages/Positions.vue'), meta: { title: '持仓' } },
  { path: '/analysis', name: 'analysis', component: () => import('@/pages/Analysis.vue'), meta: { title: 'AI 分析' } },
  { path: '/qa', name: 'qa', component: () => import('@/pages/Qa.vue'), meta: { title: '个股问答' } },
  { path: '/compare', name: 'compare', component: () => import('@/pages/Compare.vue'), meta: { title: '横向对比' } },
  { path: '/paper', name: 'paper', component: () => import('@/pages/Paper.vue'), meta: { title: '模拟交易' } },
  { path: '/etf', name: 'etf', component: () => import('@/pages/Etf.vue'), meta: { title: '指数ETF' } },
  { path: '/prompt-templates', name: 'prompts', component: () => import('@/pages/Prompts.vue'), meta: { title: '提示词模板' } },
  { path: '/recommendations', name: 'recommendations', component: () => import('@/pages/Recommendations.vue'), meta: { title: '推荐追踪' } },
  { path: '/alerts', name: 'alerts', component: () => import('@/pages/Alerts.vue'), meta: { title: '条件提醒' } },
  { path: '/thesis', name: 'thesis', component: () => import('@/pages/ThesisCards.vue'), meta: { title: '投资逻辑卡' } },
  { path: '/notes', name: 'notes', component: () => import('@/pages/Notes.vue'), meta: { title: '投资笔记' } },
  { path: '/settings', name: 'settings', component: () => import('@/pages/Settings.vue'), meta: { title: '设置' } },
  { path: '/admin', name: 'admin', component: () => import('@/pages/AdminSettings.vue'), meta: { title: '管理后台', admin: true } },
  { path: '/:pathMatch(.*)*', name: 'notfound', component: Placeholder, meta: { title: '未找到' } },
]

const router = createRouter({
  history: createWebHistory(),
  routes,
})

// 懒加载 chunk 拉取失败（多为部署更新后旧页面持有过期 hash）：整页跳转目标路由，
// 让浏览器拿到新的 index.html 与资源清单，避免点菜单无响应/白屏。
router.onError((error, to) => {
  const msg = String((error as Error)?.message || '')
  if (/Failed to fetch dynamically imported module|Importing a module script failed|error loading dynamically imported module/i.test(msg)) {
    window.location.assign(to.fullPath)
  }
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
      // GitHub 绑定与登录共用回调页：已登录且带绑定标记时放行，让回调页完成绑定。
      if (to.name === 'oauth-callback' && auth.pendingGithubBind()) return true
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
  setPageTitle((to.meta.title as string) || '')
})

export default router
