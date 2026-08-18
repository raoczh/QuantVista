<script setup lang="ts">
import { NDropdown, type DropdownOption } from 'naive-ui'

defineProps<{
  adding: boolean
  inWatchlist: boolean
  moreOptions: DropdownOption[]
}>()
const emit = defineEmits<{
  action: [value: 'watch' | 'alert' | 'analysis']
  more: [value: string | number]
}>()
</script>

<template>
  <nav class="mobile-action-bar" aria-label="当前股票快捷动作">
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
    background: var(--n-card-color);
  }
  button {
    min-width: 0;
    min-height: 36px;
    border: 0;
    border-radius: 6px;
    background: transparent;
    color: inherit;
  }
  button.primary { background: var(--qv-primary); color: var(--n-base-color); }
  button:focus-visible { outline: 2px solid var(--qv-primary); outline-offset: 2px; }
}
</style>
