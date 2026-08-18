<script setup lang="ts">
import { computed } from 'vue'
import { NCollapse, NCollapseItem, NEmpty, NTag } from 'naive-ui'
import type { PoolCandidate, RecommendationView, RecReject } from '@/api/recommendation'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'
import { useUi } from '@/composables/useUi'
import { exclusionReason } from './recommendationPresentation'

const props = defineProps<{ current: RecommendationView }>()
const { pctColor, vars } = useUi()
const sections = defineModel<string[]>('sections', { default: () => [] })

function parseArray<T>(raw?: string): T[] {
  if (!raw) return []
  try {
    const parsed = JSON.parse(raw)
    return Array.isArray(parsed) ? parsed : []
  } catch {
    return []
  }
}

function parseObject(raw?: string): unknown {
  if (!raw) return null
  try { return JSON.parse(raw) }
  catch { return raw }
}

const pool = computed(() => parseArray<PoolCandidate>(props.current.candidate_pool))
const eligible = computed(() => pool.value.filter((item) => !item.excluded).sort((a, b) => (a.rank || 9999) - (b.rank || 9999)))
const excluded = computed(() => pool.value.filter((item) => !!item.excluded))
const rejected = computed(() => parseArray<RecReject>(props.current.rejected_json))
const selectedSymbols = computed(() => new Set(props.current.items.map((item) => `${item.market || 'cn'}:${item.symbol}`)))
const technicalDiagnostics = computed(() => JSON.stringify({
  market_regime: parseObject(props.current.regime_json),
  llm_runs_and_coverage: parseObject(props.current.llm_run_json),
  reflection_shadow: parseObject(props.current.reflection_json),
}, null, 2))
const sourceLabels: Record<string, string> = {
  watchlist: '自选', gainer: '涨幅榜', active: '成交额榜', turnover: '换手率榜', dipper: '回调榜',
  lowpb: '低 PB 榜', strategy_signal: '策略信号', daily_discovery: '全市场发现', recent_discovery: '近 5 日候选记忆',
}
function sources(item: PoolCandidate) {
  const values = item.sources?.length ? item.sources : item.source ? [item.source] : []
  return values.map((value) => sourceLabels[value] || value).join(' / ') || '来源未知'
}
</script>

