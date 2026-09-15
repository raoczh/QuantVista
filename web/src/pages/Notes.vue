<script setup lang="ts">
import { ref, onMounted, onUnmounted, computed, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NAlert,
  useMessage,
} from 'naive-ui'
import { listNotes, createNote, updateNote, deleteNote, type ResearchNote, type NoteKind } from '@/api/note'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StockPicker from '@/components/StockPicker.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import type { StockRef } from '@/composables/useStockActions'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { vars, withAlpha, upColor, flatColor } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const kindOptions = [
  { label: '不分类', value: '' },
  { label: '决策记录', value: 'decision' },
  { label: '复盘笔记', value: 'review' },
  { label: '想法', value: 'idea' },
  { label: '事件记录', value: 'event' },
]
const kindMeta = computed(() => {
  const map: Record<string, { label: string; color: string }> = {
    decision: { label: '决策', color: upColor.value },
    review: { label: '复盘', color: vars.value.warningColor },
    idea: { label: '想法', color: vars.value.infoColor },
    event: { label: '事件', color: flatColor.value },
  }
  return map
})

// ---------- 列表 ----------
const notes = ref<ResearchNote[]>([])
const loading = ref(false)
const filterSymbol = ref('')
const filterStock = ref<StockRef | null>(null)
const keyword = ref('')
const loadError = ref('')
let loadSeq = 0
let disposed = false

