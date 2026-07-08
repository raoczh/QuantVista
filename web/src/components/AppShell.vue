<script setup lang="ts">
import { computed, onMounted, ref, watch, watchEffect, h } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NMenu,
  NDropdown,
  NButton,
  NIcon,
  NPopover,
  NAvatar,
  NDrawer,
  NDrawerContent,
  useThemeVars,
  type MenuOption,
  type DropdownOption,
} from 'naive-ui'
import { RouterLink, RouterView } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAppStore } from '@/stores/app'
import { useThemeStore } from '@/stores/theme'
import { useAuthStore } from '@/stores/auth'
import { getOverview } from '@/api/market'
import { getTodos } from '@/api/todo'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { setMarketTitle } from '@/lib/pageTitle'
import BrandLogo from '@/components/BrandLogo.vue'
import GlobalSearch from '@/components/GlobalSearch.vue'

// 应用主外壳：必须挂在 n-config-provider 内部，useThemeVars 才能取到主题 override。
const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const { status, error } = storeToRefs(appStore)

const themeStore = useThemeStore()
const { currentKey, preset } = storeToRefs(themeStore)

const authStore = useAuthStore()
const { user, isAdmin, isLoggedIn } = storeToRefs(authStore)

const vars = useThemeVars()
const { isDark, primaryAlpha } = useUi()

// ---------- 导航：高频一级直达，AI 工具与低频项归组，设置/管理后台只留用户菜单 ----------
const todoCount = ref(0)
async function refreshTodoCount() {
  try {
    todoCount.value = (await getTodos()).total
  } catch {
    /* 徽标失败静默，不打扰 */
  }
}
// 离开待办相关页面时刷新徽标（处理完待办数量会变）。
watch(
  () => route.name,
  (_nv, ov) => {
    if (['today', 'alerts', 'positions', 'recommendations'].includes(String(ov))) {
      refreshTodoCount()
    }
  },
)

function navLink(to: string, text: string) {
  return () => h(RouterLink, { to }, { default: () => text })
}

const menuOptions = computed<MenuOption[]>(() => [
  { label: navLink('/', '市场首页'), key: 'home' },
  { label: navLink('/news', '快讯'), key: 'news' },
  {
    label: () =>
      h(RouterLink, { to: '/today', class: 'nav-today' }, {
        default: () => [
          '今日待办',
          todoCount.value > 0
            ? h('span', { class: 'nav-badge qv-tnum' }, todoCount.value > 99 ? '99+' : String(todoCount.value))
            : null,
        ],
      }),
    key: 'today',
  },
  { label: navLink('/watchlist', '自选'), key: 'watchlist' },
  { label: navLink('/screener', '选股'), key: 'screener' },
  { label: navLink('/positions', '持仓'), key: 'positions' },
  { label: navLink('/recommendations', '推荐追踪'), key: 'recommendations' },
  {
    label: 'AI 研究',
    key: 'ai-group',
    children: [
      { label: navLink('/analysis', 'AI 分析'), key: 'analysis' },
      { label: navLink('/daily-report', '收盘日报'), key: 'daily-report' },
      { label: navLink('/qa', '个股问答'), key: 'qa' },
      { label: navLink('/compare', '横向对比'), key: 'compare' },
    ],
  },
  {
    label: '更多',
    key: 'more-group',
    children: [
      { label: navLink('/backtest', '回测时光机'), key: 'backtest' },
      { label: navLink('/thesis', '投资逻辑卡'), key: 'thesis' },
      { label: navLink('/notes', '投资笔记'), key: 'notes' },
      { label: navLink('/paper', '模拟交易'), key: 'paper' },
      { label: navLink('/etf', '指数ETF'), key: 'etf' },
      { label: navLink('/alerts', '条件提醒'), key: 'alerts' },
      { label: navLink('/prompt-templates', '提示词模板'), key: 'prompts' },
    ],
  },
])

const activeKey = computed(() => (route.name as string) || 'home')

// ---------- 主题 ----------
const themeOptions = computed<DropdownOption[]>(() =>
  themeStore.presets.map((p) => ({
    key: p.key,
    label: p.label,
    icon: () =>
      h('span', {
        style: `display:inline-block;width:14px;height:14px;border-radius:4px;background:${p.primary};border:1px solid rgba(128,128,128,.4)`,
      }),
  })),
)

