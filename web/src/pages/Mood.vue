<script setup lang="ts">
import { computed, h, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NDataTable,
  NEmpty,
  NSpin,
  NTabPane,
  NTabs,
  NTag,
  type DataTableColumns,
} from 'naive-ui'
import * as echarts from 'echarts'
import {
  getMarketLhb,
  getMarketMood,
  getMarketPopularity,
  type LhbDaily,
  type LhbDailyItem,
  type MoodOverview,
  type PopularityDaily,
  type PopularityDailyItem,
} from '@/api/market'
import { useUi, withAlpha } from '@/composables/useUi'
import { isAbortError } from '@/api/client'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const router = useRouter()
const { vars, isDark, pctColor, upColor } = useUi()

const activeTab = ref('overview')
const mood = ref<MoodOverview | null>(null)
const lhb = ref<LhbDaily | null>(null)
const popularity = ref<PopularityDaily | null>(null)
const loading = ref(false)
const moodError = ref('')
const lhbError = ref('')
const popularityError = ref('')
const trendEl = ref<HTMLDivElement | null>(null)
let trendChart: echarts.ECharts | null = null
type LhbTableRow = LhbDailyItem & { _row_key: string }
const lhbRows = computed<LhbTableRow[]>(() =>
  (lhb.value?.items ?? []).map((row, index) => ({
    ...row,
    _row_key: `${lhb.value?.trade_date || 'latest'}-${row.symbol}-${index}`,
  })),
)

// 三个查询共用一个中止器：重复点刷新/组件卸载时中止上一轮，迟到响应不再回填。
let loadAbort: AbortController | null = null
// 轮次序号：只有最新一轮的结果允许落地（abort 之外再加一道守卫，
// 因为 Promise.allSettled 里已完成的分支不会被 abort 影响）。
let loadSeq = 0

async function load() {
  loadAbort?.abort()
  const myAbort = new AbortController()
  loadAbort = myAbort
  const mySeq = ++loadSeq
  loading.value = true
  moodError.value = ''
  lhbError.value = ''
  popularityError.value = ''
  const [moodResult, lhbResult, popularityResult] = await Promise.allSettled([
    getMarketMood('cn', 30, myAbort.signal),
    getMarketLhb('cn', '', 100, myAbort.signal),
    getMarketPopularity('cn', '', myAbort.signal),
  ])
  if (mySeq !== loadSeq) return
  if (moodResult.status === 'fulfilled') mood.value = moodResult.value
  else if (!isAbortError(moodResult.reason)) {
    mood.value = null
    moodError.value = errorText(moodResult.reason)
  }
  if (lhbResult.status === 'fulfilled') lhb.value = lhbResult.value
  else if (!isAbortError(lhbResult.reason)) {
    lhb.value = null
    lhbError.value = errorText(lhbResult.reason)
  }
  if (popularityResult.status === 'fulfilled') popularity.value = popularityResult.value
  else if (!isAbortError(popularityResult.reason)) {
    popularity.value = null
    popularityError.value = errorText(popularityResult.reason)
  }
  loading.value = false
  await nextTick()
  if (mySeq !== loadSeq) return
  renderTrend()
}

function errorText(reason: unknown) {
  return reason instanceof Error ? reason.message : '数据加载失败，请稍后重试'
}

function renderTrend() {
  if (activeTab.value !== 'overview' || !trendEl.value || !mood.value?.trend.length) {
    trendChart?.dispose()
    trendChart = null
    return
  }
  trendChart?.dispose()
  trendChart = echarts.init(trendEl.value, isDark.value ? 'dark' : undefined)
  const trend = mood.value.trend
  const compact = window.innerWidth <= 768
  trendChart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
    legend: {
      top: 0,
      data: ['涨停家数', '炸板率', '昨涨停溢价'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
    },
    grid: { left: 50, right: 48, top: 38, bottom: 34 },
    xAxis: {
      type: 'category',
      data: trend.map((p) => p.trade_date),
      boundaryGap: false,
      axisLabel: { hideOverlap: true, fontSize: 10 },
    },
    yAxis: [
      {
        type: 'value',
        name: compact ? '' : '家数',
        minInterval: 1,
        splitLine: { lineStyle: { color: vars.value.dividerColor } },
      },
      {
        type: 'value',
        name: compact ? '' : '%',
        axisLabel: { formatter: '{value}%' },
        splitLine: { show: false },
      },
    ],
    series: [
      {
        name: '涨停家数',
        type: 'line',
        data: trend.map((p) => p.limit_up_count),
        symbol: 'circle',
        symbolSize: 5,
        lineStyle: { width: 2, color: vars.value.errorColor },
        itemStyle: { color: vars.value.errorColor },
        areaStyle: { color: withAlpha(vars.value.errorColor, 0.08) },
      },
      {
        name: '炸板率',
        type: 'line',
        yAxisIndex: 1,
        data: trend.map((p) => p.broken_rate),
        symbol: 'none',
        lineStyle: { width: 1.5, color: vars.value.warningColor },
        itemStyle: { color: vars.value.warningColor },
      },
      {
        name: '昨涨停溢价',
        type: 'line',
        yAxisIndex: 1,
        data: trend.map((p) => p.yzt_avg_chg),
        symbol: 'none',
        lineStyle: { width: 1.5, color: vars.value.primaryColor },
        itemStyle: { color: vars.value.primaryColor },
      },
    ],
  })
}

