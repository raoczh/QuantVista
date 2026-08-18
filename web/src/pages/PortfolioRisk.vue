<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NCheckbox,
  NEmpty,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NModal,
  NPopconfirm,
  NSelect,
  NSpin,
  NTabPane,
  NTabs,
  NTag,
  useMessage,
} from 'naive-ui'
import * as echarts from 'echarts'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import PortfolioRiskConclusion from '@/components/portfolio-risk/PortfolioRiskConclusion.vue'
import PortfolioProfessionalMetrics from '@/components/portfolio-risk/PortfolioProfessionalMetrics.vue'
import { useUi } from '@/composables/useUi'
import {
  addCashFlow,
  archivePortfolio,
  createPortfolio,
  deletePortfolio,
  getPortfolioOverview,
  getPortfolioRisk,
  getRebalance,
  getTargets,
  listCashFlows,
  listPortfolios,
  reverseCashFlow,
  runStress,
  saveTargets,
  setDefaultPortfolio,
  updatePortfolio,
  type CashFlow,
  type PortfolioAccount,
  type PortfolioKind,
  type PortfolioOverview,
  type PortfolioRisk,
  type RebalanceDraft,
  type StressResult,
  type TargetItem,
} from '@/api/portfolio'

const props = withDefaults(defineProps<{ embedded?: boolean }>(), {
  embedded: false,
})
const message = useMessage()
const route = useRoute()
const router = useRouter()
const { vars, isDark } = useUi()
const accounts = ref<PortfolioAccount[]>([])
const accountId = ref<number | null>(null)
const overview = ref<PortfolioOverview | null>(null)
const risk = ref<PortfolioRisk | null>(null)
const loadingAccounts = ref(false)
const loading = ref(false)
const loadError = ref('')
const tabQueryKey = props.embedded ? 'risk_tab' : 'tab'
const allowedTabs = new Set(['overview', 'risk', 'stress', 'targets', 'cash'])
const initialTab = String(route.query[tabQueryKey] || 'overview')
const tab = ref(allowedTabs.has(initialTab) ? initialTab : 'overview')
const windowDays = ref(Number(route.query.window) || 252)
const benchmark = ref(String(route.query.benchmark || '000001'))
const annualization = ref(252)
const riskFree = ref(0)
const accountOptions = computed(() =>
  accounts.value.map((a) => ({
    label: `${a.name} · ${a.kind === 'real' ? '真实' : '模拟'}${a.status === 'archived' ? ' · 已归档' : ''}`,
    value: a.id,
  })),
)
const currentAccount = computed(
  () => accounts.value.find((a) => a.id === accountId.value) || null,
)
function errorText(e: unknown) {
  return e instanceof Error ? e.message : '请求失败'
}
function money(v: number | undefined) {
  return v == null
    ? '-'
    : v.toLocaleString('zh-CN', {
        minimumFractionDigits: 2,
        maximumFractionDigits: 2,
      })
}
const exposureDims = computed(() =>
  overview.value?.exposure
    ? [
        {
          key: 'industry',
          title: '行业暴露',
          dim: overview.value.exposure.industry,
        },
        {
          key: 'cap',
          title: '市值风格',
          dim: overview.value.exposure.cap_style,
        },
        {
          key: 'value',
          title: '估值风格',
          dim: overview.value.exposure.value_style,
        },
      ]
    : [],
)
const riskReasons = computed(() => {
  const r = risk.value
  if (!r) return []
  const reasons = [...(r.unknown_reasons || [])]
  for (const item of [
    r.twr_pct,
    r.annualized_volatility_pct,
    r.downside_volatility_pct,
    r.sharpe,
    r.sortino,
    r.beta,
    r.alpha_pct,
    r.max_drawdown.metric,
    r.risk_contribution.predicted_volatility_pct,
  ])
    if (item.status !== 'available' && item.reason) reasons.push(item.reason)
  return [...new Set(reasons)]
})

