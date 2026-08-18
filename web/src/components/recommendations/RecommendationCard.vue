<script setup lang="ts">
import { computed, ref } from 'vue'
import { NButton, NCollapse, NCollapseItem, NTag } from 'naive-ui'
import type { PoolCandidate, RecommendationItem, RecType } from '@/api/recommendation'
import { useUi } from '@/composables/useUi'
import { useStockActions } from '@/composables/useStockActions'
import StockIdentity from '@/components/StockIdentity.vue'
import TrustBadges from '@/components/TrustBadges.vue'
import TermHelp from '@/components/TermHelp.vue'
import {
  confidenceExplanation,
  recommendationDecisionState,
  RECOMMENDATION_DECISION_LABEL,
  trackingState,
  TRACKING_STATE_LABEL,
} from './recommendationPresentation'

const props = defineProps<{
  item: RecommendationItem
  type: RecType
  candidate?: PoolCandidate
}>()
const emit = defineEmits<{ (event: 'stop-alert', item: RecommendationItem): void }>()

const { downColor, pctColor, vars } = useUi()
const { goDetail, goRecommendationReview, goAlert, addToWatchlist, goPositionDecision, goPositionFromRecommendation } = useStockActions()
const detailSections = ref<string[]>([])
const decision = computed(() => recommendationDecisionState(props.item))
const tracking = computed(() => trackingState(props.item.status))
const stock = computed(() => ({ symbol: props.item.symbol, market: props.item.market || 'cn', name: props.item.name || '' }))
const sourceText = computed(() => {
  const sources = props.candidate?.sources?.length
    ? props.candidate.sources
    : props.candidate?.source
      ? [props.candidate.source]
      : []
  const labels: Record<string, string> = {
    watchlist: '自选', gainer: '涨幅榜', active: '成交额榜', turnover: '换手率榜', dipper: '回调榜',
    lowpb: '低 PB 榜', strategy_signal: '策略信号', daily_discovery: '全市场发现', recent_discovery: '近期候选记忆',
  }
  return sources.map((source) => labels[source] || source).join(' / ') || '来源记录未提供'
})
const asOf = computed(() => props.item.detail?.quote_as_of || props.item.detail?.execution_plan?.data_as_of || '数据时点未知')
const isHeld = computed(() => !!props.item.position && props.item.position.status === 'holding')
const actionLabel = computed(() => props.item.action === 'buy' ? '继续研究买入条件' : '保持观察')
const firstReason = computed(() => props.item.detail?.reason?.[0] || props.item.summary || '暂无可复述的推荐理由')
const firstRisk = computed(() => props.item.detail?.risks?.[0] || '风险信息不足，不能按无风险处理')

function revealEvidence() {
  detailSections.value = detailSections.value.includes('evidence') ? [] : ['evidence']
}
</script>

