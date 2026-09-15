<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NEmpty,
  NModal,
  NPopconfirm,
  NSelect,
  NSpin,
  NTag,
  NTooltip,
  useMessage,
} from 'naive-ui'
import {
  listDailyReports,
  getDailyReport,
  generateDailyReport,
  deleteDailyReport,
  type DailyReportRow,
  type DailyReportView,
} from '@/api/report'
import { getSessionEpoch } from '@/api/token'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { pollUntil, isPollCancelled } from '@/lib/poll'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import TrustBadges from '@/components/TrustBadges.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'
import { useDisplayMode } from '@/composables/useDisplayMode'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { pctColor } = useUi()
const { llmLabel } = useLlmLabel()
const { label } = useDisplayMode()

const rows = ref<DailyReportRow[]>([])
const current = ref<DailyReportView | null>(null)
const loading = ref(false)
const submitting = ref(false)
const polling = ref(false)
const generating = computed(() => submitting.value || polling.value)
const selectedId = ref<number | null>(null)
const listLoading = ref(false)
const listError = ref('')
const detailError = ref('')
const deleting = ref(false)
const sessionEpoch = getSessionEpoch()
let active = true
let listSequence = 0
let selectionSeq = 0
let pollAbort: AbortController | null = null
const pageIsCurrent = () => active && sessionEpoch === getSessionEpoch()

const historyOptions = computed(() =>
  rows.value.map((r) => ({
    label: `${r.trade_date}（${statusText(r.status)}）`,
    value: r.id,
  })),
)

function statusText(s: string) {
  return s === 'success' ? '完整' : s === 'partial' ? label('数据不完整', '部分成功（partial）') : s === 'processing' ? '生成中' : '失败'
}
function statusType(s: string): 'success' | 'warning' | 'error' | 'info' {
  return s === 'success' ? 'success' : s === 'partial' ? 'warning' : s === 'processing' ? 'info' : 'error'
}

function stopPolling() {
  pollAbort?.abort()
  pollAbort = null
  polling.value = false
}
function rememberReport(view: DailyReportView) {
  ++listSequence
  listLoading.value = false
  rows.value = [view, ...rows.value.filter(row => row.id !== view.id)]
    .sort((a, b) => b.trade_date.localeCompare(a.trade_date) || b.id - a.id)
}
function syncReportRoute(id: number) {
  if (pageIsCurrent() && routeReportID() !== id) void router.replace({ query: { ...route.query, report_id: String(id) } })
}
function notifyReport(view: DailyReportView) {
  if (view.status === 'failed') message.error(view.error || '日报生成失败')
  else if (view.status === 'partial' || view.error) message.warning(view.error || '日报部分生成成功，请核对缺失部分')
  else message.success('日报已生成')
}
async function refreshRows(): Promise<boolean> {
  if (!pageIsCurrent()) return false
  const seq = ++listSequence
  listLoading.value = true
  listError.value = ''
  try {
    const nextRows = await listDailyReports(30)
    if (!pageIsCurrent() || seq !== listSequence) return false
    rows.value = nextRows
    return true
  } catch (e) {
    if (pageIsCurrent() && seq === listSequence) listError.value = (e as Error).message || '日报列表读取失败'
    return false
  } finally {
    if (pageIsCurrent() && seq === listSequence) listLoading.value = false
  }
}
async function load(preferredId: number | null = null) {
  if (!pageIsCurrent()) return
  const seq = ++selectionSeq
  stopPolling()
  loading.value = true
  detailError.value = ''
  try {
    const listed = await refreshRows()
    if (!pageIsCurrent() || seq !== selectionSeq) return
    const targetId = preferredId || (listed ? rows.value[0]?.id : current.value?.id) || null
    if (targetId) {
      selectedId.value = targetId
      if (current.value?.id !== targetId) current.value = null
      const view = await getDailyReport(targetId)
      if (!pageIsCurrent() || seq !== selectionSeq) return
      current.value = view
      rememberReport(view)
      syncReportRoute(view.id)
      if (view.status === 'processing') void trackProcessing(view.id, seq)
    } else if (listed) {
      current.value = null
      selectedId.value = null
    }
  } catch (e) {
    if (pageIsCurrent() && seq === selectionSeq) detailError.value = (e as Error).message || '日报详情读取失败'
  } finally {
    if (pageIsCurrent() && seq === selectionSeq) loading.value = false
  }
}

