<script setup lang="ts">
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { NButton, NEmpty, NInput, NModal, NSpin, NTag } from 'naive-ui'
import { searchStocks, type StockSearchItem } from '@/api/stockSearch'
import { useStockActions, type StockActionKey, type StockRef } from '@/composables/useStockActions'
import {
  clearRecentStocks,
  readRecentStocks,
  type RecentStock,
} from '@/composables/useRecentStocks'
import { useAuthStore } from '@/stores/auth'
import { useUi } from '@/composables/useUi'
import StockActionMenu from './StockActionMenu.vue'

const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'update:show', value: boolean): void }>()

const auth = useAuthStore()
const { vars, primaryAlpha } = useUi()
const { goDetail } = useStockActions(() => emit('update:show', false))
const keyword = ref('')
const results = ref<StockSearchItem[]>([])
const recentStocks = ref<RecentStock[]>([])
const loading = ref(false)
const errorMessage = ref('')
const selectedIndex = ref(0)
const inputRef = ref<InstanceType<typeof NInput> | null>(null)

type DisplayStock = StockSearchItem | RecentStock

const normalizedKeyword = computed(() => keyword.value.trim())
const showingRecent = computed(() => normalizedKeyword.value === '')
const displayItems = computed<DisplayStock[]>(() =>
  showingRecent.value ? recentStocks.value : results.value,
)
const activeDescendant = computed(() =>
  displayItems.value[selectedIndex.value] ? optionID(displayItems.value[selectedIndex.value]) : undefined,
)
const panelVars = computed(() => ({
  '--gs-bg': vars.value.cardColor,
  '--gs-divider': vars.value.dividerColor,
  '--gs-primary': vars.value.primaryColor,
  '--gs-active': primaryAlpha(0.1),
  '--gs-muted': vars.value.textColor3,
}))

let debounceTimer: ReturnType<typeof setTimeout> | null = null
let searchAbort: AbortController | null = null
let searchSeq = 0

function close() {
  emit('update:show', false)
}

function cancelSearch() {
  searchSeq++
  if (debounceTimer) clearTimeout(debounceTimer)
  debounceTimer = null
  searchAbort?.abort()
  searchAbort = null
}

function reloadRecent() {
  recentStocks.value = readRecentStocks(auth.user?.id || 0)
}

function resetSearch() {
  cancelSearch()
  keyword.value = ''
  results.value = []
  errorMessage.value = ''
  loading.value = false
  selectedIndex.value = 0
}

watch(
  () => props.show,
  async (show) => {
    if (!show) {
      resetSearch()
      return
    }
    reloadRecent()
    await nextTick()
    inputRef.value?.focus()
  },
)

watch(
  () => auth.user?.id,
  () => {
    resetSearch()
    reloadRecent()
  },
)

watch(keyword, (value) => {
  cancelSearch()
  results.value = []
  errorMessage.value = ''
  selectedIndex.value = 0
  const query = value.trim()
  if (!query) {
    loading.value = false
    reloadRecent()
    return
  }
  loading.value = true
  debounceTimer = setTimeout(() => void performSearch(query), 250)
})

async function performSearch(query = normalizedKeyword.value) {
  if (!query) return
  if (debounceTimer) clearTimeout(debounceTimer)
  debounceTimer = null
  searchAbort?.abort()
  const controller = new AbortController()
  searchAbort = controller
  const mySeq = ++searchSeq
  loading.value = true
  errorMessage.value = ''
  try {
    const result = await searchStocks(query, 20, controller.signal)
    if (mySeq !== searchSeq || query !== normalizedKeyword.value) return
    results.value = result.items || []
    selectedIndex.value = 0
  } catch (e) {
    if (mySeq !== searchSeq || controller.signal.aborted) return
    errorMessage.value = (e as Error).message || '搜索失败，请重试'
    results.value = []
  } finally {
    if (mySeq === searchSeq) {
      loading.value = false
      searchAbort = null
    }
  }
}

function onGlobalKeydown(event: KeyboardEvent) {
  if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === 'k') {
    if (event.repeat) return
    event.preventDefault()
    emit('update:show', !props.show)
    return
  }
  if (props.show && event.key === 'Escape' && !event.defaultPrevented) {
    event.preventDefault()
    close()
  }
}

function onInputKeydown(event: KeyboardEvent) {
  if (event.isComposing || event.keyCode === 229) return
  const count = displayItems.value.length
  if (event.key === 'Escape') {
    event.preventDefault()
    close()
    return
  }
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    event.preventDefault()
    if (!count) return
    const direction = event.key === 'ArrowDown' ? 1 : -1
    selectedIndex.value = (selectedIndex.value + direction + count) % count
    void nextTick(() => {
      document.getElementById(activeDescendant.value || '')?.scrollIntoView({ block: 'nearest' })
    })
    return
  }
  if (event.key === 'Enter') {
    event.preventDefault()
    const selected = displayItems.value[selectedIndex.value]
    if (selected) {
      void openStock(selected)
    } else if (normalizedKeyword.value && !loading.value) {
      void performSearch()
    }
  }
}

