<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick, watch, computed } from 'vue'
import { useRouter } from 'vue-router'
import { NInput, NButton, NGrid, NGi, NSpin, NEmpty, NAlert, NTag, useMessage } from 'naive-ui'
import * as echarts from 'echarts'
import {
  getQuote,
  getDailyBars,
  getOverview,
  getValuation,
  type Quote,
  type Bar,
  type Overview,
  type Valuation,
} from '@/api/market'
import { listPositions } from '@/api/position'
import { getTodos, type TodoResult } from '@/api/todo'
import { listWatchlists } from '@/api/watchlist'
import { listRecommendations, type RecommendationBatch } from '@/api/recommendation'
import { getLatestDailyReport, type DailyReportView } from '@/api/report'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import { useStockActions } from '@/composables/useStockActions'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import RankList from '@/components/RankList.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const router = useRouter()
const { vars, isDark, pctColor, upColor, downColor, flatColor, withAlpha } = useUi()
const { llmLabel } = useLlmLabel()
const { adding, goAnalysis, goQa, goCompare, goAlert, goDetail, addToWatchlist } = useStockActions()

// ---------- 市场概览 ----------
const overview = ref<Overview | null>(null)
const ovLoading = ref(false)

async function loadOverview(silent = false) {
  if (!silent) ovLoading.value = true
  try {
    overview.value = await getOverview('cn')
  } catch (e) {
    if (!silent) message.error('市场概览加载失败：' + (e as Error).message)
  } finally {
    if (!silent) ovLoading.value = false
  }
}

// ---------- 我的概览：仓位 / 待办 / 自选 / 推荐，一屏知全局 ----------
const minePos = ref<{ pnl: number; pct: number; n: number } | null>(null)
const mineTodo = ref<TodoResult | null>(null)
const mineWatch = ref<{ up: number; down: number; total: number } | null>(null)
const mineRec = ref<RecommendationBatch | null>(null)
const mineRecLoaded = ref(false)

async function loadMine() {
  const [pos, todos, wl, recs] = await Promise.allSettled([
    listPositions('holding'),
    getTodos(),
    listWatchlists(),
    listRecommendations(undefined, 1),
  ])
  if (pos.status === 'fulfilled') {
    let cost = 0
    let pnl = 0
    let n = 0
    for (const p of pos.value) {
      if (p.status === 'holding' && p.quote_ok) {
        cost += p.cost
        pnl += p.profit_amount
        n++
      }
    }
    minePos.value = { pnl, pct: cost > 0 ? (pnl / cost) * 100 : 0, n }
  }
  if (todos.status === 'fulfilled') mineTodo.value = todos.value
  if (wl.status === 'fulfilled') {
    let up = 0
    let down = 0
    let total = 0
    for (const g of wl.value) {
      for (const it of g.items) {
        total++
        if (!it.quote_ok) continue
        if (it.change_pct > 0) up++
        else if (it.change_pct < 0) down++
      }
    }
    mineWatch.value = { up, down, total }
  }
  if (recs.status === 'fulfilled') {
    mineRec.value = recs.value[0] ?? null
    mineRecLoaded.value = true
  }
}

function recTypeText(t: string) {
  return t.includes('short') ? '短线' : t.includes('long') ? '长线' : t
}

// ---------- AI 今日观点（最新收盘日报摘要，无则引导开启） ----------
const latestReport = ref<DailyReportView | null>(null)
const reportLoaded = ref(false)
async function loadLatestReport() {
  try {
    latestReport.value = await getLatestDailyReport()
  } catch {
    latestReport.value = null
  } finally {
    reportLoaded.value = true
  }
}
function relDay(iso: string) {
  const d = new Date(iso)
  const today = new Date()
  const days = Math.floor((today.setHours(0, 0, 0, 0) - new Date(d).setHours(0, 0, 0, 0)) / 86400000)
  if (days <= 0) return '今天'
  if (days === 1) return '昨天'
  return `${days} 天前`
}
function fmtSigned(n: number) {
  return (
    (n >= 0 ? '+' : '') +
    n.toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
  )
}

