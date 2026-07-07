<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { NButton, NEmpty, NGi, NGrid, NResult, NSpin, NTag } from 'naive-ui'
import * as echarts from 'echarts'
import {
  getQuote,
  getDailyBars,
  getValuation,
  getScore,
  getIndicators,
  getChips,
  type Quote,
  type Bar,
  type Valuation,
  type StockScore,
  type IndicatorSeries,
  type ChipDist,
} from '@/api/market'
import { getNews, newsSourceLabel, sentimentTag, type NewsItem } from '@/api/news'
import { getAnnouncements, type AnnouncementItem } from '@/api/announcement'
import { getStockFinance, type StockFinance } from '@/api/finance'
import { useUi, withAlpha } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { useStockActions } from '@/composables/useStockActions'
import { isEtfSymbol } from '@/api/etf'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const route = useRoute()
const router = useRouter()
const { vars, isDark, pctColor } = useUi()
const { adding, goAnalysis, goQa, goCompare, goAlert, goThesis, addToWatchlist } = useStockActions()

const market = computed(() => String(route.params.market || 'cn'))
const symbol = computed(() => String(route.params.symbol || ''))
// ETF/场内基金无 PE/PB 个股估值指标（腾讯源返回 0 值），估值卡对基金隐藏不适用项。
const isFund = computed(() => market.value === 'cn' && isEtfSymbol(symbol.value))

const quote = ref<Quote | null>(null)
const valuation = ref<Valuation | null>(null)
const score = ref<StockScore | null>(null)
const bars = ref<Bar[]>([])
const indicators = ref<IndicatorSeries | null>(null)
const chips = ref<ChipDist | null>(null)
const news = ref<NewsItem[]>([])
const announcements = ref<AnnouncementItem[]>([])
const finance = ref<StockFinance | null>(null)

// 情绪标签（N2）：利好/利空才渲染，颜色随涨跌色主题。
function sentiView(n: NewsItem): { text: string; color: string } | null {
  const t = sentimentTag(n)
  return t ? { text: t.text, color: pctColor(t.dir) } : null
}
const loading = ref(false)
const loadError = ref('')

const stockRef = computed(() => ({
  symbol: symbol.value,
  market: market.value,
  name: quote.value?.name || symbol.value,
}))

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const chipEl = ref<HTMLDivElement | null>(null)
let chipChart: echarts.ECharts | null = null
const chipTrendEl = ref<HTMLDivElement | null>(null)
let chipTrendChart: echarts.ECharts | null = null
const finEl = ref<HTMLDivElement | null>(null)
let finChart: echarts.ECharts | null = null

async function load(silent = false) {
  if (!symbol.value) return
  if (!silent) {
    loading.value = true
    loadError.value = ''
  }
  try {
    quote.value = await getQuote(market.value, symbol.value)
    // 估值 / 评分 / 相关新闻 best-effort：失败只是不显示对应卡片。
    getValuation(market.value, symbol.value)
      .then((v) => (valuation.value = v))
      .catch(() => (valuation.value = null))
    getScore(market.value, symbol.value)
      .then((s) => (score.value = s))
      .catch(() => (score.value = null))
    getNews({ symbol: symbol.value, limit: 15 })
      .then((r) => (news.value = r))
      .catch(() => (news.value = []))
    getAnnouncements(symbol.value, 15)
      .then((r) => (announcements.value = r))
      .catch(() => (announcements.value = []))
    // 财务摘要（F2）best-effort：A 股非基金才有；容器在 v-if 内，等 DOM 后挂图。
    if (market.value === 'cn' && !isFund.value) {
      getStockFinance(market.value, symbol.value)
        .then((r) => {
          finance.value = r
          nextTick(() => renderFinanceChart())
        })
        .catch(() => (finance.value = null))
    }
    // 指标副图 / 筹码分布 best-effort：失败时 K 线退回单图、筹码卡显示占位。
    getIndicators(market.value, symbol.value, 120)
      .then((r) => {
        indicators.value = r
        renderChart()
      })
      .catch(() => (indicators.value = null))
    getChips(market.value, symbol.value)
      .then((r) => {
        chips.value = r
        // 筹码卡容器在 v-if 内，等 DOM 渲染后再挂图。
        nextTick(() => renderChipCharts())
      })
      .catch(() => (chips.value = null))
    const b = await getDailyBars(market.value, symbol.value, 120)
    bars.value = b
    renderChart()
  } catch (e) {
    if (!silent) {
      loadError.value = (e as Error).message
      quote.value = null
    }
  } finally {
    loading.value = false
  }
}

