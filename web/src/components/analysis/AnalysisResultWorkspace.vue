<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NAlert,
  NButton,
  NCollapse,
  NCollapseItem,
  NEmpty,
  NInputNumber,
  NModal,
  NSpin,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  getAnalysisDiff,
  getAnalysisHindsight,
  type AnalysisDiff,
  type AnalysisRating,
  type AnalysisView,
  type HindsightView,
} from '@/api/analysis'
import type { RiskFlag } from '@/api/trust'
import { useUi } from '@/composables/useUi'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'
import TrustBadges from '@/components/TrustBadges.vue'
import {
  ANALYSIS_MODULE_LABELS,
  analysisStatusLabel,
  freshnessLabel,
  parseSnapshotFreshness,
  ratingLabel,
  recordStockName,
  suggestedAction,
} from './analysisPresentation'

const props = defineProps<{ current: AnalysisView | null; loading: boolean }>()
const router = useRouter()
const message = useMessage()
const { upColor, downColor, flatColor, pctColor, vars, withAlpha } = useUi()
const details = ref<string[]>([])
const snapshotShow = ref(false)
const rawShow = ref(false)
const diffShow = ref(false)
const diffLoading = ref(false)
const diff = ref<AnalysisDiff | null>(null)
const hindsightShow = ref(false)
const hindsightLoading = ref(false)
const hindsight = ref<HindsightView | null>(null)
const targetPrice = ref<number | null>(null)
const stopPrice = ref<number | null>(null)

const freshness = computed(() => parseSnapshotFreshness(props.current?.data_snapshot))
const snapshotObject = computed<Record<string, unknown> | null>(() => {
  if (!props.current?.data_snapshot) return null
  try { return JSON.parse(props.current.data_snapshot) as Record<string, unknown> }
  catch { return null }
})
const snapshotText = computed(() => {
  if (!props.current?.data_snapshot) return ''
  try { return JSON.stringify(JSON.parse(props.current.data_snapshot), null, 2) }
  catch { return props.current.data_snapshot }
})
const stockName = computed(() => props.current ? recordStockName(props.current) : '')
const validUntil = computed(() => {
  if (!props.current) return '未知'
  if (props.current.as_of) return `只对 ${props.current.as_of} 及此前证据有效`
  if (props.current.stale_mode) return `只解释截至 ${freshness.value.quoteAsOf || '快照时点'} 的历史数据`
  return '行情、持仓或风险事实发生变化即需重新评估'
})
const canDiff = computed(() => props.current?.status === 'success' && props.current.mode !== 'panel')
const canHindsight = computed(() => props.current?.status !== 'processing' && props.current?.module === 'stock' && !!props.current.symbol)
const metrics = computed(() => {
  const snapshot = snapshotObject.value
  const quote = snapshot?.quote as Record<string, unknown> | undefined
  const indicators = snapshot?.indicators as Record<string, unknown> | undefined
  const valuation = snapshot?.valuation as Record<string, unknown> | undefined
  const score = snapshot?.quant_score as Record<string, unknown> | undefined
  return [
    { label: '行情价格', value: quote?.price ?? snapshot?.price, asOf: freshness.value.quoteAsOf, range: '单点行情', missing: !(quote?.price ?? snapshot?.price) },
    { label: '日线与技术指标', value: indicators ? '已提供' : '', asOf: freshness.value.barsAsOf, range: indicators?.bar_count ? `${indicators.bar_count} 根日线` : '范围未记录', missing: !indicators },
    { label: '估值', value: valuation ? '已提供' : '', asOf: freshness.value.capturedAt, range: '快照字段', missing: !valuation },
    { label: '程序量化评分', value: score?.total ?? (score ? '已提供' : ''), asOf: freshness.value.barsAsOf || freshness.value.capturedAt, range: '统一规则评分', missing: !score },
  ]
})
const firstBasis = computed(() => props.current?.result?.highlights?.slice(0, 3) || [])
const firstRisks = computed(() => props.current?.result?.risks?.slice(0, 3) || [])

