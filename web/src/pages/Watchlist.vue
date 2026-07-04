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
  NPopselect,
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
  setItemStage,
  listMissed,
  type WatchlistGroup,
  type WatchlistItem,
  type ResearchStage,
  type MissedOpportunity,
} from '@/api/watchlist'
import { getDailyBars, type Bar } from '@/api/market'
import { useUi } from '@/composables/useUi'
import { useAutoRefresh } from '@/composables/useAutoRefresh'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const router = useRouter()
const { pctColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const groups = ref<WatchlistGroup[]>([])
const loading = ref(false)

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]

async function load(silent = false) {
  if (!silent) loading.value = true
  try {
    groups.value = await listWatchlists()
  } catch (e) {
    if (!silent) message.error((e as Error).message)
  } finally {
    if (!silent) loading.value = false
  }
}

// 盘中自动刷新行情（60s，仅交易时段+页面可见，静默）。
useAutoRefresh(() => load(true), 60_000)

// ---------- 机会池漏斗 ----------
const stageMeta: Record<string, { label: string; color: string }> = {
  discovered: { label: '发现', color: '#8a8a8a' },
  screening: { label: '初筛', color: '#5b8def' },
  watching: { label: '观察', color: '#3f9eff' },
  waiting_price: { label: '等价格', color: '#f0a020' },
  planned: { label: '有计划', color: '#9a5fe0' },
  bought: { label: '已买入', color: '#d03050' },
  passed: { label: '已放弃', color: '#7a7a7a' },
  reviewed: { label: '已复盘', color: '#18a058' },
}
const stageOptions = [
  { label: '清除标注', value: '' },
  ...Object.entries(stageMeta).map(([value, m]) => ({ label: m.label, value })),
]
// 转「已放弃」时先收集原因。
const passModal = ref(false)
const passReason = ref('')
const passTarget = ref<WatchlistItem | null>(null)