function onSelectTheme(key: string) {
  themeStore.setTheme(key)
}

// ---------- 用户菜单 ----------
const userOptions = computed<DropdownOption[]>(() => {
  const opts: DropdownOption[] = [
    { label: '设置', key: 'settings' },
    { label: '提示词模板', key: 'prompts' },
  ]
  if (isAdmin.value) opts.push({ label: '管理后台', key: 'admin' })
  opts.push({ type: 'divider', key: 'd1' }, { label: '退出登录', key: 'logout' })
  return opts
})

async function onSelectUser(key: string) {
  if (key === 'settings') router.push('/settings')
  else if (key === 'prompts') router.push('/prompt-templates')
  else if (key === 'admin') router.push('/admin')
  else if (key === 'logout') {
    setMarketTitle('')
    await authStore.logout()
    router.replace('/login')
  }
}

// ---------- 后端连接状态 ----------
const health = computed(() => {
  if (error.value || !status.value)
    return { color: vars.value.errorColor, text: '后端不可达' }
  if (!status.value.db) return { color: vars.value.warningColor, text: '数据库离线' }
  return { color: vars.value.successColor, text: '运行正常' }
})

const displayName = computed(() => user.value?.display_name || user.value?.username || '')
const avatarText = computed(() => displayName.value.slice(0, 1).toUpperCase() || 'U')

// ---------- 全局速查 ----------
const showSearch = ref(false)

// ---------- 移动端抽屉导航 ----------
// ≤768px 时顶部水平菜单放不下，收进左侧抽屉（汉堡按钮唤起）。
const showNav = ref(false)
// 抽屉内点击菜单项（RouterLink）完成导航后自动收起。
watch(
  () => route.fullPath,
  () => {
    showNav.value = false
  },
)

// ---------- 标签页标题带大盘：挂后台也能瞟一眼盘面 ----------
async function refreshMarketTitle() {
  if (!isLoggedIn.value) return
  try {
    const ix = (await getOverview('cn')).indices?.[0]
    if (ix) {
      const sign = ix.change_pct > 0 ? '+' : ''
      setMarketTitle(`${ix.name} ${ix.price.toFixed(2)} ${sign}${ix.change_pct.toFixed(2)}%`)
    }
  } catch {
    setMarketTitle('')
  }
}
useAutoRefresh(refreshMarketTitle, 60_000)

// ---------- 主题变量下发 ----------
// 注入到 :root，global.css（::selection）与弹层内容也能取到。
watchEffect(() => {
  const el = document.documentElement
  el.style.setProperty('--qv-primary', vars.value.primaryColor)
  el.style.setProperty('--qv-primary-selection', withAlpha(vars.value.primaryColor, 0.22))
})

// 外壳专用变量：毛玻璃顶栏 / 胶囊菜单 / 氛围光晕，全部源自主题，兼容 6 套。
const shellVars = computed(() => ({
  '--qv-header-bg': withAlpha(vars.value.cardColor, 0.72),
  '--qv-header-border': vars.value.dividerColor,
  '--qv-menu-active': primaryAlpha(0.13),
  '--qv-menu-active-text': vars.value.primaryColor,
  '--qv-menu-hover': isDark.value ? 'rgba(255, 255, 255, 0.07)' : 'rgba(128, 128, 128, 0.1)',
  '--qv-glow': primaryAlpha(isDark.value ? 0.12 : 0.08),
  '--qv-badge-bg': vars.value.errorColor,
}))

onMounted(() => {
  appStore.refreshStatus()
  refreshTodoCount()
  refreshMarketTitle()
})
</script>