function ratingColor(value: AnalysisRating | '') {
  return value === 'bullish' ? upColor.value : value === 'bearish' ? downColor.value : flatColor.value
}
function riskType(flag: RiskFlag): 'error' | 'warning' | 'default' {
  return flag.level === 'block' ? 'error' : flag.level === 'warn' ? 'warning' : 'default'
}
function reviewType(value: string): 'success' | 'warning' | 'error' {
  return value === 'pass' ? 'success' : value === 'reject' ? 'error' : 'warning'
}
function formatTime(value: string) { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '未知' }
function signed(value: number | undefined | null) { return value == null ? '—' : `${value > 0 ? '+' : ''}${value.toFixed(2)}%` }

async function openDiff() {
  if (!props.current) return
  diffLoading.value = true
  try {
    diff.value = await getAnalysisDiff(props.current.id)
    diffShow.value = true
  } catch (reason) { message.warning((reason as Error).message) }
  finally { diffLoading.value = false }
}
async function openHindsight(refresh = false) {
  if (!props.current) return
  hindsightLoading.value = true
  if (!refresh) {
    hindsight.value = null
    targetPrice.value = null
    stopPrice.value = null
    hindsightShow.value = true
  }
  try { hindsight.value = await getAnalysisHindsight(props.current.id, targetPrice.value || undefined, stopPrice.value || undefined) }
  catch (reason) {
    message.warning((reason as Error).message)
    if (!refresh) hindsightShow.value = false
  } finally { hindsightLoading.value = false }
}
function ask(useSnapshot: boolean) {
  if (!props.current?.symbol) return
  void router.push({
    name: 'qa',
    query: {
      ...(useSnapshot ? { from_analysis: String(props.current.id) } : {}),
      symbol: props.current.symbol,
      market: props.current.market || 'cn',
      name: stockName.value,
    },
  })
}
async function copyResult() {
  if (!props.current) return
  try {
    await navigator.clipboard.writeText(JSON.stringify(props.current, null, 2))
    message.success('分析结果已复制')
  } catch { message.error('复制失败，请检查浏览器剪贴板权限') }
}
function exportResult() {
  if (!props.current) return
  const blob = new Blob([JSON.stringify(props.current, null, 2)], { type: 'application/json;charset=utf-8' })
  const url = URL.createObjectURL(blob)
  const anchor = document.createElement('a')
  anchor.href = url
  anchor.download = `analysis-${props.current.id}.json`
  anchor.click()
  URL.revokeObjectURL(url)
}
</script>

