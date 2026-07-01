<script setup lang="ts">
import { computed, onMounted, h } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NConfigProvider,
  NMessageProvider,
  NGlobalStyle,
  NLayout,
  NLayoutHeader,
  NLayoutContent,
  NMenu,
  NSpace,
  NDropdown,
  NButton,
  NIcon,
  NPopover,
  NAvatar,
  useThemeVars,
  zhCN,
  dateZhCN,
  type MenuOption,
  type DropdownOption,
} from 'naive-ui'
import { RouterLink, RouterView } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAppStore } from '@/stores/app'
import { useThemeStore } from '@/stores/theme'
import { useAuthStore } from '@/stores/auth'
import BrandLogo from '@/components/BrandLogo.vue'

const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const { status, error } = storeToRefs(appStore)

const themeStore = useThemeStore()
const { naiveTheme, themeOverrides, currentKey, preset } = storeToRefs(themeStore)

const authStore = useAuthStore()
const { user, isAdmin, isLoggedIn } = storeToRefs(authStore)

const vars = useThemeVars()

// 登录/首启/回调页用整屏裸布局，不显示应用框架。
const isBare = computed(() => route.meta.bare === true)

const menuOptions = computed<MenuOption[]>(() => {
  const items: MenuOption[] = [
    { label: () => h(RouterLink, { to: '/' }, { default: () => '市场首页' }), key: 'home' },
    { label: () => h(RouterLink, { to: '/watchlist' }, { default: () => '自选股' }), key: 'watchlist' },
    { label: () => h(RouterLink, { to: '/positions' }, { default: () => '持仓' }), key: 'positions' },
    { label: () => h(RouterLink, { to: '/analysis' }, { default: () => 'AI 分析' }), key: 'analysis' },
    { label: () => h(RouterLink, { to: '/recommendations' }, { default: () => '推荐追踪' }), key: 'recommendations' },
    { label: () => h(RouterLink, { to: '/alerts' }, { default: () => '提醒' }), key: 'alerts' },
    { label: () => h(RouterLink, { to: '/settings' }, { default: () => '设置' }), key: 'settings' },
  ]
  if (isAdmin.value) {
    items.push({ label: () => h(RouterLink, { to: '/admin' }, { default: () => '管理后台' }), key: 'admin' })
  }
  return items
})

const activeKey = computed(() => (route.name as string) || 'home')

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

const userOptions = computed<DropdownOption[]>(() => {
  const opts: DropdownOption[] = [{ label: '设置', key: 'settings' }]
  if (isAdmin.value) opts.push({ label: '管理后台', key: 'admin' })
  opts.push({ type: 'divider', key: 'd1' }, { label: '退出登录', key: 'logout' })
  return opts
})

async function onSelectUser(key: string) {
  if (key === 'settings') router.push('/settings')
  else if (key === 'admin') router.push('/admin')
  else if (key === 'logout') {
    await authStore.logout()
    router.replace('/login')
  }
}

// 后端连接状态：正常/降级/不可达，收进一个圆点 + 悬浮详情。
const health = computed(() => {
  if (error.value || !status.value)
    return { color: vars.value.errorColor, text: '后端不可达' }
  if (!status.value.db) return { color: vars.value.warningColor, text: '数据库离线' }
  return { color: vars.value.successColor, text: '运行正常' }
})

const displayName = computed(() => user.value?.display_name || user.value?.username || '')
const avatarText = computed(() => displayName.value.slice(0, 1).toUpperCase() || 'U')

onMounted(() => {
  appStore.refreshStatus()
})
</script>

<template>
  <n-config-provider :theme="naiveTheme" :theme-overrides="themeOverrides" :locale="zhCN" :date-locale="dateZhCN">
    <n-global-style />
    <n-message-provider>
      <!-- 裸布局：登录 / 首启 / 回调 -->
      <RouterView v-if="isBare" />

      <!-- 应用主框架 -->
      <n-layout v-else style="height: 100vh">
        <n-layout-header bordered class="app-header">
          <RouterLink to="/" class="logo-link">
            <BrandLogo :size="30" />
          </RouterLink>
          <n-menu mode="horizontal" :options="menuOptions" :value="activeKey" class="app-menu" />
          <n-space align="center" :size="10" class="header-right">
            <!-- 后端状态：圆点 + 悬浮详情，替代原先一排调试标签 -->
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
                {{ preset.label }}
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
          </n-space>
        </n-layout-header>
        <n-layout-content content-style="padding: 24px 28px" :native-scrollbar="false">
          <RouterView />
        </n-layout-content>
      </n-layout>
    </n-message-provider>
  </n-config-provider>
</template>

<style scoped>
.app-header {
  display: flex;
  align-items: center;
  gap: 28px;
  padding: 0 24px;
  height: 60px;
}
.logo-link {
  text-decoration: none;
  flex-shrink: 0;
}
.app-menu {
  flex: 1;
}
.header-right {
  flex-shrink: 0;
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
</style>