async function openStock(stock: StockRef) {
  await goDetail(stock)
}

function clearHistory() {
  clearRecentStocks(auth.user?.id || 0)
  recentStocks.value = []
  selectedIndex.value = 0
}

function onActionCompleted(
  stock: DisplayStock,
  event: { action: StockActionKey; success: boolean },
) {
  if (!event.success) return
  reloadRecent()
  if (event.action === 'watchlist' && 'in_watchlist' in stock) stock.in_watchlist = true
}

function optionID(stock: DisplayStock) {
  return `gs-stock-${stock.market}-${stock.symbol}`.replace(/[^a-zA-Z0-9_-]/g, '-')
}

function marketLabel(market: string) {
  return ({ cn: 'A 股', hk: '港股', us: '美股' } as Record<string, string>)[market.toLowerCase()] || market.toUpperCase()
}

function visitTime(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return ''
  return date.toLocaleString('zh-CN', {
    month: '2-digit',
    day: '2-digit',
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  })
}

onMounted(() => window.addEventListener('keydown', onGlobalKeydown))
onUnmounted(() => {
  window.removeEventListener('keydown', onGlobalKeydown)
  cancelSearch()
})
</script>

<template>
  <n-modal
    :show="show"
    :auto-focus="false"
    transform-origin="center"
    @update:show="emit('update:show', $event)"
  >
    <div class="gs-panel" :style="panelVars">
      <div class="gs-input-row">
        <svg class="gs-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
          <circle cx="11" cy="11" r="7" />
          <path d="m20 20-3.5-3.5" stroke-linecap="round" />
        </svg>
        <n-input
          ref="inputRef"
          v-model:value="keyword"
          class="gs-input"
          placeholder="搜索名称、代码或拼音"
          :maxlength="64"
          :bordered="false"
          size="large"
          aria-label="搜索股票"
          aria-controls="gs-stock-list"
          :aria-activedescendant="activeDescendant"
          @keydown="onInputKeydown"
        />
      </div>

      <div class="gs-body">
        <div v-if="normalizedKeyword && loading" class="gs-state" aria-live="polite">
          <n-spin size="small" />
          <span>正在搜索</span>
        </div>

        <div v-else-if="normalizedKeyword && errorMessage" class="gs-state gs-error" role="alert">
          <span class="gs-state-text">{{ errorMessage }}</span>
          <n-button size="small" secondary @click="performSearch()">重试</n-button>
        </div>

        <template v-else-if="normalizedKeyword && results.length">
          <div class="gs-section-head">
            <span>搜索结果</span>
            <span class="gs-count">{{ results.length }} 项</span>
          </div>
          <div id="gs-stock-list" class="gs-list" role="listbox" aria-label="股票搜索结果">
            <div
              v-for="(stock, index) in results"
              :id="optionID(stock)"
              :key="`${stock.market}:${stock.symbol}`"
              class="gs-result-row"
              :class="{ 'is-active': index === selectedIndex }"
              role="option"
              :aria-selected="index === selectedIndex"
              @mouseenter="selectedIndex = index"
              @click="openStock(stock)"
            >
              <div class="gs-result-main">
                <div class="gs-primary-line">
                  <span class="gs-name" :title="stock.name">{{ stock.name }}</span>
                  <span class="gs-symbol qv-mono">{{ stock.symbol }}</span>
                </div>
                <div class="gs-meta-line">
                  <span>{{ marketLabel(stock.market) }}</span>
                  <span v-if="stock.industry" class="gs-industry" :title="stock.industry">{{ stock.industry }}</span>
                  <span>{{ stock.as_of ? `截至 ${stock.as_of}` : '数据日期未知' }}</span>
                </div>
                <div v-if="stock.in_watchlist || stock.has_position" class="gs-relations">
                  <n-tag v-if="stock.in_watchlist" size="small" :bordered="false">自选</n-tag>
                  <n-tag v-if="stock.has_position" size="small" type="success" :bordered="false">持仓</n-tag>
                </div>
              </div>
              <StockActionMenu
                :stock="stock"
                :watchlist-state="stock.in_watchlist ? 'in' : 'out'"
                :position-state="stock.has_position ? 'held' : 'none'"
                @close="close"
                @completed="onActionCompleted(stock, $event)"
              />
            </div>
          </div>
        </template>

        <n-empty
          v-else-if="normalizedKeyword"
          class="gs-empty"
          description="没有找到相关股票"
        />

        <template v-else>
          <div class="gs-section-head">
            <span>最近访问</span>
            <n-button v-if="recentStocks.length" size="tiny" quaternary @click="clearHistory">清空</n-button>
          </div>
          <div v-if="recentStocks.length" id="gs-stock-list" class="gs-list" role="listbox" aria-label="最近访问股票">
            <div
              v-for="(stock, index) in recentStocks"
              :id="optionID(stock)"
              :key="`${stock.market}:${stock.symbol}`"
              class="gs-result-row"
              :class="{ 'is-active': index === selectedIndex }"
              role="option"
              :aria-selected="index === selectedIndex"
              @mouseenter="selectedIndex = index"
              @click="openStock(stock)"
            >
              <div class="gs-result-main">
                <div class="gs-primary-line">
                  <span class="gs-name" :title="stock.name">{{ stock.name }}</span>
                  <span class="gs-symbol qv-mono">{{ stock.symbol }}</span>
                </div>
                <div class="gs-meta-line">
                  <span>{{ marketLabel(stock.market) }}</span>
                  <span>最近于 {{ visitTime(stock.lastVisitedAt) }}</span>
                </div>
              </div>
              <StockActionMenu
                :stock="stock"
                @close="close"
                @completed="onActionCompleted(stock, $event)"
              />
            </div>
          </div>
          <n-empty v-else class="gs-empty" description="暂无最近访问" />
        </template>
      </div>
    </div>
  </n-modal>
