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
  NTag,
  NSpace,
  NDropdown,
  NButton,
  NIcon,
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

const route = useRoute()
const router = useRouter()
const appStore = useAppStore()
const { status, error } = storeToRefs(appStore)

const themeStore = useThemeStore()
const { naiveTheme, themeOverrides, currentKey, preset } = storeToRefs(themeStore)

const authStore = useAuthStore()
const { user, isAdmin, isLoggedIn } = storeToRefs(authStore)

// 登录/首启/回调页用整屏裸布局，不显示应用框架。
const isBare = computed(() => route.meta.bare === true)

const menuOptions = computed<MenuOption[]>(() => {
  const items: MenuOption[] = [
    { label: () => h(RouterLink, { to: '/' }, { default: () => '市场首页' }), key: 'home' },
    { label: () => h(RouterLink, { to: '/watchlist' }, { default: () => '自选股' }), key: 'watchlist' },
    { label: () => h(RouterLink, { to: '/positions' }, { default: () => '持仓' }), key: 'positions' },
    { label: () => h(RouterLink, { to: '/analysis' }, { default: () => 'AI 分析' }), key: 'analysis' },
    { label: () => h(RouterLink, { to: '/recommendations' }, { default: () => '推荐追踪' }), key: 'recommendations' },
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
        style: `display:inline-block;width:14px;height:14px;border-radius:3px;background:${p.primary};border:1px solid rgba(128,128,128,.4)`,
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
        <n-layout-header bordered style="display: flex; align-items: center; gap: 24px; padding: 0 24px; height: 56px">
          <strong style="font-size: 18px">QuantVista</strong>
          <n-menu mode="horizontal" :options="menuOptions" :value="activeKey" style="flex: 1" />
          <n-space align="center" :size="8">
            <n-tag v-if="status" :type="status.db ? 'success' : 'warning'" size="small" round>
              DB {{ status.db ? 'ok' : 'off' }}
            </n-tag>
            <n-tag v-if="status" :type="status.redis ? 'success' : 'default'" size="small" round>
              Redis {{ status.redis ? 'ok' : 'off' }}
            </n-tag>
            <n-tag v-if="status" size="small" round>v{{ status.version }}</n-tag>
            <n-tag v-if="error" type="error" size="small" round>后端不可达</n-tag>
            <n-dropdown trigger="click" :options="themeOptions" :value="currentKey" @select="onSelectTheme">
              <n-button quaternary size="small">
                <template #icon>
                  <n-icon>
                    <span :style="`display:inline-block;width:14px;height:14px;border-radius:3px;background:${preset.primary}`" />
                  </n-icon>
                </template>
                {{ preset.label }}
              </n-button>
            </n-dropdown>
            <n-dropdown v-if="isLoggedIn" trigger="click" :options="userOptions" @select="onSelectUser">
              <n-button quaternary size="small">{{ user?.display_name || user?.username }}</n-button>
            </n-dropdown>
          </n-space>
        </n-layout-header>
        <n-layout-content content-style="padding: 24px" :native-scrollbar="false">
          <RouterView />
        </n-layout-content>
      </n-layout>
    </n-message-provider>
  </n-config-provider>
</template>