<template>
  <div class="app-shell" :style="shellVars">
    <div class="app-glow" aria-hidden="true" />

    <header class="app-header">
      <button class="nav-burger" type="button" aria-label="打开导航菜单" @click="showNav = true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round">
          <path d="M4 7h16M4 12h16M4 17h16" />
        </svg>
      </button>
      <RouterLink to="/" class="logo-link">
        <BrandLogo :size="30" />
      </RouterLink>
      <n-menu mode="horizontal" responsive :options="menuOptions" :value="activeKey" class="app-menu" />
      <div class="header-right">
        <!-- 全局速查入口（Ctrl+K） -->
        <button class="search-trigger" type="button" @click="showSearch = true">
          <svg class="st-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <circle cx="11" cy="11" r="7" />
            <path d="m20 20-3.5-3.5" stroke-linecap="round" />
          </svg>
          <span class="st-text">搜代码</span>
          <span class="st-kbd">Ctrl K</span>
        </button>

        <!-- 后端状态：圆点 + 悬浮详情 -->
        <n-popover trigger="hover" placement="bottom">
          <template #trigger>
            <div class="health-dot-wrap">
              <span class="health-dot" :style="{ background: health.color }" />
            </div>
          </template>
          <div class="health-detail">
            <div class="health-row">
              <span class="health-label">状态</span>
              <span :style="{ color: health.color, fontWeight: 600 }">{{ health.text }}</span>
            </div>
            <div class="health-row">
              <span class="health-label">数据库</span>
              <span>{{ status?.db ? '已连接' : '离线' }}</span>
            </div>
            <div class="health-row">
              <span class="health-label">Redis</span>
              <span>{{ status?.redis ? '已连接' : '未启用' }}</span>
            </div>
            <div v-if="status?.version" class="health-row">
              <span class="health-label">版本</span>
              <span class="qv-mono">v{{ status.version }}</span>
            </div>
          </div>
        </n-popover>

        <n-dropdown trigger="click" :options="themeOptions" :value="currentKey" @select="onSelectTheme">
          <n-button quaternary size="small">
            <template #icon>
              <n-icon>
                <span :style="`display:inline-block;width:14px;height:14px;border-radius:4px;background:${preset.primary}`" />
              </n-icon>
            </template>
            <span class="theme-label">{{ preset.label }}</span>
          </n-button>
        </n-dropdown>

        <n-dropdown v-if="isLoggedIn" trigger="click" :options="userOptions" @select="onSelectUser">
          <div class="user-chip">
            <n-avatar round :size="26" :style="{ background: vars.primaryColor, color: '#fff' }">
              {{ avatarText }}
            </n-avatar>
            <span class="user-name">{{ displayName }}</span>
          </div>
        </n-dropdown>
      </div>
    </header>

    <main class="app-main">
      <RouterView v-slot="{ Component }">
        <Transition name="page" mode="out-in" appear>
          <component :is="Component" />
        </Transition>
      </RouterView>
    </main>

    <GlobalSearch v-model:show="showSearch" />

    <!-- 移动端抽屉导航：与顶部菜单同一份 options，分组默认展开 -->
    <n-drawer v-model:show="showNav" placement="left" width="min(82vw, 300px)">
      <n-drawer-content :body-content-style="{ padding: '10px 6px' }">
        <template #header>
          <BrandLogo :size="26" />
        </template>
        <n-menu
          mode="vertical"
          :options="menuOptions"
          :value="activeKey"
          :default-expanded-keys="['ai-group', 'more-group']"
          :indent="24"
        />
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.app-shell {
  min-height: 100vh;
}

/* 顶部主色氛围光晕：随主题变色，不挡交互 */
.app-glow {
  position: fixed;
  top: 0;
  left: 0;
  right: 0;
  height: 46vh;
  pointer-events: none;
  z-index: 0;
  background: radial-gradient(ellipse 72% 100% at 50% -12%, var(--qv-glow), transparent 68%);
}

/* sticky 毛玻璃顶栏 */
.app-header {
  position: sticky;
  top: 0;
  z-index: 100;
  display: flex;
  align-items: center;
  gap: 20px;
  padding: 0 24px;
  height: 60px;
  background: var(--qv-header-bg);
  backdrop-filter: blur(16px) saturate(1.5);
  -webkit-backdrop-filter: blur(16px) saturate(1.5);
  border-bottom: 1px solid var(--qv-header-border);
}
.logo-link {
  text-decoration: none;
  flex-shrink: 0;
}
.app-menu {
  flex: 1;
}

