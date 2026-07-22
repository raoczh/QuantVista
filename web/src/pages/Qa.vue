<script setup lang="ts">
import { ref, onMounted, computed, nextTick, onBeforeUnmount, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NSpin,
  NEmpty,
  NPopconfirm,
  NAlert,
  NModal,
  NTooltip,
  NTag,
  useDialog,
  useMessage,
} from 'naive-ui'
import {
  askQa,
  listConversations,
  getConversation,
  deleteConversation,
  getQaSnapshot,
  type QaAskRequest,
  type QaTaskResult,
  type QaConversation,
  type QaConversationView,
  type QaMessage,
} from '@/api/qa'
import { getLLMTask, listLLMTasks, type LLMTask } from '@/api/llmTask'
import type { EvidenceCheck, RiskFlag } from '@/api/trust'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { getApiErrorCode } from '@/api/client'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { renderMarkdown } from '@/lib/markdown'
import { pollUntil, isPollCancelled } from '@/lib/poll'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import TrustBadges from '@/components/TrustBadges.vue'

const message = useMessage()
const dialog = useDialog()
const route = useRoute()
const router = useRouter()
const { vars, withAlpha } = useUi()
const { llmLabel } = useLlmLabel()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

// ---------- 当前会话 ----------
const current = ref<QaConversationView | null>(null)
const symbol = ref('')
const market = ref('cn')
const question = ref('')
const asking = ref(false)
const scrollBox = ref<HTMLElement | null>(null)
const activeTaskId = ref<number | null>(null)
const taskError = ref('')
const taskErrorCode = ref('')
// 从分析结果「继续问答」进入：新会话复用该分析记录的数据快照（不重复拉数）。
const fromAnalysisId = ref<number | null>(null)
const fromAnalysisName = ref('')

// ---------- 后台任务 ----------
const pendingQuestion = ref('')
let pollAbort: AbortController | null = null
onBeforeUnmount(() => pollAbort?.abort())

// ---------- LLM ----------
const llmConfigs = ref<LLMConfig[]>([])
const llmId = ref<number | undefined>(undefined)
const llmOptions = computed(() =>
  llmConfigs.value.map((c) => ({ label: c.is_default ? `${c.name}（默认）` : c.name, value: c.id })),
)
async function loadLLM() {
  try {
    llmConfigs.value = await listLLMConfigs()
    const def = llmConfigs.value.find((c) => c.is_default) || llmConfigs.value[0]
    if (def) llmId.value = def.id
  } catch (e) {
    message.error((e as Error).message)
  }
}

async function scrollToBottom() {
  await nextTick()
  if (scrollBox.value) scrollBox.value.scrollTop = scrollBox.value.scrollHeight
}

// allowStale 显式比较 === true：模板 @click/@keydown 直接绑 send 时首参是 Event 对象，
// 不得被误当成「已确认历史解释模式」。
async function send(allowStale?: boolean | Event) {
  const staleOk = allowStale === true
  if (asking.value) return // 回车/连击防抖：后台任务执行中不重复提交
  const q = question.value.trim()
  if (!q) return
  if (!current.value && !symbol.value.trim()) {
    message.warning('请输入要提问的股票代码')
    return
  }
  const startConvId = current.value?.id
  const payload: QaAskRequest = {
    conversation_id: startConvId,
    symbol: current.value ? undefined : symbol.value.trim(),
    market: current.value ? undefined : market.value,
    llm_config_id: llmId.value,
    question: q,
    analysis_record_id: !current.value && fromAnalysisId.value ? fromAnalysisId.value : undefined,
    allow_stale: staleOk || undefined,
  }
  await submitQaTask(payload, startConvId)
}

