<script setup lang="ts">
import { NAlert, NButton, NCollapse, NCollapseItem, NEmpty, NPopconfirm, NSpin, NTag } from 'naive-ui'
import type { PerformanceStats, RecommendationBatch } from '@/api/recommendation'
import type { TodoItem } from '@/api/todo'
import { useUi } from '@/composables/useUi'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import TermHelp from '@/components/TermHelp.vue'
import { businessStatusLabel } from './recommendationPresentation'

defineProps<{
  history: RecommendationBatch[]
  currentID?: number
  historyLoading: boolean
  reviews: TodoItem[]
  reviewsLoading: boolean
  reviewsError: string
  reviewAcking: number | null
  performance: PerformanceStats | null
}>()
const emit = defineEmits<{
  (event: 'open', item: RecommendationBatch): void
  (event: 'remove', item: RecommendationBatch): void
  (event: 'refresh-history'): void
  (event: 'refresh-reviews'): void
  (event: 'ack-review', item: TodoItem): void
  (event: 'audit', mode: 'attribution' | 'shadow' | 'recall'): void
}>()
const { pctColor, downColor, vars } = useUi()
function time(value: string) {
  return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '时间未知'
}
function statusType(value: string) {
  return value === 'success' ? 'success' : value === 'failed' ? 'error' : value === 'degraded' ? 'warning' : 'info'
}
</script>

<template>
  <div class="history-stack">
    <SectionCard title="推荐历史">
      <template #extra><n-button size="tiny" quaternary :loading="historyLoading" @click="emit('refresh-history')">刷新</n-button></template>
      <n-spin :show="historyLoading && !history.length">
        <n-empty v-if="!history.length" description="暂无推荐记录" size="small" />
        <div v-else class="history-list">
          <button v-for="item in history" :key="item.id" type="button" class="history-row" :class="{ active: currentID === item.id }" @click="emit('open', item)">
            <span class="history-main">
              <span class="history-title">{{ item.title || (item.type === 'short_term' ? '短线推荐' : '长线推荐') }}</span>
              <span class="history-meta">{{ time(item.created_at) }} · 数据截止见结果卡 · 策略 {{ item.strategy_version || item.strategy }}</span>
            </span>
            <span class="history-side">
              <n-tag size="tiny" :type="statusType(item.status)" :bordered="false">{{ businessStatusLabel(item.status) }}</n-tag>
              <n-popconfirm v-if="item.status !== 'processing'" @positive-click="emit('remove', item)">
                <template #trigger><n-button size="tiny" quaternary type="error" @click.stop>删除记录</n-button></template>
                删除只影响这条本人历史记录；不会改写其他历史推荐或追踪事实。
              </n-popconfirm>
            </span>
          </button>
        </div>
      </n-spin>
    </SectionCard>

    <SectionCard title="追踪与待复盘">
      <template #extra><n-button size="tiny" quaternary :loading="reviewsLoading" @click="emit('refresh-reviews')">刷新</n-button></template>
      <n-alert v-if="reviewsError" :type="reviews.length ? 'warning' : 'error'" :bordered="false">{{ reviewsError }}，已有追踪数据仍保留。</n-alert>
      <n-empty v-if="!reviews.length && !reviewsLoading" description="没有需要处理的推荐复盘" size="small" />
      <div v-else class="review-list">
        <div v-for="item in reviews" :key="item.ref_id" class="review-row">
          <div>
            <StockIdentity :symbol="item.symbol" :market="item.market || 'cn'" :name="item.name" density="table" clickable />
            <p>{{ item.title }} · {{ item.detail }}</p>
          </div>
          <n-button size="tiny" :loading="reviewAcking === item.ref_id" @click="emit('ack-review', item)">标为已读</n-button>
        </div>
      </div>
    </SectionCard>

    <SectionCard v-if="performance" title="收益表现">
      <div class="performance-grid">
        <div><span>成熟买入样本</span><b class="qv-tnum">{{ performance.buy_matured }}</b></div>
        <div><span>成熟买入胜率</span><b class="qv-tnum">{{ performance.buy_matured ? `${performance.buy_win_rate.toFixed(1)}%` : '未成熟' }}</b></div>
        <div><span>平均收益</span><b class="qv-tnum" :style="{ color: pctColor(performance.buy_avg_return_pct) }">{{ performance.buy_matured ? `${performance.buy_avg_return_pct > 0 ? '+' : ''}${performance.buy_avg_return_pct.toFixed(2)}%` : '—' }}</b></div>
        <div><span><TermHelp term="alpha" /></span><b class="qv-tnum" :style="{ color: pctColor(performance.buy_avg_alpha_pct) }">{{ performance.buy_bench_sample ? `${performance.buy_avg_alpha_pct > 0 ? '+' : ''}${performance.buy_avg_alpha_pct.toFixed(2)}%` : '—' }}</b></div>
        <div><span>平均最大回撤</span><b class="qv-tnum" :style="{ color: downColor }">{{ performance.buy_matured ? `-${performance.avg_max_drawdown_pct.toFixed(2)}%` : '—' }}</b></div>
        <div><span>尚未成熟</span><b class="qv-tnum">{{ performance.buy_active }}</b></div>
      </div>
      <p class="performance-note">未到结算时间、数据不足和量化降级批次不会被包装成“准确”或“失败”。历史推荐事实只读，追踪只追加状态和解释。</p>
      <n-collapse>
        <n-collapse-item title="专业评估与召回审计" name="audit">
          <div class="audit-actions">
            <n-button size="small" @click="emit('audit', 'attribution')">错误归因</n-button>
            <n-button size="small" @click="emit('audit', 'shadow')">影子门控</n-button>
            <n-button size="small" @click="emit('audit', 'recall')">召回评估</n-button>
          </div>
        </n-collapse-item>
      </n-collapse>
    </SectionCard>
  </div>
</template>

<style scoped>
.history-stack { display: grid; gap: 14px; }
.history-list,
.review-list { display: grid; }
.history-row {
  display: flex;
  width: 100%;
  min-width: 0;
  align-items: center;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 0;
  border: 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.history-row.active { box-shadow: inset 3px 0 v-bind('vars.primaryColor'); padding-left: 10px; }
.history-main { display: grid; min-width: 0; gap: 3px; }
.history-title { font-weight: 600; }
.history-meta { font-size: 11px; opacity: .6; overflow-wrap: anywhere; }
.history-side { display: flex; flex: 0 0 auto; align-items: center; gap: 4px; }
.review-row { display: flex; min-width: 0; align-items: center; justify-content: space-between; gap: 10px; padding: 10px 0; border-bottom: 1px solid v-bind('vars.dividerColor'); }
.review-row p { margin: 4px 0 0; font-size: 12px; opacity: .68; overflow-wrap: anywhere; }
.performance-grid { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 9px 16px; }
.performance-grid > div { display: flex; justify-content: space-between; gap: 8px; }
.performance-grid span { font-size: 12px; opacity: .68; }
.performance-note { font-size: 12px; line-height: 1.55; opacity: .65; }
.audit-actions { display: flex; flex-wrap: wrap; gap: 8px; }
@media (max-width: 480px) {
  .history-row,
  .review-row { align-items: flex-start; flex-direction: column; }
  .history-side { width: 100%; justify-content: space-between; }
  .performance-grid { grid-template-columns: 1fr; }
}
</style>
