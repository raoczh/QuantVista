<script setup lang="ts">
import { computed, h, onMounted, reactive, ref } from 'vue'
import {
  NButton,
  NDataTable,
  NInput,
  NModal,
  NSelect,
  NSpin,
  NTag,
  useMessage,
  type DataTableColumns,
} from 'naive-ui'
import { getLlmCall, listLlmCalls, listUsers, type LLMCallLogItem } from '@/api/admin'
import type { AuthUser } from '@/api/auth'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()

/* 模块取值与埋点侧（service/llm_call_log.go 的 Meta.Module）一一对应 */
const moduleLabel: Record<string, string> = {
  analysis: '个股分析',
  analysis_review: '分析复核',
  trade_plan: '交易计划',
  recommendation: '股票推荐',
  rec_review: '推荐复核',
  rec_bear: '反方研究员',
  qa: '个股问答',
  compare: '横向对比',
  daily_report: '收盘日报',
  news: '新闻情绪',
  screener_parse: '白话建策略',
  test: '测试连接',
}
const moduleOptions = [
  { label: '全部模块', value: '' },
  ...Object.entries(moduleLabel).map(([value, label]) => ({ label, value })),
]
const statusOptions = [
  { label: '全部状态', value: '' },
  { label: '成功', value: 'success' },
  { label: '失败', value: 'error' },
]

/* 筛选状态：变更即回第一页重新拉取。trace 输入 trace_id 或 run_id，列出一次业务
 * 运行（主调/repair/复核/反方/交易计划）的全部关联调用（P0-2）。 */
const filters = reactive({ user_id: 0, module: '', status: '', trace: '' })
const users = ref<AuthUser[]>([])
const userOptions = computed(() => [
  { label: '全部用户', value: 0 },
  ...users.value.map((u) => ({ label: u.display_name || u.username, value: u.id })),
])

const rows = ref<LLMCallLogItem[]>([])
const loading = ref(false)
const pagination = reactive({
  page: 1,
  pageSize: 20,
  itemCount: 0,
  pageSizes: [20, 50, 100],
  showSizePicker: true,
  onChange: (page: number) => {
    pagination.page = page
    load()
  },
  onUpdatePageSize: (size: number) => {
    pagination.pageSize = size
    pagination.page = 1
    load()
  },
})

