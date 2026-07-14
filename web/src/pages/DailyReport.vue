<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
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
  type DailyReportRow,
  type DailyReportView,
} from '@/api/report'
import { useUi } from '@/composables/useUi'
import { useStockActions } from '@/composables/useStockActions'
import { pollUntil } from '@/lib/poll'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import TrustBadges from '@/components/TrustBadges.vue'

const message = useMessage()
const router = useRouter()
const { pctColor, upColor, vars, withAlpha } = useUi()
const { goDetail } = useStockActions()

const rows = ref<DailyReportRow[]>([])
const current = ref<DailyReportView | null>(null)
const loading = ref(false)
const generating = ref(false)
const selectedId = ref<number | null>(null)

const historyOptions = computed(() =>
  rows.value.map((r) => ({
    label: `${r.trade_date}（${statusText(r.status)}）`,
    value: r.id,
  })),
)

function statusText(s: string) {
  return s === 'success' ? '完整' : s === 'partial' ? '部分成功' : s === 'processing' ? '生成中' : '失败'
}
function statusType(s: string): 'success' | 'warning' | 'error' | 'info' {
  return s === 'success' ? 'success' : s === 'partial' ? 'warning' : s === 'processing' ? 'info' : 'error'
}

async function load() {
  loading.value = true
  try {
    rows.value = await listDailyReports(30)
    if (rows.value.length) {
      selectedId.value = rows.value[0].id
      current.value = await getDailyReport(rows.value[0].id)
      // 页面刷新恢复：最新报告仍在后台生成中，继续轮询跟踪。
      if (current.value.status === 'processing') {
        void trackProcessing(current.value.id)
      }
    } else {
      current.value = null
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function pick(id: number | null) {
  if (!id) return
  loading.value = true
  try {
    current.value = await getDailyReport(id)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// trackProcessing 轮询后台任务直到脱离 processing（生成接口现在立即返回任务，
// 复盘+推荐在服务端后台并行执行——关闭/刷新页面都不影响任务本身）。
async function trackProcessing(id: number) {
  generating.value = true
  try {
    const v = await pollUntil(
      () => getDailyReport(id),
      (r) => r.status !== 'processing',
    )
    if (selectedId.value === id || !selectedId.value) {
      current.value = v
      selectedId.value = id
    }
    rows.value = await listDailyReports(30)
    if (v.status === 'failed') {
      message.error(v.error || '日报生成失败')
    } else {
      message.success('日报已生成')
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    generating.value = false
  }
}

async function doGenerate() {
  generating.value = true
  try {
    const v = await generateDailyReport()
    selectedId.value = v.id
    current.value = v
    rows.value = await listDailyReports(30)
    if (v.status === 'processing') {
      message.info('任务已创建，正在后台生成（刷新或关闭页面不影响任务）')
      await trackProcessing(v.id)
      return
    }
    message.success('日报已生成')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    generating.value = false
  }
}

const recItems = computed(() => current.value?.recommendation?.items ?? [])

// 复盘证据核验（复盘 JSON 内 evidence_check）。
const reviewCheck = computed(() => {
  const c = current.value?.review?.evidence_check
  return c && c.total > 0 ? c : null
})
const reviewCheckColor = computed(() =>
  reviewCheck.value && reviewCheck.value.matched === reviewCheck.value.total ? upColor.value : vars.value.warningColor,
)

// ---------- 数据快照透明面板（详情已带 snapshot_json） ----------
const snapshotShow = ref(false)
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

onMounted(load)
</script>

<template>
  <PageContainer title="收盘日报" subtitle="交易日 15:35 后自动生成：今日复盘 + 明日选股推荐（可在设置-偏好开启）">
    <template #actions>
      <div class="toolbar">
        <n-select
          v-model:value="selectedId"
          :options="historyOptions"
          placeholder="历史日报"
          size="small"
          style="width: 220px"
          @update:value="pick"
        />
        <n-popconfirm @positive-click="doGenerate">
          <template #trigger>
            <n-button size="small" type="primary" ghost :loading="generating">生成 / 重生成今日</n-button>
          </template>
          将调用你的 LLM 生成当日复盘与明日推荐（计 1 次配额），已有今日日报会被覆盖，继续？
        </n-popconfirm>
      </div>
    </template>

    <n-spin :show="loading">
      <n-empty
        v-if="!current"
        description="还没有日报。交易日收盘后自动生成（需在 设置→偏好 开启「收盘日报」），或点右上角立即生成。"
        style="padding: 48px 0"
      />
      <div v-else class="report">
        <SectionCard :hoverable="false">
          <div class="head">
            <span class="head-date qv-figure">{{ current.trade_date }}</span>
            <n-tag :type="statusType(current.status)" round :bordered="false">{{ statusText(current.status) }}</n-tag>
            <span v-if="current.status !== 'processing'" class="meta"
              >耗时 {{ (current.latency_ms / 1000).toFixed(1) }}s · {{ current.total_tokens }} tokens</span
            >
            <span v-else class="meta">复盘与推荐正在后台并行生成，关闭或刷新页面不影响任务…</span>
            <n-button v-if="snapshotText" size="tiny" quaternary @click="snapshotShow = true">数据快照</n-button>
          </div>
          <div v-if="current.error" class="err">{{ current.error }}</div>
        </SectionCard>

        <!-- 今日复盘 -->
        <SectionCard v-if="current.review" title="今日复盘">
          <template v-if="reviewCheck" #extra>
            <n-tooltip trigger="hover">
              <template #trigger>
                <span
                  class="check-chip"
                  :style="{ background: withAlpha(reviewCheckColor, 0.12), color: reviewCheckColor }"
                >
                  数据核验 {{ reviewCheck.matched }}/{{ reviewCheck.total }}
                </span>
              </template>
              <span v-if="reviewCheck.unmatched?.length">
                复盘里这些数字未能与快照吻合，可能是推算值或幻觉，建议人工核对：{{ reviewCheck.unmatched.join('、') }}
              </span>
              <span v-else>复盘引用的数字已逐一与数据快照程序化比对，全部吻合</span>
            </n-tooltip>
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
                  <n-tooltip trigger="hover">
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
            <n-button v-if="current.recommendation" size="tiny" quaternary type="primary" @click="router.push('/recommendations')"
              >完整详情与追踪 →</n-button
            >
          </template>
          <div v-if="recItems.length" class="recs">
            <div v-for="it in recItems" :key="it.id" class="rec">
              <div class="rec-head">
                <span class="rec-name" @click="goDetail({ symbol: it.symbol, market: it.market, name: it.name })"
                  >{{ it.name }} <span class="qv-mono rec-sym">{{ it.symbol }}</span></span
                >
                <n-tag size="small" round :bordered="false" :type="it.action === 'buy' ? 'error' : 'default'">
                  {{ it.action === 'buy' ? '买入关注' : '观察' }}
                </n-tag>
                <span class="rec-conf">置信 {{ it.confidence }}%</span>
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
              已按止盈/止损价自动创建到价卖点提醒（见「条件提醒」，note 标注「收盘日报」；命中即进今日待办并推送）。
            </p>
          </div>
          <n-spin v-else-if="current.status === 'processing'" size="small" style="width: 100%; padding: 24px 0">
            <template #description>明日推荐生成中…</template>
          </n-spin>
          <n-empty v-else description="推荐未生成（候选池为空或 LLM 失败），可重生成重试" />
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
.toolbar {
  display: flex;
  gap: 10px;
  align-items: center;
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
}
.block p,
.block ul {
  margin: 0;
  line-height: 1.7;
  flex: 1;
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
}
.rec-name {
  font-weight: 600;
  cursor: pointer;
}
.rec-sym {
  font-size: 12px;
  opacity: 0.55;
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
  word-break: break-word;
  max-height: 60vh;
  overflow: auto;
  margin: 0;
}
.sell-hint {
  font-size: 12px;
  opacity: 0.55;
  margin: 4px 0 0;
}
.disclaimer {
  font-size: 12px;
  opacity: 0.5;
  margin: 0;
}
</style>
