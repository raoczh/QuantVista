<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NCheckbox,
  NDropdown,
  NEmpty,
  NGi,
  NGrid,
  NPagination,
  NRadioButton,
  NRadioGroup,
  NSelect,
  NSpin,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  getTodoInbox,
  updateTodoInbox,
  type TodoChild,
  type TodoInboxAction,
  type TodoItem,
  type TodoResult,
  type TodoScope,
  type TodoSourceRef,
  type TodoStatus,
} from '@/api/todo'
import { getEventCalendar, type CalendarEvent, type CalendarResult } from '@/api/event'
import {
  enumQuery,
  integerQuery,
  queryRef,
  stringQuery,
  useListPageScroll,
  useRouteQueryState,
} from '@/composables/useListPageState'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import StatCard from '@/components/StatCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { upColor, downColor, flatColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({
  '--qv-divider': vars.value.dividerColor,
  '--qv-soft': withAlpha(vars.value.primaryColor, 0.05),
  '--qv-panel': vars.value.cardColor,
  '--qv-muted': vars.value.textColor3,
}))

const data = ref<TodoResult | null>(null)
const loading = ref(false)
const todoError = ref('')
const status = ref<TodoStatus>('needs_action')
const scope = ref<TodoScope>('all')
const source = ref('')
const page = ref(1)
const selected = ref<string[]>([])
const acting = ref('')
let todoAbort: AbortController | null = null
let todoSeq = 0

const statusOptions: { label: string; value: TodoStatus }[] = [
  { label: '需处理', value: 'needs_action' },
  { label: '仅知晓', value: 'awareness' },
  { label: '已完成', value: 'completed' },
]
const scopeOptions = [
  { label: '全部范围', value: 'all' },
  { label: '我的账本', value: 'ledger' },
  { label: '研究跟踪', value: 'research' },
  { label: '全市场', value: 'market' },
]

const sourceLabels: Record<string, string> = {
  alert: '提醒命中',
  rec_review: '推荐复盘',
  position_short: '短线持仓',
  position_long: '长线持仓',
  thesis_due: '逻辑卡',
  stop_loss: '持仓止损',
  corp_adjust: '公司行动',
  ipo: '打新日历',
  sell_review: '卖出复核',
  position_exit: '持仓卖出风险',
  job_failure: '任务失败',
}
const sourceOptions = computed(() => [
  { label: '全部来源', value: '' },
  ...Object.keys(data.value?.source_counts || {})
    .sort()
    .map((value) => ({ label: sourceLabels[value] || value, value })),
])

function isAbortError(error: unknown) {
  const value = error as { name?: string; code?: string }
  return value?.name === 'AbortError' || value?.code === 'ERR_CANCELED'
}

async function load() {
  todoAbort?.abort()
  const ctrl = new AbortController()
  todoAbort = ctrl
  const seq = ++todoSeq
  if (data.value && (data.value.scope !== scope.value || data.value.status !== status.value || data.value.source !== source.value)) {
    data.value = null
  }
  loading.value = true
  todoError.value = ''
  try {
    const result = await getTodoInbox(
      {
        scope: scope.value,
        status: status.value,
        source: source.value || undefined,
        page: page.value,
        page_size: 20,
        history_days: 30,
      },
      ctrl.signal,
    )
    if (seq !== todoSeq) return
    data.value = result
    selected.value = []
  } catch (error) {
    if (seq !== todoSeq || isAbortError(error)) return
    todoError.value = (error as Error).message
    message.error(todoError.value)
  } finally {
    if (seq === todoSeq) loading.value = false
  }
}

function changeFilter() {
  page.value = 1
}

// 加载统一由状态 watch 驱动：URL 驱动的变化（点导航清空 query、浏览器前进/后退）
// 会经 useRouteQueryState 写回这些 ref，若只靠 UI 事件触发加载，页面会停在
// “筛选已复位、列表还是旧数据”的错位视图。同一 tick 的多个变更（如切筛选同时
// 重置页码）由 Vue 批处理合并为一次加载；in-flight 请求由 load 内的 abort 兜底。
watch([status, scope, source, page], () => {
  void load()
})

