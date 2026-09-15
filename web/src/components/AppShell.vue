<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, ref, watch, h } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NDropdown,
  NButton,
  NIcon,
  NPopover,
  NAvatar,
  NDrawer,
  NDrawerContent,
  useThemeVars,
  useMessage,
  type DropdownOption,
} from 'naive-ui'
import { RouterLink, RouterView } from 'vue-router'
import { storeToRefs } from 'pinia'
import { workspaceLocation } from '@/navigation/workspace'
import WorkspaceNavigation from '@/components/WorkspaceNavigation.vue'
import WorkspaceIcon from '@/components/WorkspaceIcon.vue'
import { useAppStore } from '@/stores/app'
import { useThemeStore } from '@/stores/theme'
import { useAuthStore } from '@/stores/auth'
import { getOverview } from '@/api/market'
import { getTodoInbox } from '@/api/todo'
import { getSessionEpoch } from '@/api/token'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { setMarketTitle } from '@/lib/pageTitle'
import BrandLogo from '@/components/BrandLogo.vue'
import GlobalSearch from '@/components/GlobalSearch.vue'
import MobileBottomNav from '@/components/MobileBottomNav.vue'
import RecentTasks from '@/components/RecentTasks.vue'
import OnboardingGuide from '@/components/OnboardingGuide.vue'
import AIQuickActions from '@/components/AIQuickActions.vue'
import { useBrowserNotificationRuntime } from '@/composables/useBrowserNotifications'

// 应用主外壳：必须挂在 n-config-provider 内部，useThemeVars 才能取到主题 override。
const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const { status, error } = storeToRefs(appStore)

const themeStore = useThemeStore()
const { currentKey, preset } = storeToRefs(themeStore)

const authStore = useAuthStore()
const { user, isAdmin, isLoggedIn } = storeToRefs(authStore)
const message = useMessage()
const browserNotifications = useBrowserNotificationRuntime(() => user.value?.id || 0, router, message)

const vars = useThemeVars()
const { isDark, primaryAlpha } = useUi()

// ---------- 导航：按日常工作流归组；桌面侧栏、移动抽屉共用目录 ----------
const todoCount = ref(0)
const todoIncomplete = ref(false)
let disposed = false
let todoSeq = 0
let marketSeq = 0
const isCurrent = (owner: number) => !disposed && owner === getSessionEpoch()
const todoBadgeText = computed(() => {
  if (todoCount.value <= 0 && !todoIncomplete.value) return null
  if (todoIncomplete.value) return todoCount.value > 0 ? `${todoCount.value}+` : '!'
  return todoCount.value > 99 ? '99+' : String(todoCount.value)
})

async function refreshTodoCount() {
  if (disposed || !isLoggedIn.value) return
  const seq = ++todoSeq
  const owner = getSessionEpoch()
  try {
    // 徽标与今日待办默认首屏**同口径**（all + needs_action）：徽标 12 条、
    // 点进去只有 3 条会让用户以为丢了东西，反过来漏计 research 侧的失败任务
    // 也会让该处理的事悄悄积压。改口径前先改 Today.vue 的默认筛选。
    const result = await getTodoInbox({ scope: 'all', status: 'needs_action' })
    if (seq !== todoSeq || !isCurrent(owner)) return
    todoCount.value = result.total
    todoIncomplete.value = !result.complete
  } catch {
    if (seq !== todoSeq || !isCurrent(owner)) return
    todoCount.value = 0
    todoIncomplete.value = true
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

const pageLocation = computed(() => workspaceLocation(String(route.name || '')))
const activeKey = computed(() => pageLocation.value.activeKey)
const routeLabel = computed(() => String(pageLocation.value.item?.label || route.meta.title || '工作台'))
function focusMain() {
  document.getElementById('main-content')?.focus()
}

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
  if (isAdmin.value)
    opts.push(
      { label: '管理后台', key: 'admin' },
      { label: 'LLM 调用记录', key: 'admin-llm-calls' },
      { label: '因子 IC 排行', key: 'admin-factor-ic' },
      { label: 'Walk-Forward 基线', key: 'admin-walk-forward' },
      { label: '选股配对评估', key: 'admin-selection-eval' },
      { label: 'LLM 校准报表', key: 'admin-calibration' },
      { label: 'LLM 角色资产', key: 'admin-llm-roles' },
      { label: 'LLM 实验', key: 'admin-llm-experiments' },
      { label: '联合评估', key: 'admin-joint-eval' },
    )
  opts.push({ type: 'divider', key: 'd1' }, { label: '退出登录', key: 'logout' })
  return opts
})