async function loadAccounts() {
  loadingAccounts.value = true
  try {
    accounts.value = await listPortfolios()
    const query = Number(route.query.account_id)
    const current = accounts.value.find((a) => a.id === accountId.value)
    const next =
      accounts.value.find((a) => a.id === query) ||
      current ||
      accounts.value.find((a) => a.is_default && a.kind === 'real') ||
      accounts.value.find((a) => a.status === 'active')
    accountId.value = next?.id || null
  } catch (e) {
    loadError.value = errorText(e)
  } finally {
    loadingAccounts.value = false
  }
}
let loadSeq = 0
async function loadWorkspace(silent = false) {
  if (!accountId.value) return
  const seq = ++loadSeq
  if (!silent) loading.value = true
  loadError.value = ''
  try {
    const [ov, rv] = await Promise.all([
      getPortfolioOverview(accountId.value),
      getPortfolioRisk(accountId.value, {
        window: windowDays.value,
        annualization: annualization.value,
        risk_free_rate_pct: riskFree.value,
        benchmark: benchmark.value,
      }),
    ])
    if (seq !== loadSeq) return
    overview.value = ov
    risk.value = rv
    await nextTick()
    renderCharts()
  } catch (e) {
    if (seq === loadSeq) loadError.value = errorText(e)
  } finally {
    if (seq === loadSeq) loading.value = false
  }
}
watch(accountId, async (id) => {
  if (!id) return
  await router.replace({ query: { ...route.query, account_id: String(id) } })
  await loadWorkspace()
  if (tab.value === 'cash') void loadFlows()
  if (tab.value === 'targets') void loadTargetData()
})
watch(tab, (v) => {
  void router.replace({ query: { ...route.query, [tabQueryKey]: v } })
  if (v === 'cash') void loadFlows()
  if (v === 'targets') void loadTargetData()
  nextTick(renderCharts)
})

const accountModal = ref(false)
const accountSubmitting = ref(false)
const accountForm = ref<{ name: string; kind: PortfolioKind }>({
  name: '',
  kind: 'real',
})
const editingAccount = ref<PortfolioAccount | null>(null)
function openCreateAccount() {
  editingAccount.value = null
  accountForm.value = { name: '', kind: 'real' }
  accountModal.value = true
}
function openRename() {
  if (!currentAccount.value) return
  editingAccount.value = currentAccount.value
  accountForm.value = {
    name: currentAccount.value.name,
    kind: currentAccount.value.kind,
  }
  accountModal.value = true
}
async function submitAccount() {
  if (accountSubmitting.value) return
  accountSubmitting.value = true
  try {
    const row = editingAccount.value
      ? await updatePortfolio(editingAccount.value.id, accountForm.value.name)
      : await createPortfolio({ ...accountForm.value, currency: 'CNY' })
    message.success(editingAccount.value ? '已改名' : '已创建')
    accountModal.value = false
    await loadAccounts()
    accountId.value = row.id
  } catch (e) {
    message.error(errorText(e))
  } finally {
    accountSubmitting.value = false
  }
}
async function makeDefault() {
  if (!currentAccount.value || accountSubmitting.value) return
  accountSubmitting.value = true
  try {
    await setDefaultPortfolio(currentAccount.value.id)
    message.success('已设为默认')
    await loadAccounts()
  } catch (e) {
    message.error(errorText(e))
  } finally {
    accountSubmitting.value = false
  }
}
async function archive() {
  if (!currentAccount.value || accountSubmitting.value) return
  accountSubmitting.value = true
  try {
    await archivePortfolio(currentAccount.value.id)
    message.success('已归档')
    await loadAccounts()
  } catch (e) {
    message.error(errorText(e))
  } finally {
    accountSubmitting.value = false
  }
}
async function removeAccount() {
  if (!currentAccount.value || accountSubmitting.value) return
  const deletedId = currentAccount.value.id
  accountSubmitting.value = true
  try {
    await deletePortfolio(deletedId)
    message.success('已删除空账户')
    accountId.value = null
    await loadAccounts()
  } catch (e) {
    message.error(errorText(e))
  } finally {
    accountSubmitting.value = false
  }
}

const curveEl = ref<HTMLElement | null>(null)
const ddEl = ref<HTMLElement | null>(null)
let curveChart: echarts.ECharts | null = null
let ddChart: echarts.ECharts | null = null
function renderCharts() {
  if (!risk.value) return
  const complete = risk.value.curve.filter((p) => !p.partial && p.assets > 0)
  if (curveEl.value) {
    curveChart?.dispose()
    curveChart = echarts.init(curveEl.value, isDark.value ? 'dark' : undefined)
    curveChart.setOption({
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis' },
      grid: { left: 58, right: 20, top: 20, bottom: 42 },
      xAxis: { type: 'category', data: complete.map((p) => p.trade_date) },
      yAxis: { type: 'value', scale: true },
      series: [
        {
          type: 'line',
          name: '总资产',
          data: complete.map((p) => p.assets),
          showSymbol: false,
          smooth: false,
          lineStyle: { width: 2, color: vars.value.primaryColor },
          areaStyle: { opacity: 0.08 },
        },
      ],
    })
  }
  if (ddEl.value) {
    ddChart?.dispose()
    ddChart = echarts.init(ddEl.value, isDark.value ? 'dark' : undefined)
    ddChart.setOption({
      backgroundColor: 'transparent',
      tooltip: { trigger: 'axis' },
      grid: { left: 58, right: 20, top: 20, bottom: 42 },
      xAxis: { type: 'category', data: complete.map((p) => p.trade_date) },
      yAxis: { type: 'value', max: 0 },
      series: [
        {
          type: 'line',
          name: '回撤%',
          data: complete.map((p) => p.drawdown_pct ?? null),
          showSymbol: false,
          lineStyle: { color: vars.value.errorColor },
          areaStyle: { opacity: 0.12, color: vars.value.errorColor },
        },
      ],
    })
  }
}
function resize() {
  curveChart?.resize()
  ddChart?.resize()
}
watch(isDark, () => nextTick(renderCharts))
onMounted(async () => {
  window.addEventListener('resize', resize)
  await loadAccounts()
})
onBeforeUnmount(() => {
  window.removeEventListener('resize', resize)
  curveChart?.dispose()
  ddChart?.dispose()
})

