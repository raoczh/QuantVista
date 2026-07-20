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
  NAlert,
  useMessage,
} from 'naive-ui'
import {
  listPositions,
  getPortfolioOverview,
  createPosition,
  updatePosition,
  closePosition,
  deletePosition,
  type Position,
  type PositionInput,
  type PortfolioOverview,
} from '@/api/position'
import { importPositions, downloadPositionTemplate, type ImportResult } from '@/api/export'
import { useUi } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor, vars } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))
const warnColor = computed(() => vars.value.warningColor)

const positions = ref<Position[]>([])
const overview = ref<PortfolioOverview | null>(null)
const loading = ref(false)
const statusFilter = ref<'holding' | 'closed' | 'all'>('holding')
const typeFilter = ref<'all' | 'short_term' | 'long_term'>('all')

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    const [list, ov] = await Promise.all([listPositions(statusFilter.value), getPortfolioOverview()])
    positions.value = list
    overview.value = ov
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

// 汇总改为后端组合总览（GET /positions/overview：全组合口径，不随筛选变化）。
const mixLabel = computed(() => {
  const ov = overview.value
  if (!ov || ov.total_value <= 0) return '—'
  const short = (ov.short_value / ov.total_value) * 100
  return `${short.toFixed(0)}% / ${(100 - short).toFixed(0)}%`
})

// 部分估值透明化：行情失败/过期的仓位被排除出市值/盈亏汇总，不能默不作声地伪装成完整组合。
const pricedLabel = computed(() => {
  const ov = overview.value
  if (!ov) return ''
  const failed = ov.quote_failed_count ?? 0
  const stale = ov.quote_stale_count ?? 0
  if (failed + stale <= 0) return ''
  const total = ov.holding_count
  const parts: string[] = []
  if (stale > 0) parts.push(`${stale} 笔行情已过期`)
  if (failed > 0) parts.push(`${failed} 笔行情缺失`)
  return `已定价 ${total - failed - stale}/${total} 笔（${parts.join('、')}，未计入市值与盈亏）`
})

// 持仓行「N 天未分析」提示文案（从未分析则不带天数）。
function staleLabel(p: Position) {
  if (!p.last_analyzed_at) return '未分析'
  const days = Math.floor((Date.now() - new Date(p.last_analyzed_at).getTime()) / 86_400_000)
  return `${days} 天未分析`
}

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
// 编辑已平仓持仓：后端仅接受 buy_reason/user_note，其余字段隐藏，避免“保存成功”误导。
const editingClosed = ref(false)
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
  plan_stop_loss: undefined,
  plan_take_profit: undefined,
})

// 买入前检查清单（勾选状态随持仓落库，供卖出复盘对照）。
const CHECKLIST = [
  '买入理由已想清楚，能写下来（不是「感觉要涨」）',
  '已设定止损价/失效条件，并能接受对应亏损',
  '该仓位不会让单一标的占比过高',
  '已检查近期事件风险（财报/解禁/减持/停复牌）',
  '当前市场环境不明显逆风（趋势/情绪）',
]
const checklist = ref<boolean[]>(CHECKLIST.map(() => false))
function checklistToJSON(): string {
  if (!checklist.value.some(Boolean)) return ''
  return JSON.stringify({ items: CHECKLIST.map((text, i) => ({ text, checked: checklist.value[i] })) })
}
function checklistFromJSON(s: string) {
  checklist.value = CHECKLIST.map(() => false)
  if (!s) return
  try {
    const parsed = JSON.parse(s) as { items?: { text: string; checked: boolean }[] }
    parsed.items?.forEach((it, i) => {
      if (i < checklist.value.length) checklist.value[i] = !!it.checked
    })
  } catch {
    /* 兼容异常数据：忽略 */
  }
}
const checklistDone = computed(() => checklist.value.filter(Boolean).length)

// 仓位风险计算器：随表单实时计算（纯前端，无请求）。
const riskCalc = computed(() => {
  const price = form.value.buy_price || 0
  const qty = form.value.quantity || 0
  const stop = form.value.plan_stop_loss || 0
  const cost = price * qty + (form.value.buy_fee || 0) + (form.value.buy_tax || 0)
  if (price <= 0 || qty <= 0) return null
  const out: { cost: number; maxLoss: number | null; maxLossPct: number | null; gain: number | null } = {
    cost,
    maxLoss: null,
    maxLossPct: null,
    gain: null,
  }
  if (stop > 0 && stop < price) {
    out.maxLoss = (price - stop) * qty + (form.value.buy_fee || 0) + (form.value.buy_tax || 0)
    out.maxLossPct = (out.maxLoss / cost) * 100
  }
  const tp = form.value.plan_take_profit || 0
  if (tp > price) out.gain = (tp - price) * qty
  return out
})