async function onSelectUser(key: string) {
  if (key === 'settings') router.push('/settings')
  else if (key === 'prompts') router.push('/prompt-templates')
  else if (key === 'admin') router.push('/admin')
  else if (key === 'admin-llm-calls') router.push('/admin/llm-calls')
  else if (key === 'admin-factor-ic') router.push('/admin/factor-ic')
  else if (key === 'admin-walk-forward') router.push('/admin/walk-forward')
  else if (key === 'admin-selection-eval') router.push('/admin/selection-eval')
  else if (key === 'admin-calibration') router.push('/admin/calibration')
  else if (key === 'admin-llm-roles') router.push('/admin/llm-roles')
  else if (key === 'admin-llm-experiments') router.push('/admin/llm-experiments')
  else if (key === 'admin-joint-eval') router.push('/admin/joint-eval')
  else if (key === 'logout') {
    setMarketTitle('')
    await authStore.logout()
    router.replace('/login')
  }
}

// ---------- 后端连接状态 ----------
const health = computed(() => {
  if (error.value || !status.value)
    return { color: vars.value.errorColor, text: '服务暂不可用' }
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
  if (disposed || !isLoggedIn.value) return
  const seq = ++marketSeq
  const owner = getSessionEpoch()
  try {
    const ix = (await getOverview('cn')).indices?.[0]
    if (seq !== marketSeq || !isCurrent(owner)) return
    if (ix) {
      const sign = ix.change_pct > 0 ? '+' : ''
      setMarketTitle(`${ix.name} ${ix.price.toFixed(2)} ${sign}${ix.change_pct.toFixed(2)}%`)
    } else {
      setMarketTitle('')
    }
  } catch {
    if (seq === marketSeq && isCurrent(owner)) setMarketTitle('')
  }
}
useAutoRefresh(refreshMarketTitle, 60_000)

// 外壳专用变量全部源自主题，兼容 6 套主题。
const shellVars = computed(() => ({
  '--qv-header-bg': withAlpha(vars.value.cardColor, 0.95),
  '--qv-header-border': vars.value.dividerColor,
  '--qv-sidebar-bg': vars.value.cardColor,
  '--qv-menu-active': primaryAlpha(0.13),
  '--qv-menu-active-text': vars.value.primaryColor,
  '--qv-menu-hover': isDark.value ? 'rgba(255, 255, 255, 0.07)' : 'rgba(128, 128, 128, 0.1)',
  '--qv-badge-bg': vars.value.errorColor,
  '--qv-badge-incomplete-bg': vars.value.warningColor,
}))

// 健康状态低频轮询：不随交易时段限制（数据库/Redis 掉线任何时段都要感知），
// 独立 90s 定时器，卸载时清理。
let healthTimer: number | undefined
onMounted(() => {
  appStore.refreshStatus()
  refreshTodoCount()
  refreshMarketTitle()
  healthTimer = window.setInterval(() => appStore.refreshStatus(), 90_000)
  browserNotifications.start()
})
onBeforeUnmount(() => {
  disposed = true
  todoSeq++
  marketSeq++
  setMarketTitle('')
  if (healthTimer !== undefined) clearInterval(healthTimer)
  browserNotifications.stop()
})
</script>

<template>
  <div class="app-shell" :style="shellVars">
    <a class="skip-link" href="#main-content" @click.prevent="focusMain">跳转到主要内容</a>
    <aside class="app-sidebar" aria-label="工作区">
      <RouterLink to="/" class="sidebar-brand" aria-label="QuantVista 今日概览">
        <BrandLogo :size="32" />
        <span class="sidebar-caption">研究 · 决策 · 复盘</span>
      </RouterLink>
      <div class="sidebar-navigation">
        <WorkspaceNavigation :active-key="activeKey" :admin="isAdmin" :todo-badge="todoBadgeText" :todo-incomplete="todoIncomplete" id-prefix="desktop-nav" />
      </div>
      <div class="sidebar-footer">
        <RouterLink to="/settings" class="settings-link" :aria-current="route.name === 'settings' ? 'page' : undefined">
          <WorkspaceIcon name="settings" />个人设置
        </RouterLink>
        <span class="sidebar-status"><span class="health-dot" :style="{ background: health.color }" />{{ health.text }}</span>
      </div>
    </aside>

    <div class="app-content">
      <header class="app-header">
        <button class="nav-burger" type="button" aria-label="打开导航菜单" :aria-expanded="showNav" @click="showNav = true">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" aria-hidden="true"><path d="M4 7h16M4 12h16M4 17h16" /></svg>
        </button>
        <RouterLink to="/" class="header-brand" aria-label="QuantVista 今日概览"><BrandLogo :size="26" /></RouterLink>
        <div class="header-location" aria-label="当前位置">
          <span class="location-group">{{ pageLocation.group }}</span>
          <span class="location-divider" aria-hidden="true">/</span>
          <span class="location-current">{{ routeLabel }}</span>
        </div>
        <div class="header-right">
          <button class="search-trigger" type="button" aria-label="搜股票" title="搜股票 (Ctrl+K)" @click="showSearch = true">
            <svg class="st-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" aria-hidden="true"><circle cx="11" cy="11" r="7" /><path d="m20 20-3.5-3.5" stroke-linecap="round" /></svg>
            <span class="st-text">搜索股票</span><kbd class="st-kbd">Ctrl K</kbd>
          </button>
          <AIQuickActions variant="toolbar" class="header-ai-actions" />
          <RecentTasks />
          <n-popover trigger="click" placement="bottom">
            <template #trigger><button class="health-dot-wrap" type="button" :aria-label="`连接状态：${health.text}`"><span class="health-dot" :style="{ background: health.color }" /></button></template>
            <div class="health-detail">
              <div class="health-row"><span class="health-label">状态</span><span :style="{ color: health.color, fontWeight: 600 }">{{ health.text }}</span></div>
              <div class="health-row"><span class="health-label">数据库</span><span>{{ status?.db ? '已连接' : '离线' }}</span></div>
              <div class="health-row"><span class="health-label">Redis</span><span>{{ status?.redis ? '已连接' : '未启用' }}</span></div>
              <div v-if="status?.version" class="health-row"><span class="health-label">版本</span><span class="qv-mono">v{{ status.version }}</span></div>
            </div>
          </n-popover>
          <n-dropdown trigger="click" :options="themeOptions" :value="currentKey" @select="onSelectTheme">
            <n-button quaternary size="small" :aria-label="`切换外观，当前${preset.label}`">
              <template #icon><n-icon><span :style="`display:inline-block;width:14px;height:14px;border-radius:50%;background:${preset.primary}`" /></n-icon></template>
              <span class="theme-label">外观</span>
            </n-button>
          </n-dropdown>
          <n-dropdown v-if="isLoggedIn" trigger="click" :options="userOptions" @select="onSelectUser">
            <button class="user-chip" type="button" :aria-label="`${displayName || '用户'}菜单`">
              <n-avatar round :size="28" :style="{ background: vars.primaryColor, color: '#fff' }">{{ avatarText }}</n-avatar>
              <span class="user-name">{{ displayName }}</span>
            </button>
          </n-dropdown>
        </div>
      </header>

      <main id="main-content" class="app-main" tabindex="-1" :aria-label="routeLabel">
        <RouterView v-slot="{ Component }">
          <Transition name="page" mode="out-in"><component :is="Component" /></Transition>
        </RouterView>
      </main>
    </div>

    <MobileBottomNav :search-active="showSearch" :todo-badge="todoBadgeText" :todo-incomplete="todoIncomplete" @open-search="showSearch = true" />
    <GlobalSearch v-model:show="showSearch" />
    <OnboardingGuide v-if="isLoggedIn" />
    <n-drawer v-model:show="showNav" placement="left" width="min(88vw, 320px)">
      <n-drawer-content closable :body-content-style="{ padding: '8px 0' }">
        <template #header><BrandLogo :size="28" /></template>
        <div :style="shellVars">
          <WorkspaceNavigation :active-key="activeKey" :admin="isAdmin" :todo-badge="todoBadgeText" :todo-incomplete="todoIncomplete" id-prefix="mobile-nav" @navigate="showNav = false" />
          <RouterLink to="/settings" class="settings-link drawer-settings" @click="showNav = false"><WorkspaceIcon name="settings" />个人设置</RouterLink>
        </div>
      </n-drawer-content>
    </n-drawer>
  </div>
</template>

<style scoped>
.app-shell { min-height: 100vh; }
.app-sidebar { position: fixed; inset: 0 auto 0 0; z-index: 101; display: flex; flex-direction: column; width: var(--qv-sidebar-width); box-sizing: border-box; background: var(--qv-sidebar-bg); border-right: 1px solid var(--qv-header-border); }
.sidebar-brand { display: grid; gap: 9px; flex-shrink: 0; padding: 25px 23px 21px; text-decoration: none; }
.sidebar-caption { font-size: 10px; color: var(--qv-text-muted); letter-spacing: .17em; padding-left: 1px; }
.sidebar-navigation { flex: 1; min-height: 0; overflow-y: auto; overscroll-behavior: contain; }
.sidebar-footer { display: grid; flex-shrink: 0; gap: 12px; margin: 0 16px; padding: 12px 0 18px; border-top: 1px solid var(--qv-header-border); }
.settings-link { display: flex; gap: 10px; align-items: center; padding: 7px 10px; border-radius: 8px; color: var(--qv-text-secondary); font-size: 13px; text-decoration: none; }
.settings-link svg { width: 18px; height: 18px; }
.settings-link:hover, .settings-link[aria-current="page"] { background: var(--qv-menu-active); color: var(--qv-menu-active-text); }
.sidebar-status { display: flex; align-items: center; gap: 8px; padding: 0 11px; font-size: 11px; color: var(--qv-text-muted); }
.app-content { min-width: 0; margin-left: var(--qv-sidebar-width); }
.app-header { position: sticky; top: 0; z-index: 100; display: flex; align-items: center; gap: 18px; padding: 0 28px; height: var(--qv-header-height); box-sizing: border-box; background: var(--qv-header-bg); border-bottom: 1px solid var(--qv-header-border); backdrop-filter: blur(12px); }
.header-location { display: flex; align-items: center; gap: 12px; flex: 1; min-width: 0; font-size: 12px; white-space: nowrap; }
.location-group, .location-divider { color: var(--qv-text-muted); }
.location-current { color: var(--qv-text-secondary); font-weight: 500; overflow: hidden; text-overflow: ellipsis; }
.header-brand { display: none; text-decoration: none; flex-shrink: 0; }
.header-right { flex-shrink: 0; display: flex; align-items: center; gap: 10px; }
.search-trigger { display: inline-flex; align-items: center; gap: 9px; height: 34px; min-width: 180px; padding: 0 9px 0 11px; border-radius: 8px; border: 1px solid var(--qv-header-border); background: var(--qv-background); color: var(--qv-text-secondary); font: inherit; font-size: 12px; cursor: pointer; }
.search-trigger:hover { border-color: var(--qv-menu-active-text); }
.st-icon { width: 17px; height: 17px; flex-shrink: 0; }
.st-kbd { margin-left: auto; font: inherit; font-size: 10px; color: var(--qv-text-muted); border: 1px solid var(--qv-header-border); border-radius: 4px; padding: 1px 4px; }
.health-dot-wrap { display: flex; align-items: center; justify-content: center; width: 28px; height: 32px; border: 0; border-radius: 6px; background: transparent; cursor: pointer; }
.health-dot { width: 7px; height: 7px; flex: 0 0 7px; border-radius: 50%; }
.health-detail { min-width: 168px; display: grid; gap: 8px; }
.health-row { display: flex; justify-content: space-between; gap: 24px; font-size: 13px; }
.health-label { color: var(--qv-text-muted); }
.user-chip { display: flex; align-items: center; gap: 8px; padding: 3px 6px; border: 0; border-radius: 8px; background: transparent; color: inherit; font: inherit; cursor: pointer; }
.user-chip:hover, .health-dot-wrap:hover { background: var(--qv-hover); }
.user-name { max-width: 88px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-size: 12px; }
.app-main { position: relative; min-width: 0; padding: 28px 28px 56px; }
.app-main:focus { outline: none; }
.skip-link { position: fixed; top: 8px; left: 12px; z-index: 10000; padding: 10px 16px; border-radius: 8px; background: var(--qv-surface); color: var(--qv-primary); transform: translateY(-150%); }
.skip-link:focus { transform: translateY(0); }
.nav-burger { display: none; width: 36px; height: 36px; flex-shrink: 0; padding: 7px; border: 0; border-radius: 8px; background: transparent; color: inherit; cursor: pointer; }
.nav-burger:hover { background: var(--qv-hover); }
.nav-burger svg { width: 22px; height: 22px; }
.drawer-settings { margin: 4px 12px 16px; min-height: 34px; }
.page-enter-active { transition: opacity .16s ease; }
.page-leave-active { transition: opacity .1s ease; }
.page-enter-from, .page-leave-to { opacity: 0; }
@media (max-width: 1370px) { .st-kbd, .user-name { display: none; } .search-trigger { min-width: 140px; } .header-ai-actions { display: none; } }
@media (max-width: 1150px) { .app-sidebar { display: none; } .app-content { margin-left: 0; } .nav-burger { display: flex; } .header-brand { display: inline-flex; } .app-header { gap: 14px; padding: 0 22px; } .location-group, .location-divider { display: none; } }
@media (max-width: 768px) {
  .app-header { height: calc(var(--qv-header-height) + env(safe-area-inset-top, 0px)); padding: env(safe-area-inset-top, 0px) 12px 0; gap: 8px; }
  .header-brand, .theme-label, .st-text, .health-dot-wrap { display: none; }
  .header-location { font-size: 13px; }
  .header-right { gap: 5px; }
  .search-trigger { width: 34px; height: 36px; min-width: 0; justify-content: center; border: 0; background: transparent; padding: 0; }
  .user-chip { padding: 2px; }
  .app-main { padding: 20px 14px calc(92px + env(safe-area-inset-bottom, 0px)); }
}
</style>