async function pick(id: number | null) {
  if (!id || !pageIsCurrent()) return
  const seq = ++selectionSeq
  stopPolling()
  selectedId.value = id
  current.value = null
  loading.value = true
  detailError.value = ''
  try {
    const view = await getDailyReport(id)
    if (!pageIsCurrent() || seq !== selectionSeq) return
    current.value = view
    rememberReport(view)
    syncReportRoute(id)
    if (view.status === 'processing') void trackProcessing(id, seq)
  } catch (e) {
    if (pageIsCurrent() && seq === selectionSeq) detailError.value = (e as Error).message || '日报详情读取失败'
  } finally {
    if (pageIsCurrent() && seq === selectionSeq) loading.value = false
  }
}

// trackProcessing 轮询后台任务直到脱离 processing（生成接口现在立即返回任务，
// 复盘+推荐在服务端后台并行执行——关闭/刷新页面都不影响任务本身）。
// 页面卸载时取消轮询，避免后台请求空转与已销毁组件的状态回填。
onBeforeUnmount(() => {
  active = false
  ++selectionSeq
  ++listSequence
  stopPolling()
})

async function trackProcessing(id: number, seq = selectionSeq) {
  stopPolling()
  polling.value = true
  const controller = new AbortController()
  pollAbort = controller
  const isCurrent = () => pageIsCurrent() && seq === selectionSeq && pollAbort === controller && !controller.signal.aborted && selectedId.value === id
  try {
    const v = await pollUntil(
      () => getDailyReport(id),
      (r) => r.status !== 'processing',
      { signal: controller.signal },
    )
    if (!isCurrent()) return
    current.value = v
    rememberReport(v)
    notifyReport(v)
    void refreshRows()
  } catch (e) {
    if (!isCurrent() || isPollCancelled(e)) return
    detailError.value = (e as Error).message || '日报跟踪读取失败，可重新读取当前日报'
  } finally {
    if (pollAbort === controller) {
      pollAbort = null
      polling.value = false
    }
  }
}

async function doGenerate() {
  if (!pageIsCurrent() || generating.value || deleting.value || loading.value) return
  submitting.value = true
  const seq = selectionSeq
  try {
    const v = await generateDailyReport()
    if (!pageIsCurrent()) return
    rememberReport(v)
    if (seq === selectionSeq) {
      selectedId.value = v.id
      current.value = v
      detailError.value = ''
      syncReportRoute(v.id)
      if (v.status === 'processing') {
        message.info('任务已创建，正在后台生成（刷新或关闭页面不影响任务）')
        void trackProcessing(v.id, seq)
      } else notifyReport(v)
    }
    void refreshRows()
  } catch (e) {
    if (pageIsCurrent() && seq === selectionSeq) message.error((e as Error).message)
  } finally {
    submitting.value = false
  }
}

// 删除当前展示的日报（生成中的任务后端会拒删）。
async function doDelete() {
  if (!current.value || !pageIsCurrent() || deleting.value || submitting.value || current.value.status === 'processing') return
  const id = current.value.id
  const seq = selectionSeq
  deleting.value = true
  try {
    await deleteDailyReport(id)
    if (!pageIsCurrent()) return
    ++listSequence
    listLoading.value = false
    rows.value = rows.value.filter(row => row.id !== id)
    if (current.value?.id === id) current.value = null
    if (selectedId.value === id) {
      selectedId.value = null
      ++selectionSeq
      stopPolling()
      loading.value = false
      detailError.value = ''
      const nextId = rows.value[0]?.id
      if (nextId) await pick(nextId)
      else if (routeReportID() === id) void router.replace({ query: { ...route.query, report_id: undefined } })
    }
    if (!pageIsCurrent()) return
    message.success('已删除')
    void refreshRows()
  } catch (e) {
    if (pageIsCurrent() && seq === selectionSeq) message.error((e as Error).message)
  } finally {
    deleting.value = false
  }
}

