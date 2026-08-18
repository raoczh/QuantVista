<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NButton, NCheckbox, NEmpty, NPopover, NSwitch, NTable, NTag } from 'naive-ui'
import ChangeTag from '@/components/ChangeTag.vue'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import { useIsMobile } from '@/composables/useIsMobile'
import { useStockActions } from '@/composables/useStockActions'
import type { ScanResult } from '@/api/screener'

const props = defineProps<{
  result: ScanResult
  stats: string
  includeST: boolean
  includeStale: boolean
  selectedSymbols: string[]
  batchLoading: boolean
}>()

const emit = defineEmits<{
  'update:includeST': [value: boolean]
  'update:includeStale': [value: boolean]
  'update:selectedSymbols': [value: string[]]
  rescan: []
  batch: []
}>()

const { isMobile } = useIsMobile()
const actions = useStockActions()
const displayedSymbols = computed(() => (props.result.items || []).map((item) => item.symbol))
const allSelected = computed(() => displayedSymbols.value.length > 0 && displayedSymbols.value.every((s) => props.selectedSymbols.includes(s)))
const someSelected = computed(() => !allSelected.value && displayedSymbols.value.some((s) => props.selectedSymbols.includes(s)))
const completeness = computed(() => props.result.universe > 0 ? Math.round(props.result.scanned / props.result.universe * 100) : null)
const dataState = computed(() => {
  if (completeness.value == null) return { label: '数据完整度未知', type: 'warning' as const }
  if (props.result.scanned < props.result.universe || props.result.truncated || props.result.stale_skipped > 0) {
    return { label: `部分数据 · 完整度 ${completeness.value}%`, type: 'warning' as const }
  }
  return { label: `完整度 ${completeness.value}%`, type: 'success' as const }
})

function shortHash(value?: string) {
  return value ? value.slice(0, 12) : '-'
}
function toggleAll(checked: boolean) {
  emit('update:selectedSymbols', checked ? displayedSymbols.value.slice(0, 100) : [])
}
function toggleSymbol(symbol: string, checked: boolean) {
  const next = new Set(props.selectedSymbols)
  if (checked && next.size < 100) next.add(symbol)
  if (!checked) next.delete(symbol)
  emit('update:selectedSymbols', [...next])
}
function updateIncludeST(value: boolean) {
  emit('update:includeST', value)
  emit('rescan')
}
function updateIncludeStale(value: boolean) {
  emit('update:includeStale', value)
  emit('rescan')
}
</script>

