<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import {
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
import { useUi } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const { pctColor, vars } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const etfs = ref<EtfItem[]>([])
const overview = ref<PaperOverview | null>(null)
const loading = ref(false)

// ETF 持仓：从模拟账户总览里按代码前缀过滤（后端持仓不区分资产类型）。
const etfHoldings = computed<PaperHolding[]>(() =>
  (overview.value?.holdings ?? []).filter((h) => isEtfSymbol(h.symbol)),
)
const etfMarketValue = computed(() =>
  etfHoldings.value.reduce((s, h) => s + (h.quote_ok ? h.market_value : h.cost), 0),
)
const etfProfit = computed(() => etfHoldings.value.reduce((s, h) => s + h.profit_amount, 0))

async function loadQuotes() {
  try {
    etfs.value = await getEtfList()
  } catch (e) {
    message.error((e as Error).message)
  }
}

async function load() {
  loading.value = true
  try {
    ;[etfs.value, overview.value] = await Promise.all([getEtfList(), getPaperOverview()])
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// 盘中每 60s 自动刷新行情（切后台/非交易时段自动暂停）。
useAutoRefresh(loadQuotes, 60_000)

// ---------- 交易弹窗 ----------
type TradeForm = { symbol: string; name: string; side: 'buy' | 'sell'; price?: number; quantity?: number }
const tradeModal = ref(false)
const trading = ref(false)
const form = ref<TradeForm>({ symbol: '', name: '', side: 'buy', price: undefined, quantity: undefined })

function openTrade(symbol: string, name: string, side: 'buy' | 'sell', quantity?: number) {
  form.value = { symbol, name, side, price: undefined, quantity }
  tradeModal.value = true
}
function tradeFromEtf(e: EtfItem, side: 'buy' | 'sell') {
  openTrade(e.symbol, e.name, side)
}
function tradeFromHolding(h: PaperHolding, side: 'buy' | 'sell') {
  openTrade(h.symbol, h.name || h.symbol, side, side === 'sell' ? h.quantity : undefined)
}

async function submitTrade() {
  if (!form.value.quantity || form.value.quantity <= 0) {
    message.warning('请输入数量')
    return
  }
  trading.value = true
  try {
    const t = await paperTrade({
      symbol: form.value.symbol,
      market: 'cn',
      name: form.value.name,
      side: form.value.side,
      price: form.value.price,
      quantity: form.value.quantity,
    })
    message.success(`${t.side === 'buy' ? '买入' : '卖出'} ${t.name || t.symbol} ${t.quantity} 份 @ ${t.price.toFixed(3)}`)
    tradeModal.value = false
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    trading.value = false
  }
}

function fmtMoney(n: number) {
  return (n >= 0 ? '' : '-') + Math.abs(n).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}
function fmt(n: number) {
  // ETF 最小变动价位 0.001 元，价格类数值统一三位小数。
  return n == null ? '-' : n.toFixed(3)
}

onMounted(load)
</script>

<template>
  <PageContainer title="指数 ETF" subtitle="精选宽基/行业/跨境 ETF · 一键模拟买卖 · 真实行情成交">
    <template #actions>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="etf" :style="styleVars">
      <!-- 账户概览（ETF 口径） -->
      <n-grid cols="1 s:3" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="模拟盘现金" :value="fmtMoney(overview?.account.cash ?? 0)" />
        </n-gi>
        <n-gi>
          <StatCard label="ETF 持仓市值" :value="fmtMoney(etfMarketValue)" />
        </n-gi>
        <n-gi>
          <StatCard label="ETF 持仓盈亏" :value="fmtMoney(etfProfit)" />
        </n-gi>
      </n-grid>

      <!-- 指数 ETF 行情 -->
      <SectionCard title="指数 ETF 行情">
        <n-spin :show="loading && !etfs.length">
          <n-empty v-if="!etfs.length && !loading" description="行情暂不可用" />
          <n-table v-else :bordered="false" :single-line="false" size="small">
            <thead>
              <tr>
                <th>类别</th>
                <th>代码</th>
                <th>名称</th>
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
                <td class="qv-mono">{{ e.symbol }}</td>
                <td>{{ e.name }}</td>
                <td class="etf-index">{{ e.index }}</td>
                <td class="qv-tnum">{{ e.quote_ok ? fmt(e.price) : '—' }}</td>
                <td>
                  <ChangeTag v-if="e.quote_ok" :value="e.change_pct" size="small" />
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
        </n-spin>
      </SectionCard>

      <!-- 我的 ETF 持仓 -->
      <SectionCard title="我的 ETF 持仓">
        <n-empty v-if="!etfHoldings.length" description="暂无 ETF 持仓，在上方行情表买入" size="small" />
        <n-table v-else :bordered="false" :single-line="false" size="small">
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
              <td>
                <span class="hold-name">{{ h.name || h.symbol }}</span>
                <span class="qv-mono hold-symbol">{{ h.symbol }}</span>
              </td>
              <td class="qv-tnum">{{ h.quantity }}</td>
              <td class="qv-tnum">{{ fmt(h.avg_cost) }}</td>
              <td class="qv-tnum">{{ h.quote_ok ? fmt(h.price) : '—' }}</td>
              <td class="qv-tnum">{{ fmtMoney(h.market_value) }}</td>
              <td class="qv-tnum" :style="{ color: pctColor(h.profit_amount) }">
                {{ fmtMoney(h.profit_amount) }}
                <span class="hold-pct">({{ h.profit_pct.toFixed(2) }}%)</span>
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
      </SectionCard>

      <!-- 交易说明 -->
      <SectionCard title="ETF 交易说明" :hoverable="false">
        <ul class="etf-notes">
          <li>T+1 交易：当日买入次日才可卖出（模拟盘不强制校验，仅提示）。</li>
          <li>免征印花税：ETF/场内基金买卖均不收印花税（个股卖出另计万 5）。</li>
          <li>佣金：按万 2.5 计，单笔最低 5 元（与个股一致）。</li>
          <li>申购单位：场内买卖以 100 份为整数倍。</li>
        </ul>
      </SectionCard>
    </div>

    <!-- 交易弹窗 -->
    <n-modal
      v-model:show="tradeModal"
      preset="card"
      :title="`${form.side === 'buy' ? '买入' : '卖出'} ${form.name}`"
      style="max-width: 400px"
    >
      <n-form label-placement="top" :show-feedback="false" class="trade-form">
        <n-grid cols="2" :x-gap="10">
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
        <n-grid cols="2" :x-gap="10">
          <n-gi>
            <n-form-item label="价格（留空按市价）">
              <n-input-number v-model:value="form.price" :min="0" :precision="3" style="width: 100%" placeholder="市价" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="数量（份）">
              <n-input-number v-model:value="form.quantity" :min="0" :step="100" style="width: 100%" />
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
}
.etf-ops {
  display: flex;
  gap: 6px;
}
.etf-index {
  opacity: 0.6;
}
.hold-name {
  font-weight: 600;
}
.hold-symbol {
  margin-left: 8px;
  font-size: 12px;
  opacity: 0.5;
}
.hold-pct {
  font-size: 12px;
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