const stressType = ref<'market' | 'industry' | 'symbol' | 'plan_stop_loss'>(
  'market',
)
const stressShock = ref(-10)
const stressKey = ref('')
const stressLoading = ref(false)
const stress = ref<StressResult | null>(null)
async function executeStress() {
  if (!accountId.value || stressLoading.value) return
  stressLoading.value = true
  try {
    stress.value = await runStress(accountId.value, {
      type: stressType.value,
      shock_pct: stressType.value === 'plan_stop_loss' ? 0 : stressShock.value,
      symbol: stressType.value === 'symbol' ? stressKey.value : undefined,
      industry: stressType.value === 'industry' ? stressKey.value : undefined,
    })
  } catch (e) {
    message.error(errorText(e))
  } finally {
    stressLoading.value = false
  }
}

const flows = ref<CashFlow[]>([])
const flowsLoading = ref(false)
const flowModal = ref(false)
const flowSubmitting = ref(false)
const flowForm = ref({
  type: 'deposit',
  amount: 0,
  trade_date: new Date().toLocaleDateString('en-CA'),
  note: '',
})
let flowSeq = 0
async function loadFlows() {
  if (!accountId.value || currentAccount.value?.kind !== 'real') {
    flows.value = []
    return
  }
  const id = accountId.value
  const seq = ++flowSeq
  flowsLoading.value = true
  try {
    const rows = await listCashFlows(id)
    if (seq === flowSeq && id === accountId.value) flows.value = rows
  } catch (e) {
    if (seq === flowSeq) message.error(errorText(e))
  } finally {
    if (seq === flowSeq) flowsLoading.value = false
  }
}
async function submitFlow() {
  if (
    !accountId.value ||
    currentAccount.value?.status !== 'active' ||
    flowSubmitting.value
  )
    return
  flowSubmitting.value = true
  try {
    let amount = flowForm.value.amount
    if (flowForm.value.type === 'withdrawal' && amount > 0) amount = -amount
    await addCashFlow(accountId.value, {
      ...flowForm.value,
      amount,
      idempotency_key: crypto.randomUUID(),
    })
    flowModal.value = false
    message.success('现金流已记录')
    await Promise.all([loadFlows(), loadWorkspace(true)])
  } catch (e) {
    message.error(errorText(e))
  } finally {
    flowSubmitting.value = false
  }
}
async function reverseFlow(row: CashFlow) {
  if (
    !accountId.value ||
    currentAccount.value?.status !== 'active' ||
    flowSubmitting.value
  )
    return
  flowSubmitting.value = true
  try {
    await reverseCashFlow(accountId.value, row.id, {
      idempotency_key: crypto.randomUUID(),
      note: '前端冲正',
    })
    message.success('已新增反向流水')
    await Promise.all([loadFlows(), loadWorkspace(true)])
  } catch (e) {
    message.error(errorText(e))
  } finally {
    flowSubmitting.value = false
  }
}

const targets = ref<TargetItem[]>([])
const targetsLoading = ref(false)
const targetsSaving = ref(false)
const rebalance = ref<RebalanceDraft | null>(null)
let targetSeq = 0
function newTarget(): TargetItem {
  return {
    type: 'symbol',
    key: '',
    target_weight_pct: 0,
    min_weight_pct: 0,
    max_weight_pct: 100,
    enabled: true,
  }
}
async function loadTargetData() {
  if (!accountId.value) return
  const id = accountId.value
  const seq = ++targetSeq
  targetsLoading.value = true
  try {
    const data = await getTargets(id)
    const draft = data.revision
      ? await getRebalance(id, data.revision.revision)
      : null
    if (seq === targetSeq && id === accountId.value) {
      targets.value = data.items.length ? data.items : [newTarget()]
      rebalance.value = draft
    }
  } catch (e) {
    if (seq === targetSeq) {
      targets.value = [newTarget()]
      rebalance.value = null
      message.error(errorText(e))
    }
  } finally {
    if (seq === targetSeq) targetsLoading.value = false
  }
}
async function saveTargetData() {
  if (
    !accountId.value ||
    currentAccount.value?.status !== 'active' ||
    targetsSaving.value
  )
    return
  targetsSaving.value = true
  try {
    const revision = await saveTargets(accountId.value, targets.value)
    rebalance.value = await getRebalance(accountId.value, revision.revision)
    message.success(`已生成 revision ${revision.revision}`)
  } catch (e) {
    message.error(errorText(e))
  } finally {
    targetsSaving.value = false
  }
}
const enabledWeight = computed(() =>
  targets.value
    .filter((x) => x.enabled)
    .reduce((s, x) => s + (x.target_weight_pct || 0), 0),
)
</script>

