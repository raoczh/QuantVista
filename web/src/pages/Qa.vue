<script setup lang="ts">
import { ref, onMounted, computed, nextTick } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NSpin,
  NEmpty,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import {
  askQa,
  listConversations,
  getConversation,
  deleteConversation,
  type QaConversation,
  type QaConversationView,
} from '@/api/qa'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { vars, withAlpha } = useUi()
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
  try {
    current.value = await askQa({
      conversation_id: current.value?.id,
      symbol: current.value ? undefined : symbol.value.trim(),
      market: current.value ? undefined : market.value,
      llm_config_id: llmId.value,
      question: q,
    })
    question.value = ''
    await scrollToBottom()
    await loadHistory()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    asking.value = false
  }
}

function newChat() {
  current.value = null
  symbol.value = ''
  question.value = ''
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

onMounted(async () => {
  if (route.query.symbol) {
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
            <span class="chat-meta">{{ current.model }} · {{ current.total_tokens }} tokens</span>
          </template>

          <!-- 新会话：选择标的 -->
          <div v-if="!current" class="starter">
            <div class="starter-row">
              <n-input v-model:value="symbol" placeholder="股票代码，如 600000" style="max-width: 200px" />
              <n-select v-model:value="market" :options="marketOptions" style="width: 110px" />
              <n-select v-model:value="llmId" :options="llmOptions" placeholder="LLM 配置" style="width: 180px" />
            </div>
            <div class="starter-hint">
              首次提问会固定采集一次该股的行情与技术指标快照，之后多轮追问都基于这份快照，不重复拉数据。
            </div>
          </div>

          <!-- 对话流 -->
          <div ref="scrollBox" class="messages" :class="{ compact: !current }">
            <div v-if="current" class="msg-list">
              <div v-for="m in current.messages" :key="m.id" class="msg" :class="m.role">
                <div class="bubble">{{ m.content }}</div>
              </div>
              <div v-if="asking" class="msg assistant">
                <div class="bubble thinking">思考中…</div>
              </div>
            </div>
            <n-empty v-else-if="!current" description="选择标的并提问，开始一段多轮问答" style="padding: 24px 0" />
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
          <div class="composer-hint">仅依据行情与技术指标（无财务/新闻）回答，不构成投资建议。Enter 发送。</div>
        </SectionCard>
      </div>
    </div>
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
.bubble {
  max-width: 76%;
  padding: 10px 14px;
  border-radius: 12px;
  font-size: 14px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
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
