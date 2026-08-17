<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NButton, NCollapse, NCollapseItem, NEmpty, NSkeleton, NTag } from 'naive-ui'
import type { Quote } from '@/api/market'
import ChangeTag from '@/components/ChangeTag.vue'
import FreshnessTag from '@/components/FreshnessTag.vue'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'
import { useDisplayMode } from '@/composables/useDisplayMode'
import type {
  DecisionItem,
  DecisionSummary,
  PositionRelationSummary,
  StockSectionPhase,
} from './decisionSummary'

const props = defineProps<{
  quote: Quote | null
  quotePhase: StockSectionPhase
  quoteError?: string
  relationshipPhase: StockSectionPhase
  relationshipError?: string
  relationshipAsOf?: string
  watchlistKnown: boolean
  positionKnown: boolean
  inWatchlist: boolean
  position: PositionRelationSummary | null
  summary: DecisionSummary
  adding: boolean
}>()
const emit = defineEmits<{
  (event: 'retry-quote'): void
  (event: 'action', action: 'watch' | 'alert' | 'analysis' | 'position'): void
}>()

const { vars, flatColor, pctColor } = useUi()
const { label } = useDisplayMode()
const unknownText = () => label('暂时无法判断', 'unknown（暂时无法判断）')
const asOfText = () => label('数据截止时间', 'as_of（数据截止时间）')
const allEvidence = computed(() => [
  ...props.summary.changes,
  ...props.summary.risks,
  props.summary.recentEvent,
])

function itemColor(item: DecisionItem) {
  if (item.tone === 'positive') return pctColor(1)
  if (item.tone === 'negative') return pctColor(-1)
  if (item.tone === 'warning') return vars.value.warningColor
  if (item.tone === 'unknown') return flatColor.value
  return vars.value.textColor1
}

function money(value: number) {
  return `${value > 0 ? '+' : ''}${value.toFixed(2)} 元`
}
</script>

