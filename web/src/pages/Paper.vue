<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed, nextTick, watch } from 'vue'
import {
  NButton,
  NInput,
  NInputNumber,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NSpin,
  NEmpty,
  NTag,
  NGrid,
  NGi,
  NPopconfirm,
  NModal,
  NForm,
  NFormItem,
  NAlert,
  useMessage,
} from 'naive-ui'
import * as echarts from 'echarts'
import {
  getPaperOverview,
  paperTrade,
  getPaperTrades,
  resetPaper,
  getPaperCurve,
  type PaperOverview,
  type PaperHolding,
  type PaperTrade,
} from '@/api/paper'
import type { PortfolioCurve } from '@/api/position'
import { isAbortError } from '@/api/client'
import { isEtfSymbol } from '@/api/etf'
import { useUi, withAlpha } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'

const message = useMessage()
const { pctColor, vars, isDark } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

const overview = ref<PaperOverview | null>(null)
const trades = ref<PaperTrade[]>([])
const loading = ref(false)

async function load() {
  loading.value = true
  try {
    ;[overview.value, trades.value] = await Promise.all([getPaperOverview(), getPaperTrades(50)])
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// ---------- 下单 ----------
type TradeForm = { symbol: string; market: string; name?: string; side: 'buy' | 'sell'; price?: number; quantity?: number }
const form = ref<TradeForm>({ symbol: '', market: 'cn', side: 'buy', price: undefined, quantity: undefined })
const trading = ref(false)
async function submitTrade() {
  if (!form.value.symbol.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (!form.value.quantity || form.value.quantity <= 0) {
    message.warning('请输入数量')
    return
  }
  trading.value = true
  try {
    const t = await paperTrade({
      symbol: form.value.symbol.trim(),
      market: form.value.market,
      name: form.value.name,
      side: form.value.side,
      price: form.value.price,
      quantity: form.value.quantity,
    })
    message.success(`${t.side === 'buy' ? '买入' : '卖出'} ${t.name || t.symbol} ${t.quantity} 股 @ ${t.price.toFixed(2)}`)
    form.value.quantity = undefined
    form.value.price = undefined
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    trading.value = false
  }
}
// 从持仓快捷卖出预填。
function sellFrom(h: PaperHolding) {
  form.value = { symbol: h.symbol, market: h.market, name: h.name, side: 'sell', price: undefined, quantity: h.quantity }
}

// ---------- 重置 ----------
const resetModal = ref(false)
const resetCash = ref(100000)
async function doReset() {
  try {
    await resetPaper(resetCash.value)
    resetModal.value = false
    await load()
    message.success('账户已重置')
  } catch (e) {
    message.error((e as Error).message)
  }
}

function fmtMoney(n: number) {
  return (n >= 0 ? '' : '-') + Math.abs(n).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}
function fmt(n: number) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

// ---------- B7 资产曲线 ----------
const curveEl = ref<HTMLDivElement | null>(null)
let curveChart: echarts.ECharts | null = null
const curve = ref<PortfolioCurve | null>(null)
const curveDays = ref(90)
const curveLoading = ref(false)
let curveAbort: AbortController | null = null
let curveSeq = 0
const curveDayOptions = [
  { label: '近 30 天', value: 30 },
  { label: '近 90 天', value: 90 },
  { label: '近 180 天', value: 180 },
  { label: '近 1 年', value: 365 },
]

async function loadCurve() {
  curveAbort?.abort()
  const myAbort = new AbortController()
  curveAbort = myAbort
  const mySeq = ++curveSeq
  curveLoading.value = true
  try {
    const data = await getPaperCurve(curveDays.value, myAbort.signal)
    if (mySeq !== curveSeq) return
    curve.value = data
    await nextTick()
    renderCurve()
  } catch (e) {
    if (mySeq !== curveSeq || isAbortError(e)) return
    curve.value = null
  } finally {
    if (mySeq === curveSeq) curveLoading.value = false
  }
}
watch(curveDays, () => loadCurve())

function renderCurve() {
  const c = curve.value
  if (!curveEl.value || !c?.points.length) {
    curveChart?.dispose()
    curveChart = null
    return
  }
  curveChart?.dispose()
  curveChart = echarts.init(curveEl.value, isDark.value ? 'dark' : undefined)
  const primary = vars.value.primaryColor
  const warn = vars.value.warningColor
  curveChart.setOption({
    backgroundColor: 'transparent',
    tooltip: {
      trigger: 'axis',
      confine: true,
      formatter: (ps: { axisValue: string; seriesName: string; value: number; dataIndex: number }[]) => {
        if (!ps.length) return ''
        const p = c.points[ps[0].dataIndex]
        const lines = ps.map((s) => `${s.seriesName} ${fmtMoney(s.value)}`)
        if (p?.partial) lines.push(`⚠ ${p.note || '当日部分标的无有效行情，非完整净值'}`)
        return `${ps[0].axisValue}<br/>${lines.join('<br/>')}`
      },
    },
    legend: {
      top: 0,
      data: ['总资产', '持仓市值'],
      textStyle: { color: vars.value.textColor3, fontSize: 11 },
      itemWidth: 14,
      itemHeight: 8,
    },
    grid: { left: 62, right: 18, top: 30, bottom: 28 },
    xAxis: {
      type: 'category',
      data: c.points.map((p) => p.trade_date),
      boundaryGap: false,
      axisLabel: { hideOverlap: true, fontSize: 10 },
    },
    yAxis: {
      type: 'value',
      scale: true,
      splitLine: { lineStyle: { color: vars.value.dividerColor } },
      axisLabel: { fontSize: 10 },
    },
    series: [
      {
        name: '总资产',
        type: 'line',
        data: c.points.map((p) => p.total_assets),
        symbol: 'circle',
        // partial 点用空心大点标出：那天有标的没有有效行情，不是完整净值。
        symbolSize: (_v: number, params: { dataIndex: number }) => (c.points[params.dataIndex]?.partial ? 8 : 3),
        itemStyle: {
          color: (params: { dataIndex: number }) => (c.points[params.dataIndex]?.partial ? warn : primary),
        },
        lineStyle: { width: 2, color: primary },
        areaStyle: { color: withAlpha(primary, 0.1) },
      },
      {
        name: '持仓市值',
        type: 'line',
        data: c.points.map((p) => p.market_value),
        symbol: 'none',
        lineStyle: { width: 1.5, type: 'dashed', color: warn },
        itemStyle: { color: warn },
      },
    ],
  })
}
function onResize() {
  curveChart?.resize()
}
// 主题变化整套重绘（6 套主题里明暗只是其一，只监听 isDark 会留在旧色板上）。
watch([isDark, vars], () => renderCurve())

onMounted(() => {
  load()
  loadCurve()
  window.addEventListener('resize', onResize)
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', onResize)
  curveSeq++
  curveAbort?.abort()
  curveAbort = null
  curveChart?.dispose()
  curveChart = null
})
</script>

<template>
  <PageContainer title="模拟交易" subtitle="虚拟账户 · 按当前有效行情成交与估值 · 练手不担风险">
    <template #actions>
      <n-button size="small" quaternary @click="resetModal = true">重置账户</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="paper" :style="styleVars">
      <!-- 账户总览 -->
      <n-grid cols="2 s:4" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="总资产" :value="fmtMoney(overview?.total_assets ?? 0)" />
        </n-gi>
        <n-gi>
          <StatCard label="可用现金" :value="fmtMoney(overview?.account.cash ?? 0)" />
        </n-gi>
        <n-gi>
          <StatCard
            label="总盈亏"
            :value="fmtMoney(overview?.total_profit ?? 0)"
            :change-pct="overview?.total_profit_pct ?? 0"
          />
        </n-gi>
        <n-gi>
          <StatCard label="累计已实现" :value="fmtMoney(overview?.realized_pnl ?? 0)" />
        </n-gi>
      </n-grid>

      <!-- B7 资产曲线：读每交易日 16:20 落库的快照，不做插值补造 -->
      <SectionCard title="资产曲线">
        <template #extra>
          <div class="curve-actions">
            <n-select v-model:value="curveDays" :options="curveDayOptions" size="small" style="width: 120px" />
            <n-button size="tiny" quaternary :loading="curveLoading" @click="loadCurve">刷新</n-button>
          </div>
        </template>
        <n-spin :show="curveLoading && !curve">
          <div v-show="!!curve?.points.length" ref="curveEl" class="curve-chart"></div>
          <n-empty
            v-if="!curve?.points.length"
            description="暂无资产快照——曲线自启用之日起按交易日盘后积累，不回溯历史"
          />
          <div v-if="curve?.notes?.length" class="curve-notes">
            <div v-for="(n, i) in curve.notes" :key="i">{{ n }}</div>
          </div>
        </n-spin>
      </SectionCard>

      <div class="cols">
        <!-- 下单 -->
        <SectionCard title="下单">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-form-item label="方向">
              <n-radio-group v-model:value="form.side">
                <n-radio-button value="buy">买入</n-radio-button>
                <n-radio-button value="sell">卖出</n-radio-button>
              </n-radio-group>
            </n-form-item>
            <n-grid cols="1 s:2" responsive="screen" :x-gap="10">
              <n-gi>
                <n-form-item label="代码">
                  <n-input v-model:value="form.symbol" placeholder="如 600000" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item label="市场">
                  <n-select v-model:value="form.market" :options="marketOptions" />
                </n-form-item>
              </n-gi>
            </n-grid>
            <n-grid cols="1 s:2" responsive="screen" :x-gap="10">
              <n-gi>
                <n-form-item label="价格（留空按市价）">
                  <n-input-number v-model:value="form.price" :min="0" :precision="3" style="width: 100%" placeholder="市价" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item label="数量">
                  <n-input-number v-model:value="form.quantity" :min="0" style="width: 100%" />
                </n-form-item>
              </n-gi>
            </n-grid>
            <n-button
              :type="form.side === 'buy' ? 'error' : 'success'"
              block
              :loading="trading"
              @click="submitTrade"
            >
              {{ form.side === 'buy' ? '模拟买入' : '模拟卖出' }}
            </n-button>
            <div class="hint">佣金万 2.5（最低 5 元），A 股股票卖出另计印花税万 5；ETF/基金卖出免印花税；留空价格按最新行情成交。</div>
          </n-form>
        </SectionCard>

        <!-- 持仓 -->
        <SectionCard title="模拟持仓">
          <n-spin :show="loading && !overview">
            <n-alert
              v-if="overview?.valuation_note"
              type="warning"
              :bordered="false"
              style="margin-bottom: 10px"
              >{{ overview.valuation_note }}</n-alert
            >
            <n-empty v-if="!overview?.holdings.length" description="暂无持仓，在左侧下单买入" />
            <div v-else class="holdings">
              <div v-for="h in overview.holdings" :key="h.id" class="hold">
                <div class="hold-main">
                  <div class="hold-title">
                    <span class="hold-name">{{ h.name || h.symbol }}</span>
                    <span class="hold-symbol qv-mono">{{ h.symbol }}</span>
                    <n-tag v-if="isEtfSymbol(h.symbol)" size="tiny" round :bordered="false" type="info">ETF</n-tag>
                    <FreshnessTag :status="h.freshness_status" :as-of="h.quote_as_of" :reason="h.stale_reason" />
                  </div>
                  <div class="hold-sub">
                    {{ h.quantity }} 股 · 成本 {{ fmt(h.avg_cost) }} · 现价
                    {{ h.quote_ok ? fmt(h.price) : '—' }}
                    <template v-if="!h.quote_ok">（按成本估值，浮盈未知）</template>
                  </div>
                </div>
                <div class="hold-pnl">
                  <div class="pnl-val" :style="{ color: pctColor(h.profit_amount) }">
                    {{ h.quote_ok ? fmtMoney(h.profit_amount) : '—' }}
                  </div>
                  <div class="pnl-pct" :style="{ color: pctColor(h.profit_pct) }">
                    {{ h.quote_ok ? h.profit_pct.toFixed(2) + '%' : '—' }}
                  </div>
                </div>
                <n-button size="tiny" tertiary @click="sellFrom(h)">卖出</n-button>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>

      <!-- 成交流水 -->
      <SectionCard title="成交流水">
        <n-empty v-if="!trades.length" description="暂无成交" size="small" />
        <div v-else class="trades">
          <div v-for="t in trades" :key="t.id" class="trade">
            <n-tag size="tiny" round :bordered="false" :type="t.side === 'buy' ? 'error' : 'success'">{{
              t.side === 'buy' ? '买' : '卖'
            }}</n-tag>
            <span class="tr-name">{{ t.name || t.symbol }}</span>
            <n-tag v-if="isEtfSymbol(t.symbol)" size="tiny" round :bordered="false" type="info">ETF</n-tag>
            <span class="tr-detail">{{ t.quantity }} 股 @ {{ fmt(t.price) }}</span>
            <span class="tr-amount">{{ fmtMoney(t.amount) }}</span>
            <span v-if="t.side === 'sell'" class="tr-pnl" :style="{ color: pctColor(t.realized_pnl) }">
              盈亏 {{ fmtMoney(t.realized_pnl) }}
            </span>
            <span class="tr-time">{{ fmtTime(t.created_at) }}</span>
          </div>
        </div>
      </SectionCard>
    </div>

    <!-- 重置弹窗 -->
    <n-modal v-model:show="resetModal" preset="card" title="重置模拟账户" style="max-width: 380px">
      <p class="reset-tip">将清空所有模拟持仓与成交流水，现金恢复为初始资金。</p>
      <n-form-item label="初始资金">
        <n-input-number v-model:value="resetCash" :min="1000" :step="10000" style="width: 100%" />
      </n-form-item>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="resetModal = false">取消</n-button>
          <n-popconfirm @positive-click="doReset">
            <template #trigger>
              <n-button type="primary">确认重置</n-button>
            </template>
            确定清空并重置账户？
          </n-popconfirm>
        </div>
      </template>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.paper {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.cols {
  display: grid;
  grid-template-columns: 340px 1fr;
  gap: 16px;
  align-items: start;
}
@media (max-width: 900px) {
  .cols {
    grid-template-columns: 1fr;
  }
}
.form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.hint {
  font-size: 12px;
  opacity: 0.5;
  margin-top: 8px;
  line-height: 1.5;
}
.holdings {
  display: flex;
  flex-direction: column;
}
.hold {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.hold:last-child {
  border-bottom: none;
}
.hold-main {
  flex: 1;
  min-width: 0;
}
.hold-title {
  display: flex;
  align-items: baseline;
  gap: 8px;
  /* 长名 ETF（名称+代码+标签 >200px）在移动端 150px 主列里换行而非溢出压到盈亏列 */
  flex-wrap: wrap;
}
.hold-name {
  font-size: 14px;
  font-weight: 600;
}
.hold-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.hold-sub {
  font-size: 12px;
  opacity: 0.65;
  margin-top: 3px;
}
.hold-pnl {
  text-align: right;
}
.pnl-val {
  font-size: 14px;
  font-weight: 600;
}
.pnl-pct {
  font-size: 12px;
}
.trades {
  display: flex;
  flex-direction: column;
}
.trade {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 9px 4px;
  border-bottom: 1px solid var(--qv-divider);
  font-size: 13px;
  flex-wrap: wrap;
}
.trade:last-child {
  border-bottom: none;
}
.tr-name {
  font-weight: 500;
  min-width: 80px;
}
.tr-detail {
  opacity: 0.7;
}
.tr-amount {
  opacity: 0.85;
}
.tr-pnl {
  font-weight: 500;
}
.tr-time {
  margin-left: auto;
  font-size: 11px;
  opacity: 0.45;
}
.reset-tip {
  font-size: 13px;
  opacity: 0.7;
  margin: 0 0 12px;
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}

/* ---------- B7 资产曲线 ---------- */
.curve-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.curve-chart {
  width: 100%;
  height: 300px;
}
.curve-notes {
  margin-top: 8px;
  font-size: 12px;
  opacity: 0.6;
  line-height: 1.7;
}
@media (max-width: 768px) {
  .curve-chart {
    height: 240px;
  }
}
</style>