<template>
  <article class="recommendation-card">
    <header class="card-head">
      <StockIdentity
        :symbol="item.symbol"
        :market="item.market"
        :name="item.name"
        clickable
        actions
        :has-position="isHeld"
        :position-id="item.position?.position_id"
        :recommendation-id="item.id"
      />
      <div class="state-tags">
        <n-tag size="small" round :bordered="false" :type="decision === 'buy_research' ? 'success' : decision === 'insufficient' ? 'warning' : 'default'">
          {{ RECOMMENDATION_DECISION_LABEL[decision] }}
        </n-tag>
        <n-tag v-if="item.detail?.degraded_source" size="small" type="warning" :bordered="false">规则降级结果</n-tag>
      </div>
    </header>

    <div class="decision-summary">
      <div class="summary-copy">
        <span>一句话结论</span>
        <strong>{{ item.summary || firstReason }}</strong>
      </div>
      <div class="decision-facts">
        <div><span>推荐动作</span><b>{{ actionLabel }}</b></div>
        <div><span><TermHelp term="confidence" /></span><b>{{ confidenceExplanation(item.confidence, item.detail?.sys_confidence) }}</b></div>
        <div><span>AI 自评</span><b class="qv-tnum">{{ item.confidence }}/100</b></div>
      </div>
    </div>

    <div class="reason-risk">
      <div><span>推荐理由</span><p>{{ firstReason }}</p></div>
      <div><span>主要风险</span><p :style="{ color: downColor }">{{ firstRisk }}</p></div>
    </div>

    <div class="data-line">
      <span><TermHelp term="as_of" />：{{ asOf }}</span>
      <span>来源：{{ sourceText }}</span>
      <span>参考价 <b class="qv-tnum">{{ item.ref_price > 0 ? item.ref_price.toFixed(2) : '未知' }}</b></span>
    </div>

    <div v-if="isHeld" class="ownership held">
      <b>这是你的持仓</b>
      <span>普通推荐结论不能当作卖出结论；卖出判断以该笔持仓 #{{ item.position?.position_id }} 的成本和风险事实为准。</span>
      <span>持仓 {{ item.position?.quantity }} 股 · 成本 {{ item.position?.buy_price?.toFixed(2) }} · 买入日 {{ item.position?.buy_date || '未知' }}</span>
      <span v-if="item.status?.actual_return_pct != null" class="qv-tnum" :style="{ color: pctColor(item.status.actual_return_pct) }">实际持仓收益 {{ item.status.actual_return_pct > 0 ? '+' : '' }}{{ item.status.actual_return_pct.toFixed(2) }}%</span>
    </div>
    <div v-else class="ownership">
      <b>当前未关联本人持仓</b>
      <span>仅供研究追踪，不代表持仓建议，也不会自动下单。</span>
    </div>

    <div v-if="item.status" class="tracking-band">
      <n-tag size="small" :bordered="false" :type="tracking === 'insufficient' ? 'warning' : tracking === 'settled' ? 'success' : 'default'">
        {{ TRACKING_STATE_LABEL[tracking] }}
      </n-tag>
      <span v-if="tracking === 'immature'">尚未到结算时间，不评价为准确或失败。</span>
      <span v-else-if="tracking === 'settled'">结算事实：{{ item.status.outcome === 'take_profit' ? '触及止盈' : '触及止损' }}。</span>
      <span v-else-if="tracking === 'expired'">推荐有效期结束，历史结论不再代表当前盘面。</span>
      <span v-else-if="tracking === 'insufficient'">缺少可用价格数据，不能给出追踪结论。</span>
      <span v-else>仍在正常跟踪。</span>
      <span v-if="tracking !== 'insufficient'" class="qv-tnum" :style="{ color: pctColor(item.status.return_pct) }">
        跟踪收益 {{ item.status.return_pct > 0 ? '+' : '' }}{{ item.status.return_pct.toFixed(2) }}%
      </span>
      <span v-if="item.status.last_eval_date">截至 {{ item.status.last_eval_date }}</span>
    </div>

    <TrustBadges
      v-if="item.detail"
      :quant-score="item.detail.quant_score"
      :quant-rank="item.detail.quant_rank"
      :pool-size="item.detail.pool_size"
      :lot-cost="item.detail.lot_cost || item.ref_price * 100"
      :evidence-check="item.detail.evidence_check"
      :sys-confidence="item.detail.sys_confidence"
      :sys-confidence-why="item.detail.sys_confidence_why"
      :review="item.detail.review"
    />

    <n-collapse v-if="item.detail" v-model:value="detailSections" class="evidence-collapse">
      <n-collapse-item title="完整依据与专业信息" name="evidence">
        <div class="evidence-grid">
          <section>
            <h4>完整理由</h4>
            <ul><li v-for="(line, index) in item.detail.reason || []" :key="index">{{ line }}</li></ul>
          </section>
          <section>
            <h4>完整风险</h4>
            <ul><li v-for="(line, index) in item.detail.risks || []" :key="index">{{ line }}</li></ul>
          </section>
          <section v-if="item.detail.evidence?.length">
            <h4>行情与程序证据</h4>
            <ul><li v-for="(line, index) in item.detail.evidence" :key="index">{{ line }}</li></ul>
          </section>
          <section v-if="item.detail.bear">
            <h4>AI 反方观点</h4>
            <p>{{ item.detail.bear.bear_case }}</p>
            <small>影子意见，不改写推荐动作、风险事实或程序评分。</small>
          </section>
          <section v-if="item.detail.quality_gate">
            <h4>数据完整度</h4>
            <p v-if="item.detail.quality_gate.missing_critical_fields?.length">缺少：{{ item.detail.quality_gate.missing_critical_fields.join('、') }}</p>
            <p v-if="item.detail.quality_gate.senti_missing">新闻情绪缺失，不代表情绪中性。</p>
          </section>
          <section v-if="item.detail.execution_plan">
            <h4>研究预算适配</h4>
            <p v-if="item.detail.execution_plan.status === 'ready'">研究预算 {{ item.detail.execution_plan.planned_capital.toFixed(2) }}，参考数量 {{ item.detail.execution_plan.quantity }} 股，估算占用 {{ item.detail.execution_plan.estimated_capital.toFixed(2) }}。</p>
            <p v-else>{{ item.detail.execution_plan.unavailable_reasons?.join('；') || '当前不适合形成数量参考。' }}</p>
            <small>这是研究预算估算，不读取券商现金，也不会自动下单。</small>
          </section>
          <section v-if="item.detail.discovery">
            <h4>候选召回轨迹</h4>
            <p>近 5 日出现 {{ item.detail.discovery.seen_days_5d }} 天，连续出现 {{ item.detail.discovery.consecutive_days }} 天；首次 {{ item.detail.discovery.first_seen_date }}，最近 {{ item.detail.discovery.last_seen_date }}。</p>
            <small v-if="item.detail.discovery.partial_reason">数据不完整：{{ item.detail.discovery.partial_reason }}</small>
          </section>
          <section>
            <h4>有效条件</h4>
            <p>{{ item.detail.invalidation || '未提供明确失效条件，应按数据不足处理。' }}</p>
            <p v-if="type === 'short_term'">买入区间 {{ item.detail.buy_zone_low }} - {{ item.detail.buy_zone_high }}；止盈 {{ item.detail.take_profit }}；止损 {{ item.detail.stop_loss }}；有效 {{ item.detail.valid_days || '未知' }} 个交易日。</p>
            <p v-else>估值区间 {{ item.detail.valuation_low }} - {{ item.detail.valuation_high }}；复盘周期 {{ item.detail.review_cycle || '未知' }}。</p>
          </section>
        </div>
        <p class="disclaimer">{{ item.detail.disclaimer }}</p>
      </n-collapse-item>
    </n-collapse>

    <footer class="actions">
      <n-button size="small" type="primary" secondary @click="goRecommendationReview(stock, item.id, `${item.summary}；主要理由：${firstReason}；主要风险：${firstRisk}`)">AI 复核当前推荐</n-button>
      <n-button size="small" @click="revealEvidence">查看推荐依据</n-button>
      <n-button size="small" @click="goDetail(stock)">进入个股详情</n-button>
      <n-button size="small" @click="addToWatchlist(stock)">加入自选</n-button>
      <n-button size="small" @click="goAlert(stock)">设置提醒</n-button>
      <n-button v-if="isHeld" size="small" type="warning" @click="goPositionDecision(stock, item.position!.position_id)">进入持仓卖出决策</n-button>
      <n-button
        v-else-if="item.detail?.execution_plan?.status === 'ready'"
        size="small"
        type="success"
        secondary
        @click="goPositionFromRecommendation(stock, item.id, item.detail.execution_plan.quantity)"
      >按推荐记录建仓</n-button>
      <n-button v-if="type === 'short_term' && (item.detail?.stop_loss || 0) > 0" size="small" @click="emit('stop-alert', item)">设置止损提醒</n-button>
    </footer>
  </article>