<template>
  <PageContainer class="risk-page" :class="{ 'is-embedded': embedded }">
    <div class="workspace-head">
      <div>
        <h1>组合风险</h1>
        <p v-if="currentAccount">
          {{ currentAccount.kind === 'real' ? '真实账户' : '模拟账户' }} ·
          {{ currentAccount.currency }} · 截至 {{ overview?.as_of || '-' }}
        </p>
      </div>
      <div class="head-actions">
        <n-select
          v-model:value="accountId"
          :loading="loadingAccounts"
          :options="accountOptions"
          style="min-width: 220px"
        />
        <n-button @click="openCreateAccount">新建</n-button>
        <n-button
          :disabled="!currentAccount || currentAccount.status === 'archived'"
          @click="openRename"
        >
          改名
        </n-button>
        <n-button
          :disabled="
            !currentAccount ||
            currentAccount.is_default ||
            currentAccount.status === 'archived'
          "
          :loading="accountSubmitting"
          @click="makeDefault"
        >
          设为默认
        </n-button>
        <n-popconfirm @positive-click="archive">
          <template #trigger>
            <n-button
              :disabled="
                !currentAccount ||
                currentAccount.is_default ||
                currentAccount.status === 'archived'
              "
              :loading="accountSubmitting"
            >
              归档
            </n-button>
          </template>
          确认归档当前账户？历史事实会保留。
        </n-popconfirm>
        <n-popconfirm @positive-click="removeAccount">
          <template #trigger>
            <n-button
              type="error"
              :disabled="!currentAccount || currentAccount.is_default"
              :loading="accountSubmitting"
            >
              删除
            </n-button>
          </template>
          仅空账户可以删除；已有持仓、流水或历史快照的账户请归档。
        </n-popconfirm>
      </div>
    </div>
    <n-alert
      v-if="loadError"
      type="error"
      title="组合数据读取失败"
      style="margin-bottom: 16px"
      >{{ loadError }}
      <n-button text type="primary" @click="loadWorkspace()"
        >重试</n-button
      ></n-alert
    >
    <div class="parameter-bar">
      <n-input-number v-model:value="windowDays" :min="30" :max="730" /><span
        >天窗口</span
      ><n-input
        v-model:value="benchmark"
        placeholder="基准代码"
      /><n-input-number v-model:value="riskFree" :min="0" :max="20" /><span
        >无风险利率 %</span
      ><n-button type="primary" :loading="loading" @click="loadWorkspace()"
        >重新计算</n-button
      ><span class="hash qv-mono">{{
        risk?.parameter_hash?.slice(0, 12) || '-'
      }}</span>
    </div>
    <PortfolioRiskConclusion
      :account="currentAccount"
      :overview="overview"
      :risk="risk"
      :reasons="riskReasons"
    />
    <n-spin :show="loading">
      <n-tabs v-model:value="tab" type="line" animated>
        <n-tab-pane name="overview" tab="总览">
          <div class="metric-grid">
            <StatCard
              label="总资产"
              :value="
                overview?.total_assets.status === 'available'
                  ? money(overview.total_assets.value)
                  : '不可用'
              "
            /><StatCard
              label="持仓市值"
              :value="money(overview?.market_value)"
            /><StatCard
              label="现金"
              :value="
                overview?.cash.status === 'available'
                  ? money(overview.cash.value)
                  : '不可用'
              "
            /><StatCard
              label="数据覆盖"
              :value="`${overview?.coverage_pct ?? 0}%`"
            /><StatCard
              label="Top 5 集中度"
              :value="`${overview?.top_n_weight_pct ?? 0}%`"
            />
          </div>
          <n-alert
            v-if="overview?.partial_reasons.length"
            type="warning"
            title="当前总览不完整"
            >{{ overview.partial_reasons.join('；') }}</n-alert
          >
          <SectionCard title="持仓权重"
            ><div class="qv-scroll-x">
              <table class="data-table">
                <thead>
                  <tr>
                    <th>标的</th>
                    <th>行业</th>
                    <th class="num">数量</th>
                    <th class="num">价格</th>
                    <th class="num">市值</th>
                    <th class="num">权重</th>
                    <th>状态</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="h in overview?.holdings"
                    :key="`${h.market}:${h.symbol}`"
                  >
                    <td>
                      <StockIdentity :symbol="h.symbol" market="cn" :name="h.name" density="table" clickable />
                    </td>
                    <td>{{ h.industry || '未知' }}</td>
                    <td class="num">{{ h.quantity }}</td>
                    <td class="num">{{ h.price ?? '-' }}</td>
                    <td class="num">{{ money(h.value) }}</td>
                    <td class="num">{{ h.weight_pct ?? '-' }}%</td>
                    <td>
                      <n-tag
                        :type="h.status === 'available' ? 'success' : 'warning'"
                        size="small"
                        >{{
                          h.status === 'available' ? '可用' : h.reason
                        }}</n-tag
                      >
                    </td>
                  </tr>
                </tbody>
              </table>
            </div></SectionCard
          >
          <div v-if="exposureDims.length" class="exposure-grid">
            <SectionCard
              v-for="item in exposureDims"
              :key="item.key"
              :title="item.title"
              ><div
                v-for="b in item.dim.buckets"
                :key="b.key"
                class="exposure-row"
              >
                <span>{{ b.label }}</span
                ><b>{{ b.weight_pct.toFixed(1) }}%</b>
              </div>
              <n-empty
                v-if="!item.dim.available"
                size="small"
                description="数据不可用"
            /></SectionCard>
          </div>
        </n-tab-pane>
        <n-tab-pane name="risk" tab="风险与相关性">
          <PortfolioProfessionalMetrics :risk="risk" :reasons="riskReasons" />
          <div class="chart-grid">
            <SectionCard title="资金流调整后资产曲线"
              ><div ref="curveEl" class="chart"></div></SectionCard
            ><SectionCard title="回撤区间"
              ><div ref="ddEl" class="chart"></div>
              <small v-if="risk?.max_drawdown.peak_date"
                >{{ risk.max_drawdown.peak_date }} →
                {{ risk.max_drawdown.trough_date
                }}{{
                  risk.max_drawdown.recovery_date
                    ? `，${risk.max_drawdown.recovery_date} 恢复`
                    : ''
                }}</small
              ></SectionCard
            >
          </div>
          <SectionCard title="持仓收益相关矩阵"
            ><div v-if="risk?.correlation.symbols.length" class="qv-scroll-x">
              <table class="corr-table">
                <thead>
                  <tr>
                    <th></th>
                    <th v-for="s in risk.correlation.symbols" :key="s">
                      {{ s }}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(s, i) in risk.correlation.symbols" :key="s">
                    <th>{{ s }}</th>
                    <td
                      v-for="(c, j) in risk.correlation.cells[i]"
                      :key="j"
                      :title="c.reason || `${c.sample_count} 个共同样本`"
                    >
                      {{
                        c.status === 'available' ? c.value?.toFixed(2) : 'N/A'
                      }}
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <n-empty v-else description="暂无可计算的共同收益样本"
          /></SectionCard>
          <SectionCard title="持仓风险贡献"
            ><div
              v-if="risk?.risk_contribution.items.length"
              class="qv-scroll-x"
            >
              <table class="data-table">
                <thead>
                  <tr>
                    <th>标的</th>
                    <th class="num">持仓权重</th>
                    <th class="num">边际波动</th>
                    <th class="num">成分波动</th>
                    <th class="num">风险贡献</th>
                    <th>状态</th>
                  </tr>
                </thead>
                <tbody>
                  <tr
                    v-for="item in risk.risk_contribution.items"
                    :key="item.symbol"
                  >
                    <td><StockIdentity :symbol="item.symbol" market="cn" density="table" clickable /></td>
                    <td class="num">{{ item.weight_pct.toFixed(2) }}%</td>
                    <td class="num">
                      {{
                        item.status === 'available'
                          ? `${item.marginal_volatility_pct?.toFixed(2)}%`
                          : '-'
                      }}
                    </td>
                    <td class="num">
                      {{
                        item.status === 'available'
                          ? `${item.component_volatility_pct?.toFixed(2)}%`
                          : '-'
                      }}
                    </td>
                    <td class="num">
                      {{
                        item.status === 'available'
                          ? `${item.risk_contribution_pct?.toFixed(2)}%`
                          : '-'
                      }}
                    </td>
                    <td>
                      <n-tag
                        :type="
                          item.status === 'available' ? 'success' : 'warning'
                        "
                        size="small"
                        >{{ item.reason || '可计算' }}</n-tag
                      >
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <n-empty
              v-else
              :description="
                risk?.risk_contribution.predicted_volatility_pct.reason ||
                '暂无可计算的持仓风险贡献'
              "
          /></SectionCard>
        </n-tab-pane>
        <n-tab-pane name="stress" tab="压力测试">
          <div class="stress-controls">
            <n-select
              v-model:value="stressType"
              :options="[
                { label: '全市场冲击', value: 'market' },
                { label: '单一行业冲击', value: 'industry' },
                { label: '单一持仓冲击', value: 'symbol' },
                { label: '全部触发计划止损', value: 'plan_stop_loss' },
              ]"
            /><n-input
              v-if="stressType === 'industry' || stressType === 'symbol'"
              v-model:value="stressKey"
              :placeholder="stressType === 'industry' ? '行业名称' : '股票代码'"
            /><n-select
              v-if="stressType !== 'plan_stop_loss'"
              v-model:value="stressShock"
              :options="[
                { label: '下跌 5%', value: -5 },
                { label: '下跌 10%', value: -10 },
                { label: '下跌 20%', value: -20 },
              ]"
            /><n-button
              type="primary"
              :loading="stressLoading"
              @click="executeStress"
              >生成草案</n-button
            >
          </div>
          <template v-if="stress"
            ><div class="metric-grid">
              <StatCard
                label="预计损失金额"
                :value="money(stress.estimated_loss_amount)"
              /><StatCard
                label="预计损失比例"
                :value="`${stress.estimated_loss_pct.toFixed(2)}%`"
              /><StatCard
                label="已定价基数"
                :value="money(stress.base_value)"
              />
            </div>
            <n-alert type="info" title="程序化只读场景"
              >不改持仓、不创建流水、不发送提醒。</n-alert
            >
            <div class="qv-scroll-x">
              <table class="data-table">
                <thead>
                  <tr>
                    <th>持仓</th>
                    <th class="num">冲击</th>
                    <th class="num">损失贡献</th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="c in stress.contributions" :key="c.symbol">
                    <td><StockIdentity :symbol="c.symbol" market="cn" :name="c.name" density="table" clickable /></td>
                    <td class="num">{{ c.loss_pct }}%</td>
                    <td class="num">{{ money(c.loss_amount) }}</td>
                  </tr>
                </tbody>
              </table>
            </div>
            <n-alert
              v-if="stress.unknown.length"
              type="warning"
              title="未知项"
              >{{ stress.unknown.join('；') }}</n-alert
            ></template
          ><n-empty v-else description="选择场景后生成只读压力测试结果" />
        </n-tab-pane>
        <n-tab-pane name="targets" tab="目标配置与草案">
          <n-spin :show="targetsLoading">
            <div class="target-toolbar">
              <span
                >启用权重合计 <b>{{ enabledWeight.toFixed(2) }}%</b></span
              ><n-button
                :disabled="currentAccount?.status !== 'active'"
                @click="targets.push(newTarget())"
                >添加目标</n-button
              ><n-button
                type="primary"
                :disabled="currentAccount?.status !== 'active'"
                :loading="targetsSaving"
                @click="saveTargetData"
                >保存新 revision</n-button
              >
            </div>
            <div class="qv-scroll-x">
              <table class="data-table target-table">
                <thead>
                  <tr>
                    <th>启用</th>
                    <th>类型</th>
                    <th>标识</th>
                    <th>目标%</th>
                    <th>最小%</th>
                    <th>最大%</th>
                    <th></th>
                  </tr>
                </thead>
                <tbody>
                  <tr v-for="(item, i) in targets" :key="i">
                    <td>
                      <n-checkbox
                        v-model:checked="item.enabled"
                        :disabled="currentAccount?.status !== 'active'"
                      />
                    </td>
                    <td>
                      <n-select
                        v-model:value="item.type"
                        :disabled="currentAccount?.status !== 'active'"
                        :options="[
                          { label: '股票', value: 'symbol' },
                          { label: '行业', value: 'industry' },
                        ]"
                      />
                    </td>
                    <td>
                      <n-input
                        v-model:value="item.key"
                        :disabled="currentAccount?.status !== 'active'"
                      />
                    </td>
                    <td>
                      <n-input-number
                        v-model:value="item.target_weight_pct"
                        :disabled="currentAccount?.status !== 'active'"
                        :min="0"
                        :max="100"
                      />
                    </td>
                    <td>
                      <n-input-number
                        v-model:value="item.min_weight_pct"
                        :disabled="currentAccount?.status !== 'active'"
                        :min="0"
                        :max="100"
                      />
                    </td>
                    <td>
                      <n-input-number
                        v-model:value="item.max_weight_pct"
                        :disabled="currentAccount?.status !== 'active'"
                        :min="0"
                        :max="100"
                      />
                    </td>
                    <td>
                      <n-button
                        text
                        type="error"
                        :disabled="currentAccount?.status !== 'active'"
                        @click="targets.splice(i, 1)"
                        >移除</n-button
                      >
                    </td>
                  </tr>
                </tbody>
              </table>
            </div>
            <SectionCard title="再平衡草案"
              ><n-alert
                v-if="rebalance"
                type="info"
                :title="`revision ${rebalance.revision} · 只读`"
                >{{ rebalance.note }}</n-alert
              >
              <div v-if="rebalance" class="qv-scroll-x">
                <table class="data-table">
                  <thead>
                    <tr>
                      <th>目标</th>
                      <th class="num">当前%</th>
                      <th class="num">目标%</th>
                      <th class="num">偏离%</th>
                      <th class="num">金额变化</th>
                      <th class="num">股数变化</th>
                      <th class="num">费用/税</th>
                      <th>状态</th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr
                      v-for="r in rebalance.items"
                      :key="`${r.type}:${r.key}`"
                    >
                      <td>{{ r.name || r.key }}</td>
                      <td class="num">{{ r.current_weight_pct }}</td>
                      <td class="num">{{ r.target_weight_pct }}</td>
                      <td class="num">{{ r.deviation_pct }}</td>
                      <td class="num">{{ money(r.amount_change) }}</td>
                      <td class="num">{{ r.quantity_change || '-' }}</td>
                      <td class="num">
                        {{ money(r.estimated_fee + r.estimated_tax) }}
                      </td>
                      <td>
                        <n-tag
                          :type="
                            r.status === 'available' ? 'success' : 'warning'
                          "
                          size="small"
                          >{{ r.reason || '可计算' }}</n-tag
                        >
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div>
              <n-empty v-else description="保存目标配置后生成只读草案"
            /></SectionCard>
          </n-spin>
        </n-tab-pane>
        <n-tab-pane name="cash" tab="现金流">
          <template v-if="currentAccount?.kind === 'real'"
            ><div class="target-toolbar">
              <span>真实账户收益指标依赖完整外部资金流事实。</span
              ><n-button
                type="primary"
                :disabled="currentAccount.status !== 'active'"
                @click="flowModal = true"
                >新增现金流</n-button
              >
            </div>
            <n-spin :show="flowsLoading"
              ><div class="qv-scroll-x">
                <table class="data-table">
                  <thead>
                    <tr>
                      <th>日期</th>
                      <th>类型</th>
                      <th class="num">金额</th>
                      <th>备注</th>
                      <th></th>
                    </tr>
                  </thead>
                  <tbody>
                    <tr v-for="f in flows" :key="f.id">
                      <td>{{ f.trade_date }}</td>
                      <td>{{ f.type }}</td>
                      <td class="num">{{ money(f.amount) }}</td>
                      <td>{{ f.note || '-' }}</td>
                      <td>
                        <n-popconfirm
                          v-if="
                            !f.reversal_of_id &&
                            currentAccount.status === 'active'
                          "
                          @positive-click="reverseFlow(f)"
                          ><template #trigger
                            ><n-button text type="warning"
                              >冲正</n-button
                            ></template
                          >将新增一条金额相反的流水，原记录不会删除。</n-popconfirm
                        >
                      </td>
                    </tr>
                  </tbody>
                </table>
              </div></n-spin
            ></template
          ><n-alert v-else type="info" title="模拟账户沿用内置现金账本"
            >模拟成交和重置负责维护现金，不录入真实账户现金流。</n-alert
          >
        </n-tab-pane>
      </n-tabs>
    </n-spin>

    <n-modal
      v-model:show="accountModal"
      preset="card"
      :title="editingAccount ? '组合改名' : '新建组合'"
      style="width: 460px"
      ><n-form
        ><n-form-item label="名称"
          ><n-input
            v-model:value="accountForm.name"
            maxlength="64" /></n-form-item
        ><n-form-item v-if="!editingAccount" label="类型"
          ><n-select
            v-model:value="accountForm.kind"
            :options="[
              { label: '真实账户', value: 'real' },
              { label: '模拟账户', value: 'paper' },
            ]" /></n-form-item></n-form
      ><template #footer
        ><div class="modal-actions">
          <n-button @click="accountModal = false">取消</n-button
          ><n-button
            type="primary"
            :loading="accountSubmitting"
            :disabled="!accountForm.name.trim()"
            @click="submitAccount"
            >保存</n-button
          >
        </div></template
      ></n-modal
    >
    <n-modal
      v-model:show="flowModal"
      preset="card"
      title="新增现金流"
      style="width: 500px"
      ><n-form
        ><n-form-item label="类型"
          ><n-select
            v-model:value="flowForm.type"
            :options="[
              { label: '入金', value: 'deposit' },
              { label: '出金', value: 'withdrawal' },
              { label: '费用调整', value: 'fee_adjustment' },
            ]" /></n-form-item
        ><n-form-item label="金额"
          ><n-input-number
            v-model:value="flowForm.amount"
            :min="flowForm.type === 'fee_adjustment' ? -1000000000000 : 0.01"
            :max="1000000000000" /></n-form-item
        ><n-form-item label="日期"
          ><n-input
            v-model:value="flowForm.trade_date"
            placeholder="YYYY-MM-DD" /></n-form-item
        ><n-form-item label="备注"
          ><n-input
            v-model:value="flowForm.note"
            maxlength="255" /></n-form-item></n-form
      ><template #footer
        ><div class="modal-actions">
          <n-button @click="flowModal = false">取消</n-button
          ><n-button
            type="primary"
            :loading="flowSubmitting"
            @click="submitFlow"
            >记录</n-button
          >
        </div></template
      ></n-modal
    >
  </PageContainer>