const recItems = computed(() => current.value?.recommendation?.items ?? [])

// 复盘证据核验（复盘 JSON 内 evidence_check）。
const reviewCheck = computed(() => {
  const c = current.value?.review?.evidence_check
  return c || null
})

// ---------- 数据快照透明面板（详情已带 snapshot_json） ----------
const snapshotShow = ref(false)
watch(() => current.value?.id, () => { snapshotShow.value = false })
const snapshotText = computed(() => {
  const raw = current.value?.snapshot_json
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
})

// ---------- N2 今日重要事件（硬规则筛出，打分明细随快照落库） ----------
interface ReportEvent {
  title: string
  source: string
  time: string
  score: number
  src_level: number
  impact: number
  fund_sens: number
  major?: boolean
  sectors?: string[]
  merged?: number
  sentiment?: string
}
const events = computed<ReportEvent[]>(() => {
  const raw = current.value?.snapshot_json
  if (!raw) return []
  try {
    const snap = JSON.parse(raw)
    return Array.isArray(snap?.events_today) ? (snap.events_today as ReportEvent[]) : []
  } catch {
    return []
  }
})
function eventScoreTip(e: ReportEvent) {
  return `打分 ${e.score} = 来源级别 ${e.src_level} + 影响范围 ${e.impact} + 资金敏感度 ${e.fund_sens}${
    e.merged ? `；同主线合并 ${e.merged} 条` : ''
  }`
}
function sentiColor(s?: string) {
  if (s === '利好') return pctColor(1)
  if (s === '利空') return pctColor(-1)
  return ''
}

// ---------- F1 明日披露名单（自选∪持仓中次日预约披露财报的标的，随快照落库） ----------
const disclosures = computed<string[]>(() => {
  const raw = current.value?.snapshot_json
  if (!raw) return []
  try {
    const snap = JSON.parse(raw)
    return Array.isArray(snap?.disclosures_tomorrow) ? (snap.disclosures_tomorrow as string[]) : []
  } catch {
    return []
  }
})

const dataDeficiencies = computed<string[]>(() => {
  try {
    const snapshot = JSON.parse(current.value?.snapshot_json || '{}')
    return Array.isArray(snapshot.data_deficiencies) ? snapshot.data_deficiencies.filter((item: unknown) => typeof item === 'string') : []
  } catch {
    return []
  }
})

function routeReportID(): number | null {
  const raw = Array.isArray(route.query.report_id) ? route.query.report_id[0] : route.query.report_id
  const id = Number(raw)
  return Number.isSafeInteger(id) && id > 0 ? id : null
}

watch(
  () => route.query.report_id,
  () => {
    const id = routeReportID()
    if (id && current.value?.id === id && selectedId.value === id) return
    if (id) void pick(id)
    else void load()
  },
)

onMounted(() => void load(routeReportID()))
</script>