<template>
  <SectionCard :hoverable="false" class="decision-summary">
    <div class="quote-band">
      <div v-if="quote" class="quote-primary">
        <div class="quote-price-row">
          <span class="quote-price qv-figure" :style="{ color: pctColor(quote.change_pct) }">
            {{ quote.price.toFixed(2) }}
          </span>
          <ChangeTag :value="quote.change_pct" />
        </div>
        <div class="quote-time">
          <span>{{ quote.source || '行情聚合' }} · {{ asOfText() }} {{ quote.data_time || unknownText() }}</span>
          <FreshnessTag
            :status="quote.freshness?.freshness_status || 'unknown'"
            :as-of="quote.data_time"
            :reason="quote.freshness?.stale_reason"
          />
        </div>
      </div>
      <div v-else-if="quotePhase === 'loading'" class="quote-primary quote-loading">
        <n-skeleton width="150px" height="38px" />
        <n-skeleton width="220px" text />
      </div>
      <n-alert v-else type="error" :bordered="false" title="关键行情读取失败">
        {{ quoteError || '未取得当前股票报价。' }}
        <n-button text type="primary" class="inline-retry" @click="emit('retry-quote')">重试</n-button>
      </n-alert>

      <div class="quote-facts" aria-label="关键行情">
        <div><span>今开</span><strong>{{ quote ? quote.open.toFixed(2) : unknownText() }}</strong></div>
        <div><span>最高</span><strong>{{ quote ? quote.high.toFixed(2) : unknownText() }}</strong></div>
        <div><span>最低</span><strong>{{ quote ? quote.low.toFixed(2) : unknownText() }}</strong></div>
        <div><span>昨收</span><strong>{{ quote ? quote.prev_close.toFixed(2) : unknownText() }}</strong></div>
      </div>
    </div>
    <n-alert v-if="quote && quoteError" type="warning" :bordered="false" class="quote-refresh-warning">
      刷新失败，当前继续展示 {{ quote.data_time || unknownText() }} 的最近已知值：{{ quoteError }}
      <n-button text type="primary" class="inline-retry" @click="emit('retry-quote')">稍后重试</n-button>
    </n-alert>

    <section class="relationship" aria-labelledby="relationship-title">
      <div class="section-label" id="relationship-title">我的关系</div>
      <div v-if="relationshipPhase === 'loading'" class="relationship-loading">
        <n-skeleton width="90px" text /><n-skeleton width="180px" text />
      </div>
      <n-alert v-else-if="relationshipPhase === 'error'" type="warning" :bordered="false">
        {{ relationshipError || '账户关系读取失败' }}，自选和持仓状态均为{{ unknownText() }}。
      </n-alert>
      <div v-else class="relationship-content">
        <n-alert v-if="relationshipError" type="warning" :bordered="false" class="relationship-warning">
          {{ relationshipError }}；已读取的账户数据继续显示，失败部分为{{ unknownText() }}。
        </n-alert>
        <div class="relation-tags">
          <n-tag size="small" round :bordered="false" :type="watchlistKnown && inWatchlist ? 'success' : 'default'">
            {{ watchlistKnown ? (inWatchlist ? '已自选' : '未自选') : `自选${unknownText()}` }}
          </n-tag>
          <n-tag size="small" round :bordered="false" :type="positionKnown && position ? 'info' : 'default'">
            {{ positionKnown ? (position ? `持仓 ${position.quantity.toFixed(0)} 股` : '未持仓') : `持仓${unknownText()}` }}
          </n-tag>
          <span class="relation-asof">账户关系{{ asOfText() }} {{ relationshipAsOf || unknownText() }}</span>
        </div>
        <div v-if="position" class="position-facts">
          <div><span>平均成本</span><strong>{{ position.averageCost.toFixed(2) }} 元</strong></div>
          <div>
            <span>浮盈亏</span>
            <strong v-if="position.profitPct != null" :style="{ color: pctColor(position.profitPct) }">
              {{ position.profitPct > 0 ? '+' : '' }}{{ position.profitPct.toFixed(2) }}%
              · {{ money(position.profitAmount || 0) }}
            </strong>
            <strong v-else>{{ unknownText() }}</strong>
          </div>
          <div><span>已实现盈亏</span><strong :style="{ color: pctColor(position.realizedPnl) }">{{ money(position.realizedPnl) }}</strong></div>
          <div><span>行情时点</span><strong>{{ position.asOf || unknownText() }}</strong></div>
        </div>
      </div>
    </section>

    <div class="decision-grid">
      <section class="decision-block changes" aria-labelledby="changes-title">
        <div class="section-label" id="changes-title">主要变化</div>
        <div v-if="summary.changes.length" class="decision-list">
          <article v-for="item in summary.changes" :key="item.id" class="decision-item">
            <div class="decision-heading">
              <strong>{{ item.title }}</strong>
              <span class="decision-value qv-tnum" :style="{ color: itemColor(item) }">{{ item.value }}</span>
            </div>
            <p>{{ item.detail }}</p>
            <div class="evidence-meta">来源 {{ item.source }} · {{ asOfText() }} {{ item.asOf }}</div>
          </article>
        </div>
        <n-empty v-else :description="`主要变化${unknownText()}：尚无可核验数据`" size="small" />
      </section>

      <section class="decision-block risks" aria-labelledby="risks-title">
        <div class="section-label" id="risks-title">风险关注</div>
        <div v-if="summary.risks.length" class="decision-list">
          <article v-for="item in summary.risks" :key="item.id" class="decision-item">
            <div class="decision-heading">
              <strong>{{ item.title }}</strong>
              <span class="decision-value qv-tnum" :style="{ color: itemColor(item) }">{{ item.value }}</span>
            </div>
            <p>{{ item.detail }}</p>
            <div class="evidence-meta">来源 {{ item.source }} · {{ asOfText() }} {{ item.asOf }}</div>
          </article>
        </div>
        <n-empty v-else description="未命中可核验规则风险；不代表没有风险" size="small" />
      </section>

      <section class="decision-block event" aria-labelledby="event-title">
        <div class="section-label" id="event-title">最近事件</div>
        <article class="decision-item">
          <div class="decision-heading">
            <strong>{{ summary.recentEvent.title }}</strong>
            <span class="decision-value qv-tnum" :style="{ color: itemColor(summary.recentEvent) }">
              {{ summary.recentEvent.value }}
            </span>
          </div>
          <p>{{ summary.recentEvent.detail }}</p>
          <div class="evidence-meta">
            来源 {{ summary.recentEvent.source }} · {{ asOfText() }} {{ summary.recentEvent.asOf }}
          </div>
        </article>
      </section>
    </div>

    <div class="summary-actions desktop-actions" aria-label="后续动作">
      <n-button secondary :loading="adding" @click="emit('action', 'watch')">
        {{ inWatchlist ? '管理观察' : '加入观察' }}
      </n-button>
      <n-button secondary @click="emit('action', 'alert')">设提醒</n-button>
      <n-button type="primary" secondary @click="emit('action', 'analysis')">分析</n-button>
      <n-button secondary @click="emit('action', 'position')">{{ position ? '持仓管理' : '记录持仓' }}</n-button>
    </div>

    <n-collapse class="evidence-collapse">
      <n-collapse-item title="查看原始证据、最大风险与失效条件" name="evidence">
        <div class="evidence-list">
          <div v-for="item in allEvidence" :key="`evidence-${item.id}`" class="evidence-row">
            <strong>{{ item.title }}</strong>
            <span>{{ item.evidence }}</span>
            <small>来源 {{ item.source }} · {{ asOfText() }} {{ item.asOf }}</small>
          </div>
        </div>
        <div class="boundary-row"><strong>最大风险</strong>{{ summary.risks[0]?.detail || unknownText() }}</div>
        <div class="boundary-row"><strong>失效条件</strong>{{ summary.invalidation }}</div>
      </n-collapse-item>
    </n-collapse>
  </SectionCard>