// alignByDate 把指标序列按交易日对齐到 K 线（两次独立请求可能相差末根，按日期匹配防画歪）。
function alignByDate(vals: (number | null)[]): (number | null)[] {
  const ind = indicators.value
  if (!ind) return []
  const m = new Map<string, number | null>()
  ind.dates.forEach((d, i) => m.set(d, vals[i] ?? null))
  return bars.value.map((b) => m.get(b.trade_date) ?? null)
}

function renderChart() {
  if (!chartEl.value || !bars.value.length) return
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  const up = vars.value.errorColor
  const down = vars.value.successColor
  const dates = bars.value.map((b) => b.trade_date)
  const kline = {
    type: 'candlestick' as const,
    name: '日K',
    data: bars.value.map((b) => [b.open, b.close, b.low, b.high]),
    itemStyle: { color: up, color0: down, borderColor: up, borderColor0: down },
  }

  // 指标未就绪/失败：退回单图 K 线。
  if (!indicators.value) {
    chart.setOption({
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
      grid: { left: 52, right: 16, top: 16, bottom: 36 },
      xAxis: { type: 'category', data: dates, boundaryGap: false },
      yAxis: { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
      series: [kline],
    })
    return
  }

  // 主图 K 线 + BOLL(20,2σ) 叠加，副图 MACD(12,26,9)（柱=2×(DIF−DEA) A 股口径）。
  const bollColor = vars.value.warningColor
  const midColor = vars.value.primaryColor
  const difColor = vars.value.primaryColor
  const deaColor = vars.value.warningColor
  const line = (name: string, data: (number | null)[], color: string, opacity = 1, extra: object = {}) => ({
    type: 'line' as const,
    name,
    data,
    symbol: 'none',
    lineStyle: { width: 1, color, opacity },
    itemStyle: { color },
    emphasis: { disabled: true },
    ...extra,
  })
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
    axisPointer: { link: [{ xAxisIndex: 'all' }] },
    legend: {
      top: 0,
      type: 'scroll',
      data: ['上轨', '中轨', '下轨', 'DIF', 'DEA', 'MACD'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: [
      { left: 52, right: 16, top: 26, height: '58%' },
      { left: 52, right: 16, top: '76%', height: '18%' },
    ],
    xAxis: [
      { type: 'category', data: dates, boundaryGap: false },
      {
        type: 'category',
        gridIndex: 1,
        data: dates,
        boundaryGap: false,
        axisLabel: { show: false },
        axisTick: { show: false },
      },
    ],
    yAxis: [
      { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
      { type: 'value', gridIndex: 1, scale: true, splitNumber: 2, splitLine: { show: false } },
    ],
    series: [
      kline,
      line('上轨', alignByDate(indicators.value.boll_up), bollColor, 0.65),
      line('中轨', alignByDate(indicators.value.boll_mid), midColor, 0.85),
      line('下轨', alignByDate(indicators.value.boll_low), bollColor, 0.65),
      {
        type: 'bar',
        name: 'MACD',
        xAxisIndex: 1,
        yAxisIndex: 1,
        data: alignByDate(indicators.value.hist),
        itemStyle: {
          color: (p: { value: number | null }) => ((p.value ?? 0) >= 0 ? up : down),
        },
        barWidth: '60%',
      },
      line('DIF', alignByDate(indicators.value.dif), difColor, 1, { xAxisIndex: 1, yAxisIndex: 1 }),
      line('DEA', alignByDate(indicators.value.dea), deaColor, 1, { xAxisIndex: 1, yAxisIndex: 1 }),
    ],
  })
}

// 筹码峰：横向分布（获利/套牢按现价分色）+ 获利比例近 90 日趋势迷你图。
function renderChipCharts() {
  const c = chips.value
  if (!c) return
  const up = vars.value.errorColor
  const down = vars.value.successColor
  if (chipEl.value) {
    chipChart?.dispose()
    chipChart = echarts.init(chipEl.value, isDark.value ? 'dark' : undefined)
    const profit: (number | null)[] = []
    const trapped: (number | null)[] = []
    c.prices.forEach((p, i) => {
      const v = Math.round(c.chips[i] * 10000) / 100 // 占比 %
      profit.push(p <= c.last_close ? v : null)
      trapped.push(p > c.last_close ? v : null)
    })
    chipChart.setOption({
      backgroundColor: 'transparent',
      tooltip: {
        trigger: 'axis',
        confine: true,
        formatter: (ps: { axisValue: string; value: number | null }[]) => {
          const row = ps.find((x) => x.value != null)
          return row ? `价位 ${row.axisValue}<br/>筹码占比 ${row.value}%` : ''
        },
      },
      grid: { left: 56, right: 12, top: 8, bottom: 22 },
      xAxis: {
        type: 'value',
        axisLabel: { formatter: '{value}%', fontSize: 10 },
        splitLine: { lineStyle: { opacity: 0.3 } },
      },
      yAxis: {
        type: 'category',
        data: c.prices.map((p) => p.toFixed(2)),
        axisLabel: { interval: 29, fontSize: 10 },
        axisTick: { show: false },
      },
      series: [
        { type: 'bar', name: '获利', stack: 'chip', data: profit, barCategoryGap: '0%', itemStyle: { color: up, opacity: 0.85 } },
        { type: 'bar', name: '套牢', stack: 'chip', data: trapped, barCategoryGap: '0%', itemStyle: { color: down, opacity: 0.85 } },
      ],
    })
  }
  if (chipTrendEl.value) {
    chipTrendChart?.dispose()
    chipTrendChart = echarts.init(chipTrendEl.value, isDark.value ? 'dark' : undefined)
    chipTrendChart.setOption({
      backgroundColor: 'transparent',
      tooltip: {
        trigger: 'axis',
        confine: true,
        formatter: (ps: { axisValue: string; value: number }[]) =>
          ps.length ? `${ps[0].axisValue}<br/>获利比例 ${ps[0].value}%` : '',
      },
      grid: { left: 4, right: 4, top: 4, bottom: 4 },
      xAxis: { type: 'category', data: c.days.map((d) => d.date), show: false },
      yAxis: { type: 'value', min: 0, max: 100, show: false },
      series: [
        {
          type: 'line',
          data: c.days.map((d) => d.profit),
          symbol: 'none',
          lineStyle: { width: 1.5, color: vars.value.primaryColor },
          areaStyle: { color: withAlpha(vars.value.primaryColor, 0.12) },
        },
      ],
    })
  }
}

// 财务摘要图（F2）：近 8 期营收/净利柱（左轴，亿元）+ ROE/毛利率线（右轴，%）。
function renderFinanceChart() {
  const inds = finance.value?.indicators
  if (!finEl.value || !inds?.length) return
  finChart?.dispose()
  finChart = echarts.init(finEl.value, isDark.value ? 'dark' : undefined)
  const up = vars.value.errorColor
  const primary = vars.value.primaryColor
  const warn = vars.value.warningColor
  const labels = inds.map((r) => r.report_name || r.report_date)
  finChart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', confine: true },
    legend: {
      top: 0,
      data: ['营收(亿)', '净利(亿)', 'ROE%', '毛利率%'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: { left: 56, right: 48, top: 30, bottom: 28 },
    xAxis: { type: 'category', data: labels, axisLabel: { fontSize: 10, interval: 0, rotate: labels.length > 5 ? 30 : 0 } },
    yAxis: [
      { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.3 } }, axisLabel: { fontSize: 10 } },
      { type: 'value', scale: true, splitLine: { show: false }, axisLabel: { formatter: '{value}%', fontSize: 10 } },
    ],
    series: [
      { type: 'bar', name: '营收(亿)', data: inds.map((r) => Math.round(r.revenue / 1e6) / 100), itemStyle: { color: withAlpha(primary, 0.75) }, barMaxWidth: 22 },
      { type: 'bar', name: '净利(亿)', data: inds.map((r) => Math.round(r.net_profit / 1e6) / 100), itemStyle: { color: withAlpha(up, 0.75) }, barMaxWidth: 22 },
      { type: 'line', name: 'ROE%', yAxisIndex: 1, data: inds.map((r) => r.roe), symbolSize: 5, lineStyle: { width: 2, color: warn }, itemStyle: { color: warn } },
      { type: 'line', name: '毛利率%', yAxisIndex: 1, data: inds.map((r) => r.gross_margin), symbolSize: 5, lineStyle: { width: 2, type: 'dashed', color: vars.value.infoColor }, itemStyle: { color: vars.value.infoColor } },
    ],
  })
}