async function load() {
  const seq = ++loadSeq
  loading.value = true
  loadError.value = ''
  notes.value = []
  try {
    const rows = await listNotes({
      symbol: filterSymbol.value.trim() || undefined,
      market: filterStock.value?.market || undefined,
      keyword: keyword.value.trim() || undefined,
      limit: 100,
    })
    if (!disposed && seq === loadSeq) notes.value = rows
  } catch (e) {
    if (!disposed && seq === loadSeq) loadError.value = (e as Error).message
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}

// ---------- 表单 ----------
const showForm = ref(false)
const saving = ref(false)
const editingId = ref<number | null>(null)
const form = ref<{ symbol: string; market: string; kind: NoteKind; title: string; content: string }>({
  symbol: '',
  market: 'cn',
  kind: '',
  title: '',
  content: '',
})
const noteStock = ref<StockRef | null>(null)
function updateNoteStock(stock: StockRef | null) {
  noteStock.value = stock
  form.value.symbol = stock?.symbol || ''
  form.value.market = stock?.market || 'cn'
}
function updateFilterStock(stock: StockRef | null) {
  filterStock.value = stock
  filterSymbol.value = stock?.symbol || ''
  void load()
}
function resetForm() {
  editingId.value = null
  form.value = { symbol: '', market: 'cn', kind: '', title: '', content: '' }
  noteStock.value = null
}
// 顶部按钮切换：收起时无条件清空编辑态，再点开即为新建，不会残留上次的“保存修改”态。
function toggleForm() {
  if (saving.value) return
  if (showForm.value) {
    showForm.value = false
    resetForm()
  } else {
    showForm.value = true
  }
}
function editNote(n: ResearchNote) {
  if (saving.value || deleting.value.has(n.id)) return
  editingId.value = n.id
  form.value = { symbol: n.symbol, market: n.market || 'cn', kind: n.kind, title: n.title, content: n.content }
  noteStock.value = n.symbol ? { symbol: n.symbol, market: n.market || 'cn', name: n.name || '' } : null
  showForm.value = true
}

async function submit() {
  if (saving.value) return
  if (!form.value.title.trim() && !form.value.content.trim()) {
    message.warning('标题与内容至少填一个')
    return
  }
  saving.value = true
  try {
    const data = {
      symbol: form.value.symbol.trim(),
      market: form.value.symbol.trim() ? form.value.market : '',
      kind: form.value.kind,
      title: form.value.title,
      content: form.value.content,
    }
    if (editingId.value) {
      await updateNote(editingId.value, data)
      if (disposed) return
      message.success('笔记已更新')
    } else {
      await createNote(data)
      if (disposed) return
      message.success('笔记已保存')
    }
    showForm.value = false
    resetForm()
    await load()
  } catch (e) {
    if (!disposed) message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

const deleting = ref(new Set<number>())
async function doDelete(n: ResearchNote) {
  if (saving.value || deleting.value.has(n.id)) return
  deleting.value.add(n.id)
  try {
    await deleteNote(n.id)
    if (disposed) return
    if (editingId.value === n.id) {
      showForm.value = false
      resetForm()
    }
    message.success('已删除')
    await load()
  } catch (e) {
    if (!disposed) message.error((e as Error).message)
  } finally {
    deleting.value.delete(n.id)
  }
}

function fmtTime(t: string) {
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}

function applyStockActionQuery() {
  if (saving.value) return false
  // 深链预填：/notes?add=1&symbol=（个股入口）；/notes?symbol= 直接过滤时间线。
  if (route.query.symbol) {
    const filterOnly = route.query.add !== '1'
    if (route.query.add === '1') {
      resetForm()
      updateNoteStock({
        symbol: String(route.query.symbol),
        market: String(route.query.market || 'cn'),
        name: String(route.query.name || ''),
      })
      showForm.value = true
    } else {
      updateFilterStock({
        symbol: String(route.query.symbol),
        market: String(route.query.market || 'cn'),
        name: String(route.query.name || ''),
      })
    }
    const query = { ...route.query }
    for (const key of ['symbol', 'market', 'name', 'add', '_stock_action']) delete query[key]
    void router.replace({ query })
    return filterOnly
  }
  return false
}

watch(() => [route.query._stock_action, route.query.symbol, route.query.market, route.query.add, saving.value], applyStockActionQuery)

onMounted(() => {
  if (!applyStockActionQuery()) void load()
})
onUnmounted(() => { disposed = true; loadSeq++ })
</script>

<template>
  <PageContainer title="投资笔记" subtitle="留下决策理由、观察和复盘记录，让后续判断有迹可循。">
    <div class="notes" :style="styleVars">
      <SectionCard title="笔记时间线">
        <template #extra>
          <div class="toolbar">
            <StockPicker :model-value="filterStock" class="note-filter-picker" placeholder="按股票筛选" @update:model-value="updateFilterStock" />
            <n-input v-model:value="keyword" size="small" placeholder="搜标题/内容" style="width: 150px" clearable @keyup.enter="load" @clear="load()" />
            <n-button size="small" secondary @click="load">筛选</n-button>
            <n-button size="small" type="primary" :disabled="saving" @click="toggleForm">
              {{ showForm ? '收起' : '＋ 记一笔' }}
            </n-button>
          </div>
        </template>

        <div v-if="showForm" class="form">
          <div class="form-row">
            <StockPicker :model-value="noteStock" :disabled="saving" class="note-stock-picker" placeholder="关联股票（可选）" @update:model-value="updateNoteStock" />
            <n-select v-model:value="form.kind" :disabled="saving" :options="kindOptions" placeholder="类别" style="max-width: 140px" />
            <n-input v-model:value="form.title" :disabled="saving" :maxlength="128" placeholder="标题（可选）" style="flex: 1" />
          </div>
          <n-input v-model:value="form.content" :disabled="saving" type="textarea" :rows="4" placeholder="正文：当下的判断、依据、情绪、计划……写给未来复盘的自己" />
          <div class="form-actions">
            <n-button type="primary" :loading="saving" @click="submit">{{ editingId ? '保存修改' : '保存笔记' }}</n-button>
          </div>
        </div>

        <n-spin :show="loading">
          <n-alert v-if="loadError" type="warning">{{ loadError }}</n-alert>
          <div v-else-if="loading && !notes.length" aria-live="polite">正在加载笔记…</div>
          <n-empty v-else-if="!loading && !notes.length" description="当前筛选下没有笔记" />
          <div v-else class="timeline">
            <div v-for="n in notes" :key="n.id" class="note">
              <div class="note-head">
                <div class="note-meta">
                  <n-tag
                    v-if="n.kind && kindMeta[n.kind]"
                    size="tiny"
                    :bordered="false"
                    round
                    :color="{ color: withAlpha(kindMeta[n.kind].color, 0.14), textColor: kindMeta[n.kind].color }"
                  >
                    {{ kindMeta[n.kind].label }}
                  </n-tag>
                  <StockIdentity v-if="n.symbol" :symbol="n.symbol" :market="n.market" :name="n.name" density="table" clickable />
                  <span class="note-time">{{ fmtTime(n.created_at) }}</span>
                </div>
                <div class="note-ops">
                  <n-button size="tiny" quaternary :disabled="saving || deleting.has(n.id)" @click="editNote(n)">编辑</n-button>
                  <n-popconfirm @positive-click="doDelete(n)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error" :disabled="saving || deleting.has(n.id)">删除</n-button>
                    </template>
                    确认删除这条笔记？
                  </n-popconfirm>
                </div>
              </div>
              <div v-if="n.title" class="note-title">{{ n.title }}</div>
              <p v-if="n.content" class="note-content">{{ n.content }}</p>
            </div>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.notes {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.toolbar {
  display: flex;
  gap: 8px;
  align-items: center;
  flex-wrap: wrap;
  justify-content: flex-end;
}
/* StockPicker 内部是 n-select（默认 width:100%），在 flex 工具栏/表单行里必须显式给宽，
 * 否则筛选器会撑满整行把按钮挤下去。 */
.note-filter-picker {
  flex: 0 1 190px;
  min-width: 0;
}
.note-stock-picker {
  flex: 1 1 200px;
  min-width: 0;
  max-width: 260px;
}
.form {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-bottom: 14px;
  margin-bottom: 14px;
  border-bottom: 1px solid var(--qv-divider);
}
.form-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
}
.timeline {
  display: flex;
  flex-direction: column;
}
.note {
  padding: 14px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.note:last-child {
  border-bottom: none;
}
.note-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.note-meta {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
  min-width: 0;
}
.note-time {
  font-size: 12px;
  opacity: 0.55;
}
.note-ops {
  display: flex;
  gap: 2px;
  flex-shrink: 0;
}
.note-title {
  font-weight: 600;
  margin-top: 6px;
  font-size: 13.5px;
  overflow-wrap: anywhere;
}
.note-content {
  margin: 6px 0 0;
  font-size: 13px;
  line-height: 1.7;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
@media (max-width: 768px) {
  .form-row :deep(.n-select),
  .form-row :deep(.n-input) {
    min-width: 0;
  }
  .note-stock-picker {
    flex-basis: 100%;
    max-width: none;
  }
  .note-ops {
    margin-left: auto;
  }
}
</style>