function kindMeta(kind: string) {
  switch (kind) {
    case 'stop_loss':
    case 'sell_review':
    case 'position_exit':
    case 'job_failure':
      return { label: sourceLabels[kind], color: vars.value.errorColor }
    case 'corp_adjust':
      return { label: sourceLabels[kind], color: vars.value.warningColor }
    case 'alert':
      return { label: sourceLabels[kind], color: upColor.value }
    case 'rec_review':
      return { label: sourceLabels[kind], color: downColor.value }
    case 'ipo':
      return { label: sourceLabels[kind], color: vars.value.successColor }
    case 'thesis_due':
      return { label: sourceLabels[kind], color: vars.value.infoColor }
    default:
      return { label: sourceLabels[kind] || '持仓复盘', color: flatColor.value }
  }
}

function fmtTime(value?: string | null) {
  return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : ''
}

function sourceRef(child: TodoChild): TodoSourceRef {
  return {
    source_kind: child.source_kind,
    source_id: child.source_id,
    source_version: child.source_version,
  }
}

function groupRefs(item: TodoItem) {
  return item.children.map(sourceRef)
}

function groupKey(item: TodoItem) {
  return item.group_key
}

function toggleSelected(item: TodoItem, checked: boolean) {
  const key = groupKey(item)
  selected.value = checked
    ? [...new Set([...selected.value, key])]
    : selected.value.filter((value) => value !== key)
}

const selectedGroups = computed(() =>
  (data.value?.items || []).filter((item) => selected.value.includes(groupKey(item))),
)
const canBatchRead = computed(
  () => selectedGroups.value.length > 0 && selectedGroups.value.every((item) => item.can_complete),
)

async function runAction(action: TodoInboxAction, refs: TodoSourceRef[], key: string) {
  if (acting.value) return
  acting.value = key
  try {
    await updateTodoInbox(action, refs)
    message.success(action === 'read' ? '已收下' : action === 'snooze' ? '已安排明天再说' : '今天不再提醒')
    await load()
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    acting.value = ''
  }
}

function runGroupAction(item: TodoItem, action: TodoInboxAction) {
  void runAction(action, groupRefs(item), item.group_key)
}

function runChildAction(child: TodoChild, action: TodoInboxAction) {
  void runAction(action, [sourceRef(child)], `${child.source_kind}:${child.source_id}`)
}

function batchAction(action: 'read' | 'snooze') {
  const refs = selectedGroups.value.flatMap(groupRefs)
  if (!refs.length) return
  void runAction(action, refs, 'batch')
}

type TodoTarget = Pick<TodoItem, 'deep_link' | 'ref_type' | 'ref_id'>
function handle(target: TodoTarget) {
  if (target.deep_link) {
    void router.push(target.deep_link)
  } else if (target.ref_type === 'alerts') {
    void router.push({ name: 'alerts', query: { event_id: String(target.ref_id) } })
  } else if (target.ref_type === 'recommendations') {
    void router.push({ name: 'recommendations' })
  } else if (target.ref_type === 'positions') {
    void router.push({ name: 'positions' })
  } else if (target.ref_type === 'thesis') {
    void router.push({ name: 'thesis' })
  } else if (target.ref_type === 'tasks') {
    void router.push({ name: 'tasks', query: { job_id: String(target.ref_id) } })
  }
}

function actionOptions(target: TodoItem | TodoChild) {
  const options: { label: string; key: string }[] = []
  if (status.value !== 'completed') {
    if (target.can_complete) options.push({ label: '收下', key: 'read' })
    options.push({ label: '明天再说', key: 'snooze' })
    if ('symbol' in target && target.symbol) options.push({ label: '今天同股同类不再提醒', key: 'mute_today' })
  }
  if (target.ref_type !== 'ipo') options.push({ label: '去处理', key: 'handle' })
  return options
}

function handleGroupMenu(item: TodoItem, key: string) {
  if (key === 'handle') handle(item)
  else runGroupAction(item, key as TodoInboxAction)
}

function handleChildMenu(child: TodoChild, key: string) {
  if (key === 'handle') handle(child as TodoTarget)
  else runChildAction(child, key as TodoInboxAction)
}

useRouteQueryState(route, router, [
  queryRef('status', status, enumQuery<TodoStatus>('needs_action', ['needs_action', 'awareness', 'completed'])),
  queryRef('scope', scope, enumQuery<TodoScope>('all', ['all', 'ledger', 'research', 'market'])),
  queryRef('source', source, stringQuery('', 40)),
  queryRef('page', page, integerQuery(1, 1, 100_000)),
])
const { restoreScroll } = useListPageScroll(route, 'today')