const finLatest = computed(() => {
  const inds = finance.value?.indicators
  return inds?.length ? inds[inds.length - 1] : null
})

watch(isDark, () => {
  renderChart()
  renderChipCharts()
  renderFinanceChart()
})
// 同页跳转到另一只个股（如从对比/搜索进来）时整页重载。
watch([market, symbol], () => {
  valuation.value = null
  score.value = null
  indicators.value = null
  chips.value = null
  finance.value = null
  news.value = []
  announcements.value = []
  load()
})

onMounted(() => {
  load()
  window.addEventListener('resize', onResize)
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
  chipChart?.dispose()
  chipChart = null
  chipTrendChart?.dispose()
  chipTrendChart = null
  finChart?.dispose()
  finChart = null
})
function onResize() {
  chart?.resize()
  chipChart?.resize()
  chipTrendChart?.resize()
  finChart?.resize()
}
useAutoRefresh(() => load(true), 60_000)

function goPosition() {
  router.push({
    path: '/positions',
    query: { add: '1', symbol: symbol.value, market: market.value, name: quote.value?.name || '' },
  })
}

/* 展示辅助（口径与首页一致：量为手、额为元） */
function fmt(n: number | undefined | null) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtAmount(n: number | undefined) {
  if (!n) return '-'
  return (n / 1e8).toFixed(2) + ' 亿'
}
function fmtVol(n: number | undefined) {
  if (!n) return '-'
  return n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万手' : n + ' 手'
}
function fmtCap(n: number | undefined) {
  if (!n) return '-'
  return (n / 1e8).toFixed(0) + ' 亿'
}
function fmtTime(t: string | undefined) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : '-'
}
function fmtNewsTime(t: string) {
  const d = new Date(t)
  const p = (n: number) => String(n).padStart(2, '0')
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`
}

const scoreDims = computed(() => {
  const s = score.value
  if (!s) return []
  return [
    { label: '趋势', value: s.trend },
    { label: '动量', value: s.momentum },
    { label: '位置', value: s.position },
    { label: '量能', value: s.volume },
    { label: '风险(稳)', value: s.risk },
  ]
})
function scoreType(total: number) {
  if (total >= 60) return 'error' // 偏强用涨色
  if (total < 45) return 'success' // 偏弱用跌色
  return 'default'
}
</script>

<template>
  <PageContainer :title="quote ? `${quote.name} ${symbol}` : `个股详情 ${symbol}`" subtitle="行情 · K线 · 估值 · 评分一页看全">
    <n-result v-if="loadError" status="warning" title="行情获取失败" :description="loadError">
      <template #footer>
        <n-button @click="load()">重试</n-button>
        <n-button quaternary @click="router.back()">返回</n-button>
      </template>
    </n-result>

    <n-spin v-else :show="loading">
      <div v-if="quote" class="detail">
        <!-- 行情头：现价 + 关键字段 + 快捷动作 -->
        <SectionCard :hoverable="false">
          <div class="head">
            <div class="head-price">
              <span class="hp-price qv-figure" :style="{ color: pctColor(quote.change_pct) }">{{ fmt(quote.price) }}</span>
              <ChangeTag :value="quote.change_pct" />
              <n-tag v-if="valuation?.is_st" type="warning" size="small" round :bordered="false">ST</n-tag>
            </div>
            <div class="head-meta">
              <span>{{ quote.source }} · {{ fmtTime(quote.data_time) }}</span>
            </div>
          </div>
          <div class="quote-grid">
            <div class="qc"><span class="qc-k">今开</span><span class="qc-v qv-tnum">{{ fmt(quote.open) }}</span></div>
            <div class="qc"><span class="qc-k">最高</span><span class="qc-v qv-tnum" :style="{ color: pctColor(1) }">{{ fmt(quote.high) }}</span></div>
            <div class="qc"><span class="qc-k">最低</span><span class="qc-v qv-tnum" :style="{ color: pctColor(-1) }">{{ fmt(quote.low) }}</span></div>
            <div class="qc"><span class="qc-k">昨收</span><span class="qc-v qv-tnum">{{ fmt(quote.prev_close) }}</span></div>
            <div class="qc"><span class="qc-k">成交量</span><span class="qc-v qv-tnum">{{ fmtVol(quote.volume) }}</span></div>
            <div class="qc"><span class="qc-k">成交额</span><span class="qc-v qv-tnum">{{ fmtAmount(quote.amount) }}</span></div>
          </div>
          <div class="actions">
            <n-button size="small" secondary type="primary" @click="goAnalysis(stockRef)">AI 分析</n-button>
            <n-button size="small" secondary @click="goQa(stockRef)">个股问答</n-button>
            <n-button size="small" secondary @click="goCompare(stockRef)">横向对比</n-button>
            <n-button size="small" secondary @click="goAlert(stockRef)">设提醒</n-button>
            <n-button size="small" secondary @click="goThesis(stockRef)">逻辑卡</n-button>
            <n-button size="small" secondary :loading="adding" @click="addToWatchlist(stockRef)">+ 自选</n-button>
            <n-button size="small" secondary @click="goPosition">建仓</n-button>
          </div>
        </SectionCard>

        <!-- 日 K + MACD/BOLL 副图 -->
        <SectionCard title="日 K（近 120 交易日）">
          <template #extra>
            <span class="src-hint">BOLL(20,2σ) · MACD(12,26,9)</span>
          </template>
          <div ref="chartEl" class="kchart"></div>
          <n-empty v-if="!bars.length" description="日线数据暂不可用" />
        </SectionCard>

        <!-- 筹码分布（T1）：日K+换手率三角衰减本地复算，与东财展示或有复权口径差异 -->
        <SectionCard title="筹码分布">
          <template #extra>
            <span class="src-hint">本地复算 · 前复权口径</span>
          </template>
          <div v-if="chips" class="chip-wrap">
            <div ref="chipEl" class="chip-chart"></div>
            <div class="chip-side">
              <div class="chip-hero">
                <span class="qc-k">获利比例</span>
                <span class="chip-profit qv-figure" :style="{ color: pctColor(chips.profit >= 50 ? 1 : -1) }">
                  {{ chips.profit.toFixed(1) }}%
                </span>
              </div>
              <div class="quote-grid chip-grid">
                <div class="qc"><span class="qc-k">平均成本</span><span class="qc-v qv-tnum">{{ chips.avg_cost.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">现价</span><span class="qc-v qv-tnum">{{ chips.last_close.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">90% 成本区间</span><span class="qc-v qv-tnum">{{ chips.c90_low.toFixed(2) }} ~ {{ chips.c90_high.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">90% 集中度</span><span class="qc-v qv-tnum">{{ chips.conc_90.toFixed(1) }}%</span></div>
                <div class="qc"><span class="qc-k">70% 成本区间</span><span class="qc-v qv-tnum">{{ chips.c70_low.toFixed(2) }} ~ {{ chips.c70_high.toFixed(2) }}</span></div>
                <div class="qc"><span class="qc-k">70% 集中度</span><span class="qc-v qv-tnum">{{ chips.conc_70.toFixed(1) }}%</span></div>
              </div>
              <div class="chip-trend-block">
                <span class="qc-k">获利比例 · 近 {{ chips.days.length }} 交易日</span>
                <div ref="chipTrendEl" class="chip-trend"></div>
              </div>
              <div class="src-hint">
                基于近 {{ chips.bar_count }} 根日K与换手率的三角分布衰减模型本地复算<span v-if="chips.data_limited">（上市时间较短，窗口不足 210 根，精度受限）</span>；仅研究参考。
              </div>
            </div>
          </div>
          <n-empty v-else description="筹码数据暂不可用（需 ≥120 根日线与换手率，A 股标的）" />
        </SectionCard>

        <!-- 估值 + 评分 -->
        <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
          <n-gi>
            <SectionCard title="估值快照">
              <template v-if="valuation" #extra>
                <span class="src-hint">{{ valuation.source }}</span>
              </template>
              <div v-if="valuation" class="quote-grid">
                <template v-if="!isFund">
                  <div class="qc"><span class="qc-k">PE-TTM</span><span class="qc-v qv-tnum">{{ fmt(valuation.pe_ttm) }}</span></div>
                  <div class="qc"><span class="qc-k">PE(动)</span><span class="qc-v qv-tnum">{{ fmt(valuation.pe_dynamic) }}</span></div>
                  <div class="qc"><span class="qc-k">市净率</span><span class="qc-v qv-tnum">{{ fmt(valuation.pb) }}</span></div>
                  <div class="qc"><span class="qc-k">总市值</span><span class="qc-v qv-tnum">{{ fmtCap(valuation.total_cap) }}</span></div>
                  <div class="qc"><span class="qc-k">流通市值</span><span class="qc-v qv-tnum">{{ fmtCap(valuation.float_cap) }}</span></div>
                </template>
                <div v-else class="qc qc-wide"><span class="qc-k">类型</span><span class="qc-v">ETF/场内基金（无 PE/PB 个股估值指标）</span></div>
                <div class="qc"><span class="qc-k">换手率</span><span class="qc-v qv-tnum">{{ fmt(valuation.turnover_rate) }}%</span></div>
                <div class="qc"><span class="qc-k">振幅</span><span class="qc-v qv-tnum">{{ fmt(valuation.amplitude) }}%</span></div>
                <div class="qc"><span class="qc-k">量比</span><span class="qc-v qv-tnum">{{ fmt(valuation.volume_ratio) }}</span></div>
                <div class="qc"><span class="qc-k">涨停/跌停</span><span class="qc-v qv-tnum">{{ fmt(valuation.limit_up) }} / {{ fmt(valuation.limit_down) }}</span></div>
              </div>
              <n-empty v-else description="估值数据暂不可用（腾讯源）" />
            </SectionCard>
          </n-gi>
          <n-gi>
            <SectionCard title="技术面评分">
              <template v-if="score" #extra>
                <span class="src-hint">{{ score.trade_date }}</span>
              </template>
              <div v-if="score" class="score">
                <div class="score-hero">
                  <span class="score-total qv-figure">{{ score.total.toFixed(0) }}</span>
                  <n-tag :type="scoreType(score.total)" round :bordered="false">{{ score.label }}</n-tag>
                  <span v-if="score.data_limited" class="src-hint">（日线不足，精度受限）</span>
                </div>
                <div v-for="d in scoreDims" :key="d.label" class="score-dim">
                  <span class="sd-k">{{ d.label }}</span>
                  <div class="sd-bar">
                    <div class="sd-fill" :style="{ width: d.value + '%', background: vars.primaryColor }"></div>
                  </div>
                  <span class="sd-v qv-tnum">{{ d.value.toFixed(0) }}</span>
                </div>
                <div class="src-hint" style="margin-top: 6px">
                  纯技术面五维（趋势/动量/位置/量能/回撤风险），无财务维度；仅研究参考。
                </div>
              </div>
              <n-empty v-else description="评分暂不可用" />
            </SectionCard>
          </n-gi>
        </n-grid>

        <!-- 财务摘要（F2）：F10 主要指标近 8 期，营收/净利柱 + ROE/毛利率线 -->
        <SectionCard v-if="market === 'cn' && !isFund" title="财务摘要（近 8 期）">
          <template #extra>
            <span class="src-hint">东财 F10 · 季报口径</span>
          </template>
          <div v-if="finance && finance.indicators.length" class="fin-wrap">
            <div v-if="finLatest" class="quote-grid fin-grid">
              <div class="qc"><span class="qc-k">报告期</span><span class="qc-v">{{ finLatest.report_name }}</span></div>
              <div class="qc"><span class="qc-k">EPS</span><span class="qc-v qv-tnum">{{ finLatest.eps.toFixed(2) }}</span></div>
              <div class="qc"><span class="qc-k">ROE</span><span class="qc-v qv-tnum">{{ finLatest.roe.toFixed(2) }}%</span></div>
              <div class="qc"><span class="qc-k">营收同比</span><span class="qc-v qv-tnum" :style="{ color: pctColor(finLatest.revenue_yoy) }">{{ finLatest.revenue_yoy.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">净利同比</span><span class="qc-v qv-tnum" :style="{ color: pctColor(finLatest.net_profit_yoy) }">{{ finLatest.net_profit_yoy.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">毛利率</span><span class="qc-v qv-tnum">{{ finLatest.gross_margin.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">净利率</span><span class="qc-v qv-tnum">{{ finLatest.net_margin.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">资产负债率</span><span class="qc-v qv-tnum">{{ finLatest.debt_ratio.toFixed(1) }}%</span></div>
              <div class="qc"><span class="qc-k">每股经营现金流</span><span class="qc-v qv-tnum">{{ finLatest.ocf_ps.toFixed(2) }}</span></div>
            </div>
            <div ref="finEl" class="fin-chart"></div>
            <div class="src-hint">季报为累计口径且有披露滞后；0 值可能表示上游数据缺失；仅研究参考。</div>
          </div>
          <n-empty v-else description="暂无财务数据（东财 F10，A 股标的；首次访问自动拉取，可稍后刷新）" />
        </SectionCard>

        <!-- 公告（F1）：东财公告源，标题链到原文；采集范围外的股查询时按需补拉 -->
        <SectionCard title="公告">
          <div v-if="announcements.length" class="news-list">
            <div v-for="a in announcements" :key="a.art_code" class="news-row">
              <span class="news-time qv-tnum">{{ a.notice_date }}</span>
              <a :href="a.url" target="_blank" rel="noopener noreferrer" class="news-title">{{ a.title }}</a>
              <span v-if="a.notice_type" class="news-src">{{ a.notice_type }}</span>
            </div>
          </div>
          <n-empty v-else description="暂无公告数据（东财公告源，A 股标的）" />
        </SectionCard>

        <!-- 相关新闻（N1）：按代码关联财联社电报与东财个股新闻，best-effort -->
        <SectionCard title="相关新闻">
          <template #extra>
            <RouterLink :to="{ name: 'news', query: { symbol } }" class="news-more">更多快讯 →</RouterLink>
          </template>
          <div v-if="news.length" class="news-list">
            <div v-for="n in news" :key="n.id" class="news-row">
              <span class="news-time qv-tnum">{{ fmtNewsTime(n.publish_time) }}</span>
              <a
                v-if="n.url"
                :href="n.url"
                target="_blank"
                rel="noopener noreferrer"
                class="news-title"
              >{{ n.title }}</a>
              <span v-else class="news-title">{{ n.title }}</span>
              <span
                v-if="sentiView(n)"
                class="news-senti"
                :style="{ color: sentiView(n)!.color, background: withAlpha(sentiView(n)!.color, isDark ? 0.16 : 0.1) }"
              >{{ sentiView(n)!.text }}</span>
              <span class="news-src">{{ newsSourceLabel(n) }}</span>
            </div>
          </div>
          <n-empty v-else description="暂无相关新闻（覆盖财联社电报与东财个股新闻，A 股标的）" />
        </SectionCard>
      </div>
    </n-spin>
  </PageContainer>
</template>

<style scoped>
.detail {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
.head-price {
  display: flex;
  align-items: center;
  gap: 10px;
}
.hp-price {
  font-size: 32px;
  font-weight: 700;
  line-height: 1;
}
.head-meta {
  font-size: 12px;
  opacity: 0.55;
}
.quote-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(130px, 1fr));
  gap: 10px 14px;
}
.qc {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.qc-wide {
  grid-column: 1 / -1;
}
.qc-k {
  font-size: 12px;
  opacity: 0.55;
}
.qc-v {
  font-weight: 600;
}
.actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 14px;
}
.kchart {
  width: 100%;
  height: 460px;
}

/* ---------- 筹码分布 ---------- */
.chip-wrap {
  display: grid;
  grid-template-columns: minmax(0, 1.2fr) minmax(0, 1fr);
  gap: 16px;
  align-items: stretch;
}
.chip-chart {
  width: 100%;
  height: 320px;
  min-width: 0;
}
.chip-side {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-width: 0;
}
.chip-hero {
  display: flex;
  align-items: baseline;
  gap: 10px;
}
.chip-profit {
  font-size: 30px;
  font-weight: 700;
  line-height: 1;
}
.chip-grid {
  grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
}
.chip-trend-block {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.chip-trend {
  width: 100%;
  height: 64px;
}
@media (max-width: 768px) {
  .chip-wrap {
    grid-template-columns: 1fr;
  }
  .chip-chart {
    height: 260px;
  }
}
.src-hint {
  font-size: 12px;
  opacity: 0.55;
}

/* ---------- 财务摘要 ---------- */
.fin-wrap {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.fin-grid {
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
}
.fin-chart {
  width: 100%;
  height: 300px;
}
@media (max-width: 768px) {
  .fin-chart {
    height: 240px;
  }
}
.score {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.score-hero {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 4px;
}
.score-total {
  font-size: 34px;
  font-weight: 700;
  line-height: 1;
}
.score-dim {
  display: flex;
  align-items: center;
  gap: 10px;
}
.sd-k {
  width: 58px;
  font-size: 12px;
  opacity: 0.7;
  flex-shrink: 0;
}
.sd-bar {
  flex: 1;
  height: 6px;
  border-radius: 3px;
  background: color-mix(in srgb, currentColor 12%, transparent);
  overflow: hidden;
}
.sd-fill {
  height: 100%;
  border-radius: 3px;
  transition: width 0.3s ease;
}
.sd-v {
  width: 28px;
  text-align: right;
  font-size: 12px;
}

/* ---------- 相关新闻 ---------- */
.news-more {
  font-size: 12px;
  color: var(--qv-primary);
  text-decoration: none;
  opacity: 0.85;
}
.news-more:hover {
  opacity: 1;
}
.news-list {
  display: flex;
  flex-direction: column;
}
.news-row {
  display: flex;
  align-items: baseline;
  gap: 12px;
  padding: 8px 0;
}
.news-row + .news-row {
  border-top: 1px dashed rgba(128, 128, 128, 0.22);
}
.news-time {
  flex-shrink: 0;
  font-size: 12px;
  opacity: 0.55;
}
.news-title {
  flex: 1;
  min-width: 0;
  font-size: 13px;
  line-height: 1.55;
  color: inherit;
  text-decoration: none;
  overflow-wrap: anywhere;
}
a.news-title:hover {
  color: var(--qv-primary);
}
.news-src {
  flex-shrink: 0;
  font-size: 11px;
  opacity: 0.5;
}
.news-senti {
  flex-shrink: 0;
  font-size: 11px;
  font-weight: 600;
  padding: 0 7px;
  border-radius: 999px;
  line-height: 18px;
}

@media (max-width: 768px) {
  .news-row {
    flex-wrap: wrap;
    gap: 4px 10px;
  }
  .news-title {
    flex-basis: 100%;
    order: 3;
  }
}
</style>
