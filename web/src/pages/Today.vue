<script setup lang="ts">
import { ref, onMounted, onBeforeUnmount, computed } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NSpin,
  NEmpty,
  NTag,
  NGrid,
  NGi,
  NAlert,
  NRadioGroup,
  NRadioButton,
  useMessage,
} from 'naive-ui'
import { getTodos, type TodoItem, type TodoResult, type TodoScope } from '@/api/todo'
import { getEventCalendar, type CalendarEvent, type CalendarResult } from '@/api/event'
import { setAlertEventStatus } from '@/api/alert'
import { setSellReviewStatus } from '@/api/position'
import { ackRecommendationReview } from '@/api/recommendation'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'

const message = useMessage()
const router = useRouter()
const { upColor, downColor, flatColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const data = ref<TodoResult | null>(null)
const loading = ref(false)
const todoError = ref('')
// D18：默认只看与我的账本有关的条目——推荐复盘（AI 推过就追踪、没买也天天提示）
// 是旧待办的噪音主体，已挪到推荐追踪页；打新归「全市场」。数据一条不删，随时可切。
const scope = ref<TodoScope>('ledger')
const scopeOptions: { label: string; value: TodoScope }[] = [
  { label: '我的账本', value: 'ledger' },
  { label: '研究跟踪', value: 'research' },
  { label: '全市场', value: 'market' },
  { label: '全部', value: 'all' },
]
let todoAbort: AbortController | null = null
let todoSeq = 0
async function load() {
  todoAbort?.abort()
  const ctrl = new AbortController()
  todoAbort = ctrl
  const mySeq = ++todoSeq
  const requestedScope = scope.value
  // 切换范围时不保留上一范围的条目，避免加载期间把账本待办显示在“研究跟踪”下。
  if (data.value?.scope !== requestedScope) data.value = null
  loading.value = true
  todoError.value = ''
  try {
    const result = await getTodos(requestedScope, ctrl.signal)
    if (mySeq !== todoSeq) return
    data.value = result
  } catch (e) {
    if (mySeq !== todoSeq || isAbortError(e)) return
    data.value = null
    todoError.value = (e as Error).message
    message.error((e as Error).message)
  } finally {
    if (mySeq === todoSeq) loading.value = false
  }
}
// 其它范围还有多少条（提示用户「东西没丢，在别处」）。
const elsewhereHint = computed(() => {
  const d = data.value
  if (!d || d.scope === 'all' || d.filtered <= 0) return ''
  const parts: string[] = []
  const counts = d.scope_counts || {}
  if (d.scope !== 'research' && counts.research > 0) parts.push(`推荐追踪页有 ${counts.research} 条复盘提示`)
  if (d.scope !== 'market' && counts.market > 0) parts.push(`全市场有 ${counts.market} 条机会（打新等）`)
  if (d.scope !== 'ledger' && counts.ledger > 0) parts.push(`我的账本有 ${counts.ledger} 条`)
  return parts.join('，')
})

// 类型 → 展示元信息（标签 + 强调色）。
function kindMeta(kind: string) {
  switch (kind) {
    case 'alert':
      return { label: '提醒', color: upColor.value }
    case 'stop_loss':
      return { label: '止损警示', color: vars.value.errorColor }
    case 'sell_review':
      return { label: '卖出复核', color: vars.value.errorColor }
    case 'rec_review':
      return { label: '推荐复盘', color: downColor.value }
    case 'position_short':
      return { label: '短线持仓', color: vars.value.warningColor }
    case 'position_long':
      return { label: '长线持仓', color: flatColor.value }
    case 'thesis_due':
      return { label: '逻辑卡复盘', color: vars.value.infoColor }
    case 'corp_adjust':
      return { label: '除权折算', color: vars.value.warningColor }
    case 'ipo':
      return { label: '今日打新', color: vars.value.successColor }
    default:
      return { label: '待办', color: flatColor.value }
  }
}
function fmtTime(t: string | null) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

// 一键跳转到对应页面处理。
function handle(item: TodoItem) {
  if (item.deep_link) {
    router.push(item.deep_link)
  } else if (item.ref_type === 'alerts') {
    router.push({ name: 'alerts', query: { event_id: String(item.ref_id) } })
  } else if (item.ref_type === 'recommendations') {
    router.push({ name: 'recommendations' })
  } else if (item.ref_type === 'positions') {
    router.push({ name: 'positions' })
  } else if (item.ref_type === 'thesis') {
    router.push({ name: 'thesis' })
  }
}

// 提醒命中待办可就地完成：已读/忽略后从清单消失（ref_id 即 alert_event id）。
const marking = ref<number | null>(null)
async function markAlert(item: TodoItem, status: 'read' | 'dismissed') {
  if (marking.value) return
  marking.value = item.ref_id
  try {
    await setAlertEventStatus(item.ref_id, status)
    message.success(status === 'read' ? '已标记已读' : '已忽略')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    marking.value = null
  }
}

// 推荐复盘待办可就地已读消项（ref_id 即追踪状态行 id；后台追踪刷新不会打回未读）。
async function markRecReview(item: TodoItem) {
  if (marking.value) return
  marking.value = item.ref_id
  try {
    await ackRecommendationReview(item.ref_id)
    message.success('已标记已读')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    marking.value = null
  }
}

// 卖出复核就地消项（ref_id 即 sell_review id）。「已复核」= 我看过并作出了决定，
// 「忽略」= 终态不再提示；两者都不会被下一轮扫描拉回。
async function markSellReview(item: TodoItem, status: 'resolved' | 'dismissed') {
  if (marking.value) return
  marking.value = item.ref_id
  try {
    await setSellReviewStatus(item.ref_id, status)
    message.success(status === 'resolved' ? '已标记复核完成' : '已忽略')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    marking.value = null
  }
}

onMounted(load)

// ---------- B9 事件日历（未来 30 天与我相关的解禁/除权/财报 + 全市场打新） ----------

const calendar = ref<CalendarResult | null>(null)
const calLoading = ref(false)
const calError = ref('')
let calAbort: AbortController | null = null

function isAbortError(e: unknown) {
  const err = e as { name?: string; code?: string }
  return err?.name === 'AbortError' || err?.code === 'ERR_CANCELED'
}

async function loadCalendar() {
  calAbort?.abort()
  const ctrl = new AbortController()
  calAbort = ctrl
  calLoading.value = true
  calError.value = ''
  try {
    calendar.value = await getEventCalendar(30, ctrl.signal)
  } catch (e) {
    if (isAbortError(e)) return
    calError.value = (e as Error).message
  } finally {
    if (calAbort === ctrl) calLoading.value = false
  }
}

// 事件类型 → 展示元信息。
function eventMeta(kind: string) {
  switch (kind) {
    case 'lift':
      return { label: '解禁', color: downColor.value }
    case 'ex_div':
      return { label: '除权除息', color: vars.value.warningColor }
    case 'earn':
      return { label: '财报', color: vars.value.infoColor }
    case 'ipo':
      return { label: '新股申购', color: upColor.value }
    case 'cb':
      return { label: '可转债申购', color: vars.value.successColor }
    default:
      return { label: '事件', color: flatColor.value }
  }
}
function relationLabel(rel: string) {
  return rel === 'position' ? '我的持仓' : rel === 'watch' ? '我的自选' : '全市场'
}
// 日期边界：0=今天、负数不应出现（后端只查未来窗口），其余按天数展示。
function daysLabel(n: number) {
  if (n <= 0) return '今天'
  if (n === 1) return '明天'
  return `${n} 天后`
}
function openEvent(ev: CalendarEvent) {
  if (ev.route) {
    router.push(ev.route)
  } else if (ev.kind === 'ipo' || ev.kind === 'cb') {
    // 打新无个股详情可跳（申购代码不是可查行情的标的），停留在本页即可。
    message.info(`申购代码 ${ev.apply_code || ev.symbol}，请在券商 App 下单`)
  }
}

onMounted(loadCalendar)
onBeforeUnmount(() => {
  todoSeq++
  todoAbort?.abort()
  todoAbort = null
  calAbort?.abort()
  calAbort = null
})
</script>

<template>
  <PageContainer title="今日待办" subtitle="默认只看与我的账本有关的事 —— 卖出复核 · 持仓风险 · 命中提醒">
    <template #actions>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="todo" :style="styleVars">
      <n-grid cols="2 s:3" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="待办合计" :value="data ? String(data.total) : '—'" />
        </n-gi>
        <n-gi>
          <StatCard label="命中提醒" :value="data ? String(data.alerts) : '—'" />
        </n-gi>
        <n-gi>
          <StatCard label="待复盘" :value="data ? String(data.reviews) : '—'" />
        </n-gi>
      </n-grid>

      <SectionCard :title="`清单${data?.date ? ' · ' + data.date : ''}`">
        <template #extra>
          <n-radio-group v-model:value="scope" size="small" @update:value="load">
            <n-radio-button v-for="opt in scopeOptions" :key="opt.value" :value="opt.value">{{
              opt.label
            }}</n-radio-button>
          </n-radio-group>
        </template>
        <n-spin :show="loading && !data">
          <n-alert v-if="todoError" type="error" :bordered="false" title="今日待办读取失败">
            {{ todoError }}
          </n-alert>
          <n-alert
            v-else-if="data && data.complete === false"
            type="warning"
            :bordered="false"
            style="margin-bottom: 12px"
            title="待办清单可能不完整"
          >
            <div v-for="(e, i) in data.errors || []" :key="i">{{ e }}</div>
            <div v-if="!data.errors?.length">部分数据读取失败，状态不明的事项未列出，请稍后刷新重试。</div>
          </n-alert>
          <div v-if="elsewhereHint" class="scope-hint">当前只显示「{{ scopeOptions.find((o) => o.value === scope)?.label }}」；{{ elsewhereHint }}。</div>
          <n-empty
            v-if="data && !data.items.length"
            :description="
              data.complete === false
                ? '暂未取到待办事项，但部分数据读取失败，状态不明——不代表一切正常'
                : scope === 'ledger'
                  ? '我的持仓今天没有需要处理的事项 👍'
                  : '这个范围今天没有需要处理的事项 👍'
            "
            style="padding: 40px 0"
          />
          <div v-else class="items">
            <div v-for="(it, i) in data?.items || []" :key="i" class="item">
              <div class="item-bar" :style="{ background: kindMeta(it.kind).color }" />
              <div class="item-main">
                <div class="item-head">
                  <n-tag
                    size="tiny"
                    round
                    :bordered="false"
                    :color="{ color: withAlpha(kindMeta(it.kind).color, 0.14), textColor: kindMeta(it.kind).color }"
                    >{{ kindMeta(it.kind).label }}</n-tag
                  >
                  <span class="item-title">{{ it.title }}</span>
                  <span class="item-stock">{{ it.name || it.symbol }}<span class="item-symbol qv-mono"> {{ it.symbol }}</span></span>
                </div>
                <div class="item-detail">{{ it.detail }}</div>
                <div v-if="it.time" class="item-time">{{ fmtTime(it.time) }}</div>
              </div>
              <div class="item-actions">
                <template v-if="it.kind === 'alert'">
                  <n-button size="small" quaternary :loading="marking === it.ref_id" @click="markAlert(it, 'read')"
                    >已读</n-button
                  >
                  <n-button size="small" quaternary @click="markAlert(it, 'dismissed')">忽略</n-button>
                </template>
                <template v-else-if="it.kind === 'rec_review'">
                  <n-button size="small" quaternary :loading="marking === it.ref_id" @click="markRecReview(it)"
                    >已读</n-button
                  >
                </template>
                <template v-else-if="it.kind === 'sell_review'">
                  <n-button size="small" quaternary :loading="marking === it.ref_id" @click="markSellReview(it, 'resolved')"
                    >已复核</n-button
                  >
                  <n-button size="small" quaternary @click="markSellReview(it, 'dismissed')">忽略</n-button>
                </template>
                <n-button v-if="it.ref_type !== 'ipo'" size="small" tertiary @click="handle(it)">去处理</n-button>
              </div>
            </div>
          </div>
        </n-spin>
      </SectionCard>

      <SectionCard title="事件日历 · 未来 30 天">
        <template #extra>
          <n-button size="tiny" quaternary :loading="calLoading" @click="loadCalendar">刷新</n-button>
        </template>
        <n-spin :show="calLoading && !calendar">
          <n-alert v-if="calError" type="error" :bordered="false" style="margin-bottom: 12px" title="事件日历读取失败">
            {{ calError }}
          </n-alert>
          <n-alert
            v-else-if="calendar && calendar.complete === false"
            type="warning"
            :bordered="false"
            style="margin-bottom: 12px"
            title="事件清单可能不完整"
          >
            <div v-for="(e, i) in calendar.errors || []" :key="i">{{ e }}</div>
          </n-alert>
          <n-empty
            v-if="!calError && calendar && !calendar.events.length"
            :description="
              calendar.complete === false
                ? '未取到事件，但部分数据读取失败，状态不明——不代表未来 30 天无事发生'
                : '未来 30 天没有与你相关的解禁、除权、财报，也没有可申购的新股'
            "
            style="padding: 32px 0"
          />
          <div v-else-if="!calError" class="events">
            <div v-for="(ev, i) in calendar?.events || []" :key="i" class="event" @click="openEvent(ev)">
              <div class="event-date">
                <span class="event-day qv-tnum">{{ ev.date.slice(5) }}</span>
                <span class="event-left">{{ daysLabel(ev.days_left) }}</span>
              </div>
              <div class="event-bar" :style="{ background: eventMeta(ev.kind).color }" />
              <div class="event-main">
                <div class="event-head">
                  <n-tag
                    size="tiny"
                    round
                    :bordered="false"
                    :color="{ color: withAlpha(eventMeta(ev.kind).color, 0.14), textColor: eventMeta(ev.kind).color }"
                    >{{ eventMeta(ev.kind).label }}</n-tag
                  >
                  <n-tag v-if="ev.relation !== 'market'" size="tiny" round :bordered="false" type="info">{{
                    relationLabel(ev.relation)
                  }}</n-tag>
                  <span class="event-name"
                    >{{ ev.name || ev.symbol }}<span class="event-symbol qv-mono"> {{ ev.symbol }}</span></span
                  >
                </div>
                <div class="event-detail">{{ ev.detail }}</div>
              </div>
            </div>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.todo {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.scope-hint {
  font-size: 12px;
  opacity: 0.6;
  margin-bottom: 10px;
}
.items {
  display: flex;
  flex-direction: column;
}
.item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
@media (max-width: 768px) {
  .item {
    flex-wrap: wrap;
    row-gap: 4px;
  }
  .item-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
}
.item:last-child {
  border-bottom: none;
}
.item-bar {
  width: 3px;
  align-self: stretch;
  border-radius: 3px;
  flex-shrink: 0;
}
.item-main {
  flex: 1;
  min-width: 0;
}
.item-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.item-title {
  font-size: 14px;
  font-weight: 600;
}
.item-stock {
  font-size: 13px;
  opacity: 0.75;
}
.item-symbol {
  opacity: 0.5;
  font-size: 12px;
}
.item-detail {
  font-size: 13px;
  opacity: 0.75;
  margin-top: 4px;
}
.item-time {
  font-size: 11px;
  opacity: 0.5;
  margin-top: 3px;
}
.item-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}

/* B9 事件日历 */
.events {
  display: flex;
  flex-direction: column;
}
.event {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 4px;
  border-bottom: 1px solid var(--qv-divider);
  cursor: pointer;
  transition: opacity 0.15s ease;
}
.event:hover {
  opacity: 0.75;
}
.event:last-child {
  border-bottom: none;
}
.event-date {
  display: flex;
  flex-direction: column;
  align-items: flex-start;
  width: 62px;
  flex-shrink: 0;
}
.event-day {
  font-size: 14px;
  font-weight: 600;
}
.event-left {
  font-size: 11px;
  opacity: 0.55;
}
.event-bar {
  width: 3px;
  align-self: stretch;
  border-radius: 3px;
  flex-shrink: 0;
}
.event-main {
  flex: 1;
  min-width: 0;
}
.event-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.event-name {
  font-size: 14px;
  font-weight: 600;
}
.event-symbol {
  opacity: 0.5;
  font-size: 12px;
  font-weight: 400;
}
.event-detail {
  font-size: 12px;
  opacity: 0.72;
  margin-top: 3px;
}
@media (max-width: 768px) {
  .event {
    flex-wrap: wrap;
    row-gap: 4px;
  }
  .event-date {
    width: 54px;
  }
}
</style>