<template>
  <SectionCard :title="`扫描结果 · ${result.strategy}`" class="block">
    <template #extra>
      <div class="result-switches">
        <label><n-switch :value="includeST" size="small" @update:value="updateIncludeST" /> 含 ST</label>
        <label><n-switch :value="includeStale" size="small" @update:value="updateIncludeStale" /> 含停牌</label>
      </div>
    </template>
    <p class="result-meta">
      {{ stats }}
      <span>数据截止：{{ result.trade_date }} 收盘</span>
      <n-tag size="small" :type="dataState.type" :bordered="false">{{ dataState.label }}</n-tag>
    </p>
    <n-alert v-if="includeStale" type="warning" :bordered="false" class="result-warning">
      当前允许停牌或滞后数据参与扫描，命中项不能视为正常实时结果；请进入个股详情核对行情时间。
    </n-alert>
    <p v-if="result.strategy_revision_id" class="result-meta">
      固定快照 <n-tag size="tiny" type="info" :bordered="false">v{{ result.strategy_revision }}</n-tag>
      <code>{{ shortHash(result.strategy_hash) }}</code>
    </p>
    <p v-if="result.conditions?.length" class="condition-list">
      <span>命中条件</span>
      <n-tag v-for="condition in result.conditions" :key="condition" size="small" :bordered="false">{{ condition }}</n-tag>
    </p>
    <div v-if="result.items?.length" class="batch-toolbar">
      <div>
        <n-checkbox :checked="allSelected" :indeterminate="someSelected" @update:checked="toggleAll">全选当前结果</n-checkbox>
        <span>已选 {{ selectedSymbols.length }} / 100</span>
      </div>
      <div>
        <n-button v-if="selectedSymbols.length" size="small" quaternary @click="emit('update:selectedSymbols', [])">清空</n-button>
        <n-button size="small" type="primary" :disabled="!selectedSymbols.length" :loading="batchLoading" @click="emit('batch')">
          批量加入自选
        </n-button>
      </div>
    </div>
    <n-empty v-if="!result.items?.length" description="本次没有命中标的；这不代表扫描失败，可调整条件后重试" />
    <div v-else class="qv-scroll-x">
      <n-table size="small" :single-line="false" class="hits-table">
        <thead><tr><th>选择</th><th>股票</th><th class="num">现价</th><th class="num">涨跌</th><th class="num">成交额(亿)</th><th v-if="!isMobile" class="num">换手%</th><th v-if="!isMobile" class="num">60日位置</th><th>命中原因与风险</th><th>操作</th></tr></thead>
        <tbody>
          <tr v-for="hit in result.items" :key="hit.symbol">
            <td><n-checkbox :checked="selectedSymbols.includes(hit.symbol)" :aria-label="`选择 ${hit.name || '名称待补全'}`" @update:checked="toggleSymbol(hit.symbol, $event)" /></td>
            <td><StockIdentity :symbol="hit.symbol" market="cn" :name="hit.name" density="table" clickable actions /></td>
            <td class="num qv-tnum">{{ hit.price.toFixed(2) }}</td>
            <td class="num"><ChangeTag :value="hit.chg_pct" size="small" /></td>
            <td class="num qv-tnum">{{ hit.amount_yi.toFixed(2) }}</td>
            <td v-if="!isMobile" class="num qv-tnum">{{ hit.turnover_rate ? hit.turnover_rate.toFixed(2) : '未知' }}</td>
            <td v-if="!isMobile" class="num qv-tnum">{{ hit.pos_60 ? `${hit.pos_60.toFixed(0)}%` : '未知' }}</td>
            <td>
              <n-popover trigger="hover" placement="top" :disabled="hit.reasons.length <= 2">
                <template #trigger><span class="condition-list"><n-tag v-for="reason in hit.reasons.slice(0, 2)" :key="reason" size="small" :bordered="false">{{ reason }}</n-tag><n-tag v-if="hit.reasons.length > 2" size="small" :bordered="false">+{{ hit.reasons.length - 2 }}</n-tag></span></template>
                <div v-for="reason in hit.reasons" :key="reason">{{ reason }}</div>
              </n-popover>
              <small class="risk-note">主要风险：命中条件不等于买入结论，需继续核对行情时效与个股基本面。</small>
            </td>
            <td><div class="row-actions">
              <n-button size="tiny" quaternary @click="actions.goDetail({ symbol: hit.symbol, market: 'cn', name: hit.name })">详情</n-button>
              <n-button size="tiny" quaternary @click="actions.goAnalysis({ symbol: hit.symbol, market: 'cn', name: hit.name })">AI 分析</n-button>
              <n-button size="tiny" quaternary @click="actions.goAlert({ symbol: hit.symbol, market: 'cn', name: hit.name })">提醒</n-button>
            </div></td>
          </tr>
        </tbody>
      </n-table>
    </div>
  </SectionCard>
</template>

<style scoped>
.result-switches,
.result-switches label,
.condition-list,
.batch-toolbar,
.batch-toolbar > div,
.row-actions { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.result-meta { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; margin: 0 0 10px; }
.result-meta span { opacity: .62; }
.result-warning { margin-bottom: 10px; }
.risk-note { display: block; margin-top: 6px; opacity: .62; line-height: 1.45; }
.batch-toolbar { justify-content: space-between; margin: 12px 0; }
.num { text-align: right; white-space: nowrap; }
@media (max-width: 768px) {
  .batch-toolbar > div:last-child { width: 100%; justify-content: flex-end; }
}
</style>
