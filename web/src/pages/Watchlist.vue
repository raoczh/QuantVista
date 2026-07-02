<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NModal,
  NForm,
  NFormItem,
  NSelect,
  NSwitch,
  NTag,
  NPopconfirm,
  NEmpty,
  NSpin,
  NText,
  useMessage,
} from 'naive-ui'
import {
  listWatchlists,
  createGroup,
  updateGroup,
  deleteGroup,
  addItem,
  updateItem,
  deleteItem,
  type WatchlistGroup,
  type WatchlistItem,
} from '@/api/watchlist'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const router = useRouter()
const { pctColor, vars } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const groups = ref<WatchlistGroup[]>([])
const loading = ref(false)

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

async function load() {
  loading.value = true
  try {
    groups.value = await listWatchlists()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// ---------- 分组增删改 ----------
const groupModal = ref(false)
const groupForm = ref<{ id: number | null; name: string }>({ id: null, name: '' })

function openCreateGroup() {
  groupForm.value = { id: null, name: '' }
  groupModal.value = true
}
function openRenameGroup(g: WatchlistGroup) {
  groupForm.value = { id: g.id, name: g.name }
  groupModal.value = true
}
const groupSaving = ref(false)
async function submitGroup() {
  if (groupSaving.value) return
  const { id, name } = groupForm.value
  if (!name.trim()) {
    message.warning('请输入分组名称')
    return
  }
  groupSaving.value = true
  try {
    if (id) await updateGroup(id, name.trim())
    else await createGroup(name.trim())
    groupModal.value = false
    await load()
    message.success('已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    groupSaving.value = false
  }
}
async function removeGroup(g: WatchlistGroup) {
  try {
    await deleteGroup(g.id)
    await load()
    message.success('分组已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// ---------- 条目增删改 ----------
const itemModal = ref(false)
const itemEditing = ref(false)
const itemForm = ref<{
  id: number | null
  watchlist_id: number
  symbol: string
  market: string
  focus_reason: string
  note: string
  is_pinned: boolean
}>({ id: null, watchlist_id: 0, symbol: '', market: 'cn', focus_reason: '', note: '', is_pinned: false })

const groupSelectOptions = computed(() =>
  groups.value.map((g) => ({ label: g.name, value: g.id })),
)

function openAddItem(groupId?: number) {
  itemEditing.value = false
  itemForm.value = {
    id: null,
    watchlist_id: groupId || groups.value[0]?.id || 0,
    symbol: '',
    market: 'cn',
    focus_reason: '',
    note: '',
    is_pinned: false,
  }
  itemModal.value = true
}
function openEditItem(item: WatchlistItem) {
  itemEditing.value = true
  itemForm.value = {
    id: item.id,
    watchlist_id: item.watchlist_id,
    symbol: item.symbol,
    market: item.market,
    focus_reason: item.focus_reason,
    note: item.note,
    is_pinned: item.is_pinned,
  }
  itemModal.value = true
}
const itemSaving = ref(false)
async function submitItem() {
  if (itemSaving.value) return
  const f = itemForm.value
  if (!itemEditing.value && !f.symbol.trim()) {
    message.warning('请输入股票代码')
    return
  }
  itemSaving.value = true
  try {
    if (itemEditing.value && f.id) {
      await updateItem(f.id, {
        watchlist_id: f.watchlist_id,
        focus_reason: f.focus_reason,
        note: f.note,
        is_pinned: f.is_pinned,
      })
    } else {
      await addItem(f.watchlist_id, {
        symbol: f.symbol.trim(),
        market: f.market,
        focus_reason: f.focus_reason,
        note: f.note,
        is_pinned: f.is_pinned,
      })
    }
    itemModal.value = false
    await load()
    message.success('已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    itemSaving.value = false
  }
}
async function togglePin(item: WatchlistItem) {
  try {
    await updateItem(item.id, {
      focus_reason: item.focus_reason,
      note: item.note,
      is_pinned: !item.is_pinned,
    })
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function removeItem(item: WatchlistItem) {
  try {
    await deleteItem(item.id)
    await load()
    message.success('已移除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// 从自选一键建仓：跳转持仓页并预填。
function buildPosition(item: WatchlistItem) {
  router.push({
    name: 'positions',
    query: { add: '1', symbol: item.symbol, market: item.market, name: item.name },
  })
}
// 快捷入口：分析/提醒/问答页均已支持 query 预填（PRD 3.3/3.16/3.17 的跳转交互）。
function goAnalysis(item: WatchlistItem) {
  router.push({ name: 'analysis', query: { module: 'stock', symbol: item.symbol, market: item.market } })
}
function goAlert(item: WatchlistItem) {
  router.push({ name: 'alerts', query: { add: '1', symbol: item.symbol, market: item.market, name: item.name } })
}
function goQa(item: WatchlistItem) {
  router.push({ name: 'qa', query: { symbol: item.symbol, market: item.market } })
}

function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}

const totalCount = computed(() => groups.value.reduce((s, g) => s + g.items.length, 0))

onMounted(load)
</script>

<template>
  <PageContainer title="自选股" subtitle="分组管理 · 重点关注 · 实时行情">
    <template #actions>
      <n-tag size="small" round :bordered="false">{{ totalCount }} 只</n-tag>
      <n-button size="small" secondary @click="openCreateGroup">新建分组</n-button>
      <n-button size="small" type="primary" @click="openAddItem()">+ 添加自选</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <n-spin :show="loading && !groups.length">
      <div class="wl" :style="styleVars">
        <SectionCard v-for="g in groups" :key="g.id" :title="g.name">
          <template #extra>
            <div class="group-actions">
              <n-tag size="tiny" round :bordered="false">{{ g.items.length }}</n-tag>
              <n-button size="tiny" quaternary @click="openAddItem(g.id)">添加</n-button>
              <n-button size="tiny" quaternary @click="openRenameGroup(g)">重命名</n-button>
              <n-popconfirm @positive-click="removeGroup(g)">
                <template #trigger>
                  <n-button size="tiny" quaternary type="error">删除</n-button>
                </template>
                删除分组「{{ g.name }}」及其下 {{ g.items.length }} 只自选？
              </n-popconfirm>
            </div>
          </template>

          <n-empty v-if="!g.items.length" description="该分组暂无自选，点击「添加」加入股票" />
          <div v-else class="items">
            <div v-for="it in g.items" :key="it.id" class="item">
              <div class="it-main">
                <div class="it-name">
                  <n-tag v-if="it.is_pinned" size="tiny" type="warning" round :bordered="false"
                    >重点</n-tag
                  >
                  <span class="it-title">{{ it.name || it.symbol }}</span>
                  <span class="it-symbol qv-mono">{{ it.symbol }}</span>
                </div>
                <div v-if="it.focus_reason || it.note" class="it-note">
                  <span v-if="it.focus_reason">关注：{{ it.focus_reason }}</span>
                  <span v-if="it.note" class="it-memo">· {{ it.note }}</span>
                </div>
              </div>
              <div class="it-quote">
                <span v-if="it.quote_ok" class="it-price qv-tnum" :style="{ color: pctColor(it.change_pct) }">
                  {{ fmt(it.price) }}
                </span>
                <span v-else class="it-price muted">—</span>
                <ChangeTag v-if="it.quote_ok" :value="it.change_pct" size="small" />
              </div>
              <div class="it-actions">
                <n-button size="tiny" quaternary @click="togglePin(it)">{{
                  it.is_pinned ? '取消重点' : '重点'
                }}</n-button>
                <n-button size="tiny" quaternary @click="goAnalysis(it)">分析</n-button>
                <n-button size="tiny" quaternary @click="goAlert(it)">提醒</n-button>
                <n-button size="tiny" quaternary @click="goQa(it)">问答</n-button>
                <n-button size="tiny" quaternary @click="buildPosition(it)">建仓</n-button>
                <n-button size="tiny" quaternary @click="openEditItem(it)">编辑</n-button>
                <n-popconfirm @positive-click="removeItem(it)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error">移除</n-button>
                  </template>
                  从自选中移除「{{ it.name || it.symbol }}」？
                </n-popconfirm>
              </div>
            </div>
          </div>
        </SectionCard>
      </div>
    </n-spin>

    <!-- 分组新建/重命名 -->
    <n-modal
      v-model:show="groupModal"
      preset="card"
      :title="groupForm.id ? '重命名分组' : '新建分组'"
      style="max-width: 420px"
    >
      <n-form @submit.prevent="submitGroup">
        <n-form-item label="分组名称">
          <n-input v-model:value="groupForm.name" placeholder="如：核心持仓 / 观察池" maxlength="32" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="groupModal = false">取消</n-button>
          <n-button type="primary" :loading="groupSaving" @click="submitGroup">保存</n-button>
        </div>
      </template>
    </n-modal>

    <!-- 条目添加/编辑 -->
    <n-modal
      v-model:show="itemModal"
      preset="card"
      :title="itemEditing ? '编辑自选' : '添加自选'"
      style="max-width: 460px"
    >
      <n-form label-placement="top">
        <n-form-item label="所属分组">
          <n-select v-model:value="itemForm.watchlist_id" :options="groupSelectOptions" />
        </n-form-item>
        <n-form-item v-if="!itemEditing" label="股票代码">
          <n-input v-model:value="itemForm.symbol" placeholder="如 600000" />
        </n-form-item>
        <n-form-item v-if="!itemEditing" label="市场">
          <n-select v-model:value="itemForm.market" :options="marketOptions" />
        </n-form-item>
        <n-form-item label="关注原因">
          <n-input
            v-model:value="itemForm.focus_reason"
            type="textarea"
            :autosize="{ minRows: 2, maxRows: 4 }"
            placeholder="为什么关注它（可选）"
            maxlength="512"
          />
        </n-form-item>
        <n-form-item label="备注">
          <n-input v-model:value="itemForm.note" placeholder="补充备注（可选）" maxlength="512" />
        </n-form-item>
        <n-form-item label="重点关注">
          <n-switch v-model:value="itemForm.is_pinned" />
        </n-form-item>
      </n-form>
      <template #footer>
        <div class="modal-footer">
          <n-button @click="itemModal = false">取消</n-button>
          <n-button type="primary" :loading="itemSaving" @click="submitItem">保存</n-button>
        </div>
      </template>
    </n-modal>

    <n-text v-if="!loading && !groups.length" depth="3">暂无数据</n-text>
  </PageContainer>
</template>

<style scoped>
.wl {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.group-actions {
  display: flex;
  align-items: center;
  gap: 6px;
}
.items {
  display: flex;
  flex-direction: column;
}
.item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.item:last-child {
  border-bottom: none;
}
.it-main {
  flex: 1;
  min-width: 0;
}
.it-name {
  display: flex;
  align-items: center;
  gap: 8px;
}
.it-title {
  font-size: 14px;
  font-weight: 500;
}
.it-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.it-note {
  font-size: 12px;
  opacity: 0.6;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.it-memo {
  opacity: 0.7;
}
.it-quote {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 120px;
  justify-content: flex-end;
}
.it-price {
  font-size: 15px;
  font-weight: 600;
}
.it-price.muted {
  opacity: 0.4;
}
.it-actions {
  display: flex;
  align-items: center;
  gap: 2px;
  flex-shrink: 0;
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
</style>
