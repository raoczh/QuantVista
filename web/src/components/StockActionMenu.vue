<script setup lang="ts">
import { computed } from 'vue'
import { NButton, NDropdown, type DropdownOption } from 'naive-ui'
import {
  useStockActions,
  type StockActionKey,
  type StockRef,
} from '@/composables/useStockActions'

const props = withDefaults(
  defineProps<{
    stock: StockRef
    watchlistState?: 'in' | 'out'
    positionState?: 'held' | 'none'
  }>(),
  {},
)
const emit = defineEmits<{
  (e: 'close'): void
  (e: 'completed', value: { action: StockActionKey; success: boolean }): void
}>()

const { adding, runStockAction } = useStockActions(() => emit('close'))
const options = computed<DropdownOption[]>(() => [
  { key: 'detail', label: '查看详情' },
  {
    key: 'watchlist',
    label: props.watchlistState === undefined
      ? '自选入口'
      : props.watchlistState === 'in' ? '自选管理' : '加入自选',
  },
  {
    key: 'position',
    label: props.positionState === undefined
      ? '持仓入口'
      : props.positionState === 'held' ? '持仓管理' : '建仓',
  },
  { key: 'alert', label: '创建预警' },
  { key: 'analysis', label: 'AI 分析' },
  { key: 'compare', label: '股票对比' },
  { key: 'note', label: '添加笔记' },
])

async function selectAction(key: string | number) {
  const action = String(key) as StockActionKey
  const success = await runStockAction(action, props.stock, {
    inWatchlist: props.watchlistState === undefined ? undefined : props.watchlistState === 'in',
    hasPosition: props.positionState === undefined ? undefined : props.positionState === 'held',
  })
  emit('completed', { action, success: !!success })
}
</script>

<template>
  <span class="stock-action-menu" @click.stop>
    <n-dropdown trigger="click" placement="bottom-end" :options="options" @select="selectAction">
      <n-button
        size="small"
        quaternary
        circle
        :loading="adding"
        aria-label="股票操作"
        title="股票操作"
      >
        ⋯
      </n-button>
    </n-dropdown>
  </span>
</template>

<style scoped>
.stock-action-menu {
  display: inline-flex;
  flex: 0 0 auto;
}
</style>