onMounted(async () => {
  await load()
  await restoreScroll()
})

const calendar = ref<CalendarResult | null>(null)
const calLoading = ref(false)
const calError = ref('')
let calAbort: AbortController | null = null
const futureEvents = computed(() => (calendar.value?.events || []).filter((event) => event.days_left >= 0))

async function loadCalendar() {
  calAbort?.abort()
  const ctrl = new AbortController()
  calAbort = ctrl
  calLoading.value = true
  calError.value = ''
  try {
    calendar.value = await getEventCalendar(30, ctrl.signal)
  } catch (error) {
    if (!isAbortError(error)) calError.value = (error as Error).message
  } finally {
    if (calAbort === ctrl) calLoading.value = false
  }
}

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

function relationLabel(value: string) {
  return value === 'position' ? '我的持仓' : value === 'watch' ? '我的自选' : '全市场'
}

function daysLabel(value: number) {
  if (value <= 0) return '今天'
  return value === 1 ? '明天' : `${value} 天后`
}

function openEvent(event: CalendarEvent) {
  if (event.route) void router.push(event.route)
  else if (event.kind === 'ipo' || event.kind === 'cb') message.info(`申购代码 ${event.apply_code || event.symbol}`)
}

onMounted(loadCalendar)
onBeforeUnmount(() => {
  todoSeq++
  todoAbort?.abort()
  calAbort?.abort()
})
</script>

