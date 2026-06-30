<script setup lang="ts">
import { ref, onMounted, nextTick } from 'vue'
import {
  NCard,
  NSpace,
  NInput,
  NButton,
  NStatistic,
  NGrid,
  NGi,
  NText,
  NAlert,
  useMessage,
} from 'naive-ui'
import * as echarts from 'echarts'
import { getQuote, getDailyBars, type Quote, type Bar } from '@/api/market'

const message = useMessage()

const market = ref('cn')
const symbol = ref('600000')
const quote = ref<Quote | null>(null)
const loading = ref(false)
const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null

async function load() {
  if (!symbol.value.trim()) {
    message.warning('请输入股票代码')
    return
  }
  loading.value = true
  try {
    quote.value = await getQuote(market.value, symbol.value.trim())
    const bars = await getDailyBars(market.value, symbol.value.trim(), 120)
    await nextTick()
    renderChart(bars)
  } catch (e) {
    quote.value = null
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

function renderChart(bars: Bar[]) {
  if (!chartEl.value) return
  if (!chart) chart = echarts.init(chartEl.value, 'dark')
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis' },
    grid: { left: 50, right: 20, top: 20, bottom: 40 },
    xAxis: { type: 'category', data: bars.map((b) => b.trade_date), boundaryGap: false },
    yAxis: { type: 'value', scale: true },
    series: [
      {
        type: 'candlestick',
        data: bars.map((b) => [b.open, b.close, b.low, b.high]),
        itemStyle: { color: '#ef4444', color0: '#22c55e', borderColor: '#ef4444', borderColor0: '#22c55e' },
      },
    ],
  })
}

function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}

onMounted(() => {
  load()
  window.addEventListener('resize', () => chart?.resize())
})
</script>

<template>
  <n-space vertical :size="16">
    <n-card title="行情查询（阶段 0 端到端验证）" size="small">
      <n-space>
        <n-input v-model:value="symbol" placeholder="股票代码，如 600000" style="width: 200px" @keyup.enter="load" />
        <n-button type="primary" :loading="loading" @click="load">查询</n-button>
        <n-text depth="3">数据源：东方财富（主）/ 新浪（备）· 仅 A 股已打通</n-text>
      </n-space>
    </n-card>

    <n-card v-if="quote" :title="`${quote.name} ${quote.symbol}`" size="small">
      <template #header-extra>
        <n-text depth="3">
          来源 {{ quote.source }} · 数据时间 {{ new Date(quote.data_time).toLocaleString() }}
        </n-text>
      </template>
      <n-grid :cols="4" :x-gap="12" :y-gap="12">
        <n-gi>
          <n-statistic label="现价" :value="fmt(quote.price)" />
        </n-gi>
        <n-gi>
          <n-statistic label="涨跌幅(%)">
            <n-text :type="quote.change_pct >= 0 ? 'error' : 'success'">{{ fmt(quote.change_pct) }}</n-text>
          </n-statistic>
        </n-gi>
        <n-gi><n-statistic label="今开" :value="fmt(quote.open)" /></n-gi>
        <n-gi><n-statistic label="昨收" :value="fmt(quote.prev_close)" /></n-gi>
        <n-gi><n-statistic label="最高" :value="fmt(quote.high)" /></n-gi>
        <n-gi><n-statistic label="最低" :value="fmt(quote.low)" /></n-gi>
        <n-gi><n-statistic label="成交量(手)" :value="quote.volume" /></n-gi>
        <n-gi><n-statistic label="成交额" :value="(quote.amount / 1e8).toFixed(2) + ' 亿'" /></n-gi>
      </n-grid>
    </n-card>

    <n-card title="日线（前复权，近 120 交易日）" size="small">
      <div ref="chartEl" style="width: 100%; height: 360px"></div>
    </n-card>

    <n-alert type="warning" title="风险提示">
      本内容仅供研究参考，不构成投资建议。AI 可能出错，数据可能延迟或不完整，投资决策需由用户自行承担风险。
    </n-alert>
  </n-space>
</template>