<template>
  <PageContainer title="收盘日报" subtitle="复盘今天的市场与持仓，整理下一交易日需要关注的事项。">
    <template #actions>
      <div class="toolbar">
        <n-select
          v-model:value="selectedId"
          :options="historyOptions"
          :loading="listLoading"
          placeholder="历史日报"
          size="small"
          style="width: min(220px, 100%)"
          @update:value="pick"
        />
        <n-popconfirm @positive-click="doGenerate">
          <template #trigger>
            <n-button size="small" type="primary" ghost :loading="generating" :disabled="generating || deleting || loading">生成 / 重生成今日</n-button>
          </template>
          将调用你的 LLM 生成当日复盘与明日推荐（计 1 次配额），已有今日日报会被覆盖，继续？
        </n-popconfirm>
        <n-popconfirm @positive-click="doDelete">
          <template #trigger>
            <n-button size="small" quaternary type="error" :loading="deleting" :disabled="!current || current.status === 'processing' || deleting || submitting">删除</n-button>
          </template>
          删除当前展示的这份日报？关联的推荐批次与研究追踪事实不会删除；若删的是今日日报且开着自动生成，收盘窗口内可能会自动重新生成。
        </n-popconfirm>
      </div>
    </template>

    <n-alert v-if="listError" type="error" :bordered="false" class="load-error">{{ listError }} <n-button size="tiny" :loading="listLoading" @click="refreshRows">重新读取历史列表</n-button></n-alert>
    <n-alert v-if="detailError" type="error" :bordered="false" class="load-error">{{ detailError }} <n-button size="tiny" :loading="loading" @click="pick(selectedId)">重新读取当前日报</n-button></n-alert>
    <n-spin :show="loading">
      <n-empty
        v-if="!current && !listError && !detailError && !loading"
        description="还没有日报。交易日收盘后自动生成（需在 设置→偏好 开启「收盘日报」），或点右上角立即生成。"
        style="padding: 48px 0"
      />
      <div v-if="current" class="report">
        <SectionCard :hoverable="false">
          <div class="head">
            <span class="head-date qv-figure">{{ current.trade_date }}</span>
            <n-tag :type="statusType(current.status)" round :bordered="false">{{ statusText(current.status) }}</n-tag>
            <span v-if="current.status !== 'processing'" class="meta"
              >耗时 {{ (current.latency_ms / 1000).toFixed(1) }}s · {{ label('AI 用量', 'Token') }} {{ current.total_tokens }}<template
                v-if="llmLabel(current)"
              >
                · {{ llmLabel(current) }}</template
              ></span
            >
            <span v-else class="meta">复盘与推荐正在后台并行生成，关闭或刷新页面不影响任务…</span>
            <n-button v-if="snapshotText" size="tiny" quaternary @click="snapshotShow = true">数据快照</n-button>
          </div>
          <div v-if="current.error" class="err">{{ current.error }}</div>
        </SectionCard>

        <n-alert v-if="dataDeficiencies.length" type="warning" :bordered="false" title="本次数据缺口">
          <ul class="deficiencies"><li v-for="item in dataDeficiencies" :key="item">{{ item }}</li></ul>
        </n-alert>

        <!-- 今日复盘 -->
        <SectionCard v-if="current.review" title="今日复盘">
          <template v-if="reviewCheck" #extra>
            <TrustBadges :evidence-check="reviewCheck" />
          </template>
          <div class="review">
            <p class="summary">{{ current.review.summary }}</p>
            <div class="block"><span class="bk">大盘</span><p>{{ current.review.market_review }}</p></div>
            <div v-if="events.length || current.review.events_review" class="block">
              <span class="bk">事件</span>
              <div class="events">
                <p v-if="current.review.events_review">{{ current.review.events_review }}</p>
                <div v-for="(e, i) in events" :key="i" class="event-row">
                  <span class="ev-time qv-tnum">{{ e.time }}</span>
                  <n-tag v-if="e.major" size="tiny" round :bordered="false" type="error">重磅</n-tag>
                  <span class="ev-title">{{ e.title }}</span>
                  <span v-if="e.sentiment && sentiColor(e.sentiment)" class="ev-senti" :style="{ color: sentiColor(e.sentiment) }">{{
                    e.sentiment
                  }}</span>
                  <n-tooltip trigger="hover" style="max-width: 320px">
                    <template #trigger>
                      <span class="ev-score qv-tnum">{{ e.score }}</span>
                    </template>
                    {{ eventScoreTip(e) }}
                  </n-tooltip>
                </div>
              </div>
            </div>
            <div class="block"><span class="bk">持仓</span><p>{{ current.review.position_review }}</p></div>
            <div class="block"><span class="bk">自选</span><p>{{ current.review.watch_review }}</p></div>
            <div v-if="current.review.risk_warnings?.length" class="block warn">
              <span class="bk">风险</span>
              <ul>
                <li v-for="(w, i) in current.review.risk_warnings" :key="i">{{ w }}</li>
              </ul>
            </div>
            <div v-if="disclosures.length" class="block">
              <span class="bk">明日披露</span>
              <ul>
                <li v-for="(d, i) in disclosures" :key="i">{{ d }}</li>
              </ul>
            </div>
            <div class="block plan"><span class="bk">明日计划</span><p>{{ current.review.tomorrow_plan }}</p></div>
          </div>
        </SectionCard>
        <SectionCard v-else title="今日复盘">
          <n-spin v-if="current.status === 'processing'" size="small" style="width: 100%; padding: 24px 0">
            <template #description>AI 复盘生成中…</template>
          </n-spin>
          <n-empty v-else description="复盘生成失败（见上方错误），可点「生成 / 重生成今日」重试" />
        </SectionCard>

        <!-- 明日推荐 -->
        <SectionCard title="明日选股推荐（短线）">
          <template #extra>
            <n-button v-if="current.recommendation" size="tiny" quaternary type="primary" @click="router.push({ path: '/recommendations', query: { batch_id: String(current.recommendation_batch_id) } })"
              >完整详情与追踪 →</n-button
            >
          </template>
          <n-alert v-if="current.recommendation_error" type="error" :bordered="false">{{ current.recommendation_error }} <n-button size="tiny" @click="pick(selectedId)">重新读取</n-button></n-alert>
          <n-alert v-else-if="current.recommendation?.error" type="warning" :bordered="false">{{ current.recommendation.error }}</n-alert>
          <div v-if="recItems.length" class="recs">
            <div v-for="it in recItems" :key="it.id" class="rec">
              <div class="rec-head">
                <StockIdentity :symbol="it.symbol" :market="it.market" :name="it.name" clickable actions />
                <n-tag size="small" round :bordered="false" :type="it.action === 'buy' ? 'error' : 'default'">
                  {{ it.action === 'buy' ? '买入关注' : '观察' }}
                </n-tag>
                <span class="rec-conf"><TermHelp term="ai_confidence" /> {{ it.confidence }}%</span>
              </div>
              <div v-if="it.detail" class="rec-prices">
                <span
                  >买点 <b class="qv-tnum">{{ it.detail.buy_zone_low }} ~ {{ it.detail.buy_zone_high }}</b></span
                >
                <span
                  >止盈 <b class="qv-tnum" :style="{ color: pctColor(1) }">{{ it.detail.take_profit }}</b></span
                >
                <span
                  >止损 <b class="qv-tnum" :style="{ color: pctColor(-1) }">{{ it.detail.stop_loss }}</b></span
                >
                <span>有效期 {{ it.detail.valid_days }} 交易日</span>
              </div>
              <TrustBadges
                v-if="it.detail"
                class="rec-trust"
                :quant-score="it.detail.quant_score"
                :quant-rank="it.detail.quant_rank"
                :pool-size="it.detail.pool_size"
                :lot-cost="it.detail.lot_cost"
                :evidence-check="it.detail.evidence_check"
                :sys-confidence="it.detail.sys_confidence"
                :sys-confidence-why="it.detail.sys_confidence_why"
                :review="it.detail.review"
              />
              <p class="rec-summary">{{ it.summary }}</p>
            </div>
            <p class="sell-hint">
              未持有推荐只做研究追踪；建立真实持仓后，由持仓卖出风险体系统一判断并提醒。
            </p>
          </div>
          <n-spin v-else-if="current.status === 'processing'" size="small" style="width: 100%; padding: 24px 0">
            <template #description>明日推荐生成中…</template>
          </n-spin>
          <n-empty v-else-if="!current.recommendation_error" :description="current.recommendation ? '本次没有可展示的推荐，请查看批次说明与完整详情' : '推荐未生成（见上方错误），可重生成重试'" />
        </SectionCard>

        <p class="disclaimer">本内容为 AI 生成的研究参考，不构成投资建议；数据可能延迟或不完整，决策风险自担。</p>
      </div>
    </n-spin>

    <!-- 数据快照：本日复盘所依据的聚合数据 -->
    <n-modal v-model:show="snapshotShow" preset="card" title="数据快照" style="max-width: 720px">
      <pre class="snapshot-pre">{{ snapshotText }}</pre>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.load-error { margin-bottom: 12px; }
