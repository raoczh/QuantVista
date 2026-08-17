<script setup lang="ts">
import { computed, h, ref, watch } from 'vue'
import { NSelect, type SelectOption } from 'naive-ui'
import { searchStocks, type StockSearchItem } from '@/api/stockSearch'
import type { StockRef } from '@/composables/useStockActions'
import StockIdentity from './StockIdentity.vue'

const props = withDefaults(
  defineProps<{
    modelValue?: StockRef | null
    placeholder?: string
    disabled?: boolean
    clearable?: boolean
  }>(),
  {
    modelValue: null,
    placeholder: '搜索股票名称、代码或拼音',
    disabled: false,
    clearable: true,
  },
)
const emit = defineEmits<{
  (event: 'update:modelValue', value: StockRef | null): void
  (event: 'select', value: StockRef): void
}>()

interface StockOption extends SelectOption {
  stock: StockRef
}

const loading = ref(false)
const options = ref<StockOption[]>([])
let requestSeq = 0

function stockKey(stock: Pick<StockRef, 'symbol' | 'market'>): string {
  return `${stock.market || 'cn'}:${stock.symbol}`
}

function toOption(stock: StockSearchItem | StockRef): StockOption {
  return {
    label: `${stock.name?.trim() || '名称待补全'} ${stock.symbol}`,
    value: stockKey(stock),
    stock: { symbol: stock.symbol, market: stock.market || 'cn', name: stock.name || '' },
  }
}

watch(
  () => props.modelValue,
  (value) => {
    if (!value?.symbol) return
    const key = stockKey(value)
    if (!options.value.some((option) => option.value === key)) options.value = [toOption(value), ...options.value]
  },
  { immediate: true, deep: true },
)

const selectedValue = computed(() => (props.modelValue?.symbol ? stockKey(props.modelValue) : null))

async function handleSearch(query: string) {
  const keyword = query.trim()
  const seq = ++requestSeq
  if (!keyword) {
    options.value = props.modelValue?.symbol ? [toOption(props.modelValue)] : []
    return
  }
  loading.value = true
  try {
    const result = await searchStocks(keyword, 20)
    if (seq !== requestSeq) return
    const next = result.items.map(toOption)
    if (props.modelValue?.symbol && !next.some((option) => option.value === selectedValue.value)) {
      next.unshift(toOption(props.modelValue))
    }
    options.value = next
  } catch {
    if (seq === requestSeq) options.value = props.modelValue?.symbol ? [toOption(props.modelValue)] : []
  } finally {
    if (seq === requestSeq) loading.value = false
  }
}

function handleUpdate(value: string | null) {
  if (!value) {
    emit('update:modelValue', null)
    return
  }
  const option = options.value.find((item) => item.value === value)
  if (!option) return
  emit('update:modelValue', option.stock)
  emit('select', option.stock)
}

function renderLabel(option: SelectOption) {
  const stock = (option as StockOption).stock
  if (!stock) return String(option.label || '')
  return h(StockIdentity, {
    symbol: stock.symbol,
    market: stock.market,
    name: stock.name,
    density: 'compact',
  })
}
</script>

<template>
  <n-select
    :value="selectedValue"
    :options="options"
    :loading="loading"
    :placeholder="placeholder"
    :disabled="disabled"
    :clearable="clearable"
    :render-label="renderLabel"
    filterable
    remote
    @search="handleSearch"
    @update:value="handleUpdate"
  />
</template>