function goStock(symbol: string) {
  router.push(`/stocks/cn/${symbol}`)
}

// signed 仅用于有方向语义的金额（净买额/机构净买）。买入额/卖出额/成交额/封板资金
// 天然非负，加 "+" 只会读成「增加了多少」，是误导。
function fmtAmount(n: number, signed = false) {
  const abs = Math.abs(n)
  const sign = signed ? (n > 0 ? '+' : n < 0 ? '-' : '') : n < 0 ? '-' : ''
  if (abs >= 1e8) return `${sign}${(abs / 1e8).toFixed(2)} 亿`
  if (abs >= 1e4) return `${sign}${(abs / 1e4).toFixed(0)} 万`
  return `${sign}${abs.toFixed(0)} 元`
}

function fmtPct(n: number) {
  return `${n.toFixed(2)}%`
}

function fmtSealTime(n: number) {
  if (!n) return '-'
  return String(n).padStart(6, '0').replace(/^(\d{2})(\d{2})(\d{2})$/, '$1:$2:$3')
}

const lhbColumns = computed<DataTableColumns<LhbTableRow>>(() => [
  {
    title: '股票',
    key: 'name',
    width: 130,
    fixed: 'left',
    render: (row) =>
      h('button', { class: 'stock-link', onClick: () => goStock(row.symbol) }, [
        h('span', { class: 'stock-name' }, row.name || row.symbol),
        h('span', { class: 'stock-symbol qv-mono' }, row.symbol),
      ]),
  },
  { title: '收盘', key: 'close', align: 'right', width: 88, render: (row) => row.close.toFixed(2) },
  {
    title: '涨跌幅',
    key: 'change_pct',
    align: 'right',
    width: 100,
    render: (row) => h(ChangeTag, { value: row.change_pct, size: 'small' }),
  },
  {
    title: '净买额',
    key: 'net_buy',
    align: 'right',
    width: 118,
    render: (row) => h('span', { style: { color: pctColor(row.net_buy), fontWeight: 600 } }, fmtAmount(row.net_buy, true)),
  },
  { title: '买入额', key: 'buy_amt', align: 'right', width: 110, render: (row) => fmtAmount(row.buy_amt) },
  { title: '卖出额', key: 'sell_amt', align: 'right', width: 110, render: (row) => fmtAmount(row.sell_amt) },
  { title: '成交额', key: 'deal_amt', align: 'right', width: 110, render: (row) => fmtAmount(row.deal_amt) },
  { title: '净占比', key: 'net_ratio', align: 'right', width: 90, render: (row) => fmtPct(row.net_ratio) },
  { title: '换手率', key: 'turnover_rate', align: 'right', width: 90, render: (row) => fmtPct(row.turnover_rate) },
  {
    title: '机构净买',
    key: 'org_net_buy',
    align: 'right',
    width: 118,
    render: (row) => h('span', { style: { color: pctColor(row.org_net_buy) } }, fmtAmount(row.org_net_buy, true)),
  },
  {
    title: '机构席位',
    key: 'org_times',
    width: 110,
    render: (row) => `买 ${row.org_buy_times} / 卖 ${row.org_sell_times}`,
  },
  {
    title: '上榜原因',
    key: 'reason',
    minWidth: 260,
    render: (row) => row.note ? `${row.reason}（${row.note}）` : row.reason,
  },
])

const popularityColumns = computed<DataTableColumns<PopularityDailyItem>>(() => [
  {
    title: '排名',
    key: 'rank',
    width: 60,
    render: (row) => h('strong', { class: 'rank-number qv-figure' }, String(row.rank)),
  },
  {
    title: '股票',
    key: 'name',
    width: 128,
    render: (row) =>
      h('button', { class: 'stock-link is-row', onClick: () => goStock(row.symbol) }, [
        h('span', { class: 'stock-name' }, row.name || row.symbol),
        h('span', { class: 'stock-symbol qv-mono' }, row.symbol),
      ]),
  },
  {
    title: '排名变化',
    key: 'change',
    align: 'right',
    width: 116,
    render: (row) => {
      if (row.is_new) return h(NTag, { size: 'small', type: 'error', bordered: false, round: true }, () => '新上榜')
      const delta = row.prev_rank - row.rank
      const text = delta > 0 ? `上升 ${delta}` : delta < 0 ? `下降 ${-delta}` : '持平'
      return h('span', { style: { color: pctColor(delta), fontWeight: 600 } }, text)
    },
  },
  { title: '昨日排名', key: 'prev_rank', align: 'right', width: 100, render: (row) => row.prev_rank > 0 ? row.prev_rank : '-' },
])

