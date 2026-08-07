<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NInputNumber,
  NSelect,
  NSwitch,
  NForm,
  NFormItem,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NGrid,
  NGi,
  NRadioGroup,
  NRadioButton,
  NModal,
  NAlert,
  useMessage,
} from 'naive-ui'
import {
  listAlerts,
  createAlert,
  updateAlert,
  setAlertStatus,
  deleteAlert,
  evaluateAlerts,
  listAlertEvents,
  getAlertEvent,
  setAlertEventStatus,
  readAllAlertEvents,
  isPositionAlertKind,
  type AlertRule,
  type AlertInput,
  type AlertEvent,
  type AlertEventStatus,
} from '@/api/alert'
import {
  listChannels,
  createChannel,
  updateChannel,
  deleteChannel,
  testChannel,
  type NotifyChannel,
  type NotifyKind,
} from '@/api/notify'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { upColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
  { label: '港股', value: 'hk' },
  { label: '美股', value: 'us' },
]
const kindOptions = [
  { label: '【持仓】相对我的成本涨 N%（考虑落袋）', value: 'cost_gain' },
  { label: '【持仓】相对我的成本跌 N%（考虑止损）', value: 'cost_drawdown' },
  { label: '【持仓】自持仓期最高回撤 N%（移动止盈）', value: 'peak_drawdown' },
  { label: '到价提醒', value: 'price' },
  { label: '涨跌幅异动', value: 'pct_change' },
  { label: '均线（站上/跌破）', value: 'ma' },
  { label: '突破（新高/新低）', value: 'breakout' },
  { label: '放量（vs 20 日均量）', value: 'volume_surge' },
  { label: '振幅异动', value: 'amplitude' },
  { label: '财报披露临近', value: 'earn_date' },
  { label: '新业绩预告', value: 'earn_fcst' },
]
// 财报日历类：无条件方向语义，且不走盘中行情评估（每日盘后财报数据刷新时评估一次）。
const isEarnKind = computed(() => form.value.kind === 'earn_date' || form.value.kind === 'earn_fcst')
// D14/D15 持仓类：方向由 kind 自带（op 无意义）、代码可留空 = 我的全部持仓、
// 命中后不自动暂停（一条规则覆盖多笔持仓，暂停整条会让其余持仓失联）。
const isPosKind = computed(() => isPositionAlertKind(form.value.kind))
// op 选项随 kind 变化，文案更贴切。
const opOptions = computed(() => {
  switch (form.value.kind) {
    case 'ma':
      return [
        { label: '站上均线', value: 'gte' },
        { label: '跌破均线', value: 'lte' },
      ]
    case 'breakout':
      return [
        { label: '创新高', value: 'gte' },
        { label: '创新低', value: 'lte' },
      ]
    case 'volume_surge':
      return [
        { label: '放量达到倍数', value: 'gte' },
        { label: '缩量低于倍数', value: 'lte' },
      ]
    case 'amplitude':
      return [
        { label: '振幅达到', value: 'gte' },
        { label: '振幅低于', value: 'lte' },
      ]
    default:
      return [
        { label: '大于等于 ≥', value: 'gte' },
        { label: '小于等于 ≤', value: 'lte' },
      ]
  }
})
const needThreshold = computed(
  () =>
    form.value.kind === 'price' ||
    form.value.kind === 'pct_change' ||
    form.value.kind === 'volume_surge' ||
    form.value.kind === 'amplitude' ||
    form.value.kind === 'earn_date' ||
    isPosKind.value,
)
const needPeriod = computed(() => form.value.kind === 'ma' || form.value.kind === 'breakout')
const thresholdLabel = computed(() => {
  switch (form.value.kind) {
    case 'price':
      return '目标价'
    case 'pct_change':
      return '涨跌幅阈值（%）'
    case 'volume_surge':
      return '量比倍数（如 2 = 2 倍 20 日均量）'
    case 'amplitude':
      return '振幅阈值（%，(最高-最低)/昨收）'
    case 'earn_date':
      return '提前天数（距预约披露日 ≤N 天提醒）'
    case 'cost_gain':
      return '相对我的成本涨幅（%，如 20 = 涨 20% 提醒落袋）'
    case 'cost_drawdown':
      return '相对我的成本跌幅（%，如 8 = 跌 8% 提醒止损）'
    case 'peak_drawdown':
      return '自持仓期最高价的回撤（%，如 15 = 从最高点回撤 15% 提醒）'
    default:
      return '阈值'
  }
})