function showTaskFailure(messageText: string, code = '', payload?: QaAskRequest, startConvId?: number) {
  const msg = messageText || '问答任务失败'
  taskError.value = msg
  taskErrorCode.value = code
  // 行情时效 fail-closed：后台终态仍保留 stale_quote，沿用原请求显式确认后重提。
  if (payload && !payload.allow_stale && (code === 'stale_quote' || (!code && msg.includes('历史数据解释')))) {
    dialog.warning({
      title: '行情非最新，无法按最新行情回答',
      content: msg + '。是否按「截至行情时刻的历史数据解释」模式继续？该模式下回答不代表当前盘面，也不会给出当前买卖参考。',
      positiveText: '按历史数据解释提问',
      negativeText: '取消',
      onPositiveClick: () => {
        void submitQaTask({ ...payload, allow_stale: true }, startConvId)
      },
    })
    return
  }
  message.error(msg)
}

async function trackQaTask(
  initial: LLMTask<QaTaskResult>,
  payload?: QaAskRequest,
  startConvId?: number,
) {
  pollAbort?.abort()
  const controller = new AbortController()
  pollAbort = controller
  activeTaskId.value = initial.id
  asking.value = true
  try {
    const task =
      initial.status === 'processing'
        ? await pollUntil(
            () => getLLMTask<QaTaskResult>(initial.id),
            (v) => v.status !== 'processing',
            { signal: controller.signal, timeoutMs: 11 * 60 * 1000 },
          )
        : initial
    if (task.status === 'failed') {
      showTaskFailure(task.error || '问答任务失败', task.error_code || '', payload, startConvId)
      return
    }
    if (!task.result?.conversation_id) throw new Error('问答任务已完成，但未返回会话编号')
    const view = await getConversation(task.result.conversation_id)

    // 任务期间若用户切换了会话，只刷新历史，不用旧任务结果覆盖当前界面。
    if (current.value?.id === startConvId) {
      current.value = view
      if (!payload || question.value.trim() === payload.question.trim()) question.value = ''
      fromAnalysisId.value = null
      fromAnalysisName.value = ''
      await scrollToBottom()
    }
    taskError.value = ''
    taskErrorCode.value = ''
    message.success('回答已生成')
  } catch (e) {
    if (isPollCancelled(e)) return
    showTaskFailure((e as Error).message || '问答任务状态读取失败', getApiErrorCode(e) || '', payload, startConvId)
  } finally {
    if (pollAbort === controller) {
      pollAbort = null
      activeTaskId.value = null
      asking.value = false
      pendingQuestion.value = ''
      await loadHistory()
    }
  }
}

async function submitQaTask(payload: QaAskRequest, startConvId?: number) {
  if (asking.value) return
  asking.value = true
  pendingQuestion.value = payload.question
  taskError.value = ''
  taskErrorCode.value = ''
  await scrollToBottom()
  try {
    const task = await askQa(payload)
    message.info('任务已创建，正在后台生成回答（刷新或关闭页面不影响任务）')
    await loadHistory()
    await trackQaTask(task, payload, startConvId)
  } catch (e) {
    showTaskFailure((e as Error).message || '问答任务提交失败', getApiErrorCode(e) || '', payload, startConvId)
    asking.value = false
    pendingQuestion.value = ''
  } finally {
    await loadHistory()
  }
}

function newChat() {
  current.value = null
  symbol.value = ''
  question.value = ''
  taskError.value = ''
  taskErrorCode.value = ''
  fromAnalysisId.value = null
  fromAnalysisName.value = ''
}

// 用当前会话的标的开新会话：首次提问会按最新行情采集新快照（新 ID），不覆盖旧会话。
function newChatFromCurrent() {
  const c = current.value
  if (!c) return
  const sym = c.symbol
  const mkt = c.market || 'cn'
  newChat()
  symbol.value = sym
  market.value = mkt
}

// ---------- 风险闸门标签（S1）----------
// 会话快照的展示用时效：按读取时刻重判的 current_status 优先（后端 Get 回填），
// 旧记录无该字段时回退快照创建时的 freshness_status。
const qaFreshStatus = computed(() => {
  const m = current.value?.snapshot_meta
  return m?.current_status || m?.freshness_status || ''
})

function riskTagType(f: RiskFlag): 'error' | 'warning' | 'default' {
  if (f.level === 'block') return 'error'
  if (f.level === 'warn') return 'warning'
  return 'default'
}
function riskTagLabel(f: RiskFlag): string {
  const names: Record<string, string> = {
    st: 'ST/风险警示',
    delist: '退市风险',
    limit_board: '一字板',
    low_liquidity: '流动性不足',
    small_cap: '小市值',
  }
  return names[f.code] || f.code
}

