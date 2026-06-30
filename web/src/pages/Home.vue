<script setup lang="ts">
import { ref, onMounted, nextTick, watch, computed } from 'vue'
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
  NTable,
  NTag,
  NEmpty,
  NSpin,
  useMessage,
} from 'naive-ui'
import * as echarts from 'echarts'
import { storeToRefs } from 'pinia'
import {
  getQuote,
  getDailyBars,
  getOverview,
  type Quote,
  type Bar,
  type Overview,
} from '@/api/market'
import { useThemeStore } from '@/stores/theme'

const message = useMessage()
const { isDark } = storeToRefs(useThemeStore())

// ---------- 市场概览 ----------
const overview = ref<Overview | null>(null)
const ovLoading = ref(false)

async function loadOverview() {
  ovLoading.value = true
  try {
    overview.value = await getOverview('cn')
  } catch (e) {
    message.error('市场概览加载失败：' + (e as Error).message)
  } finally {
    ovLoading.value = false
  }
}

// ---------- 个股速查 ----------
const symbol = ref('600000')
const quote = ref<Quote | null>(null)
const loading = ref(false)
const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const lastBars = ref<Bar[]>([])

async function loadStock() {
  if (!symbol.value.trim()) {
    message.warning('请输入股票代码')
    return
  }
  loading.value = true
  try {
    quote.value = await getQuote('cn', symbol.value.trim())
    const bars = await getDailyBars('cn', symbol.value.trim(), 120)
    lastBars.value = bars
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
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
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

watch(isDark, () => {
  if (lastBars.value.length) renderChart(lastBars.value)
})

// ---------- 展示辅助（涨红跌绿用语义色，跟随主题）----------
function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtPct(n: number) {
  return (n >= 0 ? '+' : '') + n.toFixed(2) + '%'
}
function pctType(n: number): 'error' | 'success' | 'default' {
  return n > 0 ? 'error' : n < 0 ? 'success' : 'default'
}
function fmtAmount(n: number) {
  if (!n) return '-'
  return (n / 1e8).toFixed(2) + ' 亿'
}

const sectorsUnavailable = computed(() => !!overview.value?.errors?.sectors)

onMounted(() => {
  loadOverview()
  loadStock()
  window.addEventListener('resize', () => chart?.resize())
})
</script>

<template>
  <n-space vertical :size="16">
    <!-- 指数概览 -->
    <n-card size="small">
      <template #header>
        <n-space align="center" :size="10">
          <span>指数概览</span>
          <n-text depth="3" style="font-size: 12px" v-if="overview">
            更新 {{ new Date(overview.data_time).toLocaleTimeString() }}
          </n-text>
        </n-space>
      </template>
      <template #header-extra>
        <n-button size="tiny" :loading="ovLoading" @click="loadOverview">刷新</n-button>
      </template>
      <n-spin :show="ovLoading && !overview">
        <n-grid v-if="overview?.indices?.length" :cols="3" :x-gap="12" :y-gap="12" responsive="screen">
          <n-gi v-for="ix in overview.indices" :key="ix.code">
            <n-space vertical :size="2">
              <n-text depth="2" style="font-size: 13px">{{ ix.name }}</n-text>
              <n-space align="baseline" :size="8">
                <n-text :type="pctType(ix.change_pct)" style="font-size: 20px; font-weight: 600">
                  {{ fmt(ix.price) }}
                </n-text>
                <n-text :type="pctType(ix.change_pct)">{{ fmtPct(ix.change_pct) }}</n-text>
              </n-space>
            </n-space>
          </n-gi>
        </n-grid>
        <n-empty v-else description="指数数据暂不可用" />
      </n-spin>
    </n-card>

    <!-- 涨幅榜 + 热门(成交额)榜 -->
    <n-grid :cols="2" :x-gap="16" :y-gap="16" responsive="screen" item-responsive>
      <n-gi span="2 m:1">
        <n-card title="涨幅榜" size="small">
          <n-table v-if="overview?.gainers?.length" size="small" :bordered="false" :single-line="false">
            <thead>
              <tr><th>名称</th><th style="text-align: right">现价</th><th style="text-align: right">涨跌幅</th><th style="text-align: right">成交额</th></tr>
            </thead>
            <tbody>
              <tr v-for="s in overview.gainers" :key="s.symbol">
                <td>{{ s.name }} <n-text depth="3" style="font-size: 12px">{{ s.symbol }}</n-text></td>
                <td style="text-align: right">{{ fmt(s.price) }}</td>
                <td style="text-align: right"><n-text :type="pctType(s.change_pct)">{{ fmtPct(s.change_pct) }}</n-text></td>
                <td style="text-align: right">{{ fmtAmount(s.amount) }}</td>
              </tr>
            </tbody>
          </n-table>
          <n-empty v-else description="暂不可用" />
        </n-card>
      </n-gi>
      <n-gi span="2 m:1">
        <n-card title="热门榜（成交额）" size="small">
          <n-table v-if="overview?.actives?.length" size="small" :bordered="false" :single-line="false">
            <thead>
              <tr><th>名称</th><th style="text-align: right">现价</th><th style="text-align: right">涨跌幅</th><th style="text-align: right">成交额</th></tr>
            </thead>
            <tbody>
              <tr v-for="s in overview.actives" :key="s.symbol">
                <td>{{ s.name }} <n-text depth="3" style="font-size: 12px">{{ s.symbol }}</n-text></td>
                <td style="text-align: right">{{ fmt(s.price) }}</td>
                <td style="text-align: right"><n-text :type="pctType(s.change_pct)">{{ fmtPct(s.change_pct) }}</n-text></td>
                <td style="text-align: right">{{ fmtAmount(s.amount) }}</td>
              </tr>
            </tbody>
          </n-table>
          <n-empty v-else description="暂不可用" />
        </n-card>
      </n-gi>
    </n-grid>

    <!-- 板块榜 + 市场情绪 -->
    <n-grid :cols="2" :x-gap="16" :y-gap="16" responsive="screen" item-responsive>
      <n-gi span="2 m:1">
        <n-card title="板块涨跌榜" size="small">
          <template #header-extra v-if="sectorsUnavailable">
            <n-tag size="small" type="warning" round>数据源繁忙</n-tag>
          </template>
          <n-table v-if="overview?.sectors?.length" size="small" :bordered="false" :single-line="false">
            <thead>
              <tr><th>板块</th><th style="text-align: right">涨跌幅</th><th>领涨</th></tr>
            </thead>
            <tbody>
              <tr v-for="s in overview.sectors" :key="s.code">
                <td>{{ s.name }}</td>
                <td style="text-align: right"><n-text :type="pctType(s.change_pct)">{{ fmtPct(s.change_pct) }}</n-text></td>
                <td>{{ s.leader || '-' }}</td>
              </tr>
            </tbody>
          </n-table>
          <n-empty v-else description="板块榜依赖东财接口，当前限流暂不可用，稍后重试" />
        </n-card>
      </n-gi>
      <n-gi span="2 m:1">
        <n-card title="市场情绪" size="small">
          <n-empty description="涨跌家数 / 涨跌停 / 波动率（待数据源接入，阶段 2+）" />
        </n-card>
      </n-gi>
    </n-grid>

    <!-- 个股速查 -->
    <n-card title="个股速查" size="small">
      <n-space vertical :size="12">
        <n-space>
          <n-input v-model:value="symbol" placeholder="股票代码，如 600000" style="width: 200px" @keyup.enter="loadStock" />
          <n-button type="primary" :loading="loading" @click="loadStock">查询</n-button>
          <n-text depth="3">数据源：东财（主）/ 新浪（备）· 仅 A 股已打通</n-text>
        </n-space>

        <n-grid v-if="quote" :cols="4" :x-gap="12" :y-gap="12">
          <n-gi><n-statistic label="现价" :value="fmt(quote.price)" /></n-gi>
          <n-gi>
            <n-statistic label="涨跌幅(%)">
              <n-text :type="pctType(quote.change_pct)">{{ fmt(quote.change_pct) }}</n-text>
            </n-statistic>
          </n-gi>
          <n-gi><n-statistic label="今开" :value="fmt(quote.open)" /></n-gi>
          <n-gi><n-statistic label="昨收" :value="fmt(quote.prev_close)" /></n-gi>
          <n-gi><n-statistic label="最高" :value="fmt(quote.high)" /></n-gi>
          <n-gi><n-statistic label="最低" :value="fmt(quote.low)" /></n-gi>
          <n-gi><n-statistic label="成交量(手)" :value="quote.volume" /></n-gi>
          <n-gi><n-statistic label="成交额" :value="fmtAmount(quote.amount)" /></n-gi>
        </n-grid>

        <div ref="chartEl" style="width: 100%; height: 320px"></div>
      </n-space>
    </n-card>

    <!-- 占位 -->
    <n-card title="资金流 / 财经新闻 / AI 今日观点" size="small">
      <n-empty description="待阶段 4+ 接入（资金流向、新闻情绪、AI 市场摘要）" />
    </n-card>

    <n-alert type="warning" title="风险提示">
      本内容仅供研究参考，不构成投资建议。AI 可能出错，数据可能延迟或不完整，投资决策需由用户自行承担风险。
    </n-alert>
  </n-space>
</template>
