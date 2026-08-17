<script setup lang="ts">
import { ref, onMounted, computed, nextTick, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NRadioGroup,
  NRadioButton,
  NModal,
  NAlert,
  NDropdown,
  useMessage,
  useDialog,
  type DropdownOption,
} from 'naive-ui'
import {
  listAlerts,
  setAlertStatus,
  deleteAlert,
  evaluateAlerts,
  listAlertEvents,
  getAlertEvent,
  setAlertEventStatus,
  readAllAlertEvents,
  alertRequestMessage,
  isPositionAlertKind,
  type AlertRule,
  type AlertEvent,
  type AlertEventStatus,
} from '@/api/alert'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import AlertWizard from '@/components/alerts/AlertWizard.vue'
import type { AlertStockContext } from '@/components/alerts/alertTemplates'
import StockIdentity from '@/components/StockIdentity.vue'

const message = useMessage()
const dialog = useDialog()
const route = useRoute()
const router = useRouter()
const { upColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({
  '--qv-divider': vars.value.dividerColor,
  '--qv-primary': vars.value.primaryColor,
}))

// ---------- 模板向导 / 编辑入口 ----------
const editingRule = ref<AlertRule | null>(null)
const stockContext = ref<AlertStockContext | null>(null)

function editRule(r: AlertRule) {
  editingRule.value = r
  void nextTick(() => document.querySelector('.alert-wizard')?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
}

async function handleWizardSaved() {
  editingRule.value = null
  await load()
}

function cancelWizardEdit() {
  editingRule.value = null
}

// ---------- 列表 ----------
const rules = ref<AlertRule[]>([])
const loading = ref(false)
const rulesError = ref('')
async function load() {
  loading.value = true
  rulesError.value = ''
  try {
    rules.value = await listAlerts()
  } catch (error) {
    rulesError.value = alertRequestMessage('rules', error)
  } finally {
    loading.value = false
  }
}
const evaluating = ref(false)
async function runEvaluate() {
  evaluating.value = true
  try {
    const { hits } = await evaluateAlerts()
    message.success(hits > 0 ? `本次命中 ${hits} 条` : '暂无命中')
    await Promise.all([load(), loadEvents()])
    if (route.query.onboarding_return === '1') {
      await router.push({ name: 'home', query: { onboarding: '1' } })
    }
  } catch (error) {
    message.error(alertRequestMessage('evaluate', error))
  } finally {
    evaluating.value = false
  }
}
async function toggle(r: AlertRule) {
  try {
    await setAlertStatus(r.id, r.status === 'paused' ? 'active' : 'paused')
    await load()
  } catch (error) {
    message.error(alertRequestMessage('action', error))
  }
}
async function remove(r: AlertRule) {
  try {
    await deleteAlert(r.id)
    if (editingRule.value?.id === r.id) editingRule.value = null
    await load()
    message.success('已删除')
  } catch (error) {
    message.error(alertRequestMessage('action', error))
  }
}

function confirmRemove(r: AlertRule) {
  dialog.warning({
    title: '删除提醒',
    content: `确认删除“${ruleScope(r)}”的这条提醒？历史命中记录会保留。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: () => remove(r),
  })
}

function ruleMenuOptions(r: AlertRule): DropdownOption[] {
  return [
    { key: 'edit', label: '编辑' },
    { key: 'toggle', label: r.status === 'paused' ? '恢复' : '暂停' },
    { key: 'delete', label: '删除' },
  ]
}

function selectRuleAction(key: string | number, rule: AlertRule) {
  if (key === 'edit') editRule(rule)
  else if (key === 'toggle') void toggle(rule)
  else if (key === 'delete') confirmRemove(rule)
}

// ---------- 展示辅助 ----------
// 持仓类规则的作用域：未绑定代码 = 我的全部持仓。
function ruleScope(r: AlertRule) {
  return r.symbol ? r.name || r.symbol : '我的全部持仓'
}
function describe(r: AlertRule) {
  const p = (n: number) => n.toFixed(2)
  const g = (n: number) => String(Number(n.toFixed(2)))
  switch (r.kind) {
    case 'price':
      return `现价 ${r.op === 'gte' ? '≥' : '≤'} ${p(r.threshold)}`
    case 'pct_change':
      return `当日涨跌幅 ${r.op === 'gte' ? '≥' : '≤'} ${p(r.threshold)}%`
    case 'ma':
      return `${r.op === 'gte' ? '站上' : '跌破'} MA${r.period}`
    case 'breakout':
      return `创近 ${r.period} 日${r.op === 'gte' ? '新高' : '新低'}`
    case 'volume_surge':
      return `当日量${r.op === 'gte' ? ' ≥ ' : ' ≤ '}${p(r.threshold)} 倍 20 日均量`
    case 'amplitude':
      return `当日振幅 ${r.op === 'gte' ? '≥' : '≤'} ${p(r.threshold)}%`
    case 'earn_date':
      return `距财报预约披露日 ≤ ${r.threshold.toFixed(0)} 天`
    case 'earn_fcst':
      return '发布新业绩预告（预增/预亏等）'
    case 'cost_gain':
      return `${ruleScope(r)} 相对我的成本涨 ≥ ${g(r.threshold)}%`
    case 'cost_drawdown':
      return `${ruleScope(r)} 相对我的成本跌 ≥ ${g(r.threshold)}%`
    case 'peak_drawdown':
      return `${ruleScope(r)} 自持仓期最高价回撤 ≥ ${g(r.threshold)}%`
    default:
      return ''
  }
}
function todayString() {
  const d = new Date()
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${mm}-${dd}`
}

function isHitToday(r: AlertRule) {
  if (r.status === 'triggered') return true
  // 与后端 TriggeredForUser 同口径：非 once 规则只看 triggered_at 是否为今天。
  // 不能用 last_check_date——它每次评估（15 分钟一轮）都会刷成今天，
  // 历史命中会永久显示「已命中」。已暂停的规则不再提示。
  if (r.status === 'paused' || !r.triggered_at) return false
  const d = new Date(r.triggered_at)
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${mm}-${dd}` === todayString()
}

function statusTag(r: AlertRule) {
  if (isHitToday(r)) return { text: '已命中', type: 'warning' as const }
  if (r.status === 'paused') return { text: '已暂停', type: 'default' as const }
  return { text: '生效中', type: 'success' as const }
}
function fmtTime(t: string | null) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

function applyStockActionQuery() {
  if (route.query.add !== '1') return
  editingRule.value = null
  stockContext.value = {
    symbol: String(route.query.symbol || ''),
    market: String(route.query.market || 'cn'),
    name: String(route.query.name || ''),
    nonce: String(route.query._stock_action || Date.now()),
  }
  const query = { ...route.query }
  delete query.add
  delete query.symbol
  delete query.market
  delete query.name
  delete query._stock_action
  void router.replace({ name: 'alerts', query })
}

watch(() => route.query._stock_action, applyStockActionQuery)

onMounted(async () => {
  // 从自选/持仓「设提醒」跳转预填。
  applyStockActionQuery()
  await Promise.all([load(), loadEvents()])
  await openRouteEvent()
})

// ---------- 命中历史（明细事件状态机） ----------
const events = ref<AlertEvent[]>([])
const eventsLoading = ref(false)
const eventsError = ref('')
const eventFilter = ref<'unread' | 'all' | 'read' | 'dismissed'>('unread')
const eventFilterOptions = [
  { label: '未读', value: 'unread' },
  { label: '全部', value: 'all' },
  { label: '已读', value: 'read' },
  { label: '已忽略', value: 'dismissed' },
]
async function loadEvents() {
  eventsLoading.value = true
  eventsError.value = ''
  try {
    events.value = await listAlertEvents(eventFilter.value === 'all' ? undefined : eventFilter.value)
  } catch (error) {
    eventsError.value = alertRequestMessage('events', error)
  } finally {
    eventsLoading.value = false
  }
}
async function markEvent(ev: AlertEvent, status: AlertEventStatus) {
  try {
    await setAlertEventStatus(ev.id, status)
    await loadEvents()
  } catch (error) {
    message.error(alertRequestMessage('action', error))
  }
}
const readingAll = ref(false)
async function markAllRead() {
  readingAll.value = true
  try {
    const { updated } = await readAllAlertEvents()
    message.success(updated > 0 ? `已标记 ${updated} 条为已读` : '没有未读命中')
    await loadEvents()
  } catch (error) {
    message.error(alertRequestMessage('action', error))
  } finally {
    readingAll.value = false
  }
}
function eventStatusTag(s: AlertEventStatus) {
  if (s === 'unread') return { text: '未读', type: 'warning' as const }
  if (s === 'read') return { text: '已读', type: 'success' as const }
  return { text: '已忽略', type: 'default' as const }
}

function eventMenuOptions(ev: AlertEvent): DropdownOption[] {
  if (ev.status === 'unread') {
    return [
      { key: 'read', label: '标记已读' },
      { key: 'dismissed', label: '忽略' },
    ]
  }
  return [{ key: 'unread', label: '恢复未读' }]
}

function selectEventAction(key: string | number, event: AlertEvent) {
  if (key === 'read' || key === 'dismissed' || key === 'unread') {
    void markEvent(event, key)
  }
}
const kindLabelMap: Record<string, string> = {
  price: '到价',
  pct_change: '异动',
  ma: '均线',
  breakout: '突破',
  volume_surge: '放量',
  amplitude: '振幅',
  earn_date: '财报披露',
  earn_fcst: '业绩预告',
  cost_gain: '成本止盈',
  cost_drawdown: '成本止损',
  peak_drawdown: '移动止盈',
}

// ---------- 命中详情深链 ----------
const detailOpen = ref(false)
const detailLoading = ref(false)
const detailError = ref('')
const selectedEvent = ref<AlertEvent | null>(null)
let detailSeq = 0

function routeEventID(): number | null {
  const raw = Array.isArray(route.query.event_id) ? route.query.event_id[0] : route.query.event_id
  const id = Number(raw)
  return Number.isSafeInteger(id) && id > 0 ? id : null
}

async function openRouteEvent() {
  const id = routeEventID()
  if (!id) {
    detailOpen.value = false
    selectedEvent.value = null
    detailError.value = ''
    return
  }
  const seq = ++detailSeq
  const cached = events.value.find((event) => event.id === id)
  if (cached) selectedEvent.value = cached
  else if (selectedEvent.value?.id !== id) selectedEvent.value = null
  detailOpen.value = true
  detailLoading.value = true
  detailError.value = ''
  try {
    const event = await getAlertEvent(id)
    if (seq !== detailSeq || routeEventID() !== id) return
    selectedEvent.value = event
    detailOpen.value = true
  } catch (error) {
    if (seq !== detailSeq || routeEventID() !== id) return
    detailError.value = alertRequestMessage('detail', error)
  } finally {
    if (seq === detailSeq) detailLoading.value = false
  }
}

function openEventDetail(event: AlertEvent) {
  selectedEvent.value = event
  detailError.value = ''
  detailOpen.value = true
  void router.push(event.deep_link)
}

function closeEventDetail() {
  detailSeq++
  detailOpen.value = false
  selectedEvent.value = null
  detailError.value = ''
  const query = { ...route.query }
  delete query.event_id
  void router.replace({ name: 'alerts', query })
}

watch(() => route.query.event_id, () => void openRouteEvent())

const triggerFieldLabels: Record<string, string> = {
  'quote.price': '现价',
  'quote.high': '当日最高',
  'quote.low': '当日最低',
  'quote.change_pct': '当日涨跌幅',
  'indicator.volume_ratio': '成交量倍数',
  'indicator.amplitude': '当日振幅',
  'position.cost_gain_pct': '相对成本涨幅',
  'position.cost_drawdown_pct': '相对成本跌幅',
  'position.peak_drawdown_pct': '自峰值回撤',
  'financial.days_to_disclosure': '距披露日',
  'financial.notice_date': '新业绩预告',
  'financial.forecast_amp_lower': '预告变动下限',
}
function triggerFieldLabel(field: string) {
  return triggerFieldLabels[field] || field
}
function operatorLabel(op?: string) {
  if (op === 'gte') return '≥'
  if (op === 'lte') return '≤'
  if (op === 'new_fact') return '发现新事实'
  return op || ''
}
function contextNumber(value: number | undefined, unit = '') {
  if (value == null) return '暂时无法判断'
  return `${Number(value.toFixed(4))}${unit}`
}
function contextTime(value?: string) {
  if (!value) return '暂时无法判断'
  const parsed = new Date(value)
  return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString('zh-CN', { hour12: false })
}
function metricLabel(name: string) {
  const labels: Record<string, string> = {
    moving_average: '移动平均线',
    previous_high: '前期高点',
    previous_low: '前期低点',
    volume_ratio: '成交量倍数',
    amplitude: '振幅',
  }
  return labels[name] || name
}

</script>

<template>
  <PageContainer
    title="条件提醒"
    subtitle="持仓成本止盈止损 / 移动止盈 · 到价 / 异动 / 均线 / 突破 · 命中历史"
  >
    <template #actions>
      <n-button size="small" secondary @click="router.push({ name: 'settings', query: { tab: 'notifications' } })">通知设置</n-button>
      <n-button size="small" quaternary :loading="evaluating" @click="runEvaluate">立即检查</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="alerts" :style="styleVars">
      <!-- 左：新建/编辑 -->
      <div class="col-form">
        <SectionCard :title="editingRule ? '编辑提醒' : '新建提醒'">
          <AlertWizard
            :editing-rule="editingRule"
            :stock-context="stockContext"
            @saved="handleWizardSaved"
            @cancel-edit="cancelWizardEdit"
          />
        </SectionCard>

      </div>

      <!-- 右：规则列表 + 命中历史 -->
      <div class="col-list">
        <SectionCard title="我的提醒">
          <n-spin :show="loading && !rules.length">
            <n-alert v-if="rulesError" type="warning" title="提醒规则加载失败" :bordered="false" class="section-error">
              {{ rules.length ? '仍展示上次加载的规则，刷新失败。' : rulesError }}
              <div class="recovery-action"><n-button size="small" :loading="loading" @click="load">重试加载提醒</n-button></div>
            </n-alert>
            <n-empty v-if="!rules.length && !rulesError" description="暂无提醒规则，在左侧添加一条" />
            <div v-else class="rules">
              <div v-for="r in rules" :key="r.id" class="rule" :class="{ hit: isHitToday(r) }">
                <div class="rule-main">
                  <div class="rule-title">
                    <StockIdentity v-if="r.symbol" :symbol="r.symbol" :name="r.name" density="table" clickable />
                    <span v-else class="rule-name">{{ ruleScope(r) }}</span>
                    <n-tag size="tiny" round :bordered="false" :type="statusTag(r).type">{{
                      statusTag(r).text
                    }}</n-tag>
                    <n-tag size="tiny" round :bordered="false" :type="isPositionAlertKind(r.kind) ? 'info' : 'default'">{{
                      kindLabelMap[r.kind] || r.kind
                    }}</n-tag>
                  </div>
                  <div class="rule-cond">{{ describe(r) }}</div>
                  <div v-if="isHitToday(r) && r.trigger_msg" class="rule-hit" :style="{ color: upColor }">
                    ⚡ {{ r.trigger_msg }}<span class="rule-hit-time"> · {{ fmtTime(r.triggered_at) }}</span>
                  </div>
                  <div v-else-if="r.last_check_date" class="rule-sub">
                    最近检查 {{ r.last_check_date }}<span v-if="r.last_value"> · 观测值 {{ r.last_value.toFixed(2) }}</span>
                  </div>
                  <div v-if="r.note" class="rule-note">{{ r.note }}</div>
                </div>
                <div class="rule-actions desktop-actions">
                  <n-button size="tiny" quaternary @click="toggle(r)">{{ r.status === 'paused' ? '恢复' : '暂停' }}</n-button>
                  <n-button size="tiny" quaternary @click="editRule(r)">编辑</n-button>
                  <n-popconfirm @positive-click="remove(r)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error">删除</n-button>
                    </template>
                    删除提醒「{{ ruleScope(r) }}」？
                  </n-popconfirm>
                </div>
                <span class="mobile-actions">
                  <n-dropdown trigger="click" placement="bottom-end" :options="ruleMenuOptions(r)" @select="(key) => selectRuleAction(key, r)">
                    <n-button quaternary circle size="small" aria-label="提醒操作" title="提醒操作">⋯</n-button>
                  </n-dropdown>
                </span>
              </div>
            </div>
          </n-spin>
        </SectionCard>

        <SectionCard title="命中历史">
          <template #extra>
            <div class="ev-toolbar">
              <n-radio-group v-model:value="eventFilter" size="small" @update:value="loadEvents">
                <n-radio-button v-for="opt in eventFilterOptions" :key="opt.value" :value="opt.value">{{
                  opt.label
                }}</n-radio-button>
              </n-radio-group>
              <n-button size="small" quaternary :loading="readingAll" @click="markAllRead">全部已读</n-button>
            </div>
          </template>
          <n-spin :show="eventsLoading && !events.length">
            <n-alert v-if="eventsError" type="warning" title="命中记录加载失败" :bordered="false" class="section-error">
              {{ events.length ? '仍展示上次加载的命中记录，刷新失败。' : eventsError }}
              <div class="recovery-action"><n-button size="small" :loading="eventsLoading" @click="loadEvents">重试加载记录</n-button></div>
            </n-alert>
            <n-empty
              v-if="!events.length && !eventsError"
              :description="eventFilter === 'unread' ? '没有未读命中，规则命中后会在这里留档' : '暂无命中记录'"
              style="padding: 24px 0"
            />
            <div v-else class="events">
              <div v-for="ev in events" :key="ev.id" class="event" :class="{ unread: ev.status === 'unread' }">
                <div class="ev-main">
                  <div class="ev-title">
                    <n-tag size="tiny" round :bordered="false">{{ kindLabelMap[ev.kind] || ev.kind }}</n-tag>
                    <StockIdentity :symbol="ev.symbol" :name="ev.name" density="table" clickable />
                    <n-tag size="tiny" round :bordered="false" :type="eventStatusTag(ev.status).type">{{
                      eventStatusTag(ev.status).text
                    }}</n-tag>
                  </div>
                  <div class="ev-msg">{{ ev.message }}</div>
                  <div class="ev-time">{{ fmtTime(ev.triggered_at) }}</div>
                </div>
                <div class="ev-actions desktop-actions">
                  <n-button size="tiny" quaternary @click="openEventDetail(ev)">详情</n-button>
                  <template v-if="ev.status === 'unread'">
                    <n-button size="tiny" quaternary @click="markEvent(ev, 'read')">已读</n-button>
                    <n-button size="tiny" quaternary @click="markEvent(ev, 'dismissed')">忽略</n-button>
                  </template>
                  <n-button v-else size="tiny" quaternary @click="markEvent(ev, 'unread')">恢复未读</n-button>
                </div>
                <div class="ev-mobile-actions mobile-actions">
                  <n-button size="small" quaternary @click="openEventDetail(ev)">详情</n-button>
                  <n-dropdown trigger="click" placement="bottom-end" :options="eventMenuOptions(ev)" @select="(key) => selectEventAction(key, ev)">
                    <n-button quaternary circle size="small" aria-label="命中记录操作" title="命中记录操作">⋯</n-button>
                  </n-dropdown>
                </div>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>
    </div>

    <n-modal
      :show="detailOpen"
      preset="card"
      title="提醒命中详情"
      class="event-detail-modal"
      :style="{ width: 'min(680px, calc(100vw - 24px))' }"
      @update:show="(show) => !show && closeEventDetail()"
    >
      <n-spin :show="detailLoading">
        <n-alert v-if="detailError" type="warning" title="命中详情加载失败" :bordered="false" class="detail-error">
          {{ selectedEvent ? '正在展示列表中已有的命中信息，详情刷新失败。' : detailError }}
          <div class="recovery-action"><n-button size="small" :loading="detailLoading" @click="openRouteEvent">重试加载详情</n-button></div>
        </n-alert>
        <template v-if="selectedEvent">
          <div class="detail-head">
            <StockIdentity :symbol="selectedEvent.symbol" :name="selectedEvent.name" clickable actions />
            <n-tag size="small" :type="eventStatusTag(selectedEvent.status).type">
              {{ eventStatusTag(selectedEvent.status).text }}
            </n-tag>
          </div>
          <div class="detail-reason">{{ selectedEvent.message }}</div>
          <div class="detail-meta">命中于 {{ fmtTime(selectedEvent.triggered_at) }}</div>

          <template v-if="selectedEvent.context_available && selectedEvent.context">
            <div class="detail-section">
              <div class="detail-section-title">触发判断</div>
              <div class="detail-grid">
                <span>使用字段</span><b>{{ triggerFieldLabel(selectedEvent.context.trigger.field) }}</b>
                <span>实际值</span><b class="qv-tnum">{{ selectedEvent.context.trigger.value == null ? '见下方财报事实' : contextNumber(selectedEvent.context.trigger.value, selectedEvent.context.trigger.unit) }}</b>
                <span>判断条件</span><b class="qv-tnum">{{ operatorLabel(selectedEvent.context.trigger.operator) }}<template v-if="selectedEvent.context.trigger.threshold != null"> {{ contextNumber(selectedEvent.context.trigger.threshold, selectedEvent.context.trigger.unit) }}</template></b>
                <span>数据时点</span><b>{{ contextTime(selectedEvent.context.as_of) }}</b>
                <span>数据来源</span><b>{{ selectedEvent.context.source || '暂时无法判断' }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.quote" class="detail-section">
              <div class="detail-section-title">行情快照</div>
              <div class="detail-grid">
                <span>现价</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.quote.price, ' 元') }}</b>
                <span>最高 / 最低</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.quote.high, ' 元') }} / {{ contextNumber(selectedEvent.context.quote.low, ' 元') }}</b>
                <span>涨跌幅</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.quote.change_pct, '%') }}</b>
                <span>来源 / 数据截止时间</span><b>{{ selectedEvent.context.quote.source || '暂时无法判断' }} / {{ contextTime(selectedEvent.context.quote.as_of) }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.bar" class="detail-section">
              <div class="detail-section-title">日线依据</div>
              <div class="detail-grid">
                <span>交易日</span><b>{{ selectedEvent.context.bar.trade_date || '暂时无法判断' }}</b>
                <span>开 / 高 / 低 / 收</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.bar.open) }} / {{ contextNumber(selectedEvent.context.bar.high) }} / {{ contextNumber(selectedEvent.context.bar.low) }} / {{ contextNumber(selectedEvent.context.bar.close) }}</b>
                <span>样本数</span><b class="qv-tnum">{{ selectedEvent.context.bar.sample_size ?? '暂时无法判断' }}</b>
                <span>数据来源</span><b>{{ selectedEvent.context.bar.source || '暂时无法判断' }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.indicator" class="detail-section">
              <div class="detail-section-title">技术指标</div>
              <div class="detail-grid">
                <span>指标</span><b>{{ metricLabel(selectedEvent.context.indicator.name) }}<template v-if="selectedEvent.context.indicator.period">（{{ selectedEvent.context.indicator.period }} 日）</template></b>
                <span>指标值</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.indicator.value, selectedEvent.context.indicator.unit) }}</b>
                <span v-if="selectedEvent.context.indicator.reference != null">参考值</span><b v-if="selectedEvent.context.indicator.reference != null" class="qv-tnum">{{ contextNumber(selectedEvent.context.indicator.reference) }}</b>
                <span>来源 / 数据截止时间</span><b>{{ selectedEvent.context.indicator.source || '暂时无法判断' }} / {{ contextTime(selectedEvent.context.indicator.as_of) }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.position" class="detail-section">
              <div class="detail-section-title">持仓事实</div>
              <div class="detail-grid">
                <span>持仓记录</span><b class="qv-mono">#{{ selectedEvent.context.position.position_id }}</b>
                <span>持仓成本</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.position.avg_cost, ' 元') }}</b>
                <template v-if="selectedEvent.context.position.peak_price != null">
                  <span>持仓期峰值</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.position.peak_price, ' 元') }}<template v-if="selectedEvent.context.position.peak_date">（{{ selectedEvent.context.position.peak_date }}）</template></b>
                </template>
              </div>
            </div>

            <div v-if="selectedEvent.context.financial" class="detail-section">
              <div class="detail-section-title">财报事实</div>
              <div class="detail-grid">
                <span>事实类型</span><b>{{ selectedEvent.context.financial.fact_type === 'disclosure_schedule' ? '预约披露' : '业绩预告' }}</b>
                <span v-if="selectedEvent.context.financial.report_date">报告期</span><b v-if="selectedEvent.context.financial.report_date">{{ selectedEvent.context.financial.report_date }}</b>
                <span v-if="selectedEvent.context.financial.appoint_date">预约披露日</span><b v-if="selectedEvent.context.financial.appoint_date">{{ selectedEvent.context.financial.appoint_date }}</b>
                <span v-if="selectedEvent.context.financial.notice_date">预告发布日期</span><b v-if="selectedEvent.context.financial.notice_date">{{ selectedEvent.context.financial.notice_date }}</b>
                <span v-if="selectedEvent.context.financial.report_type">报告类型</span><b v-if="selectedEvent.context.financial.report_type">{{ selectedEvent.context.financial.report_type }}</b>
                <span v-if="selectedEvent.context.financial.predict_type">预告类型</span><b v-if="selectedEvent.context.financial.predict_type">{{ selectedEvent.context.financial.predict_type }}</b>
                <span v-if="selectedEvent.context.financial.predict_finance">预测指标</span><b v-if="selectedEvent.context.financial.predict_finance">{{ selectedEvent.context.financial.predict_finance }}</b>
                <span v-if="selectedEvent.context.financial.amp_lower != null || selectedEvent.context.financial.amp_upper != null">预计变动</span><b v-if="selectedEvent.context.financial.amp_lower != null || selectedEvent.context.financial.amp_upper != null" class="qv-tnum">{{ contextNumber(selectedEvent.context.financial.amp_lower, '%') }} 至 {{ contextNumber(selectedEvent.context.financial.amp_upper, '%') }}</b>
                <span>来源 / 数据截止时间</span><b>{{ selectedEvent.context.financial.source || '暂时无法判断' }} / {{ contextTime(selectedEvent.context.financial.as_of) }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.unknown?.length" class="detail-unknown">
              未取得：{{ selectedEvent.context.unknown.join('、') }}
            </div>
          </template>
          <n-alert v-else type="default" :bordered="false" title="命中上下文不可用">
            这是旧版事件或快照版本不可识别，仍保留原命中说明和时间。
          </n-alert>
        </template>
      </n-spin>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.alerts {
  display: grid;
  grid-template-columns: minmax(400px, 440px) minmax(0, 1fr);
  gap: 16px;
  align-items: start;
}
@media (max-width: 900px) {
  .alerts {
    grid-template-columns: 1fr;
  }
}
.col-form,
.col-list {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-width: 0;
}
.form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.section-error,
.detail-error {
  margin-bottom: 12px;
}
.recovery-action {
  margin-top: 8px;
}
.rules {
  display: flex;
  flex-direction: column;
}
.rule {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 12px 6px;
  border-bottom: 1px solid var(--qv-divider);
}
.rule:last-child {
  border-bottom: none;
}
.rule.hit {
  background: v-bind('withAlpha(upColor, 0.06)');
  border-radius: 8px;
}
.rule-main {
  flex: 1;
  min-width: 0;
}
.rule-title {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.rule-name {
  font-size: 14px;
  font-weight: 600;
}
.rule-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.rule-cond {
  font-size: 13px;
  margin-top: 4px;
  opacity: 0.85;
}
.rule-hit {
  font-size: 12px;
  font-weight: 500;
  margin-top: 4px;
}
.rule-hit-time {
  opacity: 0.6;
  font-weight: 400;
}
.rule-sub {
  font-size: 12px;
  opacity: 0.5;
  margin-top: 4px;
}
.rule-note {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 3px;
}
.rule-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
.mobile-actions {
  display: none;
}
/* 命中历史 */
.ev-toolbar {
  display: flex;
  align-items: center;
  gap: 8px;
  /* 卡头 extra 无横滚兜底：radio 组+按钮 ~280px 在 360px 卡头必溢出，允许换行 */
  flex-wrap: wrap;
  justify-content: flex-end;
}
.events {
  display: flex;
  flex-direction: column;
}
.event {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 10px 6px;
  border-bottom: 1px solid var(--qv-divider);
}
.event:last-child {
  border-bottom: none;
}
.event.unread {
  background: v-bind('withAlpha(upColor, 0.05)');
  border-radius: 8px;
}
.ev-main {
  flex: 1;
  min-width: 0;
}
.ev-title {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.ev-name {
  font-size: 13px;
  font-weight: 600;
}
.ev-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.ev-msg {
  font-size: 13px;
  margin-top: 3px;
  opacity: 0.85;
}
.ev-time {
  font-size: 11px;
  opacity: 0.5;
  margin-top: 3px;
}
.ev-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
:global(.event-detail-modal .n-card__content) {
  max-height: min(74vh, 760px);
  overflow-y: auto;
  overscroll-behavior: contain;
}
.detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
}
.detail-symbol {
  margin-left: 8px;
  font-size: 12px;
  opacity: 0.55;
}
.detail-reason {
  margin-top: 10px;
  font-size: 14px;
  line-height: 1.6;
}
.detail-meta,
.detail-unknown {
  margin-top: 5px;
  font-size: 12px;
  opacity: 0.6;
}
.detail-section {
  margin-top: 16px;
  padding-top: 14px;
  border-top: 1px solid v-bind('vars.dividerColor');
}
.detail-section-title {
  margin-bottom: 9px;
  font-size: 13px;
  font-weight: 600;
}
.detail-grid {
  display: grid;
  grid-template-columns: 100px minmax(0, 1fr);
  gap: 8px 12px;
  font-size: 13px;
}
.detail-grid > span {
  opacity: 0.58;
}
.detail-grid > b {
  min-width: 0;
  overflow-wrap: anywhere;
  font-weight: 500;
}
@media (max-width: 480px) {
  .detail-grid {
    grid-template-columns: 84px minmax(0, 1fr);
  }
}
@media (max-width: 768px) {
  .alerts {
    gap: 12px;
  }
  .rules,
  .events {
    gap: 8px;
  }
  .rule,
  .event {
    position: relative;
    border: 1px solid var(--qv-divider);
    border-radius: 6px;
    padding: 12px;
  }
  .rule:last-child,
  .event:last-child {
    border-bottom: 1px solid var(--qv-divider);
  }
  .rule-title {
    padding-right: 34px;
  }
  .rule-note {
    display: none;
  }
  .rule-hit,
  .ev-msg {
    display: -webkit-box;
    overflow: hidden;
    -webkit-box-orient: vertical;
    -webkit-line-clamp: 2;
  }
  .desktop-actions {
    display: none;
  }
  .mobile-actions {
    position: absolute;
    top: 8px;
    right: 8px;
    display: inline-flex;
  }
  .event {
    flex-wrap: wrap;
  }
  .event .ev-main {
    flex-basis: 100%;
  }
  .event .ev-symbol {
    display: none;
  }
  .event .ev-mobile-actions {
    position: static;
    display: flex;
    width: 100%;
    justify-content: flex-end;
  }
  :global(.event-detail-modal .n-card__content) {
    max-height: calc(100dvh - 132px - env(safe-area-inset-bottom, 0px));
    padding-bottom: calc(20px + env(safe-area-inset-bottom, 0px));
  }
}
@media (max-width: 768px) and (max-height: 560px) {
  :global(.mobile-bottom-nav) {
    display: none;
  }
  :global(.event-detail-modal .n-card__content) {
    max-height: calc(100dvh - 96px);
  }
}
</style>