// ---------- 历史 ----------
const history = ref<QaConversation[]>([])
const historyLoading = ref(false)
async function loadHistory() {
  historyLoading.value = true
  try {
    history.value = await listConversations(30)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    historyLoading.value = false
  }
}

async function recoverProcessingTask() {
  try {
    const tasks = await listLLMTasks<QaTaskResult>({ kind: 'qa', status: 'processing', limit: 1 })
    const task = tasks[0]
    if (!task) return
    message.info('已恢复正在后台执行的问答任务')
    void trackQaTask(task, undefined, current.value?.id)
  } catch (e) {
    message.error((e as Error).message)
  }
}

async function openConv(c: QaConversation) {
  try {
    current.value = await getConversation(c.id)
    taskError.value = ''
    taskErrorCode.value = ''
    await scrollToBottom()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function removeConv(c: QaConversation) {
  try {
    await deleteConversation(c.id)
    if (current.value?.id === c.id) newChat()
    await loadHistory()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

// 回答的证据核验（check_json 解析；旧消息无此字段则不渲染徽章）。
function msgCheck(m: QaMessage): EvidenceCheck | null {
  if (!m.check_json) return null
  try {
    const c = JSON.parse(m.check_json) as EvidenceCheck
    return c && c.total > 0 ? c : null
  } catch {
    return null
  }
}

// ---------- 数据快照透明面板（按需单取，详情不带） ----------
const snapshotShow = ref(false)
const snapshotText = ref('')
async function openSnapshot() {
  if (!current.value) return
  try {
    const res = await getQaSnapshot(current.value.id)
    try {
      snapshotText.value = JSON.stringify(JSON.parse(res.data_snapshot), null, 2)
    } catch {
      snapshotText.value = res.data_snapshot || '（无快照）'
    }
    snapshotShow.value = true
  } catch (e) {
    message.error((e as Error).message)
  }
}

// 深链快捷入口：从个股详情/分析结果带 symbol 或 from_analysis 进入。
// 用 watch 而非仅在 onMounted 读取——已停留在 /qa 时再次深链（query 变化）也能生效。
function applyRouteQuery() {
  if (route.query.from_analysis) {
    const id = Number(route.query.from_analysis)
    if (Number.isFinite(id) && id > 0) {
      fromAnalysisId.value = id
      fromAnalysisName.value = String(route.query.name || route.query.symbol || '')
    }
    symbol.value = String(route.query.symbol || '')
    market.value = String(route.query.market || 'cn')
    current.value = null // 深链进入新会话上下文
    router.replace({ name: 'qa' })
  } else if (route.query.symbol) {
    symbol.value = String(route.query.symbol)
    market.value = String(route.query.market || 'cn')
    current.value = null
    router.replace({ name: 'qa' })
  }
}

watch(() => route.query, applyRouteQuery)

onMounted(async () => {
  applyRouteQuery()
  await Promise.all([loadLLM(), loadHistory()])
  await recoverProcessingTask()
})
</script>

<template>
  <PageContainer title="个股 AI 问答" subtitle="固定一份数据快照多轮追问 · 仅依据行情与技术指标 · 研究参考">
    <div class="qa" :style="styleVars">
      <!-- 左：历史会话 -->
      <div class="col-side">
        <SectionCard title="会话">
          <template #extra>
            <n-button size="tiny" quaternary :loading="historyLoading" @click="loadHistory">刷新列表</n-button>
            <n-button size="tiny" type="primary" ghost @click="newChat">＋ 新会话</n-button>
          </template>
          <n-spin :show="historyLoading && !history.length">
            <n-empty v-if="!history.length" description="暂无会话" size="small" />
            <div v-else class="convs">
              <div
                v-for="c in history"
                :key="c.id"
                class="conv"
                :class="{ active: current?.id === c.id }"
                @click="openConv(c)"
              >
                <div class="conv-main">
                  <div class="conv-title">{{ c.name || c.symbol }}<span class="conv-symbol qv-mono"> {{ c.symbol }}</span></div>
                  <div class="conv-sub">{{ c.title }}</div>
                  <div class="conv-meta">{{ c.message_count }} 条 · {{ fmtTime(c.updated_at) }}</div>
                </div>
                <n-popconfirm :disabled="asking" @positive-click="removeConv(c)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error" :disabled="asking" @click.stop>删</n-button>
                  </template>
                  删除该会话？
                </n-popconfirm>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>

      <!-- 右：对话 -->
      <div class="col-chat">
        <SectionCard :title="current ? `${current.name || current.symbol}（${current.symbol}）` : '新问答'">
          <template v-if="current" #extra>
            <div class="extra-row">
              <n-button size="tiny" quaternary @click="newChatFromCurrent">按最新数据新建会话</n-button>
              <n-button size="tiny" quaternary @click="openSnapshot">数据快照</n-button>
              <span class="chat-meta">{{ llmLabel(current) || current.model }} · {{ current.total_tokens }} tokens</span>
            </div>
          </template>

          <n-alert v-if="taskError" type="error" title="后台问答失败" :bordered="false" style="margin-bottom: 12px">
            {{ taskError }}<span v-if="taskErrorCode" class="task-code">（{{ taskErrorCode }}）</span>
          </n-alert>
          <n-alert v-else-if="asking" type="info" title="正在后台生成回答" :bordered="false" style="margin-bottom: 12px">
            任务 #{{ activeTaskId || '创建中' }} 正在执行，刷新或关闭页面不会中断。
          </n-alert>

          <!-- 行情新鲜度（q10）：以按读取时刻重判的 current_status 为准展示——快照内的
               freshness_status 是创建时的历史事实，昨天 fresh 的会话今天必须能亮「行情非最新」。 -->
          <div v-if="current?.snapshot_meta" class="fresh-row">
            <n-tooltip
              v-if="qaFreshStatus === 'stale' || qaFreshStatus === 'unknown'"
              trigger="hover"
              style="max-width: 340px"
            >
              <template #trigger>
                <n-tag size="small" :type="qaFreshStatus === 'stale' ? 'warning' : 'default'" :bordered="false">
                  ⚠ {{ qaFreshStatus === 'stale' ? '行情非最新' : '行情时效未核验' }}
                </n-tag>
              </template>
              {{
                current.snapshot_meta.current_note ||
                '该会话快照的行情已非当前有效口径（可能停牌、休市、跨天或数据源延迟）；回答涉及价格以「行情截至时间」为准，不代表当前盘面。'
              }}
            </n-tooltip>
            <span class="fresh-text">
              行情截至 {{ current.snapshot_meta.quote_as_of || '—' }} · 技术指标截至
              {{ current.snapshot_meta.bars_as_of || '—' }} · 来源 {{ current.snapshot_meta.quote_source || '—' }}
              <template v-if="current.snapshot_meta.market_state && current.snapshot_meta.market_state !== 'trading'">
                （非交易时段，最近收盘口径）
              </template>
            </span>
            <span v-if="current.snapshot_meta.captured_at" class="fresh-text fresh-dim">
              · 快照采集于 {{ current.snapshot_meta.captured_at }}
            </span>
          </div>

          <!-- 风险闸门标签（S1：快照程序化判定，与注入 prompt 同源） -->
          <div v-if="current?.risk_flags?.length" class="risk-flags">
            <n-tooltip v-for="f in current.risk_flags" :key="f.code" trigger="hover" style="max-width: 340px">
              <template #trigger>
                <n-tag size="small" :type="riskTagType(f)" :bordered="false">
                  {{ f.level === 'block' ? '⛔' : f.level === 'warn' ? '⚠' : 'ⓘ' }} {{ riskTagLabel(f) }}
                </n-tag>
              </template>
              {{ f.text }}
            </n-tooltip>
          </div>

          <!-- 新会话：选择标的 -->
          <div v-if="!current" class="starter">
            <div class="starter-row">
              <n-input
                v-model:value="symbol"
                placeholder="股票代码，如 600000"
                style="max-width: 200px"
                :disabled="!!fromAnalysisId"
              />
              <n-select v-model:value="market" :options="marketOptions" style="width: 110px" :disabled="!!fromAnalysisId" />
              <n-select v-model:value="llmId" :options="llmOptions" :placeholder="llmConfigs.length ? 'LLM 配置' : '默认配置'" style="width: 180px" />
            </div>
            <n-alert v-if="fromAnalysisId" type="info" :bordered="false" style="margin-top: 10px">
              将基于「{{ fromAnalysisName || symbol }}」分析记录 #{{ fromAnalysisId }}
              的数据快照提问——问答所见与分析所见完全一致，可直接追问分析结论。
            </n-alert>
            <div v-else class="starter-hint">
              首次提问会采集一次该股的行情与技术指标快照并核验时效：行情非最新（停牌/休市异常/数据源故障）时会先征求你的确认，确认后按「截至行情时刻的历史数据解释」回答；之后多轮追问都基于这份快照（跨天后会重新标注时效），不重复拉数据。
            </div>
          </div>

          <!-- 对话流 -->
          <div ref="scrollBox" class="messages" :class="{ compact: !current }">
            <div v-if="current || asking" class="msg-list">
              <template v-if="current">
                <div v-for="m in current.messages" :key="m.id" class="msg" :class="m.role">
                  <div class="bubble-wrap">
                    <!-- assistant 内容经 renderMarkdown（marked+DOMPurify 消毒）后渲染 -->
                    <div v-if="m.role === 'assistant'" class="bubble md" v-html="renderMarkdown(m.content)"></div>
                    <div v-else class="bubble">{{ m.content }}</div>
                    <div v-if="m.role === 'assistant' && msgCheck(m)" class="check-badge">
                      <TrustBadges :evidence-check="msgCheck(m)!" />
                    </div>
                  </div>
                </div>
              </template>
              <!-- 后台任务中的本轮问答（完成后由正式消息替换，核验徽章随后出现） -->
              <template v-if="asking">
                <div v-if="pendingQuestion" class="msg user">
                  <div class="bubble-wrap"><div class="bubble">{{ pendingQuestion }}</div></div>
                </div>
                <div class="msg assistant">
                  <div class="bubble-wrap">
                    <div class="bubble thinking">后台生成中…</div>
                  </div>
                </div>
              </template>
            </div>
            <n-empty v-else description="选择标的并提问，开始一段多轮问答" style="padding: 24px 0" />
          </div>

          <!-- 输入 -->
          <div class="composer">
            <n-input
              v-model:value="question"
              type="textarea"
              :autosize="{ minRows: 1, maxRows: 4 }"
              placeholder="就这只股票提问，如「现在的均线排列如何？回撤风险大吗？」"
              maxlength="500"
              :disabled="asking"
              @keydown.enter.exact.prevent="send"
            />
            <n-button type="primary" :loading="asking" @click="send">发送</n-button>
          </div>
          <div class="composer-hint">仅依据行情/技术指标与已采集的新闻公告快照回答，不构成投资建议。Enter 发送。</div>
        </SectionCard>
      </div>
    </div>

    <!-- 数据快照：本会话固定、多轮问答复用的结构化数据 -->
    <n-modal v-model:show="snapshotShow" preset="card" title="数据快照" style="max-width: 720px">
      <pre class="snapshot-pre">{{ snapshotText }}</pre>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.qa {
  display: grid;
  grid-template-columns: 300px 1fr;
  gap: 16px;
  align-items: start;
}
@media (max-width: 900px) {
  .qa {
    grid-template-columns: 1fr;
  }
}
.col-side,
.col-chat {
  min-width: 0;
}
/* 整页滚动下会话栏固定跟随，过长时栏内滚动 */
.col-side {
  position: sticky;
  top: 76px;
  max-height: calc(100vh - 100px);
  overflow-y: auto;
  padding: 4px;
  margin: -4px;
}
@media (max-width: 900px) {
  .col-side {
    position: static;
    max-height: none;
    overflow: visible;
    /* 折叠单列后对话区优先展示，会话列表沉底（纯 CSS 调序，DOM 不动） */
    order: 2;
  }
}
.convs {
  display: flex;
  flex-direction: column;
}
.conv {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 10px 6px;
  border-bottom: 1px solid var(--qv-divider);
  cursor: pointer;
  border-radius: 6px;
}
.conv:last-child {
  border-bottom: none;
}
.conv:hover,
.conv.active {
  background: v-bind('withAlpha(vars.primaryColor, 0.08)');
}
.conv-main {
  flex: 1;
  min-width: 0;
}
.conv-title {
  font-size: 13px;
  font-weight: 600;
}
.conv-symbol {
  opacity: 0.5;
  font-weight: 400;
  font-size: 12px;
}
.conv-sub {
  font-size: 12px;
  opacity: 0.7;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.conv-meta {
  font-size: 11px;
  opacity: 0.45;
  margin-top: 2px;
}
.chat-meta {
  font-size: 12px;
  opacity: 0.55;
}
.starter {
  margin-bottom: 12px;
}
.starter-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.starter-hint {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 8px;
  line-height: 1.5;
}
.messages {
  min-height: 260px;
  max-height: 52vh;
  overflow-y: auto;
  padding: 4px 2px;
}
.messages.compact {
  min-height: 120px;
}
.msg-list {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.msg {
  display: flex;
}
.msg.user {
  justify-content: flex-end;
}
.bubble-wrap {
  display: flex;
  flex-direction: column;
  gap: 4px;
  max-width: 76%;
}
/* 窄屏气泡放宽：长篇 markdown 回答在 ~300px 容器里再限 76% 太窄 */
@media (max-width: 768px) {
  .bubble-wrap {
    max-width: 92%;
  }
}
.bubble {
  padding: 10px 14px;
  border-radius: 12px;
  font-size: 14px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
}
/* markdown 气泡：块级元素自带间距，关闭 pre-wrap 防双重换行 */
.bubble.md {
  white-space: normal;
}
.bubble.md :deep(p) {
  margin: 0 0 6px;
}
.bubble.md :deep(p:last-child) {
  margin-bottom: 0;
}
.bubble.md :deep(ul),
.bubble.md :deep(ol) {
  margin: 4px 0;
  padding-left: 20px;
}
.bubble.md :deep(li) {
  margin: 2px 0;
}
.bubble.md :deep(h1),
.bubble.md :deep(h2),
.bubble.md :deep(h3),
.bubble.md :deep(h4) {
  font-size: 14px;
  margin: 8px 0 4px;
}
.bubble.md :deep(code) {
  font-family: var(--qv-mono, monospace);
  font-size: 13px;
  padding: 0 4px;
  border-radius: 4px;
  background: v-bind('withAlpha(vars.textColor3, 0.12)');
}
.bubble.md :deep(pre) {
  overflow-x: auto;
  padding: 8px;
  border-radius: 8px;
  background: v-bind('withAlpha(vars.textColor3, 0.1)');
}
.bubble.md :deep(blockquote) {
  margin: 4px 0;
  padding-left: 10px;
  border-left: 3px solid var(--qv-divider);
  opacity: 0.8;
}
.task-code {
  opacity: 0.65;
}
.risk-flags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 10px;
}
.fresh-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 10px;
  font-size: 12px;
  color: v-bind('vars.textColor3');
}
.fresh-dim {
  opacity: 0.75;
}
.check-badge {
  margin-top: 4px;
}
.extra-row {
  display: flex;
  align-items: center;
  gap: 10px;
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
.msg.user .bubble {
  background: v-bind('withAlpha(vars.primaryColor, 0.14)');
}
.msg.assistant .bubble {
  background: v-bind('withAlpha(vars.textColor3, 0.1)');
}
.bubble.thinking {
  opacity: 0.6;
}
.composer {
  display: flex;
  gap: 10px;
  align-items: flex-end;
  margin-top: 14px;
  padding-top: 12px;
  border-top: 1px solid var(--qv-divider);
}
.composer-hint {
  font-size: 11px;
  opacity: 0.45;
  margin-top: 6px;
}
</style>
