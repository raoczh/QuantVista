<script setup lang="ts">
import { computed, h, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NDataTable,
  NEmpty,
  NSpin,
  NTag,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import * as echarts from 'echarts'
import {
  getBoardDetail,
  getBoardFundFlow,
  type BoardDetail,
  type BoardFundFlow,
  type BoardStock,
  type Bar,
} from '@/api/market'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const route = useRoute()
const router = useRouter()
const message = useMessage()
const { isDark, pctColor, upColor, downColor, vars } = useUi()

const code = computed(() => String(route.params.code || ''))
const boardName = computed(() => String(route.query.name || code.value))

const detail = ref<BoardDetail | null>(null)
const fundflow = ref<BoardFundFlow | null>(null)
const loading = ref(false)

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null
const ffEl = ref<HTMLDivElement | null>(null)
let ffChart: echarts.ECharts | null = null

const barsUnavailable = computed(() => !!detail.value && !detail.value.bars?.length)
const stocksUnavailable = computed(() => !!detail.value && !detail.value.stocks?.length)
const valuation = computed(() => detail.value?.valuation || null)

async function load(silent = false) {
  if (!code.value) return
  if (!silent) loading.value = true
  // 板块资金流 best-effort：push2his 限流属常态，失败只留空卡不打断详情页。
  getBoardFundFlow('cn', code.value, 90)
    .then((r) => {
      fundflow.value = r
      nextTick(() => renderFundFlowChart())
    })
    .catch(() => (fundflow.value = null))
  try {
    detail.value = await getBoardDetail('cn', code.value)
    await nextTick()
    if (detail.value.bars?.length) renderChart(detail.value.bars)
  } catch (e) {
    detail.value = null
    if (!silent) message.error('板块详情加载失败：' + (e as Error).message)
  } finally {
    if (!silent) loading.value = false
  }
}

function renderChart(bars: Bar[]) {
  if (!chartEl.value) return
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  // A 股语义色走 useUi 的涨跌映射（勿直接取 errorColor/successColor——语义映射未来可能调整）。
  const up = upColor.value
  const down = downColor.value
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

// 板块主力资金图（P3b）：复用个股卡画法——逐日主力净额柱（红入绿出，亿元）+ 累计线（右轴）。
function renderFundFlowChart() {
  const ff = fundflow.value
  if (!ffEl.value || !ff?.days.length) return
  ffChart?.dispose()
  ffChart = echarts.init(ffEl.value, isDark.value ? 'dark' : undefined)
  const up = upColor.value
  const down = downColor.value
  let acc = 0
  const cum = ff.days.map((d) => {
    acc += d.main_net_yi
    return Math.round(acc * 100) / 100
  })
  ffChart.setOption({
    backgroundColor: 'transparent',
    tooltip: {
      trigger: 'axis',
      confine: true,
      formatter: (ps: { axisValue: string; seriesName: string; value: number }[]) =>
        ps.length
          ? `${ps[0].axisValue}<br/>` + ps.map((p) => `${p.seriesName} ${p.value} 亿`).join('<br/>')
          : '',
    },
    legend: {
      top: 0,
      data: ['主力净额(亿)', '区间累计(亿)'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: { left: 52, right: 52, top: 28, bottom: 24 },
    xAxis: { type: 'category', data: ff.days.map((d) => d.date), axisLabel: { fontSize: 10 } },
    yAxis: [
      { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.3 } }, axisLabel: { fontSize: 10 } },
      { type: 'value', scale: true, splitLine: { show: false }, axisLabel: { fontSize: 10 } },
    ],
    series: [
      {
        type: 'bar',
        name: '主力净额(亿)',
        data: ff.days.map((d) => d.main_net_yi),
        itemStyle: { color: (p: { value: number }) => (p.value >= 0 ? up : down) },
        barMaxWidth: 8,
      },
      {
        type: 'line',
        name: '区间累计(亿)',
        yAxisIndex: 1,
        data: cum,
        symbol: 'none',
        lineStyle: { width: 1.5, color: vars.value.primaryColor },
      },
    ],
  })
}

function streakText(ff: BoardFundFlow) {
  if (ff.streak_days > 0) return `连续净流入 ${ff.streak_days} 天`
  if (ff.streak_days < 0) return `连续净流出 ${-ff.streak_days} 天`
  return '—'
}

function pctRankText(v: number) {
  return v >= 0 ? v.toFixed(0) + '%' : '—'
}

function fmtYi(n: number): string {
  return n ? (n / 1e8).toFixed(2) + ' 亿' : '-'
}

const columns = computed<DataTableColumns<BoardStock>>(() => [
  {
    title: '名称',
    key: 'name',
    render(row) {
      const tags = []
      if (row.is_leader) tags.push(h(NTag, { size: 'tiny', type: 'error', bordered: false, round: true }, () => '龙头'))
      if (row.is_top_gainer) tags.push(h(NTag, { size: 'tiny', type: 'warning', bordered: false, round: true }, () => '领涨'))
      return h('div', { class: 'cell-name' }, [
        h('span', { class: 'cn-title' }, row.name),
        h('span', { class: 'cn-symbol qv-mono' }, row.symbol),
        ...tags,
      ])
    },
  },
  {
    title: '现价',
    key: 'price',
    align: 'right',
    render: (row) => (row.price ? row.price.toFixed(2) : '-'),
  },
  {
    title: '涨跌幅',
    key: 'change_pct',
    align: 'right',
    render(row) {
      const sign = row.change_pct >= 0 ? '+' : ''
      return h('span', { style: { color: pctColor(row.change_pct), fontWeight: 600 } }, `${sign}${row.change_pct.toFixed(2)}%`)
    },
  },
  { title: '成交额', key: 'amount', align: 'right', render: (row) => fmtYi(row.amount) },
  {
    title: '换手率',
    key: 'turnover_rate',
    align: 'right',
    render: (row) => (row.turnover_rate ? row.turnover_rate.toFixed(2) + '%' : '-'),
  },
  { title: '流通市值', key: 'float_cap', align: 'right', render: (row) => fmtYi(row.float_cap) },
])

function rowProps(row: BoardStock) {
  return {
    style: 'cursor: pointer;',
    onClick: () => router.push(`/stocks/cn/${row.symbol}`),
  }
}

watch(isDark, () => {
  if (detail.value?.bars?.length) renderChart(detail.value.bars)
  if (fundflow.value?.days.length) renderFundFlowChart()
})
watch(code, () => load())

function onResize() {
  chart?.resize()
  ffChart?.resize()
}

onMounted(() => {
  load()
  window.addEventListener('resize', onResize)
})
onUnmounted(() => {
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
  ffChart?.dispose()
  ffChart = null
})
</script>

<template>
  <PageContainer :title="boardName" :subtitle="`板块详情 · ${code}`">
    <template #actions>
      <n-button size="small" secondary @click="router.push('/heatmap')">← 热力图</n-button>
      <n-button size="small" secondary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <n-spin :show="loading && !detail">
      <div class="board-grid">
        <SectionCard title="板块指数日线">
          <div v-show="!barsUnavailable" ref="chartEl" class="board-chart"></div>
          <n-empty
            v-if="barsUnavailable"
            description="板块指数日线依赖东财接口，当前限流暂不可用，稍后重试"
          />
        </SectionCard>

        <!-- 板块主力资金（P3b）：上游透传不落库，push2his 限流时留空 -->
        <SectionCard title="主力资金（近 90 交易日）">
          <template #extra>
            <span class="src-hint">东财板块资金流 · 主力=超大单+大单</span>
          </template>
          <div v-if="fundflow && fundflow.days.length" class="ff-wrap">
            <div class="ff-grid">
              <div class="qc"><span class="qc-k">最新一日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_1d_yi) }">{{ fundflow.main_net_1d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">近 5 日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_5d_yi) }">{{ fundflow.main_net_5d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">近 10 日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_10d_yi) }">{{ fundflow.main_net_10d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">近 20 日</span><span class="qc-v qv-tnum" :style="{ color: pctColor(fundflow.main_net_20d_yi) }">{{ fundflow.main_net_20d_yi.toFixed(2) }} 亿</span></div>
              <div class="qc"><span class="qc-k">连续方向</span><span class="qc-v" :style="{ color: pctColor(fundflow.streak_days) }">{{ streakText(fundflow) }}</span></div>
            </div>
            <div ref="ffEl" class="ff-chart"></div>
            <div class="src-hint">
              主力净额=超大单+大单口径（东财），资金流向≠板块必然方向；数据截至 {{ fundflow.last_date }}；仅研究参考。
            </div>
          </div>
          <n-empty v-else description="暂无板块资金流数据（东财源限流时可稍后刷新）" />
        </SectionCard>

        <!-- 板块估值（P3b）：行业板块聚合表；概念板块无 f100 覆盖不渲染 -->
        <SectionCard v-if="valuation" title="估值（板块中位数）">
          <template #extra>
            <span class="src-hint">正样本中位数口径 · {{ valuation.trade_date }}</span>
          </template>
          <div class="ff-wrap">
            <div class="ff-grid">
              <div class="qc"><span class="qc-k">中位 PE(TTM)</span><span class="qc-v qv-tnum">{{ valuation.median_pe_ttm > 0 ? valuation.median_pe_ttm.toFixed(2) : '—' }}</span></div>
              <div class="qc"><span class="qc-k">中位 PB</span><span class="qc-v qv-tnum">{{ valuation.median_pb > 0 ? valuation.median_pb.toFixed(2) : '—' }}</span></div>
              <div class="qc"><span class="qc-k">横截面分位</span><span class="qc-v qv-tnum">{{ pctRankText(valuation.pct_rank) }}</span></div>
              <div class="qc"><span class="qc-k">时序分位</span><span class="qc-v qv-tnum">{{ pctRankText(valuation.hist_pct_rank) }}<span class="qc-sub">（{{ valuation.hist_days }} 日积累）</span></span></div>
              <div class="qc"><span class="qc-k">PE 样本</span><span class="qc-v qv-tnum">{{ valuation.pos_pe_count }} / {{ valuation.stock_count }} 只</span></div>
            </div>
            <div class="src-hint">
              中位数只统计正 PE/PB 样本（亏损与停牌不计）；横截面分位=当日在全部行业板块中的位置（越高越贵）；时序分位窗口 ≤250 交易日，从 {{ valuation.hist_days }} 日起逐日积累，天数少时仅供参考。
            </div>
          </div>
        </SectionCard>

        <SectionCard title="成分股（成交额降序）">
          <!-- scroll-x 让 6 列宽表在窄屏走 n-data-table 自带横滚（SectionCard 兜底不覆盖
               NDataTable：其表宽恒 100% + word-break 会把数值列拦腰折行） -->
          <n-data-table
            v-if="!stocksUnavailable"
            :columns="columns"
            :data="detail?.stocks || []"
            :row-props="rowProps"
            :bordered="false"
            size="small"
            :scroll-x="640"
          />
          <n-empty
            v-else
            description="成分股依赖东财接口，当前限流暂不可用，稍后重试"
          />
        </SectionCard>
      </div>
    </n-spin>
  </PageContainer>
</template>

<style scoped>
.board-grid {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.board-chart {
  width: 100%;
  height: 360px;
}
.cell-name {
  display: flex;
  align-items: center;
  gap: 8px;
}
.cn-title {
  font-weight: 500;
}
.cn-symbol {
  font-size: 12px;
  opacity: 0.5;
}
/* 主力资金 / 估值卡（口径对齐 StockDetail 同名卡样式） */
.ff-wrap {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.ff-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(110px, 1fr));
  gap: 10px 16px;
}
.qc {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.qc-k {
  font-size: 12px;
  opacity: 0.55;
}
.qc-v {
  font-size: 14px;
  font-weight: 600;
}
.qc-sub {
  font-size: 11px;
  font-weight: 400;
  opacity: 0.55;
}
.ff-chart {
  width: 100%;
  height: 280px;
}
.src-hint {
  font-size: 12px;
  opacity: 0.55;
  line-height: 1.6;
}
</style>