<template>
  <SectionCard title="分析结论">
    <template #extra>
      <n-button v-if="canDiff" size="tiny" quaternary :loading="diffLoading" @click="openDiff">快照比较</n-button>
      <n-button v-if="canHindsight" size="tiny" quaternary @click="openHindsight()">回溯结果</n-button>
      <n-button v-if="current" size="tiny" quaternary @click="copyResult">复制</n-button>
      <n-button v-if="current" size="tiny" quaternary @click="exportResult">导出</n-button>
    </template>
    <n-spin :show="loading && !current">
      <n-empty v-if="!current" description="尚未选择分析结果。进入页面不会自动发起 AI 请求。" />
      <template v-else>
        <header class="result-head">
          <div class="identity-line">
            <StockIdentity v-if="current.module === 'stock'" :symbol="current.symbol" :market="current.market || 'cn'" :name="stockName" clickable actions />
            <b v-else>{{ current.target || current.title || ANALYSIS_MODULE_LABELS[current.module] }}</b>
            <n-tag size="small" :bordered="false">{{ ANALYSIS_MODULE_LABELS[current.module] }}</n-tag>
            <n-tag v-if="current.as_of" size="small" type="warning" :bordered="false">历史分析 {{ current.as_of }}</n-tag>
          </div>
          <n-tag :type="current.status === 'success' ? 'success' : current.status === 'failed' ? 'error' : current.status === 'degraded' ? 'warning' : 'info'" :bordered="false">{{ analysisStatusLabel(current.status) }}</n-tag>
        </header>

        <n-alert v-if="current.status === 'processing'" type="info" :bordered="false">任务正在后台采集证据并分析，刷新页面后可以恢复；页面不会重复提交。</n-alert>
        <n-alert v-else-if="current.status === 'failed'" type="error" :bordered="false">
          {{ current.error || '分析失败' }}。请先核对任务状态中的数据缺口和错误码，再由按钮明确重试。
        </n-alert>
        <n-alert v-else-if="current.status === 'degraded'" type="warning" :bordered="false">结构化结果不完整，已保留模型原文；这不是正常或中性结论。</n-alert>
        <n-alert v-if="current.stale_mode" type="warning" :bordered="false">{{ current.stale_mode_note || '这是过期行情的历史解释，不是当前盘面建议。' }}</n-alert>

        <section v-if="current.status === 'success' && current.result" class="decision-first">
          <div class="conclusion">
            <span>一句话结论</span>
            <strong>{{ current.result.summary }}</strong>
            <div class="rating-line">
              <n-tag :color="{ color: withAlpha(ratingColor(current.result.rating), 0.12), textColor: ratingColor(current.result.rating) }" :bordered="false">AI 观点：{{ ratingLabel(current.result.rating) }}</n-tag>
              <span>AI 自评 {{ current.result.confidence }}/100</span>
            </div>
          </div>
          <div class="next-action">
            <span>当前更适合做什么</span>
            <b>{{ suggestedAction(current) }}</b>
            <small>AI 观点不会覆盖程序风险等级、持仓卖出等级或任务状态。</small>
          </div>
          <div class="basis-risk">
            <div><span>主要依据</span><ul><li v-for="(line, index) in firstBasis" :key="index">{{ line }}</li><li v-if="!firstBasis.length">依据不足，暂时无法判断</li></ul></div>
            <div><span>主要风险</span><ul><li v-for="(line, index) in firstRisks" :key="index">{{ line }}</li><li v-if="!firstRisks.length">风险数据缺失，不能按无风险处理</li></ul></div>
          </div>
          <div class="freshness-grid">
            <div><span>数据新鲜度</span><b>{{ freshnessLabel(freshness.freshness) }}</b><small>{{ freshness.quoteAsOf || freshness.capturedAt || freshness.note }}</small></div>
            <div><span>结论有效时间</span><b>{{ validUntil }}</b><small>生成于 {{ formatTime(current.created_at) }}</small></div>
          </div>
        </section>

        <section v-if="current.status === 'success' && current.panel" class="panel-result">
          <div v-for="role in current.panel.roles" :key="role.role" class="role-row">
            <b>{{ role.role }}</b><n-tag :bordered="false" :color="{ color: withAlpha(ratingColor(role.rating), 0.12), textColor: ratingColor(role.rating) }">{{ ratingLabel(role.rating) }}</n-tag><span>{{ role.summary }}</span>
          </div>
          <p><b>共识：</b>{{ current.panel.consensus }}</p>
          <p><b>分歧：</b>{{ current.panel.disagreement || '未记录' }}</p>
        </section>

        <pre v-if="current.status === 'degraded'" class="raw-preview">{{ current.raw }}</pre>

        <section class="fact-layers">
          <div>
            <h3>程序事实</h3>
            <div v-if="current.risk_flags?.length" class="fact-tags">
              <n-tag v-for="flag in current.risk_flags" :key="flag.code" :type="riskType(flag)" :bordered="false" :title="flag.text">{{ flag.text || flag.code }}</n-tag>
            </div>
            <p v-else>本次未返回程序风险标志；不代表已证明没有风险。</p>
            <TrustBadges v-if="current.result" :evidence-check="current.result.evidence_check" :sys-confidence="current.result.sys_confidence" :sys-confidence-why="current.result.sys_confidence_why" :review="current.result.review" />
          </div>
          <div>
            <h3>行情事实</h3>
            <p>行情截至 {{ freshness.quoteAsOf || '未知' }}，来源 {{ freshness.quoteSource || '未知' }}；日线截至 {{ freshness.barsAsOf || '未知' }}。</p>
            <p v-if="freshness.freshness !== 'fresh'"><TermHelp :term="freshness.freshness === 'partial' ? 'partial' : 'unknown'" />：{{ freshness.note }}</p>
          </div>
          <div v-if="current.module === 'position'">
            <h3>用户持仓事实</h3>
            <p>这里只展示分析时快照中的本人持仓事实。AI 文字不能改写成本、数量、逐笔风险等级或真实账本。</p>
          </div>
        </section>

        <n-collapse v-model:value="details" class="details">
          <n-collapse-item v-if="current.result" title="完整 AI 观点、风险和失效条件" name="full-result">
            <div v-if="current.result.review" class="review-note">
              <n-alert :type="reviewType(current.result.review.verdict)" :bordered="false">AI 复核：{{ current.result.review.comment || '未补充说明' }}。复核不能修改程序风险等级。</n-alert>
            </div>
            <div class="full-grid">
              <section><h4>完整依据</h4><ul><li v-for="(line, index) in current.result.highlights" :key="index">{{ line }}</li></ul></section>
              <section><h4>机会</h4><ul><li v-for="(line, index) in current.result.opportunities" :key="index">{{ line }}</li></ul></section>
              <section><h4>风险</h4><ul><li v-for="(line, index) in current.result.risks" :key="index">{{ line }}</li></ul></section>
              <section><h4>研究方向</h4><ul><li v-for="(line, index) in current.result.suggestions" :key="index">{{ line }}</li></ul></section>
              <section><h4>反方观点</h4><ul><li v-for="(line, index) in current.result.anti_thesis || []" :key="index">{{ line }}</li></ul></section>
              <section><h4>结论失效条件</h4><ul><li v-for="(line, index) in current.result.kill_switches || []" :key="index">{{ line }}</li></ul></section>
              <section><h4>数据盲区</h4><ul><li v-for="(line, index) in current.result.unknowns || []" :key="index">{{ line }}</li></ul></section>
            </div>
            <p class="disclaimer">{{ current.result.disclaimer }}</p>
          </n-collapse-item>

          <n-collapse-item v-if="current.result?.trade_plan" title="交易计划（与持仓卖出决策分开）" name="plan">
            <n-alert v-if="current.result.trade_plan.no_plan" type="warning" :bordered="false">{{ current.result.trade_plan.no_plan_reason || '数据不足，未生成计划' }}</n-alert>
            <div v-else class="plan-grid">
              <div><span>买入区间</span><b>{{ current.result.trade_plan.buy_low }} - {{ current.result.trade_plan.buy_high }}</b></div>
              <div><span>目标价</span><b>{{ current.result.trade_plan.target_price }}</b></div>
              <div><span>止损价</span><b>{{ current.result.trade_plan.stop_price }}</b></div>
              <div><span>持有周期</span><b>{{ current.result.trade_plan.horizon_days }} 交易日</b></div>
              <div><span>盈亏比</span><b>{{ current.result.trade_plan.rr_ratio }}</b></div>
              <div><span>程序仓位</span><b>{{ current.result.trade_plan.position?.position_pct || '未知' }}%</b></div>
            </div>
            <p>{{ current.result.trade_plan.plan_note }}</p>
            <h4>计划失效条件</h4><ul><li v-for="(line, index) in current.result.trade_plan.invalidators || []" :key="index">{{ line }}</li></ul>
          </n-collapse-item>

          <n-collapse-item v-if="current.result?.debate?.triggered" title="多空辩论（并列观点，不改写主结论）" name="debate">
            <n-alert v-if="current.result.debate.degraded_reason" type="warning" :bordered="false">辩论部分失败：{{ current.result.debate.degraded_reason }}。主分析结果仍保留。</n-alert>
            <div class="debate-grid">
              <section><h4>看多方</h4><p v-for="claim in current.result.debate.bull || []" :key="claim.id"><b>{{ claim.id }}</b> {{ claim.text }}<small v-if="claim.evidence_ids?.length">证据 {{ claim.evidence_ids.join('、') }}</small></p></section>
              <section><h4>看空方</h4><p v-for="claim in current.result.debate.bear || []" :key="claim.id"><b>{{ claim.id }}</b> {{ claim.text }}<small v-if="claim.evidence_ids?.length">证据 {{ claim.evidence_ids.join('、') }}</small></p></section>
            </div>
            <p v-if="current.result.debate.judge"><b>独立裁决：</b>{{ ratingLabel(current.result.debate.judge.verdict) }} · {{ current.result.debate.judge.confidence_reason }}</p>
          </n-collapse-item>

          <n-collapse-item title="证据与专业指标" name="metrics">
            <div class="metric-table">
              <div v-for="metric in metrics" :key="metric.label" class="metric-row">
                <b>{{ metric.label }}</b><span>{{ metric.missing ? '缺失，不能按中性处理' : metric.value }}</span><span>计算 / 采集：{{ metric.asOf || '未知' }}</span><span>范围：{{ metric.range }}</span>
              </div>
            </div>
            <div class="term-index">
              <TermHelp term="alpha" /><TermHelp term="beta" /><TermHelp term="mfe" /><TermHelp term="mae" />
              <TermHelp term="twr" /><TermHelp term="sharpe" /><TermHelp term="sortino" /><TermHelp term="atr" />
              <TermHelp term="rps" /><TermHelp term="ic" /><TermHelp term="icir" /><TermHelp term="rankic" /><TermHelp term="regime" />
            </div>
            <n-button v-if="snapshotText" size="small" @click="snapshotShow = true">查看完整数据快照</n-button>
          </n-collapse-item>

          <n-collapse-item title="原始结果与审计元数据" name="raw">
            <dl class="meta-grid">
              <div><dt>分析时点</dt><dd>{{ formatTime(current.created_at) }}</dd></div>
              <div><dt><TermHelp term="as_of" /></dt><dd>{{ freshness.quoteAsOf || current.as_of || '未知' }}</dd></div>
              <div><dt>模型</dt><dd>{{ current.provider }} / {{ current.model }}</dd></div>
              <div><dt>版本</dt><dd>{{ current.prompt_version }} / {{ current.strategy_version }}</dd></div>
              <div><dt>AI 用量</dt><dd>{{ current.total_tokens || 0 }} Token</dd></div>
              <div><dt>耗时</dt><dd>{{ current.latency_ms ? `${(current.latency_ms / 1000).toFixed(1)} 秒` : '未知' }}</dd></div>
            </dl>
            <n-button size="small" @click="rawShow = true">查看原始结果</n-button>
          </n-collapse-item>
        </n-collapse>

        <footer v-if="current.module === 'stock' && current.symbol" class="actions">
          <n-button size="small" @click="ask(true)">沿用本次快照继续问答</n-button>
          <n-button size="small" @click="ask(false)">按最新数据提问</n-button>
        </footer>
      </template>
    </n-spin>

    <n-modal v-model:show="snapshotShow" preset="card" title="数据快照" :style="{ width: 'min(760px, calc(100vw - 24px))' }"><pre class="snapshot-pre">{{ snapshotText }}</pre></n-modal>
    <n-modal v-model:show="rawShow" preset="card" title="原始分析结果" :style="{ width: 'min(760px, calc(100vw - 24px))' }"><pre class="snapshot-pre">{{ current?.result_json || current?.raw || '无原始结果' }}</pre></n-modal>
    <n-modal v-model:show="diffShow" preset="card" title="快照比较" :style="{ width: 'min(700px, calc(100vw - 24px))' }">
      <div v-if="diff" class="diff-body">
        <p>上次分析：{{ diff.prev_title }} · {{ formatTime(diff.prev_at) }}</p>
        <div class="diff-highlight"><span>评级</span><b :style="{ color: ratingColor(diff.rating_from) }">{{ ratingLabel(diff.rating_from) }}</b><span>→</span><b :style="{ color: ratingColor(diff.rating_to) }">{{ ratingLabel(diff.rating_to) }}</b></div>
        <div class="diff-highlight"><span>AI 自评</span><b>{{ diff.confidence_from }}</b><span>→</span><b>{{ diff.confidence_to }}（{{ diff.confidence_delta > 0 ? '+' : '' }}{{ diff.confidence_delta }}）</b></div>
        <section><h4>结论变化</h4><p>{{ diff.summary_prev }}</p><p><b>本次：</b>{{ diff.summary_now }}</p></section>
        <section v-if="diff.highlights_added.length"><h4>新增依据</h4><ul><li v-for="line in diff.highlights_added" :key="line">{{ line }}</li></ul></section>
        <section v-if="diff.risks_added.length"><h4>新增风险</h4><ul><li v-for="line in diff.risks_added" :key="line">{{ line }}</li></ul></section>
        <p class="disclaimer">比较仅展示已保存字段变化，不会重新调用 AI 或改写任一快照。</p>
      </div>
    </n-modal>
    <n-modal v-model:show="hindsightShow" preset="card" title="回溯结果（不属于实时建议）" :style="{ width: 'min(680px, calc(100vw - 24px))' }">
      <n-spin :show="hindsightLoading">
        <n-empty v-if="!hindsight" description="正在读取已落库行情事实" />
        <div v-else class="hindsight-body">
          <StockIdentity :symbol="hindsight.symbol" market="cn" :name="hindsight.name" />
          <p>分析基准 {{ hindsight.base_date }} · 基准价 {{ hindsight.base_price.toFixed(2) }} · 已经过 {{ hindsight.elapsed_bars }} 个交易日</p>
          <div class="return-grid">
            <div v-for="key in ['d5', 'd10', 'd20', 'd60'] as const" :key="key"><span>+{{ key.slice(1) }} 日</span><b :style="{ color: hindsight.returns[key] ? pctColor(hindsight.returns[key]!.return_pct) : undefined }">{{ hindsight.returns[key] ? signed(hindsight.returns[key]!.return_pct) : '尚未成熟' }}</b></div>
          </div>
          <p>最大上涨 {{ signed(hindsight.max_gain_pct) }} · 最大回撤 -{{ hindsight.max_drawdown_pct.toFixed(2) }}% · <TermHelp term="alpha" /> {{ signed(hindsight.alpha_pct) }}</p>
          <div class="touch-form"><n-input-number v-model:value="targetPrice" :min="0" placeholder="目标价" /><n-input-number v-model:value="stopPrice" :min="0" placeholder="止损价" /><n-button :loading="hindsightLoading" @click="openHindsight(true)">验证价位首触</n-button></div>
          <p>{{ hindsight.note }}</p>
        </div>
      </n-spin>
    </n-modal>
  </SectionCard>