/* 菜单项胶囊化：激活主色浅底、hover 中性浅底 */
.app-menu :deep(.n-menu-item-content) {
  padding: 0 14px !important;
  border-radius: 999px;
  transition: background-color 0.18s ease;
}
.app-menu :deep(.n-menu-item-content:hover) {
  background: var(--qv-menu-hover);
}
.app-menu :deep(.n-menu-item-content--selected),
.app-menu :deep(.n-menu-item-content--child-active) {
  background: var(--qv-menu-active);
}
.app-menu :deep(.n-menu-item-content--selected .n-menu-item-content-header),
.app-menu :deep(.n-menu-item-content--child-active .n-menu-item-content-header) {
  font-weight: 600;
}
.app-menu :deep(.nav-today) {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.app-menu :deep(.nav-badge) {
  min-width: 16px;
  height: 16px;
  padding: 0 4px;
  border-radius: 999px;
  background: var(--qv-badge-bg);
  color: #fff;
  font-size: 11px;
  font-weight: 700;
  line-height: 16px;
  text-align: center;
}

.header-right {
  flex-shrink: 0;
  display: flex;
  align-items: center;
  gap: 10px;
}

/* 速查入口：伪输入框样式 */
.search-trigger {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  height: 30px;
  padding: 0 6px 0 10px;
  border-radius: 999px;
  border: 1px solid var(--qv-header-border);
  background: rgba(128, 128, 128, 0.07);
  color: inherit;
  font: inherit;
  font-size: 12px;
  opacity: 0.85;
  cursor: pointer;
  transition:
    border-color 0.18s ease,
    opacity 0.18s ease;
}
.search-trigger:hover {
  border-color: var(--qv-menu-active-text);
  opacity: 1;
}
.st-icon {
  width: 14px;
  height: 14px;
  opacity: 0.7;
}
.st-text {
  opacity: 0.75;
}
.st-kbd {
  font-size: 10px;
  padding: 1px 5px;
  border-radius: 4px;
  border: 1px solid var(--qv-header-border);
  opacity: 0.6;
}

.health-dot-wrap {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  cursor: default;
}
.health-dot {
  width: 9px;
  height: 9px;
  border-radius: 50%;
  box-shadow: 0 0 0 3px rgba(128, 128, 128, 0.12);
}
.health-detail {
  min-width: 168px;
  display: flex;
  flex-direction: column;
  gap: 7px;
}
.health-row {
  display: flex;
  justify-content: space-between;
  gap: 24px;
  font-size: 13px;
}
.health-label {
  opacity: 0.6;
}
.user-chip {
  display: flex;
  align-items: center;
  gap: 8px;
  cursor: pointer;
  padding: 3px 10px 3px 3px;
  border-radius: 999px;
  transition: background 0.15s ease;
}
.user-chip:hover {
  background: rgba(128, 128, 128, 0.1);
}
.user-name {
  font-size: 13px;
  font-weight: 500;
}

.app-main {
  position: relative;
  z-index: 1;
  padding: 26px 28px 56px;
}

/* 页面切换过渡：轻快的淡入上移 */
.page-enter-active {
  transition:
    opacity 0.18s ease,
    transform 0.18s ease;
}
.page-leave-active {
  transition: opacity 0.12s ease;
}
.page-enter-from {
  opacity: 0;
  transform: translateY(8px);
}
.page-leave-to {
  opacity: 0;
}

/* ---------- 移动端（≤768px）：菜单收进抽屉，顶栏只留图标 ---------- */
.nav-burger {
  display: none;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
  width: 34px;
  height: 34px;
  padding: 0;
  border: none;
  border-radius: 8px;
  background: transparent;
  color: inherit;
  cursor: pointer;
  transition: background-color 0.15s ease;
}
.nav-burger svg {
  width: 20px;
  height: 20px;
}
.nav-burger:hover,
.nav-burger:active {
  background: var(--qv-menu-hover);
}

@media (max-width: 768px) {
  .app-header {
    padding: 0 10px;
    gap: 8px;
  }
  .nav-burger {
    display: inline-flex;
  }
  .app-menu {
    display: none;
  }
  /* logo 靠左，右侧操作组自然靠右 */
  .logo-link {
    margin-right: auto;
  }
  .header-right {
    gap: 4px;
  }
  /* 搜索入口只留放大镜图标 */
  .st-text,
  .st-kbd {
    display: none;
  }
  .search-trigger {
    gap: 0;
    padding: 0 8px;
    border-color: transparent;
    background: transparent;
  }
  .st-icon {
    width: 17px;
    height: 17px;
  }
  /* 主题按钮只留色块，用户菜单只留头像 */
  .theme-label {
    display: none;
  }
  .user-name {
    display: none;
  }
  .user-chip {
    padding: 3px;
  }
  .app-main {
    padding: 16px 12px 44px;
  }
}
</style>