async function load() {
  loading.value = true
  try {
    const res = await listLlmCalls({
      user_id: filters.user_id || undefined,
      module: filters.module || undefined,
      status: filters.status || undefined,
      trace: filters.trace.trim() || undefined,
      page: pagination.page,
      page_size: pagination.pageSize,
    })
    rows.value = res.items
    pagination.itemCount = res.total
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

function onFilterChange() {
  pagination.page = 1
  load()
}

async function loadUsers() {
  try {
    users.value = await listUsers()
  } catch {
    /* 用户列表拉不到时仍可按模块/状态筛 */
  }
}

function fmtTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}
function fmtLatency(ms: number) {
  if (!ms) return '-'
  return ms < 1000 ? `${ms}ms` : `${(ms / 1000).toFixed(1)}s`
}

const columns = computed<DataTableColumns<LLMCallLogItem>>(() => [
  { title: '时间', key: 'created_at', width: 150, render: (row) => h('span', { class: 'cell-time' }, fmtTime(row.created_at)) },
  { title: '用户', key: 'username', width: 110, ellipsis: { tooltip: true }, render: (row) => row.username || `#${row.user_id}` },
  { title: '模块', key: 'module', width: 100, render: (row) => moduleLabel[row.module] || row.module || '-' },
  { title: 'Provider', key: 'provider', width: 90, ellipsis: { tooltip: true }, render: (row) => row.provider || '-' },
  { title: '模型', key: 'model', minWidth: 130, ellipsis: { tooltip: true } },
  {
    title: '端点',
    key: 'endpoint_type',
    width: 110,
    render: (row) =>
      h('span', { class: 'cell-dim' }, [
        row.endpoint_type === 'responses' ? 'responses' : 'chat',
        row.stream ? ' · 流式' : '',
      ]),
  },
  {
    title: 'Token',
    key: 'total_tokens',
    width: 110,
    align: 'right',
    render: (row) =>
      h(
        'span',
        { class: 'qv-tnum', title: `输入 ${row.prompt_tokens} / 输出 ${row.completion_tokens}` },
        row.total_tokens ? row.total_tokens.toLocaleString() : '-',
      ),
  },
  {
    title: '耗时',
    key: 'latency_ms',
    width: 110,
    align: 'right',
    render: (row) =>
      h(
        'span',
        {
          class: 'qv-tnum',
          // 首块耗时（流式首个 data 块到达）：≈总耗时说明上游整包返回（假流式），
          // 是区分「模型生成慢」与「网关不透传流」的关键观测。
          title: row.first_chunk_ms ? `首块 ${fmtLatency(row.first_chunk_ms)} / 总 ${fmtLatency(row.latency_ms)}` : undefined,
        },
        row.first_chunk_ms ? `${fmtLatency(row.first_chunk_ms)} / ${fmtLatency(row.latency_ms)}` : fmtLatency(row.latency_ms),
      ),
  },
  {
    title: '轮次',
    key: 'attempt',
    width: 90,
    render: (row) =>
      row.attempt
        ? h('span', { class: 'cell-dim', title: `run ${row.run_id || '-'}` }, [
            `#${row.attempt}`,
            row.repair ? ' · repair' : '',
          ])
        : h('span', { class: 'cell-dim' }, '-'),
  },
  {
    title: '状态',
    key: 'status',
    width: 80,
    render: (row) =>
      h(
        NTag,
        {
          size: 'small',
          round: true,
          bordered: false,
          type: row.status === 'success' ? 'success' : 'error',
          // finish_state：规范化终态（length/eof_without_marker 等一眼可辨）。
          title: row.finish_state ? `终态 ${row.finish_state}${row.finish_state_raw ? `（原始 ${row.finish_state_raw}）` : ''}` : undefined,
        },
        () => (row.status === 'success' ? '成功' : '失败'),
      ),
  },
])

const rowProps = (row: LLMCallLogItem) => ({
  style: 'cursor: pointer',
  onClick: () => openDetail(row.id),
})

/* 详情弹窗：列表不带正文，点击行再拉全文 */
const detailShow = ref(false)
const detailLoading = ref(false)
const detail = ref<LLMCallLogItem | null>(null)
async function openDetail(id: number) {
  detailShow.value = true
  detailLoading.value = true
  detail.value = null
  try {
    detail.value = await getLlmCall(id)
  } catch (e) {
    message.error((e as Error).message)
    detailShow.value = false
  } finally {
    detailLoading.value = false
  }
}
// 请求体是 messages JSON：能解析就美化缩进，坏体（截断过的）原样展示。
const requestPretty = computed(() => {
  const raw = detail.value?.request_body || ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
})

onMounted(() => {
  load()
  loadUsers()
})
</script>

<template>
  <PageContainer title="LLM 调用记录" subtitle="全用户 AI 调用审计明细，保留 90 天">
    <SectionCard :hoverable="false">
      <div class="filters">
        <n-select v-model:value="filters.user_id" :options="userOptions" filterable class="filter-item" @update:value="onFilterChange" />
        <n-select v-model:value="filters.module" :options="moduleOptions" class="filter-item" @update:value="onFilterChange" />
        <n-select v-model:value="filters.status" :options="statusOptions" class="filter-item filter-status" @update:value="onFilterChange" />
        <n-input
          v-model:value="filters.trace"
          placeholder="trace_id / run_id 追溯"
          size="small"
          clearable
          class="filter-item"
          @keyup.enter="onFilterChange"
          @clear="onFilterChange"
        />
        <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
      </div>
      <n-data-table
        :columns="columns"
        :data="rows"
        :loading="loading"
        :pagination="pagination"
        :row-props="rowProps"
        :scroll-x="920"
        remote
        size="small"
        :bordered="false"
      />
    </SectionCard>

    <n-modal v-model:show="detailShow" preset="card" title="调用详情" class="detail-modal" style="max-width: 760px">
      <n-spin :show="detailLoading">
        <div v-if="detail" class="detail">
          <div class="meta">
            <span>{{ fmtTime(detail.created_at) }}</span>
            <span>· {{ detail.username || `#${detail.user_id}` }}</span>
            <span>· {{ moduleLabel[detail.module] || detail.module }}</span>
            <span>· {{ detail.provider }} / {{ detail.model }}</span>
            <span>· {{ detail.endpoint_type === 'responses' ? 'responses' : 'chat' }}{{ detail.stream ? '（流式）' : '' }}</span>
            <span>· token {{ detail.total_tokens.toLocaleString() }}（输入 {{ detail.prompt_tokens }} / 输出 {{ detail.completion_tokens }}）</span>
            <span>· 耗时 {{ fmtLatency(detail.latency_ms) }}</span>
          </div>
          <!-- P0-2 关联元数据：trace/run/attempt/结构化方法/终态/hash（旧记录为空不渲染） -->
          <div v-if="detail.trace_id" class="meta">
            <span>trace {{ detail.trace_id }}</span>
            <span>· run {{ detail.run_id }}</span>
            <span v-if="detail.parent_run_id">· parent {{ detail.parent_run_id }}</span>
            <span v-if="detail.attempt">· 第 {{ detail.attempt }} 轮{{ detail.repair ? '（repair）' : '' }}</span>
            <span v-if="detail.structured_method">· {{ detail.structured_method }}</span>
            <span v-if="detail.schema_version">· {{ detail.schema_version }}</span>
            <span v-if="detail.prompt_version">· prompt {{ detail.prompt_version }}</span>
            <span v-if="detail.finish_state">· 终态 {{ detail.finish_state }}<template v-if="detail.finish_state_raw">（{{ detail.finish_state_raw }}）</template></span>
          </div>
          <div v-if="detail.prompt_hash || detail.data_hash" class="meta hash-meta">
            <span v-if="detail.prompt_hash">prompt_hash {{ detail.prompt_hash }}</span>
            <span v-if="detail.data_hash">data_hash {{ detail.data_hash }}</span>
          </div>
          <n-tag v-if="detail.status !== 'success'" type="error" size="small" :bordered="false" class="err-tag">
            {{ detail.error_msg || '调用失败' }}
          </n-tag>
          <div class="body-title">请求（messages）</div>
          <pre class="body-pre">{{ requestPretty || '（空）' }}</pre>
          <div class="body-title">响应</div>
          <pre class="body-pre">{{ detail.response_body || '（空）' }}</pre>
        </div>
        <div v-else class="detail-empty">加载中…</div>
      </n-spin>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 10px;
  margin-bottom: 14px;
}
.filter-item {
  width: 180px;
}
.filter-status {
  width: 130px;
}
@media (max-width: 640px) {
  .filter-item,
  .filter-status {
    width: calc(50% - 5px);
  }
}
.cell-time {
  font-size: 12px;
  white-space: nowrap;
}
.cell-dim {
  font-size: 12px;
  opacity: 0.75;
}
.detail {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.detail-empty {
  padding: 40px 0;
  text-align: center;
  opacity: 0.5;
}
.meta {
  display: flex;
  flex-wrap: wrap;
  gap: 4px 6px;
  font-size: 12px;
  opacity: 0.7;
}
.hash-meta span {
  word-break: break-all;
}
.err-tag {
  align-self: flex-start;
  max-width: 100%;
  white-space: normal;
  height: auto;
  padding-top: 2px;
  padding-bottom: 2px;
}
.body-title {
  font-size: 12px;
  font-weight: 600;
  opacity: 0.6;
  margin-top: 6px;
}
.body-pre {
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-word;
  max-height: 32vh;
  overflow: auto;
  margin: 0;
}
</style>
