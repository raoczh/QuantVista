<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed } from 'vue'
import {
  NAlert,
  NButton,
  NInput,
  NInputNumber,
  NRadioGroup,
  NRadioButton,
  NSpin,
  NEmpty,
  NTag,
  NTable,
  NGrid,
  NGi,
  NModal,
  NForm,
  NFormItem,
  useMessage,
} from 'naive-ui'
import { getPaperOverview, paperTrade, type PaperOverview, type PaperHolding } from '@/api/paper'
import { getEtfList, isEtfSymbol, type EtfItem } from '@/api/etf'
import { getSessionEpoch } from '@/api/token'
import { formatPrice } from '@/lib/formatPrice'
import { useUi } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'
import StockIdentity from '@/components/StockIdentity.vue'

const message = useMessage()
const { pctColor } = useUi()

const etfs = ref<EtfItem[]>([])
const overview = ref<PaperOverview | null>(null)
const loading = ref(false)
const loadError = ref('')
const accountId = ref<number>()
let loadSeq = 0
let disposed = false
const isCurrent = (owner: number) => !disposed && owner === getSessionEpoch()

// ETF 持仓：从模拟账户总览里按代码前缀过滤（后端持仓不区分资产类型）。
const etfHoldings = computed<PaperHolding[]>(() =>
  (overview.value?.holdings ?? []).filter((h) => h.market === 'cn' && isEtfSymbol(h.symbol)),
)
const etfMarketValue = computed(() =>
  etfHoldings.value.reduce((s, h) => s + (h.quote_ok ? h.market_value : h.cost), 0),
)
const etfProfit = computed(() => etfHoldings.value.reduce((s, h) => s + h.profit_amount, 0))
// 行情非 fresh 的持仓：后端按成本估值、盈亏记 0（fail-closed）。前端必须如实标注，
// 不得把成本冒充「市值」、把 0 冒充「盈亏」。
const etfStaleCount = computed(() => etfHoldings.value.filter((h) => !h.quote_ok).length)
const etfAllQuotesMissing = computed(() => etfHoldings.value.length > 0 && etfStaleCount.value === etfHoldings.value.length)
const etfCostUnknownCount = computed(() => etfHoldings.value.filter((h) => !!h.cost_basis_note).length)
const etfValuationReason = computed(() => etfHoldings.value.find((h) => h.valuation_unavailable_reason)?.valuation_unavailable_reason)
const etfValueSub = computed(() =>
  etfStaleCount.value > 0 ? `${etfStaleCount.value} 笔行情非最新，按成本计入（非实时市值）` : undefined,
)
const etfProfitSub = computed(() =>
  etfStaleCount.value > 0 ? `不含 ${etfStaleCount.value} 笔行情非最新持仓（盈亏未知）` : undefined,
)
const latestQuoteTime = computed(() => etfs.value.map((item) => item.quote_as_of).filter(Boolean).sort().at(-1) || '未知')

