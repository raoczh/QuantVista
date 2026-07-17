<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import { listNotes, createNote, updateNote, deleteNote, type ResearchNote, type NoteKind } from '@/api/note'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

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
const keyword = ref('')

async function load() {
  loading.value = true
  try {
    notes.value = await listNotes({
      symbol: filterSymbol.value.trim() || undefined,
      keyword: keyword.value.trim() || undefined,
      limit: 100,
    })
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// ---------- 表单 ----------
const showForm = ref(false)
const saving = ref(false)
const editingId = ref<number | null>(null)
const form = ref<{ symbol: string; kind: NoteKind; title: string; content: string }>({
  symbol: '',
  kind: '',
  title: '',
  content: '',
})
function resetForm() {
  editingId.value = null
  form.value = { symbol: '', kind: '', title: '', content: '' }
}
// 顶部按钮切换：收起时无条件清空编辑态，再点开即为新建，不会残留上次的“保存修改”态。
function toggleForm() {
  if (showForm.value) {
    showForm.value = false
    resetForm()
  } else {
    showForm.value = true
  }
}
function editNote(n: ResearchNote) {
  editingId.value = n.id
  form.value = { symbol: n.symbol, kind: n.kind, title: n.title, content: n.content }
  showForm.value = true
}

async function submit() {
  if (!form.value.title.trim() && !form.value.content.trim()) {
    message.warning('标题与内容至少填一个')
    return
  }
  saving.value = true
  try {
    const data = {
      symbol: form.value.symbol.trim(),
      market: form.value.symbol.trim() ? 'cn' : '',
      kind: form.value.kind,
      title: form.value.title,
      content: form.value.content,
    }
    if (editingId.value) {
      await updateNote(editingId.value, data)
      message.success('笔记已更新')
    } else {
      await createNote(data)
      message.success('笔记已保存')
    }
    showForm.value = false
    resetForm()
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

async function doDelete(n: ResearchNote) {
  try {
    await deleteNote(n.id)
    message.success('已删除')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}

function fmtTime(t: string) {
  return new Date(t).toLocaleString('zh-CN', { hour12: false })
}

onMounted(() => {
  // 深链预填：/notes?add=1&symbol=（个股入口）；/notes?symbol= 直接过滤时间线。
  if (route.query.symbol) {
    if (route.query.add === '1') {
      form.value.symbol = String(route.query.symbol)
      showForm.value = true
    } else {
      filterSymbol.value = String(route.query.symbol)
    }
    router.replace({ query: {} })
  }
  load()
})
</script>

<template>
  <PageContainer title="投资笔记" subtitle="决策日志 · 复盘 · 想法 · 事件——留下「当时为什么这么想」">
    <div class="notes" :style="styleVars">
      <SectionCard title="笔记时间线">
        <template #extra>
          <div class="toolbar">
            <n-input v-model:value="filterSymbol" size="small" placeholder="按代码筛选" style="width: 130px" clearable @keyup.enter="load" @clear="load()" />
            <n-input v-model:value="keyword" size="small" placeholder="搜标题/内容" style="width: 150px" clearable @keyup.enter="load" @clear="load()" />
            <n-button size="small" secondary @click="load">筛选</n-button>
            <n-button size="small" type="primary" @click="toggleForm">
              {{ showForm ? '收起' : '＋ 记一笔' }}
            </n-button>
          </div>
        </template>

        <div v-if="showForm" class="form">
          <div class="form-row">
            <n-input v-model:value="form.symbol" placeholder="关联代码（可选），如 600000" style="max-width: 200px" />
            <n-select v-model:value="form.kind" :options="kindOptions" placeholder="类别" style="max-width: 140px" />
            <n-input v-model:value="form.title" placeholder="标题（可选）" style="flex: 1" />
          </div>
          <n-input v-model:value="form.content" type="textarea" :rows="4" placeholder="正文：当下的判断、依据、情绪、计划……写给未来复盘的自己" />
          <div class="form-actions">
            <n-button type="primary" :loading="saving" @click="submit">{{ editingId ? '保存修改' : '保存笔记' }}</n-button>
          </div>
        </div>

        <n-spin :show="loading">
          <n-empty v-if="!notes.length" description="还没有笔记——研究中的判断与犹豫，都值得记下来" />
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
                  <span v-if="n.symbol" class="note-symbol qv-mono" @click="filterSymbol = n.symbol; load()">
                    {{ n.name || n.symbol }}
                  </span>
                  <span class="note-time">{{ fmtTime(n.created_at) }}</span>
                </div>
                <div class="note-ops">
                  <n-button size="tiny" quaternary @click="editNote(n)">编辑</n-button>
                  <n-popconfirm @positive-click="doDelete(n)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error">删除</n-button>
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
}
.note-symbol {
  font-size: 12.5px;
  font-weight: 600;
  cursor: pointer;
}
.note-symbol:hover {
  text-decoration: underline;
}
.note-time {
  font-size: 12px;
  opacity: 0.55;
}
.note-ops {
  display: flex;
  gap: 2px;
}
.note-title {
  font-weight: 600;
  margin-top: 6px;
  font-size: 13.5px;
}
.note-content {
  margin: 6px 0 0;
  font-size: 13px;
  line-height: 1.7;
  white-space: pre-wrap;
}
</style>
