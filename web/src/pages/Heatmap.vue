<script setup lang="ts">
import { ref, onMounted, onUnmounted, nextTick, watch } from 'vue'
import { useRouter } from 'vue-router'
import { NRadioGroup, NRadioButton, NButton, NSpin, NEmpty, useMessage } from 'naive-ui'
import * as echarts from 'echarts'
import { getBoardHeatmap, type BoardHeat, type BoardKind } from '@/api/market'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const router = useRouter()
const message = useMessage()
const { vars, isDark, upColor, downColor, flatColor, withAlpha } = useUi()

const kind = ref<BoardKind>('industry')
const boards = ref<BoardHeat[]>([])
const loading = ref(false)

const chartEl = ref<HTMLDivElement | null>(null)
let chart: echarts.ECharts | null = null

// 板块切换（industry/concept）竞态守卫：快速来回切时旧响应不覆盖新选择。
let loadSeq = 0

async function load(silent = false) {
  const mySeq = ++loadSeq
  if (!silent) loading.value = true
  try {
    const data = await getBoardHeatmap('cn', kind.value)
    if (mySeq !== loadSeq) return
    boards.value = data
    await nextTick()
    renderChart()
  } catch (e) {
    if (mySeq !== loadSeq) return
    boards.value = []
    if (!silent) message.error('板块热度加载失败：' + (e as Error).message)
  } finally {
    if (mySeq === loadSeq && !silent) loading.value = false
  }
}

// 颜色铁律：涨红跌绿取自主题语义色，按 |涨跌幅| 深浅插值；0 附近用平盘中性色。
function nodeColor(pct: number): string {
  if (Math.abs(pct) < 0.05) return withAlpha(flatColor.value, 0.35)
  const base = pct > 0 ? upColor.value : downColor.value
  const alpha = 0.25 + (Math.min(Math.abs(pct), 8) / 8) * 0.7
  return withAlpha(base, alpha)
}

function fmtYi(n: number): string {
  return (n / 1e8).toFixed(1) + ' 亿'
}

function renderChart() {
  if (!chartEl.value) return
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  // 窄屏格子小（Top100 挤 ~360×460）：标签只出名称单行、字号降档、缝隙收窄，
  // 涨跌幅靠色深与 tooltip（confine 已开）承载。
  const isNarrow = window.matchMedia('(max-width: 768px)').matches
  const data = boards.value.map((b) => ({
    name: b.name,
    value: Math.max(b.amount, 1), // 面积=成交额（0 值兜底防不显示）
    itemStyle: { color: nodeColor(b.change_pct) },
    raw: b,
  }))
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: {
      confine: true,
      formatter: (info: { data?: { raw?: BoardHeat } }) => {
        const b = info.data?.raw
        if (!b) return ''
        const sign = b.change_pct >= 0 ? '+' : ''
        const col = b.change_pct > 0 ? upColor.value : b.change_pct < 0 ? downColor.value : flatColor.value
        return `<div style="font-weight:600;margin-bottom:4px">${b.name} <span style="opacity:.6;font-size:12px">${b.code}</span></div>
          <div>涨跌幅：<span style="color:${col};font-weight:600">${sign}${b.change_pct.toFixed(2)}%</span></div>
          <div>成交额：${fmtYi(b.amount)}</div>
          <div>涨/跌家数：<span style="color:${upColor.value}">${b.advances}</span> / <span style="color:${downColor.value}">${b.declines}</span></div>
          <div>领涨股：${b.leader || '-'}</div>`
      },
    },
    series: [
      {
        type: 'treemap',
        roam: false,
        nodeClick: false,
        breadcrumb: { show: false },
        width: '100%',
        height: '100%',
        top: 0,
        left: 0,
        right: 0,
        bottom: 0,
        label: {
          show: true,
          formatter: (info: { data?: { raw?: BoardHeat } }) => {
            const b = info.data?.raw
            if (!b) return ''
            if (isNarrow) return b.name
            const sign = b.change_pct >= 0 ? '+' : ''
            return `${b.name}\n${sign}${b.change_pct.toFixed(2)}%`
          },
          fontSize: isNarrow ? 10 : 12,
          lineHeight: isNarrow ? 12 : 16,
          color: vars.value.textColor1,
        },
        itemStyle: {
          borderColor: vars.value.bodyColor,
          borderWidth: isNarrow ? 1 : 2,
          gapWidth: isNarrow ? 1 : 2,
        },
        emphasis: { itemStyle: { borderColor: vars.value.primaryColor } },
        data,
      },
    ],
  })
  chart.off('click')
  chart.on('click', (p) => {
    const b = (p.data as { raw?: BoardHeat } | undefined)?.raw
    if (b?.code) router.push(`/boards/${b.code}?name=${encodeURIComponent(b.name)}`)
  })
}

watch(kind, () => load())
watch(isDark, () => {
  if (boards.value.length) renderChart()
})

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
  <PageContainer title="行业热力图" subtitle="A 股 · 面积为成交额，颜色为涨跌幅，点击进入板块详情">
    <template #actions>
      <n-radio-group v-model:value="kind" size="small">
        <n-radio-button value="industry">行业板块</n-radio-button>
        <n-radio-button value="concept">概念板块</n-radio-button>
      </n-radio-group>
      <n-button size="small" secondary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <SectionCard :title="kind === 'industry' ? '行业板块（成交额 Top100）' : '概念板块（成交额 Top100）'">
      <n-spin :show="loading && !boards.length">
        <div v-show="boards.length" ref="chartEl" class="heatmap-chart"></div>
        <n-empty
          v-if="!loading && !boards.length"
          description="板块热度依赖东财接口，当前限流暂不可用，稍后重试"
        />
      </n-spin>
    </SectionCard>
  </PageContainer>
</template>

<style scoped>
.heatmap-chart {
  width: 100%;
  height: 620px;
}

@media (max-width: 768px) {
  .heatmap-chart {
    height: 460px;
  }
}
</style>
