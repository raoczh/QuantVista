<script setup lang="ts">
import { NButton, NEmpty, NPopconfirm, NSelect, NSpin, NTag } from 'naive-ui'
import type { AnalysisRecord } from '@/api/analysis'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import { ANALYSIS_MODULE_LABELS, analysisStatusLabel, ratingLabel, recordStockName } from './analysisPresentation'

defineProps<{
  history: AnalysisRecord[]
  currentID?: number
  loading: boolean
  module: string
  moduleOptions: Array<{ label: string; value: string }>
}>()
const emit = defineEmits<{
  (event: 'update:module', value: string): void
  (event: 'open', item: AnalysisRecord): void
  (event: 'remove', item: AnalysisRecord): void
  (event: 'refresh'): void
}>()
function time(value: string) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '时间未知' }
function statusType(value: string) { return value === 'success' ? 'success' : value === 'failed' ? 'error' : value === 'degraded' ? 'warning' : 'info' }
</script>

<template>
  <SectionCard title="历史分析">
    <template #extra>
      <n-select :value="module" :options="moduleOptions" size="tiny" class="module-filter" @update:value="emit('update:module', $event)" />
      <n-button size="tiny" quaternary :loading="loading" @click="emit('refresh')">刷新</n-button>
    </template>
    <n-spin :show="loading && !history.length">
      <n-empty v-if="!history.length" description="暂无分析记录" size="small" />
      <div v-else class="history-list">
        <button v-for="item in history" :key="item.id" type="button" class="history-row" :class="{ active: currentID === item.id }" @click="emit('open', item)">
          <span class="history-main">
            <StockIdentity v-if="item.module === 'stock'" :symbol="item.symbol" :market="item.market || 'cn'" :name="recordStockName(item)" density="table" />
            <b v-else>{{ item.target || item.title || ANALYSIS_MODULE_LABELS[item.module] }}</b>
            <span>{{ ANALYSIS_MODULE_LABELS[item.module] }} · 分析时点 {{ time(item.created_at) }} · 数据截止需打开结果核对</span>
          </span>
          <span class="history-side">
            <n-tag size="tiny" :type="statusType(item.status)" :bordered="false">{{ item.status === 'success' ? ratingLabel(item.rating) : analysisStatusLabel(item.status) }}</n-tag>
            <n-tag v-if="item.as_of" size="tiny" type="warning" :bordered="false">回溯 {{ item.as_of }}</n-tag>
            <n-popconfirm v-if="item.status !== 'processing'" @positive-click="emit('remove', item)">
              <template #trigger><n-button size="tiny" quaternary type="error" @click.stop>删除记录</n-button></template>
              删除这条本人分析记录？
            </n-popconfirm>
          </span>
        </button>
      </div>
    </n-spin>
  </SectionCard>
</template>

<style scoped>
.module-filter { width: 128px; }
.history-list { display: grid; }
.history-row { display: flex; width: 100%; min-width: 0; align-items: center; justify-content: space-between; gap: 10px; padding: 10px 0; border: 0; border-bottom: 1px solid rgba(128,128,128,.2); background: transparent; color: inherit; font: inherit; text-align: left; cursor: pointer; }
.history-row.active { padding-left: 10px; box-shadow: inset 3px 0 var(--qv-primary); }
.history-main { display: grid; min-width: 0; gap: 4px; }
.history-main > span:last-child { font-size: 11px; opacity: .6; overflow-wrap: anywhere; }
.history-side { display: flex; flex: 0 0 auto; align-items: center; flex-wrap: wrap; justify-content: flex-end; gap: 4px; }
@media (max-width: 520px) {
  .history-row { align-items: flex-start; flex-direction: column; }
  .history-side { width: 100%; justify-content: flex-start; }
}
</style>
