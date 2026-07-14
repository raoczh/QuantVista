<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NTag,
  NTable,
  NGrid,
  NGi,
  NModal,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NSwitch,
  NSpin,
  NEmpty,
  NPopover,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import {
  getScreenerStrategies,
  screenerScan,
  saveScreenerStrategy,
  deleteScreenerStrategy,
  getScreenerStatus,
  parseScreenerStrategy,
  PERIOD_LABEL,
  RISK_LABEL,
  RISK_TAG_TYPE,
  type StrategiesView,
  type BuiltinStrategy,
  type CustomStrategy,
  type ScanResult,
  type FactorTableStatus,
  type FactorDef,
  type CondNode,
  type ParseStrategyResult,
} from '@/api/screener'
import { useUi } from '@/composables/useUi'
import { useLlmLabel } from '@/composables/useLlmLabel'
import { useIsMobile } from '@/composables/useIsMobile'
import { useStockActions } from '@/composables/useStockActions'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import ChangeTag from '@/components/ChangeTag.vue'

const message = useMessage()
const router = useRouter()
const { vars } = useUi()
const { llmLabel } = useLlmLabel()
const { isMobile } = useIsMobile()
const actions = useStockActions()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

// ---------- 策略广场与宽表状态 ----------

const data = ref<StrategiesView | null>(null)
const status = ref<FactorTableStatus | null>(null)
const loading = ref(false)

const periodFilter = ref<'all' | 'short' | 'swing' | 'mid'>('all')
const builtinFiltered = computed<BuiltinStrategy[]>(() => {
  const list = data.value?.builtin ?? []
  if (periodFilter.value === 'all') return list
  return list.filter((b) => b.period === periodFilter.value)
})
const customList = computed<CustomStrategy[]>(() => data.value?.custom ?? [])
const factors = computed<FactorDef[]>(() => data.value?.factors ?? [])