// 我的概览卡样式变量（与 StatCard 同质感语言）
const mineVars = computed(() => ({
  '--mine-border': vars.value.borderColor,
  '--mine-bg': vars.value.cardColor,
  '--mine-primary': vars.value.primaryColor,
  '--mine-shadow': isDark.value
    ? 'inset 0 1px 0 rgba(255, 255, 255, 0.045)'
    : '0 1px 2px rgba(0, 0, 0, 0.03), 0 3px 12px rgba(0, 0, 0, 0.04)',
  '--mine-glow': withAlpha(vars.value.primaryColor, 0.14),
}))

// 盘中自动刷新：60s，仅交易时段 + 页面可见（见 useAutoRefresh），静默不闪 loading。
useAutoRefresh(() => {
  loadOverview(true)
  loadMine()
}, 60_000)

// ---------- 个股速查 ----------
const symbol = ref('600000')
const quote = ref<Quote | null>(null)
const valuation = ref<Valuation | null>(null)
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
    const sym = symbol.value.trim()
    quote.value = await getQuote('cn', sym)
    // 估值快照 best-effort：失败只是不显示估值行。
    getValuation('cn', sym)
      .then((v) => (valuation.value = v))
      .catch(() => (valuation.value = null))
    const bars = await getDailyBars('cn', sym, 120)
    lastBars.value = bars
    await nextTick()
    renderChart(bars)
  } catch (e) {
    quote.value = null
    valuation.value = null
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
  // 涨红跌绿取自主题语义色，坐标轴/背景交给 echarts 主题跟随明暗。
  const up = vars.value.errorColor
  const down = vars.value.successColor
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
    grid: { left: 52, right: 16, top: 16, bottom: 36 },
    xAxis: { type: 'category', data: bars.map((b) => b.trade_date), boundaryGap: false },
    yAxis: { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
    series: [
      {
        type: 'candlestick',
        data: bars.map((b) => [b.open, b.close, b.low, b.high]),
        itemStyle: { color: up, color0: down, borderColor: up, borderColor0: down },
      },
    ],
  })
}

watch(isDark, () => {
  if (lastBars.value.length) renderChart(lastBars.value)
})

// ---------- 展示辅助 ----------
function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtAmount(n: number) {
  if (!n) return '-'
  return (n / 1e8).toFixed(2) + ' 亿'
}
function fmtVol(n: number) {
  if (!n) return '-'
  return n >= 1e4 ? (n / 1e4).toFixed(1) + ' 万手' : n + ' 手'
}
function fmtTime(t: string | undefined) {
  return t ? new Date(t).toLocaleTimeString('zh-CN', { hour12: false }) : '-'
}
// 元 → 亿元，带符号（资金净流入正负）
function fmtYi(n: number | undefined) {
  if (n == null) return '-'
  const yi = n / 1e8
  return (yi >= 0 ? '+' : '') + yi.toFixed(2) + ' 亿'
}

const sectorsUnavailable = computed(() => !!overview.value?.errors?.sectors)
const breadthUnavailable = computed(() => !!overview.value && !overview.value.breadth)
const fundFlowUnavailable = computed(() => !!overview.value && !overview.value.fund_flow)

// 涨跌家数占比（市场情绪条宽度）
const breadthTotal = computed(() => {
  const b = overview.value?.breadth
  return b ? b.advances + b.declines + b.unchanged : 0
})
function breadthPct(n: number) {
  return breadthTotal.value ? (n / breadthTotal.value) * 100 : 0
}

onMounted(() => {
  loadOverview()
  loadMine()
  loadStock()
  loadLatestReport()
  window.addEventListener('resize', onResize)
})
onUnmounted(() => {
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
})
function onResize() {
  chart?.resize()
}
</script>

