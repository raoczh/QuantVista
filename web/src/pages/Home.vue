<script setup lang="ts">
import { ref, onMounted, nextTick, watch, computed } from 'vue'
import { NInput, NButton, NGrid, NGi, NSpin, NEmpty, NAlert, NTag, useMessage } from 'naive-ui'
import * as echarts from 'echarts'
import {
  getQuote,
  getDailyBars,
  getOverview,
  type Quote,
  type Bar,
  type Overview,
} from '@/api/market'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import RankList from '@/components/RankList.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const { vars, isDark, pctColor, upColor, downColor } = useUi()

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
  // 涨红跌绿取自主题语义色，坐标轴/背景交给 echarts 主题跟随明暗。
  const up = vars.value.errorColor
  const down = vars.value.successColor
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' } },
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

const sectorsUnavailable = computed(() => !!overview.value?.errors?.sectors)

onMounted(() => {
  loadOverview()
  loadStock()
  window.addEventListener('resize', () => chart?.resize())
})
</script>

<template>
  <PageContainer title="市场首页" subtitle="A 股 · 实时概览与个股速查">
    <template #actions>
      <n-tag v-if="overview" size="small" round :bordered="false">
        更新 {{ fmtTime(overview.data_time) }}
      </n-tag>
      <n-button size="small" secondary :loading="ovLoading" @click="loadOverview">刷新</n-button>
    </template>

    <div class="dashboard">
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
                <div class="stock-row">
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
                <div class="stock-row">
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
            <n-empty description="涨跌家数 / 涨跌停 / 波动率 —— 待数据源接入（阶段 2+）" />
          </SectionCard>
        </n-gi>
      </n-grid>

      <!-- 个股速查 -->
      <SectionCard title="个股速查">
        <template #extra>
          <span class="hint">东财（主）/ 新浪（备） · 仅 A 股已打通</span>
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
        </div>

        <div ref="chartEl" class="quote-chart"></div>
      </SectionCard>

      <!-- 占位 -->
      <SectionCard title="资金流 / 财经新闻 / AI 今日观点">
        <n-empty description="待阶段 4+ 接入（资金流向、新闻情绪、AI 市场摘要）" />
      </SectionCard>

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

/* 榜单行 */
.stock-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  width: 100%;
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
.sr-leader {
  font-size: 12px;
  opacity: 0.6;
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