</template>

<style scoped>
.workspace-head {
  display: flex;
  justify-content: space-between;
  gap: 16px;
  align-items: flex-start;
  margin-bottom: 16px;
}
.workspace-head h1 {
  font-size: 26px;
  margin: 0;
}
.workspace-head p {
  margin: 6px 0 0;
  color: var(--n-text-color-3);
}
.head-actions,
.parameter-bar,
.stress-controls,
.target-toolbar,
.modal-actions {
  display: flex;
  gap: 10px;
  align-items: center;
  flex-wrap: wrap;
}
.parameter-bar {
  padding: 12px 0 18px;
  border-bottom: 1px solid v-bind('vars.dividerColor');
  margin-bottom: 4px;
}
.parameter-bar .n-input,
.parameter-bar .n-input-number {
  width: 150px;
}
.hash {
  margin-left: auto;
  color: v-bind('vars.textColor3');
}
.metric-grid {
  display: grid;
  grid-template-columns: repeat(5, minmax(0, 1fr));
  gap: 12px;
  margin: 16px 0;
}
.chart-grid,
.exposure-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
}
.exposure-grid {
  grid-template-columns: repeat(3, minmax(0, 1fr));
  margin-top: 16px;
}
.chart {
  height: 300px;
  width: 100%;
}
.data-table,
.corr-table {
  width: 100%;
  border-collapse: collapse;
  min-width: 720px;
}
.data-table th,
.data-table td,
.corr-table th,
.corr-table td {
  padding: 10px 12px;
  border-bottom: 1px solid v-bind('vars.dividerColor');
  text-align: left;
  white-space: nowrap;
}
.data-table th,
.corr-table th {
  font-size: 12px;
  color: v-bind('vars.textColor3');
  font-weight: 600;
}
.data-table .num {
  text-align: right;
  font-variant-numeric: tabular-nums;
}
.data-table small {
  display: block;
  color: v-bind('vars.textColor3');
}
.corr-table {
  min-width: max-content;
}
.corr-table td {
  text-align: center;
  min-width: 64px;
  font-variant-numeric: tabular-nums;
}
.exposure-row {
  display: flex;
  justify-content: space-between;
  padding: 7px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.stress-controls,
.target-toolbar {
  margin: 16px 0;
}
.stress-controls .n-select,
.stress-controls .n-input {
  width: 210px;
}
.target-toolbar {
  justify-content: flex-end;
}
.target-toolbar span {
  margin-right: auto;
}
.target-table .n-input,
.target-table .n-input-number,
.target-table .n-select {
  min-width: 112px;
}
.modal-actions {
  justify-content: flex-end;
}
@media (max-width: 900px) {
  .workspace-head {
    flex-direction: column;
  }
  .head-actions {
    width: 100%;
  }
  .head-actions .n-select {
    flex: 1;
  }
  .metric-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .chart-grid,
  .exposure-grid {
    grid-template-columns: 1fr;
  }
  .parameter-bar .hash {
    width: 100%;
    margin-left: 0;
  }
}
@media (max-width: 480px) {
  .workspace-head h1 {
    font-size: 22px;
  }
  .head-actions > * {
    flex: 1 1 calc(50% - 10px);
  }
  .head-actions .n-select {
    flex-basis: 100%;
  }
  .metric-grid {
    grid-template-columns: 1fr 1fr;
    gap: 8px;
  }
  .parameter-bar > * {
    flex: 1 1 120px;
  }
  .parameter-bar span {
    flex: 0 0 auto;
  }
  .stress-controls > * {
    width: 100% !important;
  }
  .chart {
    height: 240px;
  }
  .target-toolbar {
    justify-content: stretch;
  }
  .target-toolbar > * {
    flex: 1;
  }
  .target-toolbar span {
    flex-basis: 100%;
  }
}
</style>
