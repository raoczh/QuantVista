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
import { getBoardDetail, type BoardDetail, type BoardStock, type Bar } from '@/api/market'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const route = useRoute()
const router = useRouter()
const message = useMessage()
const { isDark, pctColor, upColor, downColor } = useUi()

const code = computed(() => String(route.params.code || ''))
const boardName = computed(() => String(route.query.name || code.value))

const detail = ref<BoardDetail | null>(null)
const loading = ref(false)

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null

const barsUnavailable = computed(() => !!detail.value && !detail.value.bars?.length)
const stocksUnavailable = computed(() => !!detail.value && !detail.value.stocks?.length)

async function load(silent = false) {
  if (!code.value) return
  if (!silent) loading.value = true
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
})
watch(code, () => load())

function onResize() {
  chart?.resize()
}

onMounted(() => {
  load()
  window.addEventListener('resize', onResize)
})
onUnmounted(() => {
  window.removeEventListener('resize', onResize)
  chart?.dispose()
  chart = null
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

        <SectionCard title="成分股（成交额降序）">
          <n-data-table
            v-if="!stocksUnavailable"
            :columns="columns"
            :data="detail?.stocks || []"
            :row-props="rowProps"
            :bordered="false"
            size="small"
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
</style>