</template>

<style scoped>
.recommendation-card {
  display: grid;
  min-width: 0;
  gap: 12px;
  padding: 16px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.recommendation-card:last-child { border-bottom: 0; }
.card-head,
.state-tags,
.data-line,
.tracking-band,
.actions {
  display: flex;
  min-width: 0;
  align-items: center;
  flex-wrap: wrap;
  gap: 8px;
}
.card-head { justify-content: space-between; }
.decision-summary {
  display: grid;
  grid-template-columns: minmax(0, 1.6fr) minmax(260px, 1fr);
  gap: 16px;
}
.summary-copy { display: grid; gap: 5px; }
.summary-copy span,
.decision-facts span,
.reason-risk span,
.data-line,
.tracking-band { font-size: 12px; opacity: 0.72; }
.summary-copy strong { font-size: 18px; line-height: 1.45; overflow-wrap: anywhere; }
.decision-facts { display: grid; gap: 7px; }
.decision-facts > div { display: flex; justify-content: space-between; gap: 10px; }
.decision-facts b { text-align: right; }
.reason-risk,
.evidence-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.reason-risk > div { min-width: 0; padding-left: 10px; border-left: 2px solid v-bind('vars.borderColor'); }
.reason-risk p { margin: 4px 0 0; line-height: 1.55; overflow-wrap: anywhere; }
.data-line { gap: 8px 18px; }
.ownership { display: grid; gap: 2px; padding: 10px 12px; border: 1px solid v-bind('vars.borderColor'); border-radius: 6px; }
.ownership span { font-size: 12px; opacity: 0.72; }
.ownership.held { border-color: v-bind('vars.warningColor'); }
.tracking-band { padding: 9px 0; border-top: 1px dashed v-bind('vars.dividerColor'); border-bottom: 1px dashed v-bind('vars.dividerColor'); }
.evidence-grid section { min-width: 0; }
.evidence-grid h4 { margin: 0 0 6px; font-size: 13px; }
.evidence-grid p,
.evidence-grid ul { margin: 0; padding-left: 18px; line-height: 1.6; overflow-wrap: anywhere; }
.disclaimer { margin: 10px 0 0; font-size: 11px; opacity: 0.58; }
@media (max-width: 700px) {
  .decision-summary,
  .reason-risk,
  .evidence-grid { grid-template-columns: 1fr; }
  .summary-copy strong { font-size: 16px; }
  .actions > * { flex: 1 1 calc(50% - 8px); }
}
</style>
