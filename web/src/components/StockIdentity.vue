<script setup lang="ts">
import { computed } from 'vue'
import { useStockActions } from '@/composables/useStockActions'
import StockActionMenu from './StockActionMenu.vue'

const props = withDefaults(
  defineProps<{
    symbol: string
    market?: string
    name?: string | null
    density?: 'normal' | 'compact' | 'table'
    clickable?: boolean
    actions?: boolean
    inWatchlist?: boolean
    hasPosition?: boolean
    positionId?: number
    recommendationId?: number
  }>(),
  {
    market: 'cn',
    name: '',
    density: 'normal',
    clickable: false,
    actions: false,
  },
)

const { goDetail } = useStockActions()
const displayName = computed(() => props.name?.trim() || '名称待补全')
const marketLabel = computed(() => {
  const labels: Record<string, string> = { cn: 'A 股', hk: '港股', us: '美股' }
  return labels[props.market.toLowerCase()] || props.market.toUpperCase()
})
const stock = computed(() => ({
  symbol: props.symbol,
  market: props.market,
  name: props.name?.trim() || '',
}))
const accessibleLabel = computed(() => `${displayName.value}，${props.symbol}，${marketLabel.value}`)

function openDetail() {
  if (props.clickable && props.symbol) void goDetail(stock.value)
}
</script>

<template>
  <span class="stock-identity" :class="[`density-${density}`, { 'with-actions': actions }]">
    <button
      v-if="clickable"
      type="button"
      class="identity-main identity-button"
      :aria-label="`打开${accessibleLabel}详情`"
      @click="openDetail"
    >
      <span class="identity-name" :title="displayName">{{ displayName }}</span>
      <span class="identity-meta"><span class="qv-mono">{{ symbol }}</span><span>{{ marketLabel }}</span></span>
    </button>
    <span v-else class="identity-main" :aria-label="accessibleLabel">
      <span class="identity-name" :title="displayName">{{ displayName }}</span>
      <span class="identity-meta"><span class="qv-mono">{{ symbol }}</span><span>{{ marketLabel }}</span></span>
    </span>
    <StockActionMenu
      v-if="actions"
      :stock="stock"
      :watchlist-state="inWatchlist === undefined ? undefined : inWatchlist ? 'in' : 'out'"
      :position-state="hasPosition === undefined ? undefined : hasPosition ? 'held' : 'none'"
      :position-id="positionId"
      :recommendation-id="recommendationId"
    />
  </span>
</template>

<style scoped>
.stock-identity {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
  max-width: 100%;
  vertical-align: middle;
}
.identity-main {
  display: inline-flex;
  flex-direction: column;
  align-items: flex-start;
  min-width: 0;
  max-width: 100%;
  color: inherit;
}
.identity-button {
  padding: 2px 0;
  border: 0;
  background: transparent;
  font: inherit;
  cursor: pointer;
  text-align: left;
}
.identity-button:hover .identity-name {
  color: var(--qv-primary);
}
.identity-button:focus-visible {
  outline: 2px solid var(--qv-primary);
  outline-offset: 3px;
  border-radius: 4px;
}
.identity-name {
  min-width: 0;
  max-width: 100%;
  overflow: hidden;
  font-weight: 600;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.identity-meta {
  display: inline-flex;
  align-items: center;
  gap: 7px;
  margin-top: 2px;
  font-size: 11px;
  line-height: 1.25;
  opacity: 0.58;
}
.density-compact .identity-main,
.density-table .identity-main {
  flex-direction: row;
  align-items: baseline;
  gap: 7px;
}
.density-compact .identity-meta,
.density-table .identity-meta {
  margin-top: 0;
}
.density-table .identity-meta > :last-child {
  display: none;
}
.density-table .identity-name {
  max-width: 180px;
}
@media (max-width: 480px) {
  .identity-name {
    white-space: normal;
    overflow-wrap: anywhere;
  }
  .density-table .identity-name {
    max-width: 130px;
  }
}
</style>
