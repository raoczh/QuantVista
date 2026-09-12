<script setup lang="ts">
import { computed, onUnmounted } from 'vue'
import { useRoute, RouterView } from 'vue-router'
import { NConfigProvider, NMessageProvider, NDialogProvider, NGlobalStyle, zhCN, dateZhCN } from 'naive-ui'
import { storeToRefs } from 'pinia'
import { useThemeStore } from '@/stores/theme'
import AppShell from '@/components/AppShell.vue'
import { onExternalSessionChange } from '@/api/token'

// 根组件只负责主题下发与裸布局分流；外壳逻辑在 AppShell（必须位于
// n-config-provider 内部，useThemeVars 才能取到 override 后的主题变量）。
const route = useRoute()
const themeStore = useThemeStore()
const { naiveTheme, themeOverrides } = storeToRefs(themeStore)

// 登录/首启/回调页用整屏裸布局，不显示应用框架。
const isBare = computed(() => route.meta.bare === true)

// 其他标签登录/退出后重新走启动鉴权，同时卸载旧账号的全部页面与弹层。
// 同一会话的令牌轮换不触发此事件。
const stopSessionListener = onExternalSessionChange(() => window.location.reload())
onUnmounted(stopSessionListener)
</script>

<template>
  <n-config-provider :theme="naiveTheme" :theme-overrides="themeOverrides" :locale="zhCN" :date-locale="dateZhCN">
    <n-global-style />
    <n-message-provider>
      <n-dialog-provider>
        <RouterView v-if="isBare" />
        <AppShell v-else />
      </n-dialog-provider>
    </n-message-provider>
  </n-config-provider>
</template>
