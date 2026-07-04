<script setup lang="ts">
import { ref, computed, watch, nextTick, onMounted, onUnmounted } from 'vue'
import { NModal, NInput, NButton, NSpin, NEmpty, useMessage } from 'naive-ui'
import { getQuote, type Quote } from '@/api/market'
import { useUi } from '@/composables/useUi'
import { useStockActions } from '@/composables/useStockActions'
import ChangeTag from './ChangeTag.vue'

// 全局速查命令面板：Ctrl/Cmd+K 唤起，精确代码查行情 + 快捷动作直达各功能页。
// 后端无模糊搜索接口，仅支持精确代码（自用足够）；市场固定 A 股（与全站现状一致）。
const props = defineProps<{ show: boolean }>()
const emit = defineEmits<{ (e: 'update:show', v: boolean): void }>()

const message = useMessage()
const { vars, pctColor, upColor, downColor } = useUi()
const { adding, goAnalysis, goQa, goCompare, goAlert, addToWatchlist } = useStockActions(() =>
  emit('update:show', false),
)

const keyword = ref('')
const quote = ref<Quote | null>(null)
const loading = ref(false)
const inputRef = ref<InstanceType<typeof NInput> | null>(null)

const panelVars = computed(() => ({
  '--gs-bg': vars.value.cardColor,
  '--gs-divider': vars.value.dividerColor,
}))

watch(
  () => props.show,
  async (v) => {
    if (v) {
      await nextTick()
      inputRef.value?.focus()
    } else {
      keyword.value = ''
      quote.value = null
    }
  },
)

function onKeydown(e: KeyboardEvent) {
  if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') {
    e.preventDefault()
    emit('update:show', !props.show)
  }
}
onMounted(() => window.addEventListener('keydown', onKeydown))
onUnmounted(() => window.removeEventListener('keydown', onKeydown))

async function search() {
  const code = keyword.value.trim()
  if (!code) return
  loading.value = true
  quote.value = null
  try {
    quote.value = await getQuote('cn', code)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

function fmt(n: number) {
  return n.toFixed(2)
}
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
        <svg class="gs-icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <circle cx="11" cy="11" r="7" />
          <path d="m20 20-3.5-3.5" stroke-linecap="round" />
        </svg>
        <n-input
          ref="inputRef"
          v-model:value="keyword"
          class="gs-input"
          placeholder="输入股票代码回车查询，如 600519（仅 A 股）"
          :bordered="false"
          size="large"
          @keyup.enter="search"
        />
        <span class="gs-kbd">ESC</span>
      </div>

      <div class="gs-body">
        <n-spin :show="loading">
          <template v-if="quote">
            <div class="gs-head">
              <div class="gs-name">
                <span class="gs-title">{{ quote.name }}</span>
                <span class="gs-symbol qv-mono">{{ quote.symbol }}</span>
              </div>
              <div class="gs-price-row">
                <span class="gs-price qv-figure" :style="{ color: pctColor(quote.change_pct) }">
                  {{ fmt(quote.price) }}
                </span>
                <ChangeTag :value="quote.change_pct" />
              </div>
            </div>
            <div class="gs-grid">
              <div class="gs-cell">
                <span class="gs-k">今开</span>
                <span class="gs-v qv-tnum">{{ fmt(quote.open) }}</span>
              </div>
              <div class="gs-cell">
                <span class="gs-k">昨收</span>
                <span class="gs-v qv-tnum">{{ fmt(quote.prev_close) }}</span>
              </div>
              <div class="gs-cell">
                <span class="gs-k">最高</span>
                <span class="gs-v qv-tnum" :style="{ color: upColor }">{{ fmt(quote.high) }}</span>
              </div>
              <div class="gs-cell">
                <span class="gs-k">最低</span>
                <span class="gs-v qv-tnum" :style="{ color: downColor }">{{ fmt(quote.low) }}</span>
              </div>
            </div>
            <div class="gs-actions">
              <n-button size="small" secondary @click="goAnalysis(quote)">AI 分析</n-button>
              <n-button size="small" secondary @click="goQa(quote)">个股问答</n-button>
              <n-button size="small" secondary @click="goCompare(quote)">横向对比</n-button>
              <n-button size="small" secondary :loading="adding" @click="addToWatchlist(quote)">+ 自选</n-button>
              <n-button size="small" secondary @click="goAlert(quote)">设提醒</n-button>
            </div>
          </template>
          <n-empty
            v-else-if="!loading"
            class="gs-empty"
            description="查到行情后可直达 AI 分析 / 问答 / 对比 / 自选 / 提醒"
          />
        </n-spin>
      </div>
    </div>
  </n-modal>
</template>

<style scoped>
.gs-panel {
  width: min(560px, calc(100vw - 32px));
  margin-bottom: 26vh; /* 面板整体上移，命令面板惯例位 */
  border-radius: 16px;
  background: var(--gs-bg);
  border: 1px solid var(--gs-divider);
  box-shadow: 0 24px 64px rgba(0, 0, 0, 0.24);
  overflow: hidden;
}
.gs-input-row {
  display: flex;
  align-items: center;
  gap: 4px;
  padding: 6px 14px;
  border-bottom: 1px solid var(--gs-divider);
}
.gs-icon {
  width: 18px;
  height: 18px;
  opacity: 0.5;
  flex-shrink: 0;
}
.gs-input {
  flex: 1;
}
.gs-kbd {
  flex-shrink: 0;
  font-size: 11px;
  padding: 2px 6px;
  border-radius: 5px;
  border: 1px solid var(--gs-divider);
  opacity: 0.55;
}
.gs-body {
  padding: 16px;
  min-height: 132px;
}
.gs-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.gs-name {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}
.gs-title {
  font-size: 17px;
  font-weight: 700;
}
.gs-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.gs-price-row {
  display: flex;
  align-items: center;
  gap: 10px;
}
.gs-price {
  font-size: 24px;
  font-weight: 700;
}
.gs-grid {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 10px;
  margin-top: 14px;
}
.gs-cell {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.gs-k {
  font-size: 11px;
  opacity: 0.55;
}
.gs-v {
  font-size: 14px;
  font-weight: 500;
}
.gs-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px solid var(--gs-divider);
}
.gs-empty {
  padding: 18px 0;
}

@media (max-width: 480px) {
  .gs-grid {
    grid-template-columns: repeat(2, 1fr);
  }
  .gs-kbd {
    display: none;
  }
}
</style>