// ---------- 表单 ----------
const editingId = ref<number | null>(null)
const form = ref<AlertInput & { symbol: string; market: string }>({
  symbol: '',
  market: 'cn',
  name: '',
  kind: 'price',
  op: 'gte',
  threshold: undefined,
  period: 20,
  once: true,
  note: '',
})
function resetForm() {
  editingId.value = null
  form.value = { symbol: '', market: 'cn', name: '', kind: 'price', op: 'gte', threshold: undefined, period: 20, once: true, note: '' }
}
function editRule(r: AlertRule) {
  editingId.value = r.id
  form.value = {
    symbol: r.symbol,
    market: r.market,
    name: r.name,
    kind: r.kind,
    op: r.op,
    threshold: r.threshold ?? undefined,
    period: r.period || 20,
    once: r.once,
    note: r.note,
  }
}

const saving = ref(false)
async function submit() {
  // 持仓类允许留空代码（= 我的全部持仓）；其余类型必须绑定标的。
  if (!editingId.value && !isPosKind.value && !form.value.symbol.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (needThreshold.value && (form.value.threshold == null || (form.value.kind !== 'pct_change' && form.value.threshold <= 0))) {
    message.warning(`请输入${thresholdLabel.value}`)
    return
  }
  saving.value = true
  try {
    if (editingId.value) await updateAlert(editingId.value, form.value)
    else await createAlert(form.value)
    message.success('已保存')
    resetForm()
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

// ---------- 列表 ----------
const rules = ref<AlertRule[]>([])
const loading = ref(false)
async function load() {
  loading.value = true
  try {
    rules.value = await listAlerts()
  } catch (e) {
    message.error((e as Error).message)
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
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    evaluating.value = false
  }
}
async function toggle(r: AlertRule) {
  try {
    await setAlertStatus(r.id, r.status === 'paused' ? 'active' : 'paused')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function remove(r: AlertRule) {
  try {
    await deleteAlert(r.id)
    if (editingId.value === r.id) resetForm()
    await load()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
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
  resetForm()
  form.value.symbol = String(route.query.symbol || '')
  form.value.market = String(route.query.market || 'cn')
  form.value.name = String(route.query.name || '')
  void router.replace({ name: 'alerts' })
}

watch(() => route.query._stock_action, applyStockActionQuery)

onMounted(async () => {
  // 从自选/持仓「设提醒」跳转预填。
  applyStockActionQuery()
  await Promise.all([load(), loadChannels(), loadEvents()])
  await openRouteEvent()
})

// ---------- 命中历史（明细事件状态机） ----------
const events = ref<AlertEvent[]>([])
const eventsLoading = ref(false)
const eventFilter = ref<'unread' | 'all' | 'read' | 'dismissed'>('unread')
const eventFilterOptions = [
  { label: '未读', value: 'unread' },
  { label: '全部', value: 'all' },
  { label: '已读', value: 'read' },
  { label: '已忽略', value: 'dismissed' },
]
async function loadEvents() {
  eventsLoading.value = true
  try {
    events.value = await listAlertEvents(eventFilter.value === 'all' ? undefined : eventFilter.value)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    eventsLoading.value = false
  }
}
async function markEvent(ev: AlertEvent, status: AlertEventStatus) {
  try {
    await setAlertEventStatus(ev.id, status)
    await loadEvents()
  } catch (e) {
    message.error((e as Error).message)
  }
}
const readingAll = ref(false)
async function markAllRead() {
  readingAll.value = true
  try {
    const { updated } = await readAllAlertEvents()
    message.success(updated > 0 ? `已标记 ${updated} 条为已读` : '没有未读命中')
    await loadEvents()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    readingAll.value = false
  }
}
function eventStatusTag(s: AlertEventStatus) {
  if (s === 'unread') return { text: '未读', type: 'warning' as const }
  if (s === 'read') return { text: '已读', type: 'success' as const }
  return { text: '已忽略', type: 'default' as const }
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
    return
  }
  const seq = ++detailSeq
  detailLoading.value = true
  try {
    const event = await getAlertEvent(id)
    if (seq !== detailSeq || routeEventID() !== id) return
    selectedEvent.value = event
    detailOpen.value = true
  } catch (e) {
    if (seq !== detailSeq || routeEventID() !== id) return
    selectedEvent.value = null
    detailOpen.value = false
    message.error((e as Error).message)
  } finally {
    if (seq === detailSeq) detailLoading.value = false
  }
}

function openEventDetail(event: AlertEvent) {
  selectedEvent.value = event
  detailOpen.value = true
  void router.push(event.deep_link)
}

function closeEventDetail() {
  detailSeq++
  detailOpen.value = false
  selectedEvent.value = null
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
  if (value == null) return 'unknown'
  return `${Number(value.toFixed(4))}${unit}`
}
function contextTime(value?: string) {
  if (!value) return 'unknown'
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

// ---------- 推送通道 ----------
const channels = ref<NotifyChannel[]>([])
const chForm = ref<{ kind: NotifyKind; name: string; target: string; enabled: boolean }>({
  kind: 'serverchan',
  name: '',
  target: '',
  enabled: true,
})
// ntfy 通道三字段（提交时拼 JSON 作 target，整串加密落库）。
const ntfyForm = ref({ url: '', topic: '', token: '' })
const kindNotifyOptions = [
  { label: 'Server酱', value: 'serverchan' },
  { label: '自定义 Webhook', value: 'webhook' },
  { label: 'ntfy（App 推送）', value: 'ntfy' },
]
async function loadChannels() {
  try {
    channels.value = await listChannels()
  } catch (e) {
    message.error((e as Error).message)
  }
}
const chAdding = ref(false)
async function addChannel() {
  if (chAdding.value) return
  let target = chForm.value.target.trim()
  if (chForm.value.kind === 'ntfy') {
    const { url, topic, token } = ntfyForm.value
    if (!url.trim() || !topic.trim()) {
      message.warning('请填写 ntfy 服务地址与 topic')
      return
    }
    target = JSON.stringify({ url: url.trim(), topic: topic.trim(), token: token.trim() })
  } else if (!target) {
    message.warning(chForm.value.kind === 'serverchan' ? '请输入 Server酱 SendKey' : '请输入 Webhook 地址')
    return
  }
  chAdding.value = true
  try {
    await createChannel({ ...chForm.value, target })
    chForm.value.target = ''
    chForm.value.name = ''
    ntfyForm.value = { url: '', topic: '', token: '' }
    await loadChannels()
    message.success('已添加推送通道')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    chAdding.value = false
  }
}
async function toggleChannel(ch: NotifyChannel) {
  try {
    await updateChannel(ch.id, { kind: ch.kind, name: ch.name, enabled: !ch.enabled })
    await loadChannels()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function testCh(ch: NotifyChannel) {
  try {
    await testChannel(ch.id)
    message.success('测试推送已发送，请查收')
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function removeChannel(ch: NotifyChannel) {
  try {
    await deleteChannel(ch.id)
    await loadChannels()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}
function channelKindLabel(k: string) {
  if (k === 'serverchan') return 'Server酱'
  if (k === 'ntfy') return 'ntfy'
  return 'Webhook'
}
</script>

<template>
  <PageContainer
    title="条件提醒"
    subtitle="持仓成本止盈止损 / 移动止盈 · 到价 / 异动 / 均线 / 突破 · 可配推送通道主动通知"
  >
    <template #actions>
      <n-button size="small" quaternary :loading="evaluating" @click="runEvaluate">立即检查</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="alerts" :style="styleVars">
      <!-- 左：新建/编辑 -->
      <div class="col-form">
        <SectionCard :title="editingId ? '编辑提醒' : '新建提醒'">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-form-item label="提醒类型">
              <n-select v-model:value="form.kind" :options="kindOptions" />
            </n-form-item>
            <div v-if="isPosKind" class="hint" style="margin: -4px 0 4px">
              持仓类提醒基于<b>我的实际持仓成本</b>（加权均价，加减仓自动重算），无需手工填价位。
              股票代码<b>留空即覆盖我的全部持仓</b>；命中后不会自动暂停，且无当前有效行情的持仓本轮不评（不用旧价误报）。
            </div>
            <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
              <n-gi>
                <n-form-item :label="isPosKind ? '股票代码（留空=全部持仓）' : '股票代码'">
                  <n-input
                    v-model:value="form.symbol"
                    :placeholder="isPosKind ? '留空覆盖全部持仓' : '如 600000'"
                    :disabled="!!editingId"
                  />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item label="市场">
                  <n-select v-model:value="form.market" :options="marketOptions" :disabled="!!editingId" />
                </n-form-item>
              </n-gi>
            </n-grid>
            <n-form-item v-if="!isEarnKind && !isPosKind" label="条件方向">
              <n-select v-model:value="form.op" :options="opOptions" />
            </n-form-item>
            <n-form-item v-if="needThreshold" :label="thresholdLabel">
              <n-input-number
                v-model:value="form.threshold"
                :precision="form.kind === 'earn_date' ? 0 : 2"
                :min="form.kind === 'earn_date' ? 1 : undefined"
                :max="form.kind === 'earn_date' ? 30 : undefined"
                style="width: 100%"
              />
            </n-form-item>
            <div v-if="isEarnKind" class="hint" style="margin: -4px 0 4px">
              财报提醒按每日盘后刷新的披露/预告数据评估（每天一次），不占用盘中行情检查。
            </div>
            <n-form-item v-if="needPeriod" label="周期（交易日）">
              <n-input-number v-model:value="form.period" :min="2" :max="250" style="width: 100%" />
            </n-form-item>
            <n-form-item v-if="!isPosKind" label="命中后自动暂停">
              <n-switch v-model:value="form.once" />
              <span class="switch-hint">开启后命中一次即暂停，避免重复提示</span>
            </n-form-item>
            <n-form-item label="备注">
              <n-input v-model:value="form.note" placeholder="可选" maxlength="256" />
            </n-form-item>
            <div class="form-actions">
              <n-button v-if="editingId" quaternary @click="resetForm">取消编辑</n-button>
              <n-button type="primary" :loading="saving" @click="submit">{{ editingId ? '保存修改' : '添加提醒' }}</n-button>
            </div>
          </n-form>
        </SectionCard>

        <SectionCard title="推送通道">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-form-item label="通道类型">
              <n-select v-model:value="chForm.kind" :options="kindNotifyOptions" />
            </n-form-item>
            <template v-if="chForm.kind === 'ntfy'">
              <n-form-item label="ntfy 服务地址">
                <n-input v-model:value="ntfyForm.url" placeholder="https://ntfy.example.com（自建，必须 https）" />
              </n-form-item>
              <n-form-item label="Topic">
                <n-input v-model:value="ntfyForm.topic" placeholder="如 qv-u1（与手机 ntfy App 订阅一致）" />
              </n-form-item>
              <n-form-item label="访问令牌 Token">
                <n-input v-model:value="ntfyForm.token" placeholder="tk_xxx（服务端 ntfy token add 生成，可空）" />
              </n-form-item>
            </template>
            <n-form-item v-else :label="chForm.kind === 'serverchan' ? 'SendKey' : 'Webhook 地址'">
              <n-input
                v-model:value="chForm.target"
                :placeholder="chForm.kind === 'serverchan' ? 'Server酱 SendKey' : 'https://...'"
              />
            </n-form-item>
            <n-button type="primary" ghost block :loading="chAdding" @click="addChannel">添加通道</n-button>
            <div class="hint">
              提醒命中时会主动推送到已启用的通道（同一提醒每天最多推一次）。密钥加密存储、不回显。ntfy
              为自建 App 系统级推送，服务端部署与手机配置见 mobile/README.md。
            </div>
          </n-form>

          <div v-if="channels.length" class="channels">
            <div v-for="ch in channels" :key="ch.id" class="channel">
              <div class="ch-main">
                <div class="ch-title">
                  <n-tag
                    size="tiny"
                    round
                    :bordered="false"
                    :type="ch.kind === 'serverchan' ? 'info' : ch.kind === 'ntfy' ? 'success' : 'default'"
                    >{{ channelKindLabel(ch.kind) }}</n-tag
                  >
                  <span class="ch-name">{{ ch.name }}</span>
                  <n-tag size="tiny" round :bordered="false" :type="ch.enabled ? 'success' : 'default'">{{
                    ch.enabled ? '启用' : '停用'
                  }}</n-tag>
                </div>
                <div v-if="ch.last_error" class="ch-err">上次推送失败：{{ ch.last_error }}</div>
              </div>
              <div class="ch-actions">
                <n-button size="tiny" quaternary @click="testCh(ch)">测试</n-button>
                <n-button size="tiny" quaternary @click="toggleChannel(ch)">{{ ch.enabled ? '停用' : '启用' }}</n-button>
                <n-popconfirm @positive-click="removeChannel(ch)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error">删</n-button>
                  </template>
                  删除该推送通道？
                </n-popconfirm>
              </div>
            </div>
          </div>
        </SectionCard>
      </div>

      <!-- 右：规则列表 + 命中历史 -->
      <div class="col-list">
        <SectionCard title="我的提醒">
          <n-spin :show="loading && !rules.length">
            <n-empty v-if="!rules.length" description="暂无提醒规则，在左侧添加一条" />
            <div v-else class="rules">
              <div v-for="r in rules" :key="r.id" class="rule" :class="{ hit: isHitToday(r) }">
                <div class="rule-main">
                  <div class="rule-title">
                    <span class="rule-name">{{ ruleScope(r) }}</span>
                    <span v-if="r.symbol" class="rule-symbol qv-mono">{{ r.symbol }}</span>
                    <n-tag size="tiny" round :bordered="false" :type="statusTag(r).type">{{
                      statusTag(r).text
                    }}</n-tag>
                    <n-tag v-if="isPositionAlertKind(r.kind)" size="tiny" round :bordered="false" type="info">{{
                      kindLabelMap[r.kind]
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
                <div class="rule-actions">
                  <n-button size="tiny" quaternary @click="toggle(r)">{{ r.status === 'paused' ? '恢复' : '暂停' }}</n-button>
                  <n-button size="tiny" quaternary @click="editRule(r)">编辑</n-button>
                  <n-popconfirm @positive-click="remove(r)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error">删除</n-button>
                    </template>
                    删除提醒「{{ ruleScope(r) }}」？
                  </n-popconfirm>
                </div>
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
            <n-empty
              v-if="!events.length"
              :description="eventFilter === 'unread' ? '没有未读命中，规则命中后会在这里留档' : '暂无命中记录'"
              style="padding: 24px 0"
            />
            <div v-else class="events">
              <div v-for="ev in events" :key="ev.id" class="event" :class="{ unread: ev.status === 'unread' }">
                <div class="ev-main">
                  <div class="ev-title">
                    <n-tag size="tiny" round :bordered="false">{{ kindLabelMap[ev.kind] || ev.kind }}</n-tag>
                    <span class="ev-name">{{ ev.name || ev.symbol }}</span>
                    <span class="ev-symbol qv-mono">{{ ev.symbol }}</span>
                    <n-tag size="tiny" round :bordered="false" :type="eventStatusTag(ev.status).type">{{
                      eventStatusTag(ev.status).text
                    }}</n-tag>
                  </div>
                  <div class="ev-msg">{{ ev.message }}</div>
                  <div class="ev-time">{{ fmtTime(ev.triggered_at) }}</div>
                </div>
                <div class="ev-actions">
                  <n-button size="tiny" quaternary @click="openEventDetail(ev)">详情</n-button>
                  <template v-if="ev.status === 'unread'">
                    <n-button size="tiny" quaternary @click="markEvent(ev, 'read')">已读</n-button>
                    <n-button size="tiny" quaternary @click="markEvent(ev, 'dismissed')">忽略</n-button>
                  </template>
                  <n-button v-else size="tiny" quaternary @click="markEvent(ev, 'unread')">恢复未读</n-button>
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
        <template v-if="selectedEvent">
          <div class="detail-head">
            <div>
              <strong>{{ selectedEvent.name || selectedEvent.symbol }}</strong>
              <span class="detail-symbol qv-mono">{{ selectedEvent.symbol }}</span>
            </div>
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
                <span>数据来源</span><b>{{ selectedEvent.context.source || 'unknown' }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.quote" class="detail-section">
              <div class="detail-section-title">行情快照</div>
              <div class="detail-grid">
                <span>现价</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.quote.price, ' 元') }}</b>
                <span>最高 / 最低</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.quote.high, ' 元') }} / {{ contextNumber(selectedEvent.context.quote.low, ' 元') }}</b>
                <span>涨跌幅</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.quote.change_pct, '%') }}</b>
                <span>来源 / 时点</span><b>{{ selectedEvent.context.quote.source || 'unknown' }} / {{ contextTime(selectedEvent.context.quote.as_of) }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.bar" class="detail-section">
              <div class="detail-section-title">日线依据</div>
              <div class="detail-grid">
                <span>交易日</span><b>{{ selectedEvent.context.bar.trade_date || 'unknown' }}</b>
                <span>开 / 高 / 低 / 收</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.bar.open) }} / {{ contextNumber(selectedEvent.context.bar.high) }} / {{ contextNumber(selectedEvent.context.bar.low) }} / {{ contextNumber(selectedEvent.context.bar.close) }}</b>
                <span>样本数</span><b class="qv-tnum">{{ selectedEvent.context.bar.sample_size ?? 'unknown' }}</b>
                <span>数据来源</span><b>{{ selectedEvent.context.bar.source || 'unknown' }}</b>
              </div>
            </div>

            <div v-if="selectedEvent.context.indicator" class="detail-section">
              <div class="detail-section-title">技术指标</div>
              <div class="detail-grid">
                <span>指标</span><b>{{ metricLabel(selectedEvent.context.indicator.name) }}<template v-if="selectedEvent.context.indicator.period">（{{ selectedEvent.context.indicator.period }} 日）</template></b>
                <span>指标值</span><b class="qv-tnum">{{ contextNumber(selectedEvent.context.indicator.value, selectedEvent.context.indicator.unit) }}</b>
                <span v-if="selectedEvent.context.indicator.reference != null">参考值</span><b v-if="selectedEvent.context.indicator.reference != null" class="qv-tnum">{{ contextNumber(selectedEvent.context.indicator.reference) }}</b>
                <span>来源 / 时点</span><b>{{ selectedEvent.context.indicator.source || 'unknown' }} / {{ contextTime(selectedEvent.context.indicator.as_of) }}</b>
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
                <span>来源 / 时点</span><b>{{ selectedEvent.context.financial.source || 'unknown' }} / {{ contextTime(selectedEvent.context.financial.as_of) }}</b>
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
  grid-template-columns: 340px 1fr;
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
.switch-hint {
  font-size: 12px;
  opacity: 0.5;
  margin-left: 10px;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 4px;
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
@media (max-width: 768px) {
  .rule {
    flex-wrap: wrap;
    row-gap: 4px;
  }
  .rule-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
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
/* 推送通道 */
.channels {
  display: flex;
  flex-direction: column;
  margin-top: 8px;
  border-top: 1px solid var(--qv-divider);
}
.channel {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.channel:last-child {
  border-bottom: none;
}
.ch-main {
  flex: 1;
  min-width: 0;
}
.ch-title {
  display: flex;
  align-items: center;
  gap: 6px;
}
.ch-name {
  font-size: 13px;
  font-weight: 500;
}
.ch-err {
  font-size: 11px;
  color: v-bind('upColor');
  margin-top: 3px;
}
.ch-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
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
</style>
