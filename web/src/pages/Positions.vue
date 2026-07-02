<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NInputNumber,
  NModal,
  NForm,
  NFormItem,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NTag,
  NPopconfirm,
  NEmpty,
  NSpin,
  NGrid,
  NGi,
  useMessage,
} from 'naive-ui'
import {
  listPositions,
  createPosition,
  updatePosition,
  closePosition,
  deletePosition,
  type Position,
  type PositionInput,
} from '@/api/position'
import { useUi } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor, vars } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))
const warnColor = computed(() => vars.value.warningColor)

const positions = ref<Position[]>([])
const loading = ref(false)
const statusFilter = ref<'holding' | 'closed' | 'all'>('holding')
const typeFilter = ref<'all' | 'short_term' | 'long_term'>('all')

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    positions.value = await listPositions(statusFilter.value)
  } catch (e) {
    if (!silent) message.error((e as Error).message)
  } finally {
    if (!silent) loading.value = false
  }
}

// 盘中自动刷新盈亏（60s，仅交易时段+页面可见，静默）。
useAutoRefresh(() => load(true), 60_000)

const filtered = computed(() =>
  typeFilter.value === 'all'
    ? positions.value
    : positions.value.filter((p) => p.position_type === typeFilter.value),
)

// 汇总（仅持仓中且取到现价的部分）。
const summary = computed(() => {
  let cost = 0,
    mv = 0,
    pnl = 0
  let n = 0
  for (const p of filtered.value) {
    if (p.status === 'holding' && p.quote_ok) {
      cost += p.cost
      mv += p.market_value
      pnl += p.profit_amount
      n++
    }
  }
  return { cost, mv, pnl, pct: cost > 0 ? (pnl / cost) * 100 : 0, n }
})

function typeLabel(t: string) {
  return t === 'short_term' ? '短线' : '长线'
}
function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}
function fmtMoney(n: number) {
  return (n >= 0 ? '' : '-') + Math.abs(n).toLocaleString('zh-CN', { maximumFractionDigits: 2 })
}
function todayStr() {
  return new Date().toLocaleDateString('en-CA') // YYYY-MM-DD 本地
}

// ---------- 建仓 / 编辑 ----------
const editModal = ref(false)
const editing = ref(false)
const form = ref<PositionInput & { id: number | null }>({
  id: null,
  symbol: '',
  market: 'cn',
  name: '',
  position_type: 'short_term',
  buy_price: undefined,
  buy_date: todayStr(),
  quantity: undefined,
  buy_fee: 0,
  buy_tax: 0,
  buy_reason: '',
  user_note: '',
})