function onResize() {
  trendChart?.resize()
}

watch(activeTab, async (tab) => {
  if (tab === 'overview') {
    await nextTick()
    renderTrend()
  }
})
// 主题变化整套重绘：6 套主题中明暗只是其一，同为浅色的两套换主题时 isDark 不变而
// primaryColor/errorColor/dividerColor 全变——只监听 isDark 图表会留在旧色板上。
watch([isDark, vars], () => renderTrend())

onMounted(() => {
  load()
  window.addEventListener('resize', onResize)
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  loadSeq++
  loadAbort?.abort()
  loadAbort = null
  trendChart?.dispose()
  trendChart = null
})
</script>

<template>
  <PageContainer title="盘面情绪" subtitle="涨停生态、资金席位与市场人气">
    <template #actions>
      <n-button size="small" secondary :loading="loading" @click="load">刷新</n-button>
    </template>

    <n-spin :show="loading && !mood && !lhb && !popularity">
      <n-tabs v-model:value="activeTab" type="line" animated class="mood-tabs">
        <n-tab-pane name="overview" tab="情绪总览">
          <n-alert v-if="moodError" type="error" :bordered="false" title="情绪数据加载失败">
            {{ moodError }}
          </n-alert>
          <template v-else-if="mood?.latest">
            <div class="stat-grid">
              <StatCard label="涨停家数" :value="mood.latest.limit_up_count" :sub="mood.latest.trade_date" />
              <StatCard label="炸板率" :value="fmtPct(mood.latest.broken_rate)" :sub="`${mood.latest.broken_count} 家炸板`" />
              <StatCard label="最高连板" :value="`${mood.latest.max_streak} 板`" :sub="`${Object.keys(mood.streak_dist).length} 个梯度`" />
              <StatCard label="昨涨停溢价" :value="fmtPct(mood.latest.yzt_avg_chg)" :change-pct="mood.latest.yzt_avg_chg" :sub="`${mood.latest.yzt_up_ratio.toFixed(1)}% 红盘`" />
            </div>

            <SectionCard title="近 30 个情绪快照" :hoverable="false">
              <div v-if="mood.trend.length" ref="trendEl" class="trend-chart"></div>
              <n-empty v-else description="暂无历史趋势；涨停池不可回溯，将随每日盘后快照积累" />
            </SectionCard>

            <SectionCard title="封板资金 Top" :hoverable="false">
              <div v-if="mood.seal_fund_top.length" class="fund-list">
                <button
                  v-for="(stock, index) in mood.seal_fund_top"
                  :key="stock.symbol"
                  class="fund-row"
                  type="button"
                  @click="goStock(stock.symbol)"
                >
                  <span class="fund-rank qv-figure">{{ index + 1 }}</span>
                  <span class="fund-stock"><strong>{{ stock.name }}</strong><small class="qv-mono">{{ stock.symbol }}</small></span>
                  <span class="fund-industry">{{ stock.industry || '行业未知' }}</span>
                  <span class="fund-value qv-tnum" :style="{ color: upColor }">{{ fmtAmount(stock.seal_fund) }}</span>
                </button>
              </div>
              <n-empty v-else description="该交易日暂无封板资金排行" />
            </SectionCard>
          </template>
          <n-empty v-else description="暂无盘面情绪快照；数据将在交易日盘后逐日积累" />
        </n-tab-pane>

        <n-tab-pane name="ladder" tab="连板梯队">
          <n-alert v-if="moodError" type="error" :bordered="false" title="梯队数据加载失败">
            {{ moodError }}
          </n-alert>
          <div v-else-if="mood?.streak_ladders.length" class="ladder-list">
            <SectionCard
              v-for="ladder in mood.streak_ladders"
              :key="ladder.streak"
              :title="`${ladder.streak} 连板`"
              :hoverable="false"
            >
              <template #extra><n-tag size="small" round :bordered="false">{{ ladder.count }} 只</n-tag></template>
              <div class="stock-grid">
                <button
                  v-for="stock in ladder.stocks"
                  :key="stock.symbol"
                  class="ladder-stock"
                  type="button"
                  @click="goStock(stock.symbol)"
                >
                  <span class="ladder-head"><strong>{{ stock.name }}</strong><span class="qv-mono">{{ stock.symbol }}</span></span>
                  <span class="ladder-meta">{{ stock.industry || '行业未知' }} · 封单 {{ fmtAmount(stock.seal_fund) }}</span>
                  <span class="ladder-meta">首次封板 {{ fmtSealTime(stock.first_seal_at) }} · 炸板 {{ stock.break_count }} 次</span>
                </button>
              </div>
            </SectionCard>
          </div>
          <n-empty v-else description="最近情绪快照中暂无连板梯队" />
        </n-tab-pane>

        <n-tab-pane name="lhb" tab="龙虎榜">
          <n-alert v-if="lhbError" type="error" :bordered="false" title="龙虎榜加载失败">
            {{ lhbError }}
          </n-alert>
          <SectionCard v-else :title="lhb?.trade_date ? `${lhb.trade_date} 龙虎榜` : '龙虎榜'" :hoverable="false">
            <n-data-table
              v-if="lhbRows.length"
              :columns="lhbColumns"
              :data="lhbRows"
              :row-key="(row: LhbTableRow) => row._row_key"
              :scroll-x="1490"
              :pagination="{ pageSize: 20 }"
              size="small"
            />
            <n-empty v-else description="暂无龙虎榜数据" />
          </SectionCard>
        </n-tab-pane>

        <n-tab-pane name="popularity" tab="人气榜">
          <n-alert v-if="popularityError" type="error" :bordered="false" title="人气榜加载失败">
            {{ popularityError }}
          </n-alert>
          <SectionCard v-else :title="popularity?.trade_date ? `${popularity.trade_date} 人气榜` : '人气榜'" :hoverable="false">
            <n-data-table
              v-if="popularity?.items.length"
              :columns="popularityColumns"
              :data="popularity.items"
              :row-key="(row: PopularityDailyItem) => row.symbol"
              :scroll-x="404"
              :pagination="{ pageSize: 20 }"
              size="small"
            />
            <n-empty v-else description="暂无人气榜数据" />
          </SectionCard>
        </n-tab-pane>
      </n-tabs>
    </n-spin>
  </PageContainer>
