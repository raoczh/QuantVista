<script setup lang="ts">
import { ref, onMounted, computed, nextTick, onBeforeUnmount } from 'vue'
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
  useMessage,
} from 'naive-ui'
import {
  askQaStream,
  listConversations,
  getConversation,
  deleteConversation,
  getQaSnapshot,
  type QaConversation,
  type QaConversationView,
  type QaMessage,
} from '@/api/qa'
import type { EvidenceCheck, RiskFlag } from '@/api/trust'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { renderMarkdown } from '@/lib/markdown'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { upColor, vars, withAlpha } = useUi()
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
// 从分析结果「继续问答」进入：新会话复用该分析记录的数据快照（不重复拉数）。
const fromAnalysisId = ref<number | null>(null)
const fromAnalysisName = ref('')

// ---------- 流式渲染（S1）----------
// 增量先进 streamRaw 缓冲，100ms 节流转 markdown（renderMarkdown 已 DOMPurify 消毒），
// 避免每个 token 都重排。核验徽章在 done 行替换 current 后自然后置出现。
const pendingQuestion = ref('')
const streamHtml = ref('')
let streamRaw = ''
let renderTimer: number | null = null

function flushStreamRender() {
  renderTimer = null
  streamHtml.value = renderMarkdown(streamRaw)
  void scrollToBottom()
}
function onStreamChunk(text: string) {
  streamRaw += text
  if (renderTimer === null) {
    renderTimer = window.setTimeout(flushStreamRender, 100)
  }
}
function resetStream() {
  if (renderTimer !== null) {
    window.clearTimeout(renderTimer)
    renderTimer = null
  }
  streamRaw = ''
  streamHtml.value = ''
  pendingQuestion.value = ''
}
onBeforeUnmount(() => {
  if (renderTimer !== null) window.clearTimeout(renderTimer)
})

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

async function send() {
  if (asking.value) return // 回车/连击防抖：请求在途时不重复提交
  const q = question.value.trim()
  if (!q) return
  if (!current.value && !symbol.value.trim()) {
    message.warning('请输入要提问的股票代码')
    return
  }
  if (!llmConfigs.value.length) {
    message.warning('请先在「设置」中添加并测试 LLM 配置')
    return
  }
  asking.value = true
  pendingQuestion.value = q
  streamRaw = ''
  streamHtml.value = ''
  await scrollToBottom()
  try {
    current.value = await askQaStream(
      {
        conversation_id: current.value?.id,
        symbol: current.value ? undefined : symbol.value.trim(),
        market: current.value ? undefined : market.value,
        llm_config_id: llmId.value,
        question: q,
        analysis_record_id: !current.value && fromAnalysisId.value ? fromAnalysisId.value : undefined,
      },
      onStreamChunk,
    )
    question.value = ''
    fromAnalysisId.value = null
    fromAnalysisName.value = ''
    await scrollToBottom()
    await loadHistory()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    asking.value = false
    resetStream()
  }
}

function newChat() {
  current.value = null
  symbol.value = ''
  question.value = ''
  fromAnalysisId.value = null
  fromAnalysisName.value = ''
}

// ---------- 风险闸门标签（S1）----------
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
async function openConv(c: QaConversation) {
  try {
    current.value = await getConversation(c.id)
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
function checkColor(c: EvidenceCheck) {
  return c.matched === c.total ? upColor.value : vars.value.warningColor
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

onMounted(async () => {
  if (route.query.from_analysis) {
    const id = Number(route.query.from_analysis)
    if (Number.isFinite(id) && id > 0) {
      fromAnalysisId.value = id
      fromAnalysisName.value = String(route.query.name || route.query.symbol || '')
    }
    symbol.value = String(route.query.symbol || '')
    market.value = String(route.query.market || 'cn')
    router.replace({ name: 'qa' })
  } else if (route.query.symbol) {
    symbol.value = String(route.query.symbol)
    market.value = String(route.query.market || 'cn')
    router.replace({ name: 'qa' })
  }
  await Promise.all([loadLLM(), loadHistory()])
})
</script>

<template>
  <PageContainer title="个股 AI 问答" subtitle="固定一份数据快照多轮追问 · 仅依据行情与技术指标 · 研究参考">
    <div class="qa" :style="styleVars">
      <!-- 左：历史会话 -->
      <div class="col-side">
        <SectionCard title="会话">
          <template #extra>
            <n-button size="tiny" quaternary :loading="historyLoading" @click="loadHistory">刷新</n-button>
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
                <n-popconfirm @positive-click="removeConv(c)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error" @click.stop>删</n-button>
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
              <n-button size="tiny" quaternary @click="openSnapshot">数据快照</n-button>
              <span class="chat-meta">{{ llmLabel(current) || current.model }} · {{ current.total_tokens }} tokens</span>
            </div>
          </template>

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
              <n-select v-model:value="llmId" :options="llmOptions" placeholder="LLM 配置" style="width: 180px" />
            </div>
            <n-alert v-if="fromAnalysisId" type="info" :bordered="false" style="margin-top: 10px">
              将基于「{{ fromAnalysisName || symbol }}」分析记录 #{{ fromAnalysisId }}
              的数据快照提问——问答所见与分析所见完全一致，可直接追问分析结论。
            </n-alert>
            <div v-else class="starter-hint">
              首次提问会固定采集一次该股的行情与技术指标快照，之后多轮追问都基于这份快照，不重复拉数据。
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
                    <n-tooltip v-if="m.role === 'assistant' && msgCheck(m)" trigger="hover">
                      <template #trigger>
                        <span
                          class="check-chip"
                          :style="{ background: withAlpha(checkColor(msgCheck(m)!), 0.12), color: checkColor(msgCheck(m)!) }"
                        >
                          数据核验 {{ msgCheck(m)!.matched }}/{{ msgCheck(m)!.total }}
                        </span>
                      </template>
                      <span v-if="msgCheck(m)!.unmatched?.length">
                        这些数字未能与数据快照吻合，可能是推算值或幻觉，建议人工核对：{{ msgCheck(m)!.unmatched!.join('、') }}
                      </span>
                      <span v-else>回答引用的数字已逐一与数据快照程序化比对，全部吻合</span>
                    </n-tooltip>
                  </div>
                </div>
              </template>
              <!-- 流式中的本轮问答（done 后被 current 的正式消息替换，核验徽章随之后置出现） -->
              <template v-if="asking">
                <div class="msg user">
                  <div class="bubble-wrap"><div class="bubble">{{ pendingQuestion }}</div></div>
                </div>
                <div class="msg assistant">
                  <div class="bubble-wrap">
                    <div v-if="streamHtml" class="bubble md streaming" v-html="streamHtml"></div>
                    <div v-else class="bubble thinking">思考中…</div>
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
              @keydown.enter.exact.prevent="send"
            />
            <n-button type="primary" :loading="asking" @click="send">发送</n-button>
          </div>
          <div class="composer-hint">仅依据行情/技术指标与已采集的新闻公告快照回答（流式输出），不构成投资建议。Enter 发送。</div>
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
/* 流式中的气泡尾部光标 */
.bubble.streaming::after {
  content: '▍';
  opacity: 0.5;
  animation: qv-blink 1s step-start infinite;
}
@keyframes qv-blink {
  50% {
    opacity: 0;
  }
}
.risk-flags {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-bottom: 10px;
}
.check-chip {
  align-self: flex-start;
  font-size: 12px;
  font-weight: 600;
  padding: 1px 8px;
  border-radius: 12px;
  cursor: default;
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