function openCreate(prefill?: { symbol?: string; market?: string; name?: string }) {
  editing.value = false
  form.value = {
    id: null,
    symbol: prefill?.symbol || '',
    market: prefill?.market || 'cn',
    name: prefill?.name || '',
    position_type: 'short_term',
    buy_price: undefined,
    buy_date: todayStr(),
    quantity: undefined,
    buy_fee: 0,
    buy_tax: 0,
    buy_reason: '',
    user_note: '',
  }
  editModal.value = true
}
function openEdit(p: Position) {
  editing.value = true
  form.value = {
    id: p.id,
    symbol: p.symbol,
    market: p.market,
    name: p.name,
    position_type: p.position_type,
    buy_price: p.buy_price,
    buy_date: p.buy_date,
    quantity: p.quantity,
    buy_fee: p.buy_fee,
    buy_tax: p.buy_tax,
    buy_reason: p.buy_reason,
    user_note: p.user_note,
  }
  editModal.value = true
}
const submitting = ref(false)
async function submit() {
  if (submitting.value) return
  const f = form.value
  if (!editing.value && !f.symbol?.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (!f.buy_price || f.buy_price <= 0) {
    message.warning('请输入买入价格')
    return
  }
  if (!f.quantity || f.quantity <= 0) {
    message.warning('请输入买入数量')
    return
  }
  submitting.value = true
  try {
    const payload: PositionInput = {
      symbol: f.symbol?.trim(),
      market: f.market,
      position_type: f.position_type,
      buy_price: f.buy_price,
      buy_date: f.buy_date,
      quantity: f.quantity,
      buy_fee: f.buy_fee,
      buy_tax: f.buy_tax,
      buy_reason: f.buy_reason,
      user_note: f.user_note,
    }
    if (editing.value && f.id) await updatePosition(f.id, payload)
    else await createPosition(payload)
    editModal.value = false
    await load()
    message.success('已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    submitting.value = false
  }
}

// ---------- 平仓 ----------
const closeModal = ref(false)
const closing = ref<Position | null>(null)
const closeForm = ref({
  sell_price: undefined as number | undefined,
  sell_date: todayStr(),
  sell_fee: 0,
  sell_tax: 0,
  sell_reason: '',
  review_note: '',
})
function openClose(p: Position) {
  closing.value = p
  closeForm.value = {
    sell_price: p.current_price || undefined,
    sell_date: todayStr(),
    sell_fee: 0,
    sell_tax: 0,
    sell_reason: '',
    review_note: '',
  }
  closeModal.value = true
}
const closingSubmit = ref(false)
async function submitClose() {
  if (!closing.value || closingSubmit.value) return
  if (!closeForm.value.sell_price || closeForm.value.sell_price <= 0) {
    message.warning('请输入卖出价格')
    return
  }
  closingSubmit.value = true
  try {
    await closePosition(closing.value.id, {
      sell_price: closeForm.value.sell_price,
      sell_date: closeForm.value.sell_date,
      sell_fee: closeForm.value.sell_fee,
      sell_tax: closeForm.value.sell_tax,
      sell_reason: closeForm.value.sell_reason,
      review_note: closeForm.value.review_note,
    })
    closeModal.value = false
    await load()
    message.success('已标记卖出')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    closingSubmit.value = false
  }
}

async function remove(p: Position) {
  try {
    await deletePosition(p.id)
    await load()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// 快捷入口：分析/提醒页均已支持 query 预填（PRD 3.3/3.16 的跳转交互）。
function goAnalysis(p: Position) {
  router.push({ name: 'analysis', query: { module: 'stock', symbol: p.symbol, market: p.market } })
}
function goAlert(p: Position) {
  router.push({ name: 'alerts', query: { add: '1', symbol: p.symbol, market: p.market, name: p.name } })
}
function goThesis(p: Position) {
  router.push({ name: 'thesis', query: { add: '1', symbol: p.symbol, market: p.market, name: p.name } })
}

onMounted(async () => {
  // 从自选「建仓」跳转而来：预填并打开建仓弹窗，然后清掉 query。
  if (route.query.add === '1') {
    openCreate({
      symbol: String(route.query.symbol || ''),
      market: String(route.query.market || 'cn'),
      name: String(route.query.name || ''),
    })
    router.replace({ name: 'positions' })
  }
  await load()
})
</script>

<template>
  <PageContainer title="持仓" subtitle="短线 / 长线 · 实时盈亏 · 卖出复盘">
    <template #actions>
      <n-button size="small" type="primary" @click="openCreate()">+ 新建持仓</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <div class="pos" :style="styleVars">
      <!-- 汇总 -->
      <n-grid cols="2 s:4" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="持仓成本" :value="fmtMoney(summary.cost)" />
        </n-gi>
        <n-gi>
          <StatCard label="当前市值" :value="fmtMoney(summary.mv)" />
        </n-gi>
        <n-gi>
          <StatCard label="浮动盈亏" :value="fmtMoney(summary.pnl)" :change-pct="summary.pct" />
        </n-gi>
        <n-gi>
          <StatCard label="持仓笔数" :value="String(summary.n)" />
        </n-gi>
      </n-grid>

      <SectionCard title="持仓明细">
        <template #extra>
          <div class="filters">
            <n-radio-group v-model:value="typeFilter" size="small">
              <n-radio-button value="all">全部</n-radio-button>
              <n-radio-button value="short_term">短线</n-radio-button>
              <n-radio-button value="long_term">长线</n-radio-button>
            </n-radio-group>
            <n-radio-group v-model:value="statusFilter" size="small" @update:value="load">
              <n-radio-button value="holding">持仓中</n-radio-button>
              <n-radio-button value="closed">已卖出</n-radio-button>
              <n-radio-button value="all">全部</n-radio-button>
            </n-radio-group>
          </div>
        </template>

        <n-spin :show="loading && !positions.length">
          <n-empty v-if="!filtered.length" description="暂无持仓，点击「新建持仓」记录一笔买入" />
          <div v-else class="rows">
            <div v-for="p in filtered" :key="p.id" class="row">
              <div class="r-name">
                <div class="r-title-line">
                  <n-tag
                    size="tiny"
                    round
                    :bordered="false"
                    :type="p.position_type === 'short_term' ? 'warning' : 'info'"
                    >{{ typeLabel(p.position_type) }}</n-tag
                  >
                  <span class="r-title">{{ p.name || p.symbol }}</span>
                  <span class="r-symbol qv-mono">{{ p.symbol }}</span>
                  <n-tag v-if="p.status === 'closed'" size="tiny" :bordered="false">已卖出</n-tag>
                </div>
                <div class="r-sub">
                  买入 {{ fmt(p.buy_price) }} × {{ p.quantity }}
                  <span v-if="p.buy_date">· {{ p.buy_date }}</span>
                  <span v-if="p.status === 'holding' && p.held_trade_days > 0">· 持有 {{ p.held_trade_days }} 交易日</span>
                  <span v-if="p.status === 'closed'"> · 卖出 {{ fmt(p.sell_price) }}</span>
                </div>
                <div v-if="p.short_term_review" class="r-hint" :style="{ color: warnColor }">
                  ⚠ 短线已持有 {{ p.held_trade_days }} 交易日，建议复盘是否止盈/止损或转长线
                </div>
                <div v-if="p.status === 'closed' && p.review_note" class="r-review">
                  复盘：{{ p.review_note }}
                </div>
              </div>

              <div class="r-figures">
                <div class="r-fig">
                  <span class="r-fig-label">{{ p.status === 'closed' ? '卖出价' : '现价' }}</span>
                  <span class="r-fig-val qv-tnum">{{ p.quote_ok ? fmt(p.current_price) : '—' }}</span>
                </div>
                <div class="r-fig">
                  <span class="r-fig-label">盈亏</span>
                  <span class="r-fig-val qv-tnum" :style="{ color: pctColor(p.profit_amount) }">
                    {{ p.quote_ok ? fmtMoney(p.profit_amount) : '—' }}
                  </span>
                </div>
                <div class="r-fig">
                  <span class="r-fig-label">收益率</span>
                  <span class="r-fig-val qv-tnum" :style="{ color: pctColor(p.profit_pct) }">
                    {{ p.quote_ok ? p.profit_pct.toFixed(2) + '%' : '—' }}
                  </span>
                </div>
              </div>

              <div class="r-actions">
                <n-button v-if="p.status === 'holding'" size="tiny" type="primary" ghost @click="openClose(p)"
                  >卖出</n-button
                >
                <n-button size="tiny" quaternary @click="goAnalysis(p)">分析</n-button>
                <n-button size="tiny" quaternary @click="goAlert(p)">提醒</n-button>
                <n-button size="tiny" quaternary @click="goThesis(p)">逻辑卡</n-button>
                <n-button size="tiny" quaternary @click="openEdit(p)">编辑</n-button>
                <n-popconfirm @positive-click="remove(p)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error">删除</n-button>
                  </template>
                  删除持仓「{{ p.name || p.symbol }}」？
                </n-popconfirm>
              </div>
            </div>
          </div>
        </n-spin>
      </SectionCard>
    </div>

    <!-- 建仓 / 编辑 -->
    <n-modal
      v-model:show="editModal"
      preset="card"
      :title="editing ? '编辑持仓' : '新建持仓'"
      style="max-width: 520px"
    >
      <n-form label-placement="top">
        <n-grid cols="2" :x-gap="12">
          <n-gi>
            <n-form-item label="股票代码">
              <n-input v-model:value="form.symbol" placeholder="如 600000" :disabled="editing" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="市场">
              <n-select v-model:value="form.market" :options="marketOptions" :disabled="editing" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="类型">
          <n-radio-group v-model:value="form.position_type">
            <n-radio-button value="short_term">短线</n-radio-button>
            <n-radio-button value="long_term">长线</n-radio-button>
          </n-radio-group>
        </n-form-item>
        <n-grid cols="3" :x-gap="12">
          <n-gi>
            <n-form-item label="买入价">
              <n-input-number v-model:value="form.buy_price" :min="0" :precision="4" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="数量">
              <n-input-number v-model:value="form.quantity" :min="0" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="买入日期">
              <n-input v-model:value="form.buy_date" placeholder="YYYY-MM-DD" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-grid cols="2" :x-gap="12">
          <n-gi>
            <n-form-item label="买入手续费">
              <n-input-number v-model:value="form.buy_fee" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="买入税费">
              <n-input-number v-model:value="form.buy_tax" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="买入理由">
          <n-input
            v-model:value="form.buy_reason"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="为什么买入（可选）"
            maxlength="512"
          />
        </n-form-item>
        <n-form-item label="备注">
          <n-input v-model:value="form.user_note" placeholder="补充备注（可选）" maxlength="512" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="editModal = false">取消</n-button>
          <n-button type="primary" :loading="submitting" @click="submit">保存</n-button>
        </div>
      </template>
    </n-modal>

    <!-- 平仓 -->
    <n-modal
      v-model:show="closeModal"
      preset="card"
      :title="`卖出 · ${closing?.name || closing?.symbol || ''}`"
      style="max-width: 480px"
    >
      <n-form label-placement="top">
        <n-grid cols="3" :x-gap="12">
          <n-gi>
            <n-form-item label="卖出价">
              <n-input-number
                v-model:value="closeForm.sell_price"
                :min="0"
                :precision="4"
                style="width: 100%"
              />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="手续费">
              <n-input-number v-model:value="closeForm.sell_fee" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="税费(印花税)">
              <n-input-number v-model:value="closeForm.sell_tax" :min="0" :precision="2" style="width: 100%" />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="卖出日期">
          <n-input v-model:value="closeForm.sell_date" placeholder="YYYY-MM-DD" />
        </n-form-item>
        <n-form-item label="卖出原因">
          <n-input v-model:value="closeForm.sell_reason" placeholder="止盈 / 止损 / 逻辑变化…（可选）" maxlength="512" />
        </n-form-item>
        <n-form-item label="复盘">
          <n-input
            v-model:value="closeForm.review_note"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="这笔交易的复盘总结（可选）"
            maxlength="512"
          />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="closeModal = false">取消</n-button>
          <n-button type="primary" :loading="closingSubmit" @click="submitClose">确认卖出</n-button>
        </div>
      </template>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.pos {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.filters {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.rows {
  display: flex;
  flex-direction: column;
}
.row {
  display: flex;
  align-items: center;
  gap: 16px;
  padding: 12px 4px;
  border-bottom: 1px solid var(--qv-divider);
  flex-wrap: wrap;
}
.row:last-child {
  border-bottom: none;
}
.r-name {
  flex: 1;
  min-width: 180px;
}
.r-title-line {
  display: flex;
  align-items: center;
  gap: 8px;
}
.r-title {
  font-size: 14px;
  font-weight: 500;
}
.r-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.r-sub {
  font-size: 12px;
  opacity: 0.6;
  margin-top: 3px;
}
.r-review {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 2px;
}
.r-hint {
  font-size: 12px;
  font-weight: 500;
  margin-top: 4px;
}
.r-figures {
  display: flex;
  gap: 22px;
}
.r-fig {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 70px;
  text-align: right;
}
.r-fig-label {
  font-size: 11px;
  opacity: 0.5;
}
.r-fig-val {
  font-size: 14px;
  font-weight: 600;
}
.r-actions {
  display: flex;
  align-items: center;
  gap: 4px;
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