<template>
  <PageContainer title="市场首页" subtitle="A 股 · 实时概览与个股速查">
    <template #actions>
      <n-tag v-if="overview" size="small" round :bordered="false">
        更新 {{ fmtTime(overview.data_time) }}
      </n-tag>
      <n-button size="small" secondary :loading="ovLoading" @click="loadOverview()">刷新</n-button>
    </template>

    <div class="dashboard">
      <!-- 我的概览：仓位 / 待办 / 自选 / 推荐，点击直达 -->
      <n-grid cols="2 s:4" :x-gap="14" :y-gap="14" responsive="screen" :style="mineVars">
        <n-gi>
          <div class="mine-card" @click="router.push('/positions')">
            <div class="mc-label">持仓浮动盈亏</div>
            <div
              class="mc-value qv-figure"
              :style="minePos && minePos.n ? { color: pctColor(minePos.pnl) } : undefined"
            >
              {{ minePos ? (minePos.n ? fmtSigned(minePos.pnl) : '暂无持仓') : '—' }}
            </div>
            <div class="mc-sub">
              <template v-if="minePos && minePos.n">
                <ChangeTag :value="minePos.pct" size="small" />
                <span>{{ minePos.n }} 笔持仓</span>
              </template>
              <span v-else>记一笔 →</span>
            </div>
          </div>
        </n-gi>
        <n-gi>
          <div class="mine-card" @click="router.push('/today')">
            <div class="mc-label">今日待办</div>
            <div class="mc-value qv-figure" :class="{ 'mc-calm': mineTodo && !mineTodo.total }">
              {{ mineTodo ? (mineTodo.total ? `${mineTodo.total} 项` : '无待办') : '—' }}
            </div>
            <div class="mc-sub">
              <span v-if="mineTodo && mineTodo.total">提醒 {{ mineTodo.alerts }} · 复盘 {{ mineTodo.reviews }}</span>
              <span v-else>一切都在轨道上</span>
            </div>
          </div>
        </n-gi>
        <n-gi>
          <div class="mine-card" @click="router.push('/watchlist')">
            <div class="mc-label">自选股</div>
            <div class="mc-value qv-figure">
              <template v-if="mineWatch && mineWatch.total">
                <span :style="{ color: upColor }">{{ mineWatch.up }} 涨</span>
                <span class="mc-slash">/</span>
                <span :style="{ color: downColor }">{{ mineWatch.down }} 跌</span>
              </template>
              <template v-else>{{ mineWatch ? '还没有自选' : '—' }}</template>
            </div>
            <div class="mc-sub">
              <span v-if="mineWatch && mineWatch.total">共 {{ mineWatch.total }} 只</span>
              <span v-else>去添加 →</span>
            </div>
          </div>
        </n-gi>
        <n-gi>
          <div class="mine-card" @click="router.push('/recommendations')">
            <div class="mc-label">最新推荐</div>
            <div class="mc-value qv-figure" :class="{ 'mc-calm': mineRecLoaded && !mineRec }">
              {{ mineRec ? recTypeText(mineRec.type) + ' 策略' : mineRecLoaded ? '还没生成过' : '—' }}
            </div>
            <div class="mc-sub">
              <span v-if="mineRec">{{ relDay(mineRec.created_at) }} · {{ mineRec.strategy }}</span>
              <span v-else>去生成 →</span>
            </div>
          </div>
        </n-gi>
      </n-grid>

      <!-- 指数概览 -->
      <SectionCard title="指数概览">
        <n-spin :show="ovLoading && !overview">
          <n-grid
            v-if="overview?.indices?.length"
            cols="2 s:3 l:4"
            :x-gap="14"
            :y-gap="14"
            responsive="screen"
          >
            <n-gi v-for="ix in overview.indices" :key="ix.code">
              <StatCard :label="ix.name" :value="fmt(ix.price)" :change-pct="ix.change_pct" />
            </n-gi>
          </n-grid>
          <n-empty v-else description="指数数据暂不可用" />
        </n-spin>
      </SectionCard>

      <!-- 涨幅榜 + 热门榜 -->
      <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
        <n-gi>
          <SectionCard title="涨幅榜">
            <RankList v-if="overview?.gainers?.length" :items="overview.gainers">
              <template #row="{ item }">
                <div class="stock-row stock-row-link" @click="goDetail({ symbol: item.symbol, market: 'cn', name: item.name })">
                  <div class="sr-name">
                    <span class="sr-title">{{ item.name }}</span>
                    <span class="sr-symbol qv-mono">{{ item.symbol }}</span>
                  </div>
                  <div class="sr-figures">
                    <span class="sr-price qv-tnum">{{ fmt(item.price) }}</span>
                    <ChangeTag :value="item.change_pct" size="small" />
                    <span class="sr-amount qv-tnum">{{ fmtAmount(item.amount) }}</span>
                  </div>
                </div>
              </template>
            </RankList>
            <n-empty v-else description="暂不可用" />
          </SectionCard>
        </n-gi>
        <n-gi>
          <SectionCard title="热门榜（成交额）">
            <RankList v-if="overview?.actives?.length" :items="overview.actives">
              <template #row="{ item }">
                <div class="stock-row stock-row-link" @click="goDetail({ symbol: item.symbol, market: 'cn', name: item.name })">
                  <div class="sr-name">
                    <span class="sr-title">{{ item.name }}</span>
                    <span class="sr-symbol qv-mono">{{ item.symbol }}</span>
                  </div>
                  <div class="sr-figures">
                    <span class="sr-price qv-tnum">{{ fmt(item.price) }}</span>
                    <ChangeTag :value="item.change_pct" size="small" />
                    <span class="sr-amount qv-tnum">{{ fmtAmount(item.amount) }}</span>
                  </div>
                </div>
              </template>
            </RankList>
            <n-empty v-else description="暂不可用" />
          </SectionCard>
        </n-gi>
      </n-grid>

      <!-- 板块榜 + 市场情绪 -->
      <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
        <n-gi>
          <SectionCard title="板块涨跌榜">
            <template v-if="sectorsUnavailable" #extra>
              <n-tag size="small" type="warning" round :bordered="false">数据源繁忙</n-tag>
            </template>
            <RankList v-if="overview?.sectors?.length" :items="overview.sectors">
              <template #row="{ item }">
                <div class="stock-row">
                  <span class="sr-title">{{ item.name }}</span>
                  <div class="sr-figures">
                    <ChangeTag :value="item.change_pct" size="small" />
                    <span class="sr-leader">领涨 {{ item.leader || '-' }}</span>
                  </div>
                </div>
              </template>
            </RankList>
            <n-empty v-else description="板块榜依赖东财接口，当前限流暂不可用，稍后重试" />
          </SectionCard>
        </n-gi>
        <n-gi>
          <SectionCard title="市场情绪">
            <template v-if="breadthUnavailable" #extra>
              <n-tag size="small" type="warning" round :bordered="false">数据源繁忙</n-tag>
            </template>
            <div v-if="overview?.breadth" class="breadth">
              <div class="breadth-summary">
                <div class="bs-cell">
                  <span class="bs-num qv-tnum" :style="{ color: upColor }">{{
                    overview.breadth.advances
                  }}</span>
                  <span class="bs-label">上涨</span>
                </div>
                <div class="bs-cell">
                  <span class="bs-num qv-tnum" :style="{ color: flatColor }">{{
                    overview.breadth.unchanged
                  }}</span>
                  <span class="bs-label">平盘</span>
                </div>
                <div class="bs-cell">
                  <span class="bs-num qv-tnum" :style="{ color: downColor }">{{
                    overview.breadth.declines
                  }}</span>
                  <span class="bs-label">下跌</span>
                </div>
              </div>
              <div class="breadth-bar">
                <div
                  class="seg"
                  :style="{
                    width: breadthPct(overview.breadth.advances) + '%',
                    background: upColor,
                  }"
                ></div>
                <div
                  class="seg"
                  :style="{
                    width: breadthPct(overview.breadth.unchanged) + '%',
                    background: flatColor,
                  }"
                ></div>
                <div
                  class="seg"
                  :style="{
                    width: breadthPct(overview.breadth.declines) + '%',
                    background: downColor,
                  }"
                ></div>
              </div>
              <div class="breadth-limits">
                <span class="bl" :style="{ background: withAlpha(upColor, 0.14), color: upColor }">
                  涨停 {{ overview.breadth.limit_up }}
                </span>
                <span
                  class="bl"
                  :style="{ background: withAlpha(downColor, 0.14), color: downColor }"
                >
                  跌停 {{ overview.breadth.limit_down }}
                </span>
                <span class="bl-date">{{ overview.breadth.trade_date }}</span>
              </div>
            </div>
            <n-empty v-else description="涨跌家数依赖东财接口，当前限流暂不可用，稍后重试" />
          </SectionCard>
        </n-gi>
      </n-grid>

      <!-- 个股速查 -->
      <SectionCard title="个股速查">
        <template #extra>
          <span class="hint">东财 → 腾讯 → 新浪 三源自动切换 · 仅 A 股已打通</span>
        </template>
        <div class="quote-search">
          <n-input
            v-model:value="symbol"
            placeholder="股票代码，如 600000"
            style="width: 200px"
            @keyup.enter="loadStock"
          />
          <n-button type="primary" :loading="loading" @click="loadStock">查询</n-button>
        </div>

        <div v-if="quote" class="quote-panel">
          <div class="quote-hero">
            <span class="qh-price qv-figure" :style="{ color: pctColor(quote.change_pct) }">
              {{ fmt(quote.price) }}
            </span>
            <ChangeTag :value="quote.change_pct" />
          </div>
          <div class="quote-grid">
            <div class="quote-cell">
              <span class="qc-label">今开</span>
              <span class="qc-value qv-tnum">{{ fmt(quote.open) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">昨收</span>
              <span class="qc-value qv-tnum">{{ fmt(quote.prev_close) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">最高</span>
              <span class="qc-value qv-tnum" :style="{ color: upColor }">{{ fmt(quote.high) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">最低</span>
              <span class="qc-value qv-tnum" :style="{ color: downColor }">{{ fmt(quote.low) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">成交量</span>
              <span class="qc-value qv-tnum">{{ fmtVol(quote.volume) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">成交额</span>
              <span class="qc-value qv-tnum">{{ fmtAmount(quote.amount) }}</span>
            </div>
          </div>

          <!-- 估值快照（腾讯免费源，best-effort） -->
          <div v-if="valuation" class="quote-grid" style="margin-top: 8px">
            <div class="quote-cell">
              <span class="qc-label">PE-TTM</span>
              <span class="qc-value qv-tnum">{{ valuation.pe_ttm < 0 ? '亏损' : fmt(valuation.pe_ttm) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">市净率</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.pb) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">总市值</span>
              <span class="qc-value qv-tnum">{{ valuation.total_cap > 0 ? (valuation.total_cap / 1e8).toFixed(0) + ' 亿' : '—' }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">换手率</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.turnover_rate) }}%</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">量比</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.volume_ratio) }}</span>
            </div>
            <div class="quote-cell">
              <span class="qc-label">涨停 / 跌停</span>
              <span class="qc-value qv-tnum">{{ fmt(valuation.limit_up) }} / {{ fmt(valuation.limit_down) }}</span>
            </div>
          </div>

          <!-- 快捷动作：查到即可直达，不用换页面重输代码 -->
          <div class="quote-actions">
            <n-button size="small" secondary type="primary" @click="goDetail(quote)">个股详情</n-button>
            <n-button size="small" secondary @click="goAnalysis(quote)">AI 分析</n-button>
            <n-button size="small" secondary @click="goQa(quote)">个股问答</n-button>
            <n-button size="small" secondary @click="goCompare(quote)">横向对比</n-button>
            <n-button size="small" secondary :loading="adding" @click="addToWatchlist(quote)">+ 自选</n-button>
            <n-button size="small" secondary @click="goAlert(quote)">设提醒</n-button>
          </div>
        </div>

        <div ref="chartEl" class="quote-chart"></div>
      </SectionCard>

      <!-- 资金流向 + AI 今日观点（最新收盘日报摘要；新闻情绪仍待新闻源/Tushare，见 docs/DATA_SOURCES.md §7） -->
      <n-grid cols="1 m:2" :x-gap="16" :y-gap="16" responsive="screen">
        <n-gi>
          <SectionCard title="资金流向">
            <template v-if="fundFlowUnavailable" #extra>
              <n-tag size="small" type="warning" round :bordered="false">数据源繁忙</n-tag>
            </template>
            <div v-if="overview?.fund_flow" class="fundflow">
              <div class="ff-hero">
                <span class="ff-label">主力净流入</span>
                <span
                  class="ff-main qv-figure"
                  :style="{ color: pctColor(overview.fund_flow.main_net) }"
                >
                  {{ fmtYi(overview.fund_flow.main_net) }}
                </span>
                <span class="ff-date">{{ overview.fund_flow.trade_date }} · 沪深两市</span>
              </div>
              <div class="ff-grid">
                <div class="ff-cell">
                  <span class="ff-k">超大单</span>
                  <span
                    class="ff-v qv-tnum"
                    :style="{ color: pctColor(overview.fund_flow.super_net) }"
                    >{{ fmtYi(overview.fund_flow.super_net) }}</span
                  >
                </div>
                <div class="ff-cell">
                  <span class="ff-k">大单</span>
                  <span
                    class="ff-v qv-tnum"
                    :style="{ color: pctColor(overview.fund_flow.large_net) }"
                    >{{ fmtYi(overview.fund_flow.large_net) }}</span
                  >
                </div>
                <div class="ff-cell">
                  <span class="ff-k">中单</span>
                  <span
                    class="ff-v qv-tnum"
                    :style="{ color: pctColor(overview.fund_flow.medium_net) }"
                    >{{ fmtYi(overview.fund_flow.medium_net) }}</span
                  >
                </div>
                <div class="ff-cell">
                  <span class="ff-k">小单</span>
                  <span
                    class="ff-v qv-tnum"
                    :style="{ color: pctColor(overview.fund_flow.small_net) }"
                    >{{ fmtYi(overview.fund_flow.small_net) }}</span
                  >
                </div>
              </div>
            </div>
            <n-empty v-else description="两市资金流依赖东财接口，当前限流暂不可用，稍后重试" />
          </SectionCard>
        </n-gi>
        <n-gi>
          <SectionCard title="AI 今日观点">
            <template v-if="latestReport" #extra>
              <n-button size="tiny" quaternary type="primary" @click="router.push('/daily-report')">查看日报 →</n-button>
            </template>
            <div v-if="latestReport?.review" class="report-brief">
              <div class="rb-date">{{ latestReport.trade_date }} 收盘日报<template v-if="llmLabel(latestReport)"> · {{ llmLabel(latestReport) }}</template></div>
              <p class="rb-summary">{{ latestReport.review.summary }}</p>
              <p v-if="latestReport.review.tomorrow_plan" class="rb-plan">明日：{{ latestReport.review.tomorrow_plan }}</p>
            </div>
            <n-empty
              v-else-if="reportLoaded"
              description="暂无收盘日报。在 设置→偏好 开启后，交易日 15:35 起自动生成今日复盘与明日推荐。"
            >
              <template #extra>
                <n-button size="small" ghost type="primary" @click="router.push('/daily-report')">去看看</n-button>
              </template>
            </n-empty>
          </SectionCard>
        </n-gi>
      </n-grid>

      <n-alert type="warning" title="风险提示" :bordered="false">
        本内容仅供研究参考，不构成投资建议。AI 可能出错，数据可能延迟或不完整，投资决策需由用户自行承担风险。
      </n-alert>
    </div>
  </PageContainer>
</template>

<style scoped>
.dashboard {
  display: flex;
  flex-direction: column;
  gap: 16px;
}

/* 我的概览卡：可点击直达，与 StatCard 同质感 */
.mine-card {
  padding: 14px 16px;
  border-radius: 12px;
  border: 1px solid var(--mine-border);
  background: var(--mine-bg);
  box-shadow: var(--mine-shadow);
  cursor: pointer;
  transition:
    border-color 0.2s ease,
    box-shadow 0.2s ease,
    transform 0.2s ease;
}
.mine-card:hover {
  border-color: var(--mine-primary);
  box-shadow: var(--mine-shadow), 0 6px 18px var(--mine-glow);
  transform: translateY(-1px);
}
.mc-label {
  font-size: 13px;
  opacity: 0.7;
  margin-bottom: 8px;
}
.mc-value {
  font-size: 22px;
  font-weight: 700;
  line-height: 1.15;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.mc-value.mc-calm {
  opacity: 0.55;
  font-weight: 600;
}
.mc-slash {
  opacity: 0.35;
  margin: 0 4px;
  font-weight: 400;
}
.mc-sub {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 8px;
  min-height: 20px;
  font-size: 12px;
  opacity: 0.65;
}

/* 速查快捷动作条 */
.quote-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  margin-top: 14px;
}

/* 榜单行 */
.stock-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
}
.stock-row-link {
  cursor: pointer;
}
.sr-name {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}
.sr-title {
  font-size: 14px;
  font-weight: 500;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.sr-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.sr-figures {
  display: flex;
  align-items: center;
  gap: 12px;
  flex-shrink: 0;
}
.sr-price {
  font-size: 14px;
  min-width: 56px;
  text-align: right;
}
.sr-amount {
  font-size: 12px;
  opacity: 0.6;
  min-width: 64px;
  text-align: right;
}
/* 超窄屏：右侧价格+涨跌+成交额三段不可收缩，名称列会被挤到 2~3 字，隐藏次要的成交额 */
@media (max-width: 480px) {
  .sr-figures {
    gap: 8px;
  }
  .sr-amount {
    display: none;
  }
}
.sr-leader {
  font-size: 12px;
  opacity: 0.6;
}

/* 市场情绪：涨跌家数 */
.breadth {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding: 4px 0;
}
.breadth-summary {
  display: flex;
  justify-content: space-around;
  text-align: center;
}
.bs-cell {
  display: flex;
  flex-direction: column;
  gap: 4px;
}
.bs-num {
  font-size: 26px;
  font-weight: 700;
  line-height: 1;
}
.bs-label {
  font-size: 12px;
  opacity: 0.6;
}
.breadth-bar {
  display: flex;
  height: 10px;
  border-radius: 6px;
  overflow: hidden;
}
.breadth-bar .seg {
  height: 100%;
  transition: width 0.4s ease;
}
.breadth-limits {
  display: flex;
  align-items: center;
  gap: 10px;
}
.bl {
  font-size: 12px;
  font-weight: 600;
  padding: 2px 10px;
  border-radius: 999px;
}
.bl-date {
  font-size: 12px;
  opacity: 0.45;
  margin-left: auto;
}

/* 资金流向 */
/* AI 今日观点（收盘日报摘要） */
.report-brief {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.rb-date {
  font-size: 12px;
  opacity: 0.55;
}
.rb-summary {
  margin: 0;
  font-size: 14px;
  font-weight: 600;
  line-height: 1.7;
}
.rb-plan {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.75;
}

.fundflow {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.ff-hero {
  display: flex;
  align-items: baseline;
  gap: 12px;
  flex-wrap: wrap;
}
.ff-label {
  font-size: 13px;
  opacity: 0.6;
}
.ff-main {
  font-size: 30px;
  font-weight: 700;
  line-height: 1;
}
.ff-date {
  font-size: 12px;
  opacity: 0.45;
}
.ff-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 10px 16px;
}
.ff-cell {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.ff-k {
  font-size: 12px;
  opacity: 0.55;
}
.ff-v {
  font-size: 16px;
  font-weight: 600;
}

/* 个股速查 */
.hint {
  font-size: 12px;
  opacity: 0.55;
}
.quote-search {
  display: flex;
  gap: 10px;
  margin-bottom: 16px;
}
.quote-panel {
  margin-bottom: 12px;
}
.quote-hero {
  display: flex;
  align-items: baseline;
  gap: 12px;
  margin-bottom: 14px;
}
.qh-price {
  font-size: 34px;
  font-weight: 700;
  line-height: 1;
}
.quote-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(120px, 1fr));
  gap: 10px 16px;
}
.quote-cell {
  display: flex;
  flex-direction: column;
  gap: 3px;
}
.qc-label {
  font-size: 12px;
  opacity: 0.55;
}
.qc-value {
  font-size: 16px;
  font-weight: 500;
}
.quote-chart {
  width: 100%;
  height: 340px;
  margin-top: 8px;
}
</style>