.deficiencies { margin: 0; padding-left: 18px; overflow-wrap: anywhere; }
.toolbar {
  display: flex;
  gap: 10px;
  align-items: center;
  /* 220px 下拉+两按钮合计 ~425px，360px 页头必换行否则整页横滚 */
  flex-wrap: nowrap;
}
.report {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.head {
  display: flex;
  align-items: center;
  gap: 12px;
  /* 窄屏日期+状态+meta 长文本同行会把 22px 日期在连字符处折断，换行更自然 */
  flex-wrap: wrap;
}
.head-date {
  font-size: 22px;
  font-weight: 700;
}
.meta {
  font-size: 12px;
  opacity: 0.55;
}
.err {
  margin-top: 8px;
  font-size: 12px;
  opacity: 0.7;
}
.review .summary {
  font-size: 15px;
  font-weight: 600;
  line-height: 1.7;
  margin: 0 0 12px;
}
.block {
  display: flex;
  gap: 10px;
  margin-bottom: 10px;
  min-width: 0;
}
.block p,
.block ul {
  margin: 0;
  line-height: 1.7;
  flex: 1;
  min-width: 0;
  overflow-wrap: anywhere;
}
.block ul {
  padding-left: 18px;
}
.bk {
  flex-shrink: 0;
  width: 62px;
  font-size: 12px;
  font-weight: 600;
  opacity: 0.6;
  padding-top: 3px;
}
.events {
  flex: 1;
  min-width: 0;
}
.events > p {
  margin: 0 0 8px;
  line-height: 1.7;
}
.event-row {
  display: flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
  min-width: 0;
  padding: 3px 0;
  font-size: 13px;
  line-height: 1.6;
}
.ev-time {
  flex-shrink: 0;
  font-size: 12px;
  opacity: 0.5;
}
.ev-title {
  min-width: 0;
  overflow-wrap: anywhere;
}
.ev-senti {
  flex-shrink: 0;
  font-size: 12px;
  font-weight: 600;
}
.ev-score {
  flex-shrink: 0;
  font-size: 12px;
  opacity: 0.55;
  cursor: help;
}
.recs {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.rec {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.rec-head {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  min-width: 0;
}
.rec-conf {
  font-size: 12px;
  opacity: 0.55;
}
.rec-prices {
  display: flex;
  gap: 18px;
  flex-wrap: wrap;
  font-size: 13px;
}
.rec-summary {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.85;
}
.rec-trust {
  margin: 2px 0;
}
.check-chip {
  font-size: 12px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 12px;
  cursor: default;
}
.snapshot-pre {
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
  max-height: 60vh;
  overflow: auto;
  margin: 0;
}
.sell-hint {
  font-size: 12px;
  opacity: 0.55;
  margin: 4px 0 0;
  line-height: 1.55;
}
.disclaimer {
  font-size: 12px;
  opacity: 0.5;
  margin: 0;
  line-height: 1.55;
}
/* 62px 定宽小标题在 360px 下吃掉近两成宽度，手机改上下堆叠 */
@media (max-width: 768px) {
  .toolbar { flex-wrap: wrap; }
  .block {
    flex-direction: column;
    gap: 3px;
  }
  .bk {
    width: auto;
    padding-top: 0;
  }
  .head-date {
    font-size: 20px;
  }
}
</style>