</template>

<style scoped>
.gs-panel {
  width: min(640px, calc(100vw - 32px));
  max-height: min(620px, calc(100dvh - 64px));
  margin-bottom: 12vh;
  border: 1px solid var(--gs-divider);
  border-radius: 8px;
  background: var(--gs-bg);
  box-shadow: 0 20px 56px rgba(0, 0, 0, 0.24);
  overflow: hidden;
}
.gs-input-row {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
  padding: 7px 14px;
  border-bottom: 1px solid var(--gs-divider);
}
.gs-icon {
  width: 18px;
  height: 18px;
  flex: 0 0 auto;
  opacity: 0.55;
}
.gs-input {
  min-width: 0;
  flex: 1;
}
.gs-body {
  min-height: 154px;
  max-height: min(500px, calc(100dvh - 142px));
  overflow-y: auto;
  overscroll-behavior: contain;
  padding: 10px;
}
.gs-section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  min-height: 30px;
  padding: 0 8px 6px;
  font-size: 12px;
  font-weight: 600;
  color: var(--gs-muted);
}
.gs-count {
  font-variant-numeric: tabular-nums;
  font-weight: 400;
}
.gs-list {
  display: grid;
  gap: 2px;
}
.gs-result-row {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  align-items: center;
  gap: 10px;
  min-height: 70px;
  padding: 9px 8px 9px 11px;
  border-left: 3px solid transparent;
  border-radius: 6px;
  cursor: pointer;
  outline: none;
}
.gs-result-row.is-active {
  border-left-color: var(--gs-primary);
  background: var(--gs-active);
}
.gs-result-main {
  min-width: 0;
}
.gs-primary-line {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}
.gs-name {
  min-width: 0;
  overflow: hidden;
  font-size: 15px;
  font-weight: 650;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.gs-symbol {
  flex: 0 0 auto;
  font-size: 12px;
  color: var(--gs-muted);
}
.gs-meta-line {
  display: flex;
  flex-wrap: wrap;
  gap: 3px 10px;
  min-width: 0;
  margin-top: 4px;
  font-size: 11px;
  color: var(--gs-muted);
}
.gs-meta-line > span {
  min-width: 0;
  overflow-wrap: anywhere;
}
.gs-industry {
  max-width: 180px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.gs-relations {
  display: flex;
  flex-wrap: wrap;
  gap: 5px;
  margin-top: 6px;
}
.gs-state {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: 10px;
  min-height: 132px;
  padding: 16px;
  color: var(--gs-muted);
}
.gs-error {
  flex-direction: column;
  text-align: center;
}
.gs-state-text {
  max-width: 100%;
  overflow-wrap: anywhere;
}
.gs-empty {
  padding: 28px 0;
}

@media (max-width: 480px) {
  .gs-panel {
    width: calc(100vw - 16px);
    max-height: calc(100dvh - 24px);
    margin-bottom: 0;
  }
  .gs-input-row {
    padding-inline: 10px;
  }
  .gs-body {
    max-height: calc(100dvh - 94px);
    padding: 8px 6px;
  }
  .gs-result-row {
    gap: 4px;
    padding-inline: 8px 4px;
  }
  .gs-primary-line {
    flex-wrap: wrap;
    gap: 2px 7px;
  }
  .gs-name {
    max-width: 100%;
    white-space: normal;
    overflow-wrap: anywhere;
  }
  .gs-industry {
    max-width: 120px;
  }
}
</style>