function openCreate(prefill?: { symbol?: string; market?: string; name?: string; recId?: number }) {
  editing.value = false
  editingClosed.value = false
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
    plan_stop_loss: undefined,
    plan_take_profit: undefined,
    recommendation_id: prefill?.recId || 0,
  }
  checklistFromJSON('')
  editModal.value = true
}
function openEdit(p: Position) {
  editing.value = true
  editingClosed.value = p.status === 'closed'
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
    plan_stop_loss: p.plan_stop_loss || undefined,
    plan_take_profit: p.plan_take_profit || undefined,
  }
  checklistFromJSON(p.checklist_json)
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
  if (f.plan_stop_loss && f.buy_price && f.plan_stop_loss >= f.buy_price) {
    message.warning('计划止损价应低于买入价')
    return
  }
  if (f.plan_take_profit && f.buy_price && f.plan_take_profit <= f.buy_price) {
    message.warning('计划止盈价应高于买入价')
    return
  }
  submitting.value = true
  try {
    // 已平仓持仓：后端仅接受 buy_reason/user_note，只提交这两项，避免误导用户以为改了成交数据。
    const payload: PositionInput =
      editing.value && editingClosed.value
        ? { buy_reason: f.buy_reason, user_note: f.user_note }
        : {
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
            plan_stop_loss: f.plan_stop_loss || 0,
            plan_take_profit: f.plan_take_profit || 0,
            checklist_json: checklistToJSON(),
            recommendation_id: f.recommendation_id || 0,
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
  sell_planned: '',
  ai_verdict: '',
  lesson_learned: '',
})
const sellPlannedOptions = [
  { label: '按计划卖出', value: 'yes' },
  { label: '未按计划（冲动/被动）', value: 'no' },
  { label: '部分按计划', value: 'partial' },
]
const aiVerdictOptions = [
  { label: 'AI 判断正确', value: 'right' },
  { label: 'AI 判断错误', value: 'wrong' },
  { label: '对错参半', value: 'mixed' },
  { label: '未参考 AI', value: 'unused' },
]
function openClose(p: Position) {
  closing.value = p
  closeForm.value = {
    sell_price: p.current_price || undefined,
    sell_date: todayStr(),
    sell_fee: 0,
    sell_tax: 0,
    sell_reason: '',
    review_note: '',
    sell_planned: '',
    ai_verdict: '',
    lesson_learned: '',
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
      sell_planned: closeForm.value.sell_planned,
      ai_verdict: closeForm.value.ai_verdict,
      lesson_learned: closeForm.value.lesson_learned,
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

// ---------- CSV 导入（批次 J） ----------
const importModal = ref(false)
const importFile = ref<File | null>(null)
const importing = ref(false)
const importResult = ref<ImportResult | null>(null)
function openImport() {
  importFile.value = null
  importResult.value = null
  importModal.value = true
}
function onImportFileChange(e: Event) {
  const files = (e.target as HTMLInputElement).files
  importFile.value = files && files.length ? files[0] : null
  importResult.value = null
}
async function submitImport() {
  if (!importFile.value) {
    message.warning('请选择 CSV 文件')
    return
  }
  importing.value = true
  try {
    importResult.value = await importPositions(importFile.value)
    if (importResult.value.imported > 0) {
      message.success(`成功导入 ${importResult.value.imported} 条持仓`)
      await load()
    } else if (!importResult.value.failed.length) {
      message.warning('文件中没有可导入的数据行')
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    importing.value = false
  }
}

onMounted(async () => {
  // 从自选/推荐「建仓」跳转而来：预填并打开建仓弹窗，然后清掉 query。
  // rec_id 为来源推荐（血缘），落库后推荐详情可展示「已建仓」与价格对比。
  if (route.query.add === '1') {
    openCreate({
      symbol: String(route.query.symbol || ''),
      market: String(route.query.market || 'cn'),
      name: String(route.query.name || ''),
      recId: Number(route.query.rec_id) || 0,
    })
    router.replace({ name: 'positions' })
  }
  await load()
})
</script>

<template>
  <PageContainer title="持仓" subtitle="短线 / 长线 · 盈亏跟踪 · 卖出复盘">
    <template #actions>
      <n-button size="small" type="primary" @click="openCreate()">+ 新建持仓</n-button>
      <n-button size="small" quaternary @click="openImport">导入</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load()">刷新</n-button>
    </template>

    <div class="pos" :style="styleVars">
      <!-- 汇总（组合总览：全组合口径） -->
      <n-grid cols="2 s:4" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="持仓成本" :value="fmtMoney(overview?.total_cost ?? 0)" />
        </n-gi>
        <n-gi>
          <StatCard
            label="当前市值"
            :value="fmtMoney(overview?.total_value ?? 0)"
            :sub="pricedLabel"
          />
        </n-gi>
        <n-gi>
          <StatCard
            label="浮动盈亏"
            :value="fmtMoney(overview?.total_profit ?? 0)"
            :change-pct="overview?.profit_pct ?? 0"
          />
        </n-gi>
        <n-gi>
          <StatCard label="已实现盈亏" :value="fmtMoney(overview?.realized_profit ?? 0)" sub="已平仓累计" />
        </n-gi>
        <n-gi>
          <StatCard
            label="持仓笔数"
            :value="String(overview?.holding_count ?? 0)"
            :sub="`盈 ${overview?.win_count ?? 0} · 亏 ${overview?.lose_count ?? 0}`"
          />
        </n-gi>
        <n-gi>
          <StatCard label="短线 / 长线" :value="mixLabel" sub="按市值占比" />
        </n-gi>
        <n-gi>
          <StatCard
            label="最大持仓占比"
            :value="overview?.top_weight_pct ? overview.top_weight_pct.toFixed(1) + '%' : '—'"
            :sub="overview?.top_name || overview?.top_symbol || ''"
          />
        </n-gi>
      </n-grid>

      <!-- 组合风控信号（集中度/止损/未分析） -->
      <n-alert v-if="overview?.signals?.length" type="warning" title="组合风控信号">
        <div v-for="(s, i) in overview.signals" :key="i" class="signal-line">{{ s }}</div>
      </n-alert>

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
                  <n-tag v-if="p.below_stop_loss" size="tiny" type="error" :bordered="false">破止损</n-tag>
                  <n-tag v-else-if="p.near_stop_loss" size="tiny" type="warning" :bordered="false">近止损</n-tag>
                  <FreshnessTag
                    v-if="p.status === 'holding'"
                    :status="p.freshness_status"
                    :as-of="p.quote_as_of"
                    :reason="p.stale_reason"
                  />
                  <n-tag
                    v-if="p.status === 'holding' && p.analysis_stale"
                    size="tiny"
                    :bordered="false"
                    class="tag-click"
                    title="点击发起个股分析"
                    @click="goAnalysis(p)"
                    >{{ staleLabel(p) }}</n-tag
                  >
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
                  <span
                    v-if="!p.quote_ok && p.status === 'holding' && p.last_price"
                    class="r-fig-stale qv-tnum"
                    :title="`最近已知价（截至 ${p.quote_as_of || '未知'}，已过期，不代表当前价格）`"
                    >旧 {{ fmt(p.last_price) }}</span
                  >
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
        <n-alert v-if="editingClosed" type="info" :bordered="false" style="margin-bottom: 14px">
          已平仓持仓仅可修改「买入理由」与「备注」，其余成交数据不可再更改。
        </n-alert>
        <template v-if="!editingClosed">
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
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
          <n-grid cols="1 s:3" responsive="screen" :x-gap="12">
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
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
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
        </template>
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

        <!-- 风险计划 + 仓位风险计算器（实时纯前端计算） -->
        <template v-if="!editingClosed">
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="计划止损价（可选）">
                <n-input-number v-model:value="form.plan_stop_loss" :min="0" :precision="4" style="width: 100%" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="计划止盈价（可选）">
                <n-input-number v-model:value="form.plan_take_profit" :min="0" :precision="4" style="width: 100%" />
              </n-form-item>
            </n-gi>
          </n-grid>
          <div v-if="riskCalc" class="risk-calc qv-tnum">
            <span>投入 {{ riskCalc.cost.toFixed(0) }} 元</span>
            <template v-if="riskCalc.maxLoss != null">
              <span :style="{ color: vars.errorColor }">
                触发止损亏 {{ riskCalc.maxLoss.toFixed(0) }} 元（-{{ riskCalc.maxLossPct!.toFixed(1) }}%）
              </span>
            </template>
            <span v-else class="risk-hint">填写止损价即可预估最大亏损</span>
            <span v-if="riskCalc.gain != null && riskCalc.maxLoss" >
              盈亏比 {{ (riskCalc.gain / riskCalc.maxLoss).toFixed(1) }}
            </span>
          </div>

          <!-- 买入前检查清单 -->
          <div class="checklist">
            <div class="checklist-head">
              <span>买入前检查（{{ checklistDone }}/{{ CHECKLIST.length }}）</span>
              <span class="risk-hint">勾选状态会随持仓保存，卖出复盘时对照</span>
            </div>
            <label v-for="(text, i) in CHECKLIST" :key="i" class="check-item">
              <input v-model="checklist[i]" type="checkbox" />
              <span>{{ text }}</span>
            </label>
          </div>
        </template>
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
        <n-grid cols="1 s:3" responsive="screen" :x-gap="12">
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

        <!-- 结构化复盘：固定维度，供跨笔统计与自我校准 -->
        <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
          <n-gi>
            <n-form-item label="是否按计划卖出">
              <n-select v-model:value="closeForm.sell_planned" :options="sellPlannedOptions" placeholder="（可选）" clearable />
            </n-form-item>
          </n-gi>
          <n-gi>
            <n-form-item label="当时 AI 判断">
              <n-select v-model:value="closeForm.ai_verdict" :options="aiVerdictOptions" placeholder="（可选）" clearable />
            </n-form-item>
          </n-gi>
        </n-grid>
        <n-form-item label="下次策略调整点">
          <n-input
            v-model:value="closeForm.lesson_learned"
            placeholder="这笔交易教会了什么？下次怎么改？（可选）"
            maxlength="512"
          />
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

    <!-- CSV 批量导入 -->
    <n-modal v-model:show="importModal" preset="card" title="导入持仓（CSV）" style="max-width: 560px">
      <div class="import-body">
        <div class="import-tip">
          模板列：<code>symbol,market,type,buy_price,buy_date,quantity,buy_fee,buy_tax,reason</code>。
          type 支持 short_term/long_term（或 短线/长线），market 留空默认 cn，日期格式 YYYY-MM-DD，单次最多 500 行。
          <n-button size="tiny" quaternary type="primary" @click="downloadPositionTemplate">下载模板</n-button>
        </div>
        <input type="file" accept=".csv,text/csv" @change="onImportFileChange" />
        <n-alert
          v-if="importResult && importResult.failed.length"
          type="warning"
          :bordered="false"
          style="margin-top: 12px"
        >
          {{ importResult.imported }} 条成功，{{ importResult.failed.length }} 条失败：
          <ul class="import-errors">
            <li v-for="f in importResult.failed" :key="f.row">第 {{ f.row }} 行：{{ f.error }}</li>
          </ul>
        </n-alert>
        <n-alert
          v-else-if="importResult && importResult.imported > 0"
          type="success"
          :bordered="false"
          style="margin-top: 12px"
        >
          全部 {{ importResult.imported }} 条导入成功。
        </n-alert>
      </div>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="importModal = false">关闭</n-button>
          <n-button type="primary" :loading="importing" :disabled="!importFile" @click="submitImport">开始导入</n-button>
        </div>
      </template>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.import-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.import-tip {
  font-size: 12px;
  opacity: 0.75;
  line-height: 1.7;
}
.import-tip code {
  font-size: 11px;
  opacity: 0.9;
}
.import-errors {
  margin: 6px 0 0;
  padding-left: 18px;
  font-size: 12px;
  max-height: 180px;
  overflow-y: auto;
}
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
  flex-wrap: wrap;
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
.tag-click {
  cursor: pointer;
}
.signal-line {
  line-height: 1.7;
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
.r-fig-stale {
  font-size: 11px;
  opacity: 0.55;
  text-decoration: line-through dotted;
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

@media (max-width: 768px) {
  /* 行操作区（卖出/分析/复盘/删除等 6 个 tiny 按钮）加大触摸目标 */
  .r-actions {
    flex-wrap: wrap;
    gap: 6px;
    row-gap: 4px;
  }
  .r-actions :deep(.n-button) {
    height: 30px;
    padding: 0 10px;
  }
  .r-figures {
    gap: 14px;
    flex-wrap: wrap;
  }
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
.risk-calc {
  display: flex;
  gap: 14px;
  flex-wrap: wrap;
  font-size: 12.5px;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--qv-hover, rgba(128, 128, 128, 0.08));
  margin-bottom: 12px;
}
.risk-hint {
  opacity: 0.6;
}
.checklist {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: 12.5px;
  border-top: 1px dashed var(--qv-divider);
  padding-top: 10px;
}
.checklist-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  font-weight: 600;
}
.check-item {
  display: flex;
  align-items: flex-start;
  gap: 8px;
  cursor: pointer;
  line-height: 1.5;
}
.check-item input {
  margin-top: 3px;
  accent-color: v-bind('vars.primaryColor');
}
</style>
