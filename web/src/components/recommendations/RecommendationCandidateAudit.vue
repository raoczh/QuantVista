<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NCollapse, NCollapseItem, NEmpty, NTag } from 'naive-ui'
import type { PoolCandidate, RecommendationView } from '@/api/recommendation'
import { SCORE_PROFILE_LABEL } from '@/api/screener'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'
import { useUi } from '@/composables/useUi'
import { entryQualityLabel, exclusionReason, omittedCandidates, parseCandidateSnapshot, parseRejectedSnapshot, parseSourceCoverage, rankedScore, scoreComponentLabel, scoringVersionLabel } from './recommendationPresentation'

const props = defineProps<{ current: RecommendationView }>()
const { pctColor, vars } = useUi()
const sections = defineModel<string[]>('sections', { default: () => [] })

function parseObject(raw?: string): unknown {
  if (!raw) return null
  try { return JSON.parse(raw) }
  catch { return raw }
}

const poolSnapshot = computed(() => parseCandidateSnapshot(props.current.candidate_pool))
const pool = computed(() => poolSnapshot.value.items)
const eligible = computed(() => pool.value.filter((item) => !item.excluded).sort((a, b) => (a.rank || 9999) - (b.rank || 9999)))
const excluded = computed(() => pool.value.filter((item) => !!item.excluded))
const rejectedSnapshot = computed(() => parseRejectedSnapshot(props.current.rejected_json))
const rejected = computed(() => rejectedSnapshot.value.items)
const omitted = computed(() => omittedCandidates(props.current.filters_json))
const coverage = computed(() => parseSourceCoverage(props.current.filters_json))
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

    <n-alert v-if="poolSnapshot.invalid || rejectedSnapshot.invalid" type="warning" :bordered="false" class="snapshot-warning">部分历史候选或落选快照损坏，以下只展示可核验条目，数量不代表当时的完整候选池。</n-alert>
    <n-alert v-if="omitted" type="info" :bordered="false" class="snapshot-warning">本批快照省略了 {{ omitted }} 条排除记录；下方排除数量仅统计已留存的条目。</n-alert>
    <n-collapse v-model:expanded-names="sections">
      <n-collapse-item v-if="coverage.length" title="来源覆盖与预算" name="sources">
        <p class="raw-note">按完整候选事实统计；同一股票可能属于多个来源，各来源数量不能直接相加。预算限制与策略不满足会分别记录。</p>
        <dl class="source-coverage">
          <div v-for="row in coverage" :key="row.source">
            <dt>{{ sourceLabels[row.source] || row.source }}</dt>
            <dd>观察 {{ row.observed }} · 富化 {{ row.intake }} · 评分 {{ row.scored }} · 送模 {{ row.sent }}</dd>
            <dd v-if="row.omitted">超出观察上限，另省略 {{ row.omitted }} 只</dd>
          </div>
        </dl>
      </n-collapse-item>
      <n-collapse-item :title="`候选池（${eligible.length}）`" name="pool">
        <n-empty v-if="!eligible.length" description="本批没有保存可展示的候选池快照" size="small" />
        <div v-else class="candidate-list">
          <div v-for="item in eligible" :key="`${item.market}:${item.symbol}`" class="candidate-row">
            <StockIdentity :symbol="item.symbol" :market="item.market || 'cn'" :name="item.name" density="table" clickable />
            <span>{{ sources(item) }}</span>
            <span class="qv-tnum">
              排名 {{ item.rank || '—' }} · 量化分 {{ rankedScore(item.score, item.rank)?.toFixed(1) ?? '—' }}
              <small v-if="item.ranking_score != null && rankedScore(item.score, item.rank) !== item.ranking_score" class="ranking-detail">完整排序分 {{ item.ranking_score }}</small>
              <small v-if="item.entry_quality" class="ranking-detail">{{ entryQualityLabel(item.entry_quality.status) }}</small>
              <small v-if="item.preselection?.rank" class="ranking-detail">全量命中预选 {{ item.preselection.rank }}/{{ item.preselection.matched }}<template v-if="item.preselection.truncated"> · 截断 {{ item.preselection.truncated }}</template></small>
            </span>
            <n-tag v-if="item.strategy_hit" size="tiny" :type="item.strategy_hit.full ? 'success' : 'default'" :bordered="false"
              :title="[...(item.strategy_hit.matched || []), ...(item.strategy_hit.missed || []).map((m) => '✗ ' + m), ...(item.strategy_hit.unknown || []).map((m) => '缺数据：' + m)].join('；')">
              收盘策略 {{ item.strategy_hit.hit }}/{{ item.strategy_hit.total }}
            </n-tag>
            <span class="qv-tnum" :style="{ color: pctColor(item.change_pct) }">
              {{ item.change_pct > 0 ? '+' : '' }}{{ item.change_pct.toFixed(2) }}%
              <small v-if="item.time_facts" class="ranking-detail">信号 {{ item.time_facts.signal_date }}<template v-if="item.time_facts.current_returns?.['5'] != null"> · 至现价 5 日 {{ item.time_facts.current_returns['5'].toFixed(1) }}%</template></small>
            </span>
            <n-tag v-if="selectedSymbols.has(`${item.market || 'cn'}:${item.symbol}`)" size="tiny" type="success" :bordered="false">最终推荐</n-tag>
            <n-tag v-else-if="item.sent_to_llm" size="tiny" type="info" :bordered="false">进入 AI 名单</n-tag>
            <span v-else>仅参与量化排名</span>
            <details v-if="item.score_breakdown || item.scoring_comparison" class="candidate-evidence">
              <summary>查看质量分组与评分对照</summary>
              <p v-if="item.scoring_comparison">同一候选：原加法 {{ item.scoring_comparison.legacy_score.toFixed(2) }} · 质量规则 {{ item.scoring_comparison.quality_score.toFixed(2) }}<template v-if="item.scoring_comparison.learned_score != null"> · 学习排序 {{ item.scoring_comparison.learned_score.toFixed(2) }}</template>。本批生效 {{ scoringVersionLabel(current.scoring_version) }}。</p>
              <ul v-if="item.score_breakdown">
                <li v-for="part in item.score_breakdown.components" :key="part.key">{{ scoreComponentLabel(part.key) }} {{ part.value > 0 ? '+' : '' }}{{ part.value.toFixed(2) }}<template v-if="part.notes?.length">：{{ part.notes.join('；') }}</template><template v-if="part.status === 'missing'">（数据缺失）</template></li>
                <li v-for="gap in item.score_breakdown.missing || []" :key="gap">缺少：{{ gap }}</li>
              </ul>
              <small>质量分组解释质量规则；它不代表其他算法的生效总分，也不是收益概率。</small>
            </details>
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
          <div><dt>评分侧重</dt><dd>{{ current.score_profile ? SCORE_PROFILE_LABEL[current.score_profile] || current.score_profile : '历史未记录' }}</dd></div>
          <div><dt>评分配置版本</dt><dd>{{ current.profile_version || '历史未记录' }}</dd></div>
          <div><dt>评分算法</dt><dd>{{ scoringVersionLabel(current.scoring_version) }}<template v-if="current.scoring_artifact_id"> · 模型 #{{ current.scoring_artifact_id }}</template></dd></div>
          <div v-if="current.scoring_artifact_hash"><dt>模型摘要</dt><dd class="qv-mono">{{ current.scoring_artifact_hash }}</dd></div>
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
.snapshot-warning { margin-bottom: 12px; }
.source-coverage { display: grid; grid-template-columns: repeat(auto-fit, minmax(200px, 1fr)); gap: 12px; }
.source-coverage dd { margin: 3px 0 0; font-size: 12px; overflow-wrap: anywhere; }
.candidate-audit { margin-top: 18px; padding-top: 16px; border-top: 1px solid v-bind('vars.dividerColor'); }
header,
.counts { display: flex; min-width: 0; align-items: flex-start; justify-content: space-between; flex-wrap: wrap; gap: 8px; }
h3 { margin: 0; font-size: 15px; }
header p { margin: 4px 0 12px; font-size: 12px; opacity: 0.66; }
.candidate-list { display: grid; }
.candidate-row {
  display: grid;
  grid-template-columns: minmax(180px, 1.2fr) minmax(120px, 1fr) auto auto auto auto;
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
.candidate-row > * { min-width: 0; }
.ranking-detail { display: block; font-size: 11px; opacity: .7; overflow-wrap: anywhere; }
.candidate-evidence { grid-column: 1 / -1; line-height: 1.7; overflow-wrap: anywhere; }
.candidate-evidence summary { cursor: pointer; }
.candidate-evidence ul { padding-left: 20px; }
.reason { overflow-wrap: anywhere; }
.version-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 8px 16px; margin: 0; }
.version-grid div { min-width: 0; }
.version-grid dt { font-size: 11px; opacity: 0.58; }
.version-grid dd { margin: 2px 0 0; overflow-wrap: anywhere; }
.raw-note { margin: 14px 0 6px; font-size: 12px; opacity: .66; overflow-wrap: anywhere; }
.raw-diagnostics { max-width: 100%; max-height: 360px; margin: 0; overflow: auto; white-space: pre-wrap; overflow-wrap: anywhere; font-size: 11px; }
@media (max-width: 768px) {
  .candidate-row,
  .candidate-row.excluded,
  .candidate-row.rejected { grid-template-columns: 1fr; gap: 3px; }
  .version-grid { grid-template-columns: 1fr; }
}
</style>