</template>

<style scoped>
.mood-tabs {
  --mood-border: v-bind('vars.borderColor');
  --mood-card: v-bind('vars.cardColor');
  --mood-hover: v-bind('withAlpha(vars.primaryColor, isDark ? 0.14 : 0.07)');
  --mood-primary: v-bind('vars.primaryColor');
}
.mood-tabs :deep(.n-tab-pane) {
  display: flex;
  flex-direction: column;
  gap: 16px;
  padding-top: 4px;
}
.stat-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 12px;
}
.trend-chart {
  width: 100%;
  height: 340px;
}
.fund-list {
  display: flex;
  flex-direction: column;
}
.fund-row {
  display: grid;
  grid-template-columns: 34px minmax(140px, 1fr) minmax(100px, 0.7fr) 120px;
  align-items: center;
  gap: 12px;
  width: 100%;
  padding: 10px 4px;
  color: inherit;
  border: 0;
  border-bottom: 1px solid var(--mood-border);
  background: transparent;
  text-align: left;
  cursor: pointer;
}
.fund-row:hover,
.ladder-stock:hover {
  background: var(--mood-hover);
}
.fund-rank {
  font-size: 18px;
  font-weight: 700;
  color: var(--mood-primary);
  text-align: center;
}
.fund-stock {
  display: flex;
  align-items: baseline;
  gap: 8px;
  min-width: 0;
}
.fund-stock small,
:deep(.stock-symbol) {
  opacity: 0.55;
}
.fund-industry {
  opacity: 0.68;
}
.fund-value {
  text-align: right;
  font-weight: 700;
}
.ladder-list {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.stock-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 10px;
}
.ladder-stock {
  display: flex;
  flex-direction: column;
  gap: 6px;
  min-width: 0;
  padding: 12px;
  color: inherit;
  border: 1px solid var(--mood-border);
  border-radius: 8px;
  background: var(--mood-card);
  text-align: left;
  cursor: pointer;
}
.ladder-head {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: 8px;
}
.ladder-head span,
.ladder-meta {
  font-size: 12px;
  opacity: 0.62;
}
:deep(.stock-link) {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  gap: 1px;
  padding: 0;
  color: inherit;
  border: 0;
  background: transparent;
  text-align: left;
  cursor: pointer;
}
:deep(.stock-link:hover .stock-name) {
  color: var(--mood-primary);
}
:deep(.stock-name) {
  font-weight: 600;
}
:deep(.stock-symbol) {
  font-size: 11px;
}
:deep(.rank-number) {
  color: var(--mood-primary);
  font-size: 18px;
}

@media (max-width: 768px) {
  .stat-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .trend-chart {
    height: 280px;
  }
  .fund-row {
    grid-template-columns: 28px minmax(120px, 1fr) 104px;
    gap: 8px;
  }
  .fund-industry {
    display: none;
  }
  .stock-grid {
    grid-template-columns: 1fr;
  }
}
</style>