</template>

<style scoped>
.quote-band {
  display: grid;
  grid-template-columns: minmax(220px, 0.9fr) minmax(360px, 1.4fr);
  gap: 24px;
  align-items: center;
  padding-bottom: 16px;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.quote-primary {
  min-width: 0;
}
.inline-retry {
  margin-left: 8px;
}
.quote-loading {
  display: grid;
  gap: 10px;
}
.quote-price-row {
  display: flex;
  align-items: center;
  gap: 12px;
}
.quote-price {
  font-size: 36px;
  font-weight: 700;
  line-height: 1;
}
.quote-time {
  display: flex;
  align-items: center;
  gap: 8px;
  min-height: 22px;
  margin-top: 8px;
  color: v-bind('vars.textColor3');
  font-size: 12px;
  flex-wrap: wrap;
}
.quote-facts,
.position-facts {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 12px;
}
.quote-facts div,
.position-facts div {
  min-width: 0;
}
.quote-facts span,
.position-facts span {
  display: block;
  color: v-bind('vars.textColor3');
  font-size: 12px;
}
.quote-facts strong,
.position-facts strong {
  display: block;
  margin-top: 3px;
  font-size: 14px;
  overflow-wrap: anywhere;
}
.relationship {
  padding: 16px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.quote-refresh-warning {
  margin-top: 12px;
}
.section-label {
  margin-bottom: 9px;
  color: v-bind('vars.textColor3');
  font-size: 12px;
  font-weight: 600;
}
.relationship-loading {
  display: flex;
  gap: 18px;
}
.relationship-content {
  display: flex;
  align-items: center;
  gap: 24px;
  flex-wrap: wrap;
}
.relationship-warning {
  flex: 1 1 100%;
}
.relation-tags {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 0 0 auto;
  flex-wrap: wrap;
}
.relation-asof {
  color: v-bind('vars.textColor3');
  font-size: 11px;
}
.position-facts {
  flex: 1;
}
.decision-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.35fr) minmax(0, 1fr);
}
.decision-block {
  min-width: 0;
  padding: 16px 0;
}
.decision-block.changes {
  padding-right: 20px;
  border-right: 1px solid v-bind('vars.dividerColor');
}
.decision-block.risks {
  padding-left: 20px;
}
.decision-block.event {
  grid-column: 1 / -1;
  border-top: 1px solid v-bind('vars.dividerColor');
}
.decision-list {
  display: grid;
  gap: 12px;
}
.decision-item {
  min-width: 0;
}
.decision-item + .decision-item {
  padding-top: 12px;
  border-top: 1px dashed v-bind('vars.dividerColor');
}
.decision-heading {
  display: flex;
  justify-content: space-between;
  gap: 12px;
}
.decision-heading strong {
  font-size: 14px;
}
.decision-value {
  flex: 0 0 auto;
  font-size: 13px;
  font-weight: 700;
  text-align: right;
}
.decision-item p {
  margin: 5px 0 3px;
  color: v-bind('vars.textColor2');
  font-size: 13px;
  line-height: 1.55;
  overflow-wrap: anywhere;
}
.evidence-meta {
  color: v-bind('vars.textColor3');
  font-size: 11px;
  overflow-wrap: anywhere;
}
.summary-actions {
  display: flex;
  gap: 8px;
  padding-top: 14px;
  border-top: 1px solid v-bind('vars.dividerColor');
  flex-wrap: wrap;
}
.evidence-collapse {
  margin-top: 10px;
}
.evidence-list {
  display: grid;
  gap: 8px;
}
.evidence-row {
  display: grid;
  grid-template-columns: minmax(100px, 0.4fr) minmax(0, 1fr);
  gap: 4px 14px;
  font-size: 12px;
}
.evidence-row small {
  grid-column: 2;
  color: v-bind('vars.textColor3');
}
.boundary-row {
  display: grid;
  grid-template-columns: 76px minmax(0, 1fr);
  gap: 12px;
  margin-top: 12px;
  font-size: 12px;
  line-height: 1.6;
}
@media (max-width: 768px) {
  .quote-band {
    grid-template-columns: 1fr;
    gap: 16px;
  }
  .quote-price {
    font-size: 32px;
  }
  .quote-facts,
  .position-facts {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .relationship-content {
    align-items: stretch;
    flex-direction: column;
    gap: 14px;
  }
  .decision-grid {
    grid-template-columns: 1fr;
  }
  .decision-block.changes,
  .decision-block.risks {
    padding: 14px 0;
    border-right: 0;
  }
  .decision-block.risks {
    border-top: 1px solid v-bind('vars.dividerColor');
  }
  .desktop-actions {
    display: none;
  }
  .decision-heading {
    align-items: flex-start;
    flex-direction: column;
    gap: 3px;
  }
  .decision-value {
    text-align: left;
  }
  .evidence-row {
    grid-template-columns: 1fr;
  }
  .evidence-row small {
    grid-column: 1;
  }
}
</style>
