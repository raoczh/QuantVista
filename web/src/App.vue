<script setup lang="ts">
import { computed, onMounted, h } from 'vue'
import { useRoute } from 'vue-router'
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

const route = useRoute()
const appStore = useAppStore()
const { status, error } = storeToRefs(appStore)

const themeStore = useThemeStore()
const { naiveTheme, themeOverrides, currentKey, preset } = storeToRefs(themeStore)

const menuOptions: MenuOption[] = [
  { label: () => h(RouterLink, { to: '/' }, { default: () => '市场首页' }), key: 'home' },
  { label: () => h(RouterLink, { to: '/watchlist' }, { default: () => '自选股' }), key: 'watchlist' },
  { label: () => h(RouterLink, { to: '/positions' }, { default: () => '持仓' }), key: 'positions' },
  { label: () => h(RouterLink, { to: '/analysis' }, { default: () => 'AI 分析' }), key: 'analysis' },
  { label: () => h(RouterLink, { to: '/recommendations' }, { default: () => '推荐追踪' }), key: 'recommendations' },
  { label: () => h(RouterLink, { to: '/settings' }, { default: () => '设置' }), key: 'settings' },
]

const activeKey = computed(() => (route.name as string) || 'home')

// 主题切换下拉：每项前面渲染一个主色色块预览。
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

onMounted(() => {
  appStore.refreshStatus()
})
</script>

<template>
  <n-config-provider :theme="naiveTheme" :theme-overrides="themeOverrides" :locale="zhCN" :date-locale="dateZhCN">
    <n-global-style />
    <n-message-provider>
      <n-layout style="height: 100vh">
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
                    <span
                      :style="`display:inline-block;width:14px;height:14px;border-radius:3px;background:${preset.primary}`"
                    />
                  </n-icon>
                </template>
                {{ preset.label }}
              </n-button>
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
