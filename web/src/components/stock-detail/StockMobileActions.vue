<script setup lang="ts">
import { computed } from 'vue'
import { NDropdown, type DropdownOption } from 'naive-ui'
import { useUi } from '@/composables/useUi'

defineProps<{
  adding: boolean
  inWatchlist: boolean
  moreOptions: DropdownOption[]
}>()
const emit = defineEmits<{
  action: [value: 'watch' | 'alert' | 'analysis']
  more: [value: string | number]
}>()

// 原实现写的是 var(--n-card-color) / var(--n-base-color)：naive 的 card 只产出 --n-color，
// 且这两个变量都没有全局注入——本组件挂在 PageContainer 下、不在任何 n-card 内，
// 取到的是空值，导致底部操作条背景透明、主按钮文字色丢失。改走 useUi 主题变量。
const { vars } = useUi()
const barVars = computed(() => ({
  '--mab-bg': vars.value.cardColor,
  '--mab-primary-text': vars.value.baseColor,
}))
</script>

<template>
  <nav class="mobile-action-bar" :style="barVars" aria-label="当前股票快捷动作">
    <button type="button" :disabled="adding" @click="emit('action', 'watch')">{{ inWatchlist ? '观察中' : '观察' }}</button>
    <button type="button" @click="emit('action', 'alert')">提醒</button>
    <button type="button" class="primary" @click="emit('action', 'analysis')">分析</button>
    <n-dropdown trigger="click" placement="top-end" :options="moreOptions" @select="emit('more', $event)">
      <button type="button" aria-label="更多股票操作" title="更多股票操作">⋯</button>
    </n-dropdown>
  </nav>
</template>

<style scoped>
.mobile-action-bar { display: none; }
@media (max-width: 768px) {
  .mobile-action-bar {
    position: sticky;
    z-index: 8;
    bottom: calc(58px + env(safe-area-inset-bottom));
    display: grid;
    grid-template-columns: repeat(4, minmax(0, 1fr));
    gap: 6px;
    padding: 8px;
    border: 1px solid var(--qv-divider);
    border-radius: 8px;
    background: var(--mab-bg);
  }
  button {
    min-width: 0;
    min-height: 36px;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: inherit;
    font: inherit;
    cursor: pointer;
  }
  button.primary { background: var(--qv-primary); color: var(--mab-primary-text); }
  button:focus-visible { outline: 2px solid var(--qv-primary); outline-offset: 2px; }
  button:disabled { opacity: .55; cursor: default; }
}
</style>