async function onStageSelect(it: WatchlistItem, stage: ResearchStage) {
  if (stage === 'passed') {
    passTarget.value = it
    passReason.value = ''
    passModal.value = true
    return
  }
  try {
    await setItemStage(it.id, stage)
    await load(true)
    message.success('研究阶段已更新')
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function confirmPass() {
  if (!passTarget.value) return
  try {
    await setItemStage(passTarget.value.id, 'passed', passReason.value)
    passModal.value = false
    await load(true)
    message.success('已标记放弃，进入「错过机会」复盘池')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// ---------- 错过机会复盘 ----------
const missedModal = ref(false)
const missedRows = ref<MissedOpportunity[]>([])
const missedLoading = ref(false)
const verdictMeta: Record<string, { label: string; type: 'success' | 'error' | 'default' | 'warning' }> = {
  avoided_loss: { label: '回避正确', type: 'success' },
  missed_gain: { label: '错过上涨', type: 'error' },
  neutral: { label: '基本持平', type: 'default' },
  no_base: { label: '无基准价', type: 'warning' },
}
async function openMissed() {
  missedModal.value = true
  missedLoading.value = true
  try {
    missedRows.value = await listMissed()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    missedLoading.value = false
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
function goThesis(item: WatchlistItem) {
  router.push({ name: 'thesis', query: { add: '1', symbol: item.symbol, market: item.market, name: item.name } })
}

function fmt(n: number | undefined) {
  return n == null ? '-' : n.toFixed(2)
}

// ---------- 行展开迷你走势：按需加载单只近 60 日日线，避开数据源限流 ----------
const expanded = ref<Record<number, boolean>>({})
const sparkBars = ref<Record<number, Bar[]>>({})
const sparkLoading = ref<Record<number, boolean>>({})

async function toggleExpand(it: WatchlistItem) {
  if (expanded.value[it.id]) {
    expanded.value[it.id] = false
    return
  }
  expanded.value[it.id] = true
  if (sparkBars.value[it.id]?.length) return
  sparkLoading.value[it.id] = true
  try {
    sparkBars.value[it.id] = await getDailyBars(it.market, it.symbol, 60)
  } catch (e) {
    expanded.value[it.id] = false
    message.error((e as Error).message)
  } finally {
    sparkLoading.value[it.id] = false
  }
}

// 近 60 日收盘价 → SVG 路径（viewBox 560x72，preserveAspectRatio=none 拉伸自适应）
const SPARK_W = 560
const SPARK_H = 72
function sparkPaths(bars: Bar[]) {
  const closes = bars.map((b) => b.close)
  if (closes.length < 2) return null
  const min = Math.min(...closes)
  const max = Math.max(...closes)
  const span = max - min || 1
  const stepX = SPARK_W / (closes.length - 1)
  const pts = closes.map(
    (c, i) => `${(i * stepX).toFixed(1)},${(SPARK_H - 5 - ((c - min) / span) * (SPARK_H - 10)).toFixed(1)}`,
  )
  const line = 'M' + pts.join(' L')
  return { line, area: `${line} L${SPARK_W},${SPARK_H} L0,${SPARK_H} Z` }
}
function sparkStats(bars: Bar[]) {
  const first = bars[0]
  const last = bars[bars.length - 1]
  const chg = first?.close ? ((last.close - first.close) / first.close) * 100 : 0
  return {
    chg,
    high: Math.max(...bars.map((b) => b.high)),
    low: Math.min(...bars.map((b) => b.low)),
  }
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
      <n-button size="small" secondary @click="openMissed">错过机会</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load()">刷新</n-button>
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
            <div v-for="it in g.items" :key="it.id" class="item-wrap">
              <div class="item" @click="toggleExpand(it)">
                <span class="it-chevron" :class="{ 'is-open': expanded[it.id] }">▸</span>
                <div class="it-main">
                  <div class="it-name">
                    <n-tag v-if="it.is_pinned" size="tiny" type="warning" round :bordered="false"
                      >重点</n-tag
                    >
                    <span class="it-title">{{ it.name || it.symbol }}</span>
                    <span class="it-symbol qv-mono">{{ it.symbol }}</span>
                    <n-popselect
                      :value="it.research_stage"
                      :options="stageOptions"
                      trigger="click"
                      @update:value="(v: ResearchStage) => onStageSelect(it, v)"
                    >
                      <n-tag
                        size="tiny"
                        round
                        :bordered="false"
                        style="cursor: pointer"
                        :color="it.research_stage && stageMeta[it.research_stage]
                          ? { color: withAlpha(stageMeta[it.research_stage].color, 0.15), textColor: stageMeta[it.research_stage].color }
                          : undefined"
                        @click.stop
                      >
                        {{ it.research_stage && stageMeta[it.research_stage] ? stageMeta[it.research_stage].label : '标阶段' }}
                      </n-tag>
                    </n-popselect>
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
                <div class="it-actions" @click.stop>
                  <n-button size="tiny" quaternary @click="togglePin(it)">{{
                    it.is_pinned ? '取消重点' : '重点'
                  }}</n-button>
                  <n-button size="tiny" quaternary @click="goAnalysis(it)">分析</n-button>
                  <n-button size="tiny" quaternary @click="goAlert(it)">提醒</n-button>
                  <n-button size="tiny" quaternary @click="goQa(it)">问答</n-button>
                  <n-button size="tiny" quaternary @click="goThesis(it)">逻辑卡</n-button>
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

              <!-- 展开：近 60 日迷你走势（按需加载） -->
              <div v-if="expanded[it.id]" class="item-spark">
                <n-spin :show="!!sparkLoading[it.id]" size="small">
                  <template v-if="sparkBars[it.id]?.length">
                    <svg
                      class="spark-svg"
                      :viewBox="`0 0 ${SPARK_W} ${SPARK_H}`"
                      preserveAspectRatio="none"
                      aria-hidden="true"
                    >
                      <path
                        :d="sparkPaths(sparkBars[it.id])!.area"
                        :fill="withAlpha(pctColor(sparkStats(sparkBars[it.id]).chg), 0.12)"
                      />
                      <path
                        :d="sparkPaths(sparkBars[it.id])!.line"
                        fill="none"
                        :stroke="pctColor(sparkStats(sparkBars[it.id]).chg)"
                        stroke-width="1.5"
                        vector-effect="non-scaling-stroke"
                      />
                    </svg>
                    <div class="spark-meta qv-tnum">
                      <span>近 {{ sparkBars[it.id].length }} 个交易日</span>
                      <span :style="{ color: pctColor(sparkStats(sparkBars[it.id]).chg) }">
                        区间 {{ sparkStats(sparkBars[it.id]).chg >= 0 ? '+' : '' }}{{ sparkStats(sparkBars[it.id]).chg.toFixed(2) }}%
                      </span>
                      <span>最高 {{ fmt(sparkStats(sparkBars[it.id]).high) }}</span>
                      <span>最低 {{ fmt(sparkStats(sparkBars[it.id]).low) }}</span>
                    </div>
                  </template>
                  <div v-else class="spark-placeholder" />
                </n-spin>
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

    <!-- 标记放弃：记录原因与当时价格 -->
    <n-modal v-model:show="passModal" preset="card" title="标记放弃" style="max-width: 460px">
      <p class="pass-hint">
        将「{{ passTarget?.name || passTarget?.symbol }}」标记为已放弃。系统会记录当前价格，
        之后可在「错过机会」中复盘：是正确回避了风险，还是错过了机会。
      </p>
      <n-input
        v-model:value="passReason"
        type="textarea"
        :rows="2"
        placeholder="放弃原因（建议填写）：如估值过高 / 逻辑存疑 / 仓位已满"
      />
      <template #footer>
        <div class="modal-footer">
          <n-button @click="passModal = false">取消</n-button>
          <n-button type="warning" @click="confirmPass">确认放弃</n-button>
        </div>
      </template>
    </n-modal>

    <!-- 错过机会复盘 -->
    <n-modal v-model:show="missedModal" preset="card" title="错过机会复盘" style="max-width: 720px">
      <n-spin :show="missedLoading">
        <n-empty v-if="!missedRows.length" description="还没有已放弃的标的——在自选条目上把阶段标为「已放弃」后，这里会记录并跟踪" />
        <div v-else class="missed-list">
          <div v-for="m in missedRows" :key="m.id" class="missed-row">
            <div class="missed-main">
              <div class="missed-name">
                <span class="it-title">{{ m.name || m.symbol }}</span>
                <span class="it-symbol qv-mono">{{ m.symbol }}</span>
                <n-tag size="tiny" :type="verdictMeta[m.verdict]?.type" round :bordered="false">
                  {{ verdictMeta[m.verdict]?.label }}
                </n-tag>
              </div>
              <div v-if="m.passed_reason" class="missed-reason">放弃原因：{{ m.passed_reason }}</div>
            </div>
            <div class="missed-nums qv-tnum">
              <span>放弃价 {{ m.passed_price > 0 ? m.passed_price.toFixed(2) : '—' }}</span>
              <span>现价 {{ m.quote_ok ? m.current_price.toFixed(2) : '—' }}</span>
              <span v-if="m.passed_price > 0 && m.quote_ok" :style="{ color: pctColor(m.change_since_pct) }">
                {{ m.change_since_pct >= 0 ? '+' : '' }}{{ m.change_since_pct.toFixed(2) }}%
              </span>
            </div>
          </div>
        </div>
      </n-spin>
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
.item-wrap {
  border-bottom: 1px solid var(--qv-divider);
}
.item-wrap:last-child {
  border-bottom: none;
}
.item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 10px 4px;
  cursor: pointer;
  border-radius: 8px;
  transition: background-color 0.15s ease;
}
.item:hover {
  background: rgba(128, 128, 128, 0.06);
}
.it-chevron {
  flex-shrink: 0;
  width: 14px;
  font-size: 11px;
  opacity: 0.4;
  transition: transform 0.18s ease;
}
.it-chevron.is-open {
  transform: rotate(90deg);
}
.item-spark {
  padding: 2px 4px 12px 30px;
}
.spark-svg {
  display: block;
  width: 100%;
  height: 72px;
}
.spark-placeholder {
  height: 72px;
}
.spark-meta {
  display: flex;
  align-items: center;
  gap: 16px;
  flex-wrap: wrap;
  margin-top: 6px;
  font-size: 12px;
  opacity: 0.72;
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

@media (max-width: 768px) {
  .item {
    flex-wrap: wrap;
    row-gap: 2px;
  }
  .it-name {
    flex-wrap: wrap;
  }
  .it-quote {
    min-width: 0;
  }
  /* 操作按钮整行换到下一行，与内容左对齐 */
  .it-actions {
    flex-basis: 100%;
    flex-wrap: wrap;
    padding-left: 18px;
  }
}
.modal-footer {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
}
.pass-hint {
  margin: 0 0 10px;
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.8;
}
.missed-list {
  display: flex;
  flex-direction: column;
}
.missed-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 12px;
  padding: 10px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.missed-row:last-child {
  border-bottom: none;
}
.missed-name {
  display: flex;
  align-items: center;
  gap: 8px;
}
.missed-reason {
  font-size: 12px;
  opacity: 0.65;
  margin-top: 2px;
}
.missed-nums {
  display: flex;
  gap: 12px;
  font-size: 12.5px;
  white-space: nowrap;
}
</style>
