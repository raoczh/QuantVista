<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { RouterLink, useRoute, useRouter } from 'vue-router'
import { NButton, NEmpty, NGi, NGrid, NResult, NSpin, NTag } from 'naive-ui'
import * as echarts from 'echarts'
import {
  getQuote,
  getDailyBars,
  getValuation,
  getScore,
  type Quote,
  type Bar,
  type Valuation,
  type StockScore,
} from '@/api/market'
import { getNews, newsSourceLabel, sentimentTag, type NewsItem } from '@/api/news'
import { getAnnouncements, type AnnouncementItem } from '@/api/announcement'
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
const news = ref<NewsItem[]>([])
const announcements = ref<AnnouncementItem[]>([])

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

function renderChart() {
  if (!chartEl.value || !bars.value.length) return
  if (chart) {
    chart.dispose()
    chart = null
  }
  chart = echarts.init(chartEl.value, isDark.value ? 'dark' : undefined)
  const up = vars.value.errorColor
  const down = vars.value.successColor
  chart.setOption({
    backgroundColor: 'transparent',
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' }, confine: true },
    grid: { left: 52, right: 16, top: 16, bottom: 36 },
    xAxis: { type: 'category', data: bars.value.map((b) => b.trade_date), boundaryGap: false },
    yAxis: { type: 'value', scale: true, splitLine: { lineStyle: { opacity: 0.4 } } },
    series: [
      {
        type: 'candlestick',
        data: bars.value.map((b) => [b.open, b.close, b.low, b.high]),
        itemStyle: { color: up, color0: down, borderColor: up, borderColor0: down },
      },
    ],
  })
}

watch(isDark, () => renderChart())
// 同页跳转到另一只个股（如从对比/搜索进来）时整页重载。
watch([market, symbol], () => {
  valuation.value = null
  score.value = null
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
})
function onResize() {
  chart?.resize()
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

        <!-- 日 K -->
        <SectionCard title="日 K（近 120 交易日）">
          <div ref="chartEl" class="kchart"></div>
          <n-empty v-if="!bars.length" description="日线数据暂不可用" />
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
  height: 360px;
}
.src-hint {
  font-size: 12px;
  opacity: 0.55;
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