<template>
  <PageContainer title="今日收件箱" subtitle="需处理 · 仅知晓 · 已完成">
    <template #actions>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="today" :style="styleVars">
      <n-grid cols="3" :x-gap="10" :y-gap="10">
        <n-gi><StatCard label="需处理" :value="String(data?.status_counts?.needs_action ?? '—')" /></n-gi>
        <n-gi><StatCard label="仅知晓" :value="String(data?.status_counts?.awareness ?? '—')" /></n-gi>
        <n-gi><StatCard label="已完成" :value="String(data?.status_counts?.completed ?? '—')" /></n-gi>
      </n-grid>

      <SectionCard :title="`收件箱${data?.date ? ' · ' + data.date : ''}`">
        <div class="filters">
          <n-radio-group v-model:value="status" size="small" @update:value="changeFilter">
            <n-radio-button v-for="option in statusOptions" :key="option.value" :value="option.value">
              {{ option.label }} {{ data?.status_counts?.[option.value] ?? '' }}
            </n-radio-button>
          </n-radio-group>
          <div class="filter-selects">
            <n-select v-model:value="scope" size="small" :options="scopeOptions" @update:value="changeFilter" />
            <n-select v-model:value="source" size="small" :options="sourceOptions" @update:value="changeFilter" />
          </div>
        </div>

        <div v-if="selected.length" class="batch-bar">
          <span>已选 {{ selected.length }} 组</span>
          <n-button size="small" :disabled="!canBatchRead" :loading="acting === 'batch'" @click="batchAction('read')">批量收下</n-button>
          <n-button size="small" secondary :loading="acting === 'batch'" @click="batchAction('snooze')">批量稍后</n-button>
        </div>

        <n-spin :show="loading && !data">
          <n-alert v-if="todoError" type="error" :bordered="false" title="收件箱读取失败">
            {{ todoError }}
          </n-alert>
          <n-alert
            v-else-if="data?.partial"
            type="warning"
            :bordered="false"
            class="partial"
            title="收件箱数据不完整"
          >
            <div v-for="(error, index) in data.errors || []" :key="index">{{ error }}</div>
          </n-alert>

          <n-empty
            v-if="data && !data.items.length"
            :description="data.partial ? '当前没有已知事项，部分来源状态未知' : '这个筛选下没有事项'"
            class="empty"
          />

          <div v-else class="items">
            <article v-for="item in data?.items || []" :key="item.group_key" class="item">
              <n-checkbox
                v-if="status !== 'completed'"
                class="item-check"
                :checked="selected.includes(item.group_key)"
                @update:checked="(checked) => toggleSelected(item, checked)"
              />
              <div class="item-bar" :style="{ background: kindMeta(item.kind).color }" />
              <div class="item-main">
                <div class="item-head">
                  <n-tag
                    size="tiny"
                    :bordered="false"
                    :color="{ color: withAlpha(kindMeta(item.kind).color, 0.14), textColor: kindMeta(item.kind).color }"
                  >{{ kindMeta(item.kind).label }}</n-tag>
                  <strong>{{ item.title }}</strong>
                  <StockIdentity v-if="item.symbol" :symbol="item.symbol" :market="item.market || 'cn'" :name="item.name" density="table" clickable />
                </div>
                <p>{{ item.detail }}</p>
                <div class="meta">
                  <span>{{ item.source_label }}</span>
                  <span v-if="item.time">{{ fmtTime(item.time) }}</span>
                  <span v-if="item.due_at">截止 {{ fmtTime(item.due_at) }}</span>
                </div>

                <details v-if="item.child_count > 1" class="children">
                  <summary>{{ item.child_count }} 条来源</summary>
                  <div v-for="child in item.children" :key="`${child.source_kind}-${child.source_id}`" class="child">
                    <div class="child-main">
                      <span>{{ child.source_label }}</span>
                      <strong>{{ child.title }}</strong>
                      <small>{{ child.detail }}</small>
                    </div>
                    <n-dropdown
                      trigger="click"
                      :options="actionOptions(child)"
                      @select="(key) => handleChildMenu(child, String(key))"
                    >
                      <n-button circle quaternary size="small" title="来源操作" aria-label="来源操作">⋯</n-button>
                    </n-dropdown>
                  </div>
                </details>
              </div>

              <div class="desktop-actions">
                <n-button
                  v-if="item.can_complete"
                  size="small"
                  quaternary
                  :loading="acting === item.group_key"
                  @click="runGroupAction(item, 'read')"
                >收下</n-button>
                <n-button v-if="status !== 'completed'" size="small" quaternary @click="runGroupAction(item, 'snooze')">明天再说</n-button>
                <n-button
                  v-if="status !== 'completed' && item.symbol"
                  size="small"
                  quaternary
                  @click="runGroupAction(item, 'mute_today')"
                >今天不再提醒</n-button>
                <n-button v-if="item.ref_type !== 'ipo'" size="small" tertiary @click="handle(item)">去处理</n-button>
              </div>
              <n-dropdown
                class="mobile-actions"
                trigger="click"
                :options="actionOptions(item)"
                @select="(key) => handleGroupMenu(item, String(key))"
              >
                <n-button circle quaternary title="事项操作" aria-label="事项操作">⋯</n-button>
              </n-dropdown>
            </article>
          </div>
        </n-spin>

        <n-pagination
          v-if="(status === 'completed' || status === 'all') && (data?.matched_total || 0) > 20"
          v-model:page="page"
          :page-size="20"
          :item-count="data?.matched_total || 0"
          class="pagination"
        />
      </SectionCard>

      <SectionCard title="后续 30 天">
        <template #extra>
          <n-button size="tiny" quaternary :loading="calLoading" @click="loadCalendar">刷新</n-button>
        </template>
        <n-spin :show="calLoading && !calendar">
          <n-alert v-if="calError" type="error" :bordered="false" class="partial" title="事件日历读取失败">
            {{ calError }}
          </n-alert>
          <n-alert v-else-if="calendar?.complete === false" type="warning" :bordered="false" class="partial" title="事件清单不完整">
            <div v-for="(error, index) in calendar.errors || []" :key="index">{{ error }}</div>
          </n-alert>
          <n-empty v-if="calendar && !futureEvents.length" description="后续 30 天没有已知事件" class="empty" />
          <div v-else class="events">
            <button v-for="event in futureEvents" :key="`${event.kind}-${event.date}-${event.symbol}`" type="button" class="event" @click="openEvent(event)">
              <span class="event-date"><strong class="qv-tnum">{{ event.date.slice(5) }}</strong><small>{{ daysLabel(event.days_left) }}</small></span>
              <span class="event-bar" :style="{ background: eventMeta(event.kind).color }" />
              <span class="event-main">
                <span class="event-head">
                  <n-tag size="tiny" :bordered="false">{{ eventMeta(event.kind).label }}</n-tag>
                  <n-tag v-if="event.relation !== 'market'" size="tiny" :bordered="false" type="info">{{ relationLabel(event.relation) }}</n-tag>
                  <StockIdentity :symbol="event.symbol" :market="event.market || 'cn'" :name="event.name" density="table" clickable />
                </span>
                <small>{{ event.detail }}</small>
              </span>
            </button>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.today {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.filters,
.filter-selects,
.batch-bar,
.item,
.item-head,
.meta,
.desktop-actions,
.child,
.event,
.event-head {
  display: flex;
  align-items: center;
}
.filters {
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 12px;
}
.filter-selects {
  gap: 8px;
}
.filter-selects :deep(.n-select) {
  width: 150px;
}
.batch-bar {
  gap: 8px;
  min-height: 44px;
  padding: 7px 10px;
  margin-bottom: 6px;
  background: var(--qv-soft);
}
.batch-bar span {
  flex: 1;
  font-size: 12px;
}
.partial {
  margin-bottom: 12px;
}
.empty {
  padding: 36px 0;
}
.items {
  display: flex;
  flex-direction: column;
}
.item {
  gap: 10px;
  min-width: 0;
  padding: 13px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.item:last-child {
  border-bottom: 0;
}
.item-check {
  flex: 0 0 auto;
}
.item-bar,
.event-bar {
  width: 3px;
  align-self: stretch;
  flex: 0 0 auto;
}
.item-main,
.event-main,
.child-main {
  min-width: 0;
  flex: 1;
}
.item-head,
.event-head {
  gap: 7px;
  flex-wrap: wrap;
}
.item-head strong {
  font-size: 14px;
  line-height: 1.45;
}
.stock,
.meta,
.child-main span,
.child-main small,
.event-main small {
  color: var(--qv-muted);
  font-size: 12px;
}
.stock {
  margin-left: auto;
}
.item-main > p {
  margin: 5px 0 0;
  font-size: 13px;
  line-height: 1.55;
  overflow-wrap: anywhere;
}
.meta {
  gap: 10px;
  flex-wrap: wrap;
  margin-top: 5px;
}
.desktop-actions {
  gap: 4px;
  flex: 0 0 auto;
}
.mobile-actions {
  display: none;
}
.children {
  margin-top: 9px;
  border-top: 1px solid var(--qv-divider);
}
.children summary {
  padding: 8px 0 4px;
  cursor: pointer;
  font-size: 12px;
}
.child {
  gap: 8px;
  padding: 8px 0 8px 10px;
  border-left: 2px solid var(--qv-divider);
}
.child-main {
  display: grid;
  grid-template-columns: auto 1fr;
  gap: 3px 8px;
}
.child-main small {
  grid-column: 1 / -1;
  line-height: 1.45;
}
.pagination {
  justify-content: flex-end;
  margin-top: 14px;
}
.events {
  display: flex;
  flex-direction: column;
}
.event {
  gap: 11px;
  width: 100%;
  padding: 11px 2px;
  border: 0;
  border-bottom: 1px solid var(--qv-divider);
  color: inherit;
  background: transparent;
  text-align: left;
  cursor: pointer;
}
.event-date {
  display: flex;
  flex: 0 0 54px;
  flex-direction: column;
}
.event-date small,
.event-main small {
  line-height: 1.45;
}
.event-main small {
  display: block;
  margin-top: 4px;
}

@media (max-width: 768px) {
  .filters {
    align-items: stretch;
    flex-direction: column;
  }
  .filters :deep(.n-radio-group) {
    display: flex;
    width: 100%;
  }
  .filters :deep(.n-radio-button) {
    min-width: 0;
    flex: 1 1 0;
  }
  .filters :deep(.n-radio-button__label) {
    width: 100%;
    padding: 0 5px;
    text-align: center;
  }
  .filter-selects :deep(.n-select) {
    width: auto;
    min-width: 0;
    flex: 1;
  }
  .item {
    align-items: flex-start;
    margin-bottom: 8px;
    padding: 12px 10px;
    border: 1px solid var(--qv-divider);
    background: var(--qv-panel);
  }
  .item-bar {
    min-height: 70px;
  }
  .stock {
    width: 100%;
    margin-left: 0;
  }
  .desktop-actions {
    display: none;
  }
  .mobile-actions {
    display: block;
    flex: 0 0 auto;
  }
  .batch-bar {
    flex-wrap: wrap;
  }
}

@media (max-width: 400px) {
  .item {
    gap: 7px;
    padding: 11px 8px;
  }
  .item-check {
    margin-top: 5px;
  }
  .event {
    align-items: flex-start;
  }
}
</style>
