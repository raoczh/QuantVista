<script setup lang="ts">
import { NTabPane, NTabs } from 'naive-ui'
import StockTabState from './StockTabState.vue'
import type { StockSectionPhase } from './decisionSummary'

type TabKey = 'trend' | 'event' | 'fundamental' | 'research'
interface TabState { phase: StockSectionPhase; error: string; updatedAt: string }

defineProps<{
  modelValue: TabKey
  states: Record<TabKey, TabState>
  hasData: Record<TabKey, boolean>
}>()
const emit = defineEmits<{
  'update:modelValue': [value: TabKey]
  retry: [value: TabKey]
}>()
const tabs: { key: TabKey; label: string }[] = [
  { key: 'trend', label: '行情与技术' },
  { key: 'event', label: '新闻与事件' },
  { key: 'fundamental', label: '财务与估值' },
  { key: 'research', label: '研究证据' },
]
</script>

<template>
  <n-tabs
    :value="modelValue"
    type="line"
    :animated="false"
    pane-class="stock-tab-pane"
    @update:value="emit('update:modelValue', $event as TabKey)"
  >
    <n-tab-pane v-for="item in tabs" :key="item.key" :name="item.key" :tab="item.label" display-directive="show:lazy">
      <StockTabState
        :phase="states[item.key].phase"
        :error="states[item.key].error"
        :has-data="hasData[item.key]"
        @retry="emit('retry', item.key)"
      >
        <div class="tab-content"><slot :name="item.key" /></div>
      </StockTabState>
    </n-tab-pane>
  </n-tabs>
</template>

<style scoped>
.tab-content {
  display: flex;
  min-width: 0;
  min-height: 420px;
  flex-direction: column;
  gap: 16px;
}
</style>