async function load() {
  loading.value = true
  try {
    ;[data.value, status.value] = await Promise.all([getScreenerStrategies(), getScreenerStatus()])
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
onMounted(load)

const statusText = computed(() => {
  const s = status.value
  if (!s) return ''
  if (s.building) return '因子宽表构建中…'
  if (!s.ready) return '因子宽表未构建：首次扫描会自动构建（需全市场日线数据，约 5~10 秒）'
  return `数据日期 ${s.trade_date} · 覆盖 ${s.universe} 只 · ${s.factors} 个因子 · 构建耗时 ${((s.build_ms ?? 0) / 1000).toFixed(1)}s`
})

// ---------- 扫描 ----------

const scanning = ref('') // 正在扫描的 strategy_key / `custom-{id}` / 'temp'
const result = ref<ScanResult | null>(null)
const includeST = ref(false)
const includeStale = ref(false)
// 最近一次扫描目标（开关切换后重扫用）。
let lastScanTarget: { strategy_key?: string; strategy_id?: number; tree?: CondNode } | null = null

async function runScan(target: { strategy_key?: string; strategy_id?: number; tree?: CondNode }, tag: string) {
  scanning.value = tag
  try {
    result.value = await screenerScan({
      ...target,
      include_st: includeST.value,
      include_stale: includeStale.value,
    })
    lastScanTarget = target
    status.value = await getScreenerStatus().catch(() => status.value)
    if (!result.value?.matched) {
      message.info('本次扫描无命中（条件较严或市况不配合，属正常情况）')
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    scanning.value = ''
  }
}
function rescan() {
  if (lastScanTarget) runScan(lastScanTarget, 'rescan')
}

const resultStats = computed(() => {
  const r = result.value
  if (!r) return ''
  const parts = [`命中 ${r.matched}`, `参与判定 ${r.scanned}/${r.universe} 只`]
  if (r.st_skipped) parts.push(`ST 跳过 ${r.st_skipped}`)
  if (r.stale_skipped) parts.push(`停牌/滞后跳过 ${r.stale_skipped}`)
  if (r.truncated) parts.push(`仅展示前 ${r.items?.length ?? 0} 只（按成交额降序）`)
  return parts.join(' · ')
})

// ---------- 自定义策略编辑器 ----------

// 行式条件（行间 AND）：数值因子支持 值比较/区间/与因子比，布尔因子支持 是/否。
interface CondRow {
  factor: string
  op: string // '>' '>=' '<' '<=' 'between' 'is_true' 'is_false' '>ref' '<ref'
  value?: number
  value2?: number
  ref?: string
}
const editorShow = ref(false)
const editorSaving = ref(false)
const editorForm = ref<{ id: number; name: string; desc: string; period: string; risk: string }>({
  id: 0,
  name: '',
  desc: '',
  period: 'swing',
  risk: 'mid',
})
const editorRows = ref<CondRow[]>([])

const factorOptions = computed(() => {
  const groups = new Map<string, { label: string; value: string }[]>()
  for (const f of factors.value) {
    if (!groups.has(f.group)) groups.set(f.group, [])
    groups.get(f.group)!.push({ label: `${f.name}（${f.key}）`, value: f.key })
  }
  return Array.from(groups.entries()).map(([g, children]) => ({
    type: 'group' as const,
    label: g,
    key: g,
    children,
  }))
})
// 数值因子（ref 右侧可选：排除布尔）。
const refFactorOptions = computed(() =>
  factors.value
    .filter((f) => f.kind !== 'bool')
    .map((f) => ({ label: f.name, value: f.key })),
)
function factorDef(key: string): FactorDef | undefined {
  return factors.value.find((f) => f.key === key)
}
function opOptions(factorKey: string) {
  const def = factorDef(factorKey)
  if (def?.kind === 'bool') {
    return [
      { label: '为是', value: 'is_true' },
      { label: '为否', value: 'is_false' },
    ]
  }
  return [
    { label: '大于', value: '>' },
    { label: '大于等于', value: '>=' },
    { label: '小于', value: '<' },
    { label: '小于等于', value: '<=' },
    { label: '介于', value: 'between' },
    { label: '大于某因子', value: '>ref' },
    { label: '小于某因子', value: '<ref' },
  ]
}
function onRowFactorChange(row: CondRow) {
  const def = factorDef(row.factor)
  if (def?.kind === 'bool') {
    row.op = 'is_true'
    row.value = undefined
    row.value2 = undefined
    row.ref = undefined
  } else if (row.op === 'is_true' || row.op === 'is_false') {
    row.op = '>'
  }
}
function addRow() {
  editorRows.value.push({ factor: 'chg_pct', op: '>', value: 0 })
}
function removeRow(i: number) {
  editorRows.value.splice(i, 1)
}

function rowsToTree(rows: CondRow[]): CondNode | null {
  const leaves: CondNode[] = []
  for (const r of rows) {
    if (!r.factor) continue
    switch (r.op) {
      case 'is_true':
      case 'is_false':
        leaves.push({ factor: r.factor, op: r.op })
        break
      case 'between':
        if (r.value == null || r.value2 == null) return null
        leaves.push({ factor: r.factor, op: 'between', value: r.value, value2: r.value2 })
        break
      case '>ref':
      case '<ref':
        if (!r.ref) return null
        leaves.push({ factor: r.factor, op: r.op[0], ref: r.ref })
        break
      default:
        if (r.value == null) return null
        leaves.push({ factor: r.factor, op: r.op, value: r.value })
    }
  }
  if (!leaves.length) return null
  return { all: leaves }
}

// flattenTree 将「单层 all 叶子」树回填为编辑行；含嵌套 any/组的高级树返回 null（不可行式编辑）。
function flattenTree(tree: CondNode | null): CondRow[] | null {
  if (!tree || !tree.all?.length || tree.any?.length) return null
  const rows: CondRow[] = []
  for (const n of tree.all) {
    if (n.all?.length || n.any?.length || !n.factor || !n.op) return null
    if (n.ref) {
      if (n.op !== '>' && n.op !== '<') return null
      rows.push({ factor: n.factor, op: `${n.op}ref`, ref: n.ref })
    } else {
      rows.push({ factor: n.factor, op: n.op, value: n.value, value2: n.value2 })
    }
  }
  return rows
}

function openCreate() {
  editorForm.value = { id: 0, name: '', desc: '', period: 'swing', risk: 'mid' }
  editorRows.value = [{ factor: 'chg_pct', op: 'between', value: 1, value2: 6 }]
  resetAiGen()
  editorShow.value = true
}
function openEdit(cs: CustomStrategy) {
  const rows = flattenTree(cs.tree)
  if (!rows) {
    message.warning('该策略含高级嵌套条件（any 组），暂不支持表单编辑，可删除后重建')
    return
  }
  editorForm.value = { id: cs.id, name: cs.name, desc: cs.desc, period: cs.period || 'swing', risk: cs.risk || 'mid' }
  editorRows.value = rows
  resetAiGen()
  editorShow.value = true
}

// ---------- AI 白话生成（P3c）----------
// AI 只负责生成：预览（人话条件 + unmatched 警示）→ 用户点「套用」才落编辑器，
// 不直接执行扫描。嵌套 any 树无法行式编辑，以只读条件清单形态套用（可保存/试扫）。

const aiText = ref('')
const aiParsing = ref(false)
const aiResult = ref<ParseStrategyResult | null>(null)
// 套用的嵌套树（行式编辑器只支持一层 all 的既有约束）：非空时行编辑区切只读展示。
const aiAdvancedTree = ref<CondNode | null>(null)
const aiAdvancedConditions = ref<string[]>([])

function resetAiGen() {
  aiText.value = ''
  aiParsing.value = false
  aiResult.value = null
  aiAdvancedTree.value = null
  aiAdvancedConditions.value = []
}

async function runAiParse() {
  const text = aiText.value.trim()
  if (!text) {
    message.warning('请先用白话描述选股条件')
    return
  }
  aiParsing.value = true
  aiResult.value = null
  try {
    aiResult.value = await parseScreenerStrategy(text)
    if (!aiResult.value.tree) {
      message.warning('AI 未能映射出任何条件（因子库没有对应数据，详见提示）')
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    aiParsing.value = false
  }
}

function adoptAiResult() {
  const r = aiResult.value
  if (!r?.tree) return
  const rows = flattenTree(r.tree)
  if (rows) {
    editorRows.value = rows
    aiAdvancedTree.value = null
    aiAdvancedConditions.value = []
  } else {
    aiAdvancedTree.value = r.tree
    aiAdvancedConditions.value = r.conditions ?? []
    message.info('AI 生成了嵌套条件组（满足其一），已按只读方式套用，可直接保存或试扫')
  }
  if (!editorForm.value.desc && r.explain) {
    editorForm.value.desc = r.explain.slice(0, 120)
  }
  message.success('已套用到编辑器，请确认后保存')
}

function clearAdvancedTree() {
  aiAdvancedTree.value = null
  aiAdvancedConditions.value = []
  if (!editorRows.value.length) addRow()
}

// 编辑器当前生效的条件树：AI 嵌套树优先，否则由编辑行构造。
function editorTree(): CondNode | null {
  return aiAdvancedTree.value ?? rowsToTree(editorRows.value)
}

async function saveEditor() {
  const tree = editorTree()
  if (!editorForm.value.name.trim()) {
    message.warning('请填写策略名称')
    return
  }
  if (!tree) {
    message.warning('请补全条件（每行需选因子并填值）')
    return
  }
  editorSaving.value = true
  try {
    await saveScreenerStrategy({
      id: editorForm.value.id || undefined,
      name: editorForm.value.name,
      desc: editorForm.value.desc,
      period: editorForm.value.period,
      risk: editorForm.value.risk,
      tree,
    })
    message.success('策略已保存')
    editorShow.value = false
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    editorSaving.value = false
  }
}

async function tryScanEditor() {
  const tree = editorTree()
  if (!tree) {
    message.warning('请补全条件后再试扫')
    return
  }
  editorShow.value = false
  await runScan({ tree }, 'temp')
}

async function removeCustom(id: number) {
  try {
    await deleteScreenerStrategy(id)
    message.success('已删除')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
</script>

<template>
  <PageContainer
    title="策略选股"
    subtitle="基于全市场日线因子宽表的条件选股：内置白话策略一键扫描，命中原因逐条可解释"
  >
    <div class="screener" :style="styleVars">
      <!-- 宽表状态条 -->
      <div class="status-line qv-anim-in">
        <n-spin v-if="status?.building || scanning" :size="14" />
        <span class="status-text">{{ statusText }}</span>
      </div>

      <!-- 扫描结果 -->
      <SectionCard v-if="result" :title="`扫描结果 · ${result.strategy}`" class="block">
        <template #extra>
          <div class="result-switches">
            <label class="switch-item">
              <n-switch v-model:value="includeST" size="small" @update:value="rescan" />
              <span>含ST</span>
            </label>
            <label class="switch-item">
              <n-switch v-model:value="includeStale" size="small" @update:value="rescan" />
              <span>含停牌</span>
            </label>
          </div>
        </template>
        <p class="result-stats">
          {{ resultStats }}
          <span class="muted">（数据为 {{ result.trade_date }} 收盘口径）</span>
        </p>
        <p v-if="result.conditions?.length" class="result-conds">
          条件：<n-tag v-for="c in result.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
        </p>
        <n-empty v-if="!result.items?.length" description="无命中标的" class="empty-pad" />
        <div v-else class="qv-scroll-x">
          <n-table size="small" :single-line="false" class="hits-table">
            <thead>
              <tr>
                <th>代码</th>
                <th>名称</th>
                <th class="num">现价</th>
                <th class="num">涨跌</th>
                <th class="num">成交额(亿)</th>
                <th v-if="!isMobile" class="num">换手%</th>
                <th v-if="!isMobile" class="num">60日位置</th>
                <th>命中原因</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              <tr v-for="h in result.items" :key="h.symbol">
                <td class="qv-tnum">{{ h.symbol }}</td>
                <td>
                  <a class="stock-link" @click="actions.goDetail({ symbol: h.symbol, market: 'cn', name: h.name })">{{
                    h.name || h.symbol
                  }}</a>
                </td>
                <td class="num qv-tnum">{{ h.price.toFixed(2) }}</td>
                <td class="num"><ChangeTag :value="h.chg_pct" size="small" /></td>
                <td class="num qv-tnum">{{ h.amount_yi.toFixed(2) }}</td>
                <td v-if="!isMobile" class="num qv-tnum">{{ h.turnover_rate ? h.turnover_rate.toFixed(2) : '—' }}</td>
                <td v-if="!isMobile" class="num qv-tnum">{{ h.pos_60 ? h.pos_60.toFixed(0) + '%' : '—' }}</td>
                <td class="reasons-cell">
                  <n-popover trigger="hover" placement="top" :disabled="h.reasons.length <= 2">
                    <template #trigger>
                      <span class="reasons-brief">
                        <n-tag v-for="r in h.reasons.slice(0, 2)" :key="r" size="small" :bordered="false" class="cond-tag">{{ r }}</n-tag>
                        <n-tag v-if="h.reasons.length > 2" size="small" :bordered="false" class="cond-tag more-tag"
                          >+{{ h.reasons.length - 2 }}</n-tag
                        >
                      </span>
                    </template>
                    <div class="reasons-full">
                      <div v-for="r in h.reasons" :key="r">{{ r }}</div>
                    </div>
                  </n-popover>
                </td>
                <td>
                  <div class="row-actions">
                    <n-button size="tiny" quaternary @click="actions.goDetail({ symbol: h.symbol, market: 'cn', name: h.name })"
                      >详情</n-button
                    >
                    <n-button size="tiny" quaternary @click="actions.goAnalysis({ symbol: h.symbol, market: 'cn', name: h.name })"
                      >AI分析</n-button
                    >
                    <n-button
                      size="tiny"
                      quaternary
                      :loading="actions.adding.value"
                      @click="actions.addToWatchlist({ symbol: h.symbol, market: 'cn', name: h.name })"
                      >加自选</n-button
                    >
                  </div>
                </td>
              </tr>
            </tbody>
          </n-table>
        </div>
      </SectionCard>

      <!-- 策略广场 -->
      <SectionCard title="内置策略广场" class="block">
        <template #extra>
          <n-radio-group v-model:value="periodFilter" size="small">
            <n-radio-button value="all">全部</n-radio-button>
            <n-radio-button value="short">短线</n-radio-button>
            <n-radio-button value="swing">波段</n-radio-button>
            <n-radio-button value="mid">中线</n-radio-button>
          </n-radio-group>
        </template>
        <n-spin :show="loading">
          <n-grid cols="1 s:2 l:3" responsive="screen" :x-gap="12" :y-gap="12">
            <n-gi v-for="b in builtinFiltered" :key="b.key">
              <div class="strategy-card">
                <div class="sc-head">
                  <span class="sc-name">{{ b.name }}</span>
                  <span class="sc-tags">
                    <n-tag size="tiny" :bordered="false">{{ PERIOD_LABEL[b.period] || b.period }}</n-tag>
                    <n-tag size="tiny" :bordered="false" :type="RISK_TAG_TYPE[b.risk] || 'default'">{{
                      RISK_LABEL[b.risk] || b.risk
                    }}</n-tag>
                  </span>
                </div>
                <p class="sc-desc">{{ b.desc }}</p>
                <div class="sc-conds">
                  <n-tag v-for="c in b.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
                </div>
                <div class="sc-foot">
                  <n-button
                    size="small"
                    type="primary"
                    secondary
                    :loading="scanning === b.key"
                    :disabled="!!scanning && scanning !== b.key"
                    @click="runScan({ strategy_key: b.key }, b.key)"
                    >一键扫描</n-button
                  >
                  <n-button size="small" quaternary @click="router.push(`/backtest?strategy_key=${b.key}`)">回测</n-button>
                </div>
              </div>
            </n-gi>
          </n-grid>
        </n-spin>
      </SectionCard>

      <!-- 我的策略 -->
      <SectionCard title="我的自定义策略" class="block">
        <template #extra>
          <n-button size="small" @click="openCreate">新建策略</n-button>
        </template>
        <n-empty v-if="!customList.length" description="还没有自定义策略：点右上角「新建策略」，用因子条件组合自己的选股逻辑" class="empty-pad" />
        <div v-else class="custom-list">
          <div v-for="cs in customList" :key="cs.id" class="custom-row">
            <div class="cr-main">
              <div class="cr-head">
                <span class="sc-name">{{ cs.name }}</span>
                <n-tag size="tiny" :bordered="false">{{ PERIOD_LABEL[cs.period] || cs.period }}</n-tag>
                <n-tag size="tiny" :bordered="false" :type="RISK_TAG_TYPE[cs.risk] || 'default'">{{
                  RISK_LABEL[cs.risk] || cs.risk
                }}</n-tag>
              </div>
              <p v-if="cs.desc" class="sc-desc">{{ cs.desc }}</p>
              <div class="sc-conds">
                <n-tag v-for="c in cs.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
              </div>
            </div>
            <div class="cr-actions">
              <n-button
                size="small"
                type="primary"
                secondary
                :loading="scanning === `custom-${cs.id}`"
                :disabled="!!scanning && scanning !== `custom-${cs.id}`"
                @click="runScan({ strategy_id: cs.id }, `custom-${cs.id}`)"
                >扫描</n-button
              >
              <n-button size="small" quaternary @click="openEdit(cs)">编辑</n-button>
              <n-popconfirm @positive-click="removeCustom(cs.id)">
                <template #trigger>
                  <n-button size="small" quaternary type="error">删除</n-button>
                </template>
                确认删除策略「{{ cs.name }}」？
              </n-popconfirm>
            </div>
          </div>
        </div>
      </SectionCard>

      <!-- 自定义策略编辑器（单根约束：n-modal 必须在 PageContainer 内） -->
      <n-modal
        v-model:show="editorShow"
        preset="card"
        :title="editorForm.id ? '编辑策略' : '新建策略'"
        class="editor-modal"
        :style="{ maxWidth: '760px' }"
      >
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 76">
          <n-grid cols="1 s:2" responsive="screen" :x-gap="12">
            <n-gi>
              <n-form-item label="名称" required>
                <n-input v-model:value="editorForm.name" placeholder="如：温和放量低位股" maxlength="32" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="说明">
                <n-input v-model:value="editorForm.desc" placeholder="一句话描述策略意图（可选）" maxlength="120" />
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="周期">
                <n-radio-group v-model:value="editorForm.period" size="small">
                  <n-radio-button value="short">短线</n-radio-button>
                  <n-radio-button value="swing">波段</n-radio-button>
                  <n-radio-button value="mid">中线</n-radio-button>
                </n-radio-group>
              </n-form-item>
            </n-gi>
            <n-gi>
              <n-form-item label="风险">
                <n-radio-group v-model:value="editorForm.risk" size="small">
                  <n-radio-button value="low">低</n-radio-button>
                  <n-radio-button value="mid">中</n-radio-button>
                  <n-radio-button value="high">高</n-radio-button>
                </n-radio-group>
              </n-form-item>
            </n-gi>
          </n-grid>
        </n-form>
        <!-- AI 白话生成（P3c）：预览确认后才落编辑器，AI 不直接执行扫描 -->
        <div class="ai-gen">
          <p class="rows-hint">
            AI 生成：用白话描述选股条件（如「缩量回踩 20 日线且获利盘低于 15%」），生成后先预览，点「套用」才会写入下方编辑器（消耗 1 次 AI 配额）。
          </p>
          <div class="ai-input-row">
            <n-input
              v-model:value="aiText"
              type="textarea"
              :rows="2"
              maxlength="300"
              show-count
              placeholder="例：量比 2 以上放量突破 20 日新高，换手率别超过 20%"
            />
            <n-button size="small" type="primary" secondary :loading="aiParsing" @click="runAiParse">AI 生成</n-button>
          </div>
          <div v-if="aiResult" class="ai-preview">
            <p v-if="aiResult.explain" class="ai-explain">AI 理解：{{ aiResult.explain }}</p>
            <p v-if="llmLabel(aiResult)" class="ai-explain">解析模型：{{ llmLabel(aiResult) }}</p>
            <div v-if="aiResult.conditions?.length" class="sc-conds">
              <n-tag v-for="c in aiResult.conditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
            </div>
            <div v-if="aiResult.unmatched?.length" class="ai-unmatched">
              <p class="ai-unmatched-hint">以下表述在因子库中没有对应数据，未纳入条件（AI 不会硬凑相近因子）：</p>
              <n-tag v-for="u in aiResult.unmatched" :key="u" size="small" type="warning" :bordered="false" class="cond-tag"
                >⚠ {{ u }}</n-tag
              >
            </div>
            <div v-if="aiResult.tree" class="ai-adopt">
              <n-button size="small" type="primary" @click="adoptAiResult">套用到编辑器</n-button>
            </div>
          </div>
        </div>
        <div v-if="aiAdvancedTree" class="editor-rows">
          <p class="rows-hint">
            AI 生成的条件含嵌套组（满足其一），不支持逐行编辑，以下为只读条件清单——可直接保存或试扫。
          </p>
          <div class="sc-conds">
            <n-tag v-for="c in aiAdvancedConditions" :key="c" size="small" :bordered="false" class="cond-tag">{{ c }}</n-tag>
          </div>
          <n-button size="small" quaternary @click="clearAdvancedTree">放弃嵌套条件，改用逐行编辑</n-button>
        </div>
        <div v-else class="editor-rows">
          <p class="rows-hint">条件之间为「且」（全部满足才命中）；布尔因子选「为是/为否」，数值因子可与固定值或另一因子比较。</p>
          <div v-for="(row, i) in editorRows" :key="i" class="cond-row">
            <n-select
              v-model:value="row.factor"
              :options="factorOptions"
              filterable
              placeholder="因子"
              size="small"
              class="w-factor"
              @update:value="onRowFactorChange(row)"
            />
            <n-select v-model:value="row.op" :options="opOptions(row.factor)" size="small" class="w-op" />
            <template v-if="row.op === 'between'">
              <n-input-number v-model:value="row.value" size="small" class="w-num" placeholder="下限" />
              <span class="tilde">~</span>
              <n-input-number v-model:value="row.value2" size="small" class="w-num" placeholder="上限" />
            </template>
            <n-select
              v-else-if="row.op === '>ref' || row.op === '<ref'"
              v-model:value="row.ref"
              :options="refFactorOptions"
              filterable
              placeholder="对比因子"
              size="small"
              class="w-factor"
            />
            <n-input-number
              v-else-if="row.op !== 'is_true' && row.op !== 'is_false'"
              v-model:value="row.value"
              size="small"
              class="w-num"
              placeholder="值"
            />
            <n-button size="small" quaternary type="error" :disabled="editorRows.length <= 1" @click="removeRow(i)">删</n-button>
          </div>
          <n-button size="small" dashed block @click="addRow">+ 添加条件</n-button>
        </div>
        <template #footer>
          <div class="editor-foot">
            <n-button size="small" @click="tryScanEditor">先试扫一次</n-button>
            <n-button size="small" type="primary" :loading="editorSaving" @click="saveEditor">保存策略</n-button>
          </div>
        </template>
      </n-modal>
    </div>
  </PageContainer>
</template>

<style scoped>
.screener {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.status-line {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
  opacity: 0.75;
}
.block {
  width: 100%;
}
.result-switches {
  display: flex;
  gap: 14px;
  align-items: center;
}
.switch-item {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  font-size: 12px;
  opacity: 0.85;
}
.result-stats {
  margin: 0 0 8px;
  font-size: 13px;
}
.result-stats .muted {
  opacity: 0.6;
}
.result-conds {
  margin: 0 0 10px;
  font-size: 12px;
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  align-items: center;
}
.cond-tag {
  font-size: 11px;
}
.more-tag {
  opacity: 0.75;
}
.hits-table th.num,
.hits-table td.num {
  text-align: right;
}
.hits-table td,
.hits-table th {
  white-space: nowrap;
}
.reasons-cell {
  max-width: 340px;
}
.reasons-brief {
  display: inline-flex;
  gap: 4px;
  flex-wrap: wrap;
}
.reasons-full {
  max-width: 380px;
  font-size: 12px;
  line-height: 1.9;
}
.stock-link {
  cursor: pointer;
  text-decoration: none;
}
.stock-link:hover {
  text-decoration: underline;
}
.row-actions {
  display: flex;
  gap: 2px;
}
.empty-pad {
  padding: 22px 0;
}
/* 策略卡 */
.strategy-card {
  border: 1px solid var(--qv-divider);
  border-radius: 10px;
  padding: 12px 14px;
  height: 100%;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.sc-head {
  display: flex;
  justify-content: space-between;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.sc-name {
  font-weight: 600;
  font-size: 14px;
}
.sc-tags {
  display: inline-flex;
  gap: 4px;
}
.sc-desc {
  margin: 0;
  font-size: 12px;
  line-height: 1.7;
  opacity: 0.72;
  display: -webkit-box;
  -webkit-line-clamp: 3;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
.sc-conds {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
.sc-foot {
  margin-top: auto;
  display: flex;
  justify-content: flex-end;
}
/* 自定义策略行 */
.custom-list {
  display: flex;
  flex-direction: column;
}
.custom-row {
  display: flex;
  justify-content: space-between;
  gap: 12px;
  padding: 12px 2px;
  border-bottom: 1px solid var(--qv-divider);
  flex-wrap: wrap;
}
.custom-row:last-child {
  border-bottom: none;
}
.cr-main {
  flex: 1;
  min-width: 240px;
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.cr-head {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.cr-actions {
  display: flex;
  gap: 4px;
  align-items: flex-start;
}
/* 编辑器 */
.ai-gen {
  display: flex;
  flex-direction: column;
  gap: 8px;
  margin-bottom: 14px;
  padding: 10px 12px;
  border: 1px dashed var(--qv-divider);
  border-radius: 10px;
}
.ai-input-row {
  display: flex;
  gap: 8px;
  align-items: flex-end;
}
.ai-input-row .n-input {
  flex: 1;
}
.ai-preview {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.ai-explain {
  margin: 0;
  font-size: 12px;
  opacity: 0.8;
}
.ai-unmatched {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  align-items: center;
}
.ai-unmatched-hint {
  margin: 0;
  font-size: 12px;
  opacity: 0.7;
  flex-basis: 100%;
}
.ai-adopt {
  display: flex;
  justify-content: flex-end;
}
.editor-rows {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.rows-hint {
  margin: 0 0 2px;
  font-size: 12px;
  opacity: 0.6;
}
.cond-row {
  display: flex;
  gap: 6px;
  align-items: center;
  flex-wrap: wrap;
}
.w-factor {
  width: 200px;
}
.w-op {
  width: 120px;
}
.w-num {
  width: 110px;
}
.tilde {
  opacity: 0.5;
}
.editor-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}
@media (max-width: 768px) {
  .ai-input-row {
    flex-direction: column;
    align-items: stretch;
  }
  .cr-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
  .w-factor {
    width: 100%;
  }
  .w-op,
  .w-num {
    flex: 1;
    min-width: 96px;
  }
  .reasons-cell {
    max-width: 220px;
  }
}
</style>