</template>

<style scoped>
.result-head,
.identity-line,
.rating-line,
.fact-tags,
.actions,
.term-index,
.touch-form { display: flex; min-width: 0; align-items: center; flex-wrap: wrap; gap: 8px; }
.result-head { justify-content: space-between; margin-bottom: 12px; }
.decision-first { display: grid; gap: 16px; margin-top: 14px; }
.conclusion,
.next-action { display: grid; gap: 6px; }
.conclusion > span,
.next-action > span,
.basis-risk span,
.freshness-grid span,
.plan-grid span { font-size: 12px; opacity: .65; }
.conclusion > strong { font-size: 20px; line-height: 1.45; overflow-wrap: anywhere; }
.next-action > b { font-size: 15px; }
.next-action small { opacity: .62; }
.basis-risk,
.freshness-grid,
.fact-layers,
.full-grid,
.debate-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
.basis-risk > div,
.fact-layers > div { min-width: 0; padding: 12px; border: 1px solid v-bind('vars.borderColor'); border-radius: 6px; }
.basis-risk ul,
.full-grid ul { margin: 6px 0 0; padding-left: 18px; line-height: 1.6; }
.freshness-grid > div { display: grid; gap: 4px; padding: 10px 0; border-top: 1px solid v-bind('vars.dividerColor'); }
.freshness-grid small { opacity: .58; }
.panel-result,
.fact-layers,
.details { margin-top: 16px; }
.role-row { display: grid; grid-template-columns: 100px auto minmax(0, 1fr); gap: 8px; align-items: start; padding: 9px 0; border-bottom: 1px solid v-bind('vars.dividerColor'); }
.raw-preview,
.snapshot-pre { max-width: 100%; max-height: 62vh; margin: 12px 0 0; overflow: auto; white-space: pre-wrap; word-break: break-word; }
.fact-layers { grid-template-columns: repeat(3, minmax(0, 1fr)); }
.fact-layers h3 { margin: 0 0 8px; font-size: 14px; }
.fact-layers p { margin: 6px 0 0; font-size: 12px; line-height: 1.55; opacity: .72; }
.full-grid h4,
.debate-grid h4 { margin: 0 0 6px; }
.full-grid section,
.debate-grid section { min-width: 0; }
.plan-grid,
.meta-grid { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 10px; }
.plan-grid > div { display: grid; gap: 3px; }
.debate-grid p { display: grid; gap: 3px; }
.debate-grid small { opacity: .6; }
.metric-table { display: grid; }
.metric-row { display: grid; grid-template-columns: minmax(130px,.7fr) minmax(160px,1fr) minmax(180px,1fr) minmax(130px,.8fr); gap: 8px; padding: 9px 0; border-bottom: 1px solid v-bind('vars.dividerColor'); font-size: 12px; }
.term-index { margin: 14px 0; gap: 8px 14px; }
.meta-grid { margin: 0 0 12px; }
.meta-grid div { min-width: 0; }
.meta-grid dt { font-size: 11px; opacity: .58; }
.meta-grid dd { margin: 2px 0 0; overflow-wrap: anywhere; }
.disclaimer { font-size: 11px; opacity: .58; }
.actions { margin-top: 14px; }
.diff-highlight { display: grid; grid-template-columns: 100px 1fr auto 1fr; gap: 8px; padding: 8px 0; }
.return-grid { display: grid; grid-template-columns: repeat(4, minmax(0, 1fr)); gap: 8px; }
.return-grid > div { display: grid; gap: 4px; padding: 10px; border: 1px solid v-bind('vars.borderColor'); border-radius: 6px; }
.return-grid span { font-size: 11px; opacity: .6; }
.touch-form :deep(.n-input-number) { width: 140px; }
@media (max-width: 820px) {
  .fact-layers { grid-template-columns: 1fr; }
  .metric-row { grid-template-columns: 1fr 1fr; }
}
@media (max-width: 600px) {
  .basis-risk,
  .freshness-grid,
  .full-grid,
  .debate-grid,
  .plan-grid,
  .meta-grid { grid-template-columns: 1fr; }
  .conclusion > strong { font-size: 17px; }
  .role-row { grid-template-columns: 1fr auto; }
  .role-row span { grid-column: 1 / -1; }
  .metric-row { grid-template-columns: 1fr; gap: 3px; }
  .return-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
  .actions > * { flex: 1 1 auto; }
}
</style>