<template>
  <section class="candidate-audit">
    <header>
      <div>
        <h3>候选池与召回审计</h3>
        <p>候选、排除、AI 名单和最终结果分开显示。最终推荐只能来自本批候选池。</p>
      </div>
      <div class="counts">
        <n-tag size="small" :bordered="false">候选 {{ eligible.length }}</n-tag>
        <n-tag size="small" :bordered="false">排除 {{ excluded.length }}</n-tag>
        <n-tag size="small" :bordered="false">最终 {{ current.items.length }}</n-tag>
      </div>
    </header>

    <n-collapse v-model:value="sections">
      <n-collapse-item :title="`候选池（${eligible.length}）`" name="pool">
        <n-empty v-if="!eligible.length" description="本批没有保存可展示的候选池快照" size="small" />
        <div v-else class="candidate-list">
          <div v-for="item in eligible" :key="`${item.market}:${item.symbol}`" class="candidate-row">
            <StockIdentity :symbol="item.symbol" :market="item.market || 'cn'" :name="item.name" density="table" clickable />
            <span>{{ sources(item) }}</span>
            <span class="qv-tnum">排名 {{ item.rank || '—' }} · 量化分 {{ item.score?.toFixed(1) || '—' }}</span>
            <span class="qv-tnum" :style="{ color: pctColor(item.change_pct) }">{{ item.change_pct > 0 ? '+' : '' }}{{ item.change_pct.toFixed(2) }}%</span>
            <n-tag v-if="selectedSymbols.has(`${item.market || 'cn'}:${item.symbol}`)" size="tiny" type="success" :bordered="false">最终推荐</n-tag>
            <n-tag v-else-if="item.sent_to_llm" size="tiny" type="info" :bordered="false">进入 AI 名单</n-tag>
            <span v-else>仅参与规则排名</span>
          </div>
        </div>
      </n-collapse-item>
      <n-collapse-item :title="`筛选排除（${excluded.length}）`" name="excluded">
        <n-empty v-if="!excluded.length" description="没有保存排除记录" size="small" />
        <div v-else class="candidate-list">
          <div v-for="item in excluded" :key="`x:${item.market}:${item.symbol}`" class="candidate-row excluded">
            <StockIdentity :symbol="item.symbol" :market="item.market || 'cn'" :name="item.name" density="table" clickable />
            <span>{{ sources(item) }}</span>
            <span class="reason">{{ exclusionReason(item.excluded || '') }}</span>
          </div>
        </div>
      </n-collapse-item>
      <n-collapse-item :title="`池内落选（${rejected.length}）`" name="rejected">
        <n-empty v-if="!rejected.length" description="没有保存池内落选解释" size="small" />
        <div v-else class="candidate-list">
          <div v-for="(item, index) in rejected" :key="`${item.symbol}:${index}`" class="candidate-row rejected">
            <StockIdentity :symbol="item.symbol" market="cn" :name="item.name" density="table" clickable />
            <span class="reason">{{ item.reason || '不满足当前策略条件' }}</span>
          </div>
        </div>
      </n-collapse-item>
      <n-collapse-item title="版本与原始技术字段" name="raw">
        <dl class="version-grid">
          <div><dt>策略版本</dt><dd>{{ current.strategy_version || '未知' }}</dd></div>
          <div><dt>Prompt 版本</dt><dd>{{ current.prompt_version || '未知' }}</dd></div>
          <div><dt><TermHelp term="as_of" /></dt><dd>以各推荐卡片行情时点为准</dd></div>
          <div><dt>候选数量</dt><dd>{{ current.candidate_count }}</dd></div>
          <div><dt>调用追踪</dt><dd class="qv-mono">{{ current.trace_id || '未记录' }}</dd></div>
        </dl>
        <p class="raw-note">以下是只读运行诊断，用于核对市场状态、召回覆盖和反思影子命中；这些字段不会在前端重算或改写推荐事实。</p>
        <pre class="raw-diagnostics">{{ technicalDiagnostics }}</pre>
      </n-collapse-item>
    </n-collapse>
  </section>
</template>

<style scoped>
.candidate-audit { margin-top: 18px; padding-top: 16px; border-top: 1px solid v-bind('vars.dividerColor'); }
header,
.counts { display: flex; min-width: 0; align-items: flex-start; justify-content: space-between; flex-wrap: wrap; gap: 8px; }
h3 { margin: 0; font-size: 15px; }
header p { margin: 4px 0 12px; font-size: 12px; opacity: 0.66; }
.candidate-list { display: grid; }
.candidate-row {
  display: grid;
  grid-template-columns: minmax(180px, 1.2fr) minmax(120px, 1fr) auto auto auto;
  min-width: 0;
  align-items: center;
  gap: 8px 14px;
  padding: 9px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
  font-size: 12px;
}
.candidate-row.excluded,
.candidate-row.rejected { grid-template-columns: minmax(180px, 0.7fr) minmax(120px, 0.5fr) minmax(240px, 1fr); }
.candidate-row.rejected { grid-template-columns: minmax(180px, 0.7fr) minmax(240px, 1.5fr); }
.reason { overflow-wrap: anywhere; }
.version-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px 16px; margin: 0; }
.version-grid div { min-width: 0; }
.version-grid dt { font-size: 11px; opacity: 0.58; }
.version-grid dd { margin: 2px 0 0; overflow-wrap: anywhere; }
.raw-note { margin: 14px 0 6px; font-size: 12px; opacity: .66; }
.raw-diagnostics { max-width: 100%; max-height: 360px; margin: 0; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-size: 11px; }
@media (max-width: 760px) {
  .candidate-row,
  .candidate-row.excluded,
  .candidate-row.rejected { grid-template-columns: 1fr; gap: 3px; }
  .version-grid { grid-template-columns: 1fr; }
}
</style>