async function load(silent = false) {
  if (disposed) return
  const owner = getSessionEpoch()
  const seq = ++loadSeq
  loading.value = true
  try {
    const [quotes, account] = await Promise.allSettled([getEtfList(), getPaperOverview(accountId.value)])
    if (seq !== loadSeq || !isCurrent(owner)) return
    const errors: string[] = []
    if (quotes.status === 'fulfilled') etfs.value = quotes.value
    else {
      etfs.value = []
      errors.push('ETF 行情：' + (quotes.reason as Error).message)
    }
    if (account.status === 'fulfilled') {
      overview.value = account.value
      accountId.value = account.value.account.account_id
    } else {
      overview.value = null
      errors.push('模拟账户：' + (account.reason as Error).message)
    }
    loadError.value = errors.join('；')
    if (loadError.value && !silent) message.error(loadError.value)
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// 盘中每 60s 自动刷新行情（切后台/非交易时段自动暂停）。
useAutoRefresh(() => load(true), 60_000)

// ---------- 交易弹窗 ----------
type TradeForm = { symbol: string; name: string; side: 'buy' | 'sell'; price?: number; quantity?: number }
const tradeModal = ref(false)
const trading = ref(false)
const form = ref<TradeForm>({ symbol: '', name: '', side: 'buy', price: undefined, quantity: undefined })

function openTrade(symbol: string, name: string, side: 'buy' | 'sell', quantity?: number) {
  if (trading.value) return
  form.value = { symbol, name, side, price: undefined, quantity }
  tradeModal.value = true
}
function tradeFromEtf(e: EtfItem, side: 'buy' | 'sell') {
  openTrade(e.symbol, e.name, side)
}
function tradeFromHolding(h: PaperHolding, side: 'buy' | 'sell') {
  openTrade(h.symbol, h.name || '名称待补全', side, side === 'sell' ? h.quantity : undefined)
}

async function submitTrade() {
  if (trading.value) return
  if (!overview.value || !accountId.value) {
    message.warning('请先成功加载模拟账户，再提交交易')
    return
  }
  if (!form.value.quantity || form.value.quantity <= 0) {
    message.warning('请输入数量')
    return
  }
  trading.value = true
  const owner = getSessionEpoch()
  try {
    const t = await paperTrade({
      symbol: form.value.symbol,
      market: 'cn',
      name: form.value.name,
      side: form.value.side,
      price: form.value.price,
      quantity: form.value.quantity,
    }, accountId.value)
    if (!isCurrent(owner)) return
    message.success(`${t.side === 'buy' ? '买入' : '卖出'} ${t.name || '名称待补全'}（${t.symbol}）${t.quantity} 份 @ ${formatPrice(t.price)}`)
    tradeModal.value = false
    await load()
  } catch (e) {
    if (isCurrent(owner)) message.error((e as Error).message)
  } finally {
    if (isCurrent(owner)) trading.value = false
  }
}

function fmtMoney(n: number) {
  return (n >= 0 ? '' : '-') + Math.abs(n).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}
function fmt(n: number) {
  // ETF 最小变动价位 0.001 元，价格类数值统一三位小数。
  return n == null ? '-' : n.toFixed(3)
}

onMounted(() => load())
onBeforeUnmount(() => { disposed = true; loadSeq++ })
</script>

<template>
  <PageContainer title="指数 ETF" subtitle="精选宽基/行业/跨境 ETF · 一键模拟买卖 · 行情带时效标注">
    <template #actions>
      <n-button size="small" quaternary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <div class="etf">
      <n-alert v-if="loadError" type="error" :bordered="false" title="部分数据读取失败">{{ loadError }}</n-alert>
      <n-alert v-if="!loading && !etfs.length && !loadError" type="warning" :bordered="false" title="ETF 行情暂不可用">
        请重试；模拟账户余额和历史模拟持仓仍与真实持仓分开。数据时间：未知。
      </n-alert>
      <p v-else class="data-status">行情时间 {{ latestQuoteTime }} · 模拟账户仅用于研究练习，不会写入真实持仓</p>
      <!-- 账户概览（ETF 口径） -->
      <!-- 用 m:（1024px）而非 s:（640px）：三张卡的 sub 文案较长，
           640~1024px 每列仅约 200px 会挤成竖条。§4.3 的 `cols="1 s:N"` 约定针对弹窗内栅格。 -->
      <n-grid cols="1 m:3" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="模拟盘现金" :value="overview && !overview.currency_unavailable_reason ? fmtMoney(overview.account.cash) : '—'" :sub="overview?.currency_unavailable_reason" />
        </n-gi>
        <n-gi>
          <StatCard label="ETF 持仓市值" :value="overview && !etfValuationReason ? fmtMoney(etfMarketValue) : '—'" :sub="etfValuationReason || etfValueSub" />
        </n-gi>
        <n-gi>
          <StatCard label="ETF 持仓盈亏" :value="overview && !etfValuationReason && !etfCostUnknownCount && !etfAllQuotesMissing ? fmtMoney(etfProfit) : '—'" :sub="etfValuationReason || (etfCostUnknownCount ? '部分持仓成本待核验' : etfProfitSub)" />
        </n-gi>
      </n-grid>

      <!-- 指数 ETF 行情 -->
      <SectionCard title="指数 ETF 行情">
        <n-spin :show="loading && !etfs.length">
          <n-empty v-if="!etfs.length && !loading" description="行情暂不可用" />
          <div v-else class="table-scroll" tabindex="0" aria-label="ETF 行情表，可左右滚动">
          <n-table :bordered="false" :single-line="false" size="small" class="etf-table">
            <thead>
              <tr>
                <th>类别</th>
                <th colspan="2">ETF</th>
                <th>跟踪指数</th>
                <th>现价</th>
                <th>涨跌幅</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="e in etfs" :key="e.symbol">
                <td>
                  <n-tag size="tiny" round :bordered="false">{{ e.category }}</n-tag>
                </td>
                <td colspan="2" class="etf-identity"><StockIdentity :symbol="e.symbol" market="cn" :name="e.name" density="table" clickable actions /></td>
                <td class="etf-index">{{ e.index }}</td>
                <td class="qv-tnum">
                  {{ e.quote_ok ? fmt(e.price) : '—' }}
                  <FreshnessTag :status="e.freshness_status" :as-of="e.quote_as_of" />
                </td>
                <td>
                  <ChangeTag v-if="e.quote_ok && e.freshness_status === 'fresh'" :value="e.change_pct" size="small" />
                  <span v-else class="etf-index">—</span>
                </td>
                <td>
                  <div class="etf-ops">
                    <n-button size="tiny" type="error" tertiary @click="tradeFromEtf(e, 'buy')">买入</n-button>
                    <n-button size="tiny" type="success" tertiary @click="tradeFromEtf(e, 'sell')">卖出</n-button>
                  </div>
                </td>
              </tr>
            </tbody>
          </n-table>
          </div>
        </n-spin>
      </SectionCard>

      <!-- 我的 ETF 持仓 -->
      <SectionCard title="我的 ETF 持仓">
        <n-empty v-if="!overview" description="模拟账户数据暂不可用，请刷新重试" size="small" />
        <n-empty v-else-if="!etfHoldings.length" description="暂无 ETF 持仓，在上方行情表买入" size="small" />
        <div v-else class="table-scroll" tabindex="0" aria-label="ETF 持仓表，可左右滚动">
        <n-table :bordered="false" :single-line="false" size="small" class="etf-table">
          <thead>
            <tr>
              <th>名称</th>
              <th>数量</th>
              <th>成本</th>
              <th>现价</th>
              <th>市值</th>
              <th>盈亏</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="h in etfHoldings" :key="h.id">
              <td class="etf-identity">
                <StockIdentity :symbol="h.symbol" market="cn" :name="h.name" density="table" clickable actions />
              </td>
              <td class="qv-tnum">{{ h.quantity }}</td>
              <td class="qv-tnum">{{ h.cost_basis_note ? '—（待核验）' : formatPrice(h.avg_cost) }}</td>
              <td class="qv-tnum">
                <template v-if="h.quote_ok">{{ fmt(h.price) }}</template>
                <template v-else>
                  <span v-if="h.last_price" class="hold-last">{{ fmt(h.last_price) }}</span>
                  <span v-else>—</span>
                  <FreshnessTag
                    :status="h.freshness_status || 'stale'"
                    :as-of="h.quote_as_of"
                    :reason="h.stale_reason"
                  />
                </template>
              </td>
              <!-- 行情非最新：后端用成本代市值、盈亏置 0——如实标「≈成本」与「未知」，
                   不冒充实时市值/真实盈亏。 -->
              <td class="qv-tnum">
                <template v-if="h.valuation_unavailable_reason">—</template>
                <template v-else-if="h.quote_ok">{{ fmtMoney(h.market_value) }}</template>
                <template v-else>
                  {{ fmtMoney(h.cost) }}<span class="hold-approx">（按成本，非实时）</span>
                </template>
              </td>
              <td v-if="h.quote_ok && !h.cost_basis_note" class="qv-tnum" :style="{ color: pctColor(h.profit_amount) }">
                {{ fmtMoney(h.profit_amount) }}
                <span class="hold-pct">({{ h.profit_pct.toFixed(2) }}%)</span>
              </td>
              <td v-else class="qv-tnum">
                <span class="hold-approx">{{ h.valuation_unavailable_reason || h.cost_basis_note || '未知（行情非最新）' }}</span>
              </td>
              <td>
                <div class="etf-ops">
                  <n-button size="tiny" type="error" tertiary @click="tradeFromHolding(h, 'buy')">加仓</n-button>
                  <n-button size="tiny" type="success" tertiary @click="tradeFromHolding(h, 'sell')">卖出</n-button>
                </div>
              </td>
            </tr>
          </tbody>
        </n-table>
        </div>
      </SectionCard>

      <!-- 交易说明 -->
      <SectionCard title="ETF 交易说明" :hoverable="false">
        <ul class="etf-notes">
          <li>境内股票型 ETF 通常为 T+1，黄金、跨境等部分 ETF 支持 T+0；模拟盘当前不限制当日卖出。</li>
          <li>免征印花税：ETF/场内基金买卖均不收印花税（个股卖出另计万 5）。</li>
          <li>佣金：按万 2.5 计，单笔最低 5 元（与个股一致）。</li>
          <li>场内交易通常以 100 份为单位；模拟盘允许输入自定义数量。</li>
        </ul>
      </SectionCard>
    </div>

    <!-- 交易弹窗 -->
    <n-modal
      v-model:show="tradeModal"
      preset="card"
      :title="`${form.side === 'buy' ? '买入' : '卖出'} ${form.name}`"
      style="width: calc(100vw - 32px); max-width: 400px"
      :mask-closable="!trading"
      :close-on-esc="!trading"
      :closable="!trading"
    >
      <n-form label-placement="top" :show-feedback="false" :disabled="trading" class="trade-form">
        <n-grid cols="1 s:2" responsive="screen" :x-gap="10" :y-gap="10">
          <n-gi>
            <n-form-item label="代码">
              <n-input :value="form.symbol" readonly />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="名称">
              <n-input :value="form.name" readonly />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="方向">
          <n-radio-group v-model:value="form.side">
            <n-radio-button value="buy">买入</n-radio-button>
            <n-radio-button value="sell">卖出</n-radio-button>
          </n-radio-group>
        </n-form-item>
        <n-grid cols="1 s:2" responsive="screen" :x-gap="10" :y-gap="10">
          <n-gi>
            <n-form-item label="价格（留空按市价）">
              <n-input-number v-model:value="form.price" :min="0" :precision="3" style="width: 100%" placeholder="市价" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="数量（份）">
              <n-input-number v-model:value="form.quantity" :min="0" :step="100" :precision="4" style="width: 100%" />
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
      </n-form>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.etf {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-width: 0;
}
.table-scroll { width: 100%; max-width: 100%; overflow-x: auto; }
.etf-table { min-width: 880px; }
.etf-identity { min-width: 210px; }
.data-status { margin: -6px 0 0; font-size: 12px; opacity: .62; }
.etf-ops {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
.etf-index {
  opacity: 0.6;
}
.hold-pct {
  font-size: 12px;
}
/* 行情非最新的降级展示：最近已知价（非现价）与「按成本/未知」说明 */
.hold-last {
  opacity: 0.65;
  margin-right: 4px;
}
.hold-approx {
  font-size: 12px;
  opacity: 0.65;
}
.etf-notes {
  margin: 0;
  padding-left: 18px;
  font-size: 13px;
  line-height: 1.9;
  opacity: 0.75;
}
.trade-form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
</style>
