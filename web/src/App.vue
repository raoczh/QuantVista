<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute } from 'vue-router'
import {
  NConfigProvider,
  NMessageProvider,
  NLayout,
  NLayoutHeader,
  NLayoutContent,
  NMenu,
  NTag,
  NSpace,
  zhCN,
  dateZhCN,
  darkTheme,
  type MenuOption,
} from 'naive-ui'
import { h } from 'vue'
import { RouterLink, RouterView } from 'vue-router'
import { storeToRefs } from 'pinia'
import { useAppStore } from '@/stores/app'

const route = useRoute()
const appStore = useAppStore()
const { status, error } = storeToRefs(appStore)

const menuOptions: MenuOption[] = [
  { label: () => h(RouterLink, { to: '/' }, { default: () => '市场首页' }), key: 'home' },
  { label: () => h(RouterLink, { to: '/watchlist' }, { default: () => '自选股' }), key: 'watchlist' },
  { label: () => h(RouterLink, { to: '/positions' }, { default: () => '持仓' }), key: 'positions' },
  { label: () => h(RouterLink, { to: '/analysis' }, { default: () => 'AI 分析' }), key: 'analysis' },
  { label: () => h(RouterLink, { to: '/recommendations' }, { default: () => '推荐追踪' }), key: 'recommendations' },
  { label: () => h(RouterLink, { to: '/settings' }, { default: () => '设置' }), key: 'settings' },
]

const activeKey = computed(() => (route.name as string) || 'home')

onMounted(() => {
  appStore.refreshStatus()
})
</script>

<template>
  <n-config-provider :theme="darkTheme" :locale="zhCN" :date-locale="dateZhCN">
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
          </n-space>
        </n-layout-header>
        <n-layout-content content-style="padding: 24px" :native-scrollbar="false">
          <RouterView />
        </n-layout-content>
      </n-layout>
    </n-message-provider>
  </n-config-provider>
</template>
