<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { NAlert, NButton, NEmpty, NSpin, NTag } from 'naive-ui'
import type { DiscoveryStatusView, PoolCandidate, RecommendationItem, RecommendationView } from '@/api/recommendation'
import { SCORE_PROFILE_LABEL } from '@/api/screener'
import SectionCard from '@/components/SectionCard.vue'
import RecommendationCard from './RecommendationCard.vue'
import RecommendationCandidateAudit from './RecommendationCandidateAudit.vue'
import { businessStatusLabel, parseCandidateSnapshot, recommendationDecisionState, scoringVersionLabel } from './recommendationPresentation'

const props = defineProps<{
  current: RecommendationView | null
  discovery: DiscoveryStatusView | null
  loading: boolean
  error: string
  tracking: boolean
  stopAlerting: Record<number, boolean>
  sections: string[]
}>()
const emit = defineEmits<{
  (event: 'refresh-tracking'): void
  (event: 'stop-alert', item: RecommendationItem): void
  (event: 'linked', batchID: number): void
  (event: 'retry'): void
  (event: 'update:sections', value: string[]): void
}>()

const showAll = ref(false)
watch(() => props.current?.id, () => { showAll.value = false })
const sectionModel = computed({
  get: () => props.sections,
  set: (value: string[]) => emit('update:sections', value),
})
const poolMap = computed(() => {
  const map = new Map<string, PoolCandidate>()
  if (!props.current?.candidate_pool) return map
  parseCandidateSnapshot(props.current.candidate_pool).items.forEach(item => map.set(`${item.market || 'cn'}:${item.symbol}`, item))
  return map
})
const ordered = computed(() => {
  if (!props.current) return []
  const priority = { buy_research: 0, watch: 1, no_action: 2, insufficient: 3, expired: 4 }
  return [...props.current.items].sort((a, b) => priority[recommendationDecisionState(a)] - priority[recommendationDecisionState(b)])
})
const visible = computed(() => showAll.value ? ordered.value : ordered.value.slice(0, 5))
const discoveryLabel = computed(() => ({ success: '完整', partial: '部分可用', failed: '失败', processing: '运行中' })[props.discovery?.status || ''] || '不可用')
</script>

<template>
  <SectionCard title="今日推荐">
    <template #extra>
      <n-button v-if="current?.items.length" size="tiny" quaternary :loading="tracking" @click="emit('refresh-tracking')">刷新追踪</n-button>
    </template>

    <div class="discovery-band">
      <div><b>全市场候选发现</b><n-tag size="tiny" :bordered="false" :type="discovery?.status === 'success' ? 'success' : discovery?.status === 'partial' ? 'warning' : 'default'">{{ discoveryLabel }}</n-tag></div>
      <span v-if="discovery?.run">{{ discovery.run.trade_date }} · 覆盖 {{ discovery.run.universe_count }} 只 · 命中 {{ discovery.run.success_count }} 条</span>
      <span v-if="discovery?.run?.partial_reason || discovery?.run?.error || discovery?.reason">{{ discovery?.run?.partial_reason || discovery?.run?.error || discovery?.reason }}</span>
    </div>

    <n-alert v-if="error" type="error" :bordered="false" class="result-error">
      {{ error }}<template v-if="current">；下方保留上次读取的批次</template>。
      <n-button size="tiny" :loading="loading" @click="emit('retry')">重试</n-button>
    </n-alert>
    <n-spin :show="loading && !current">
      <n-empty v-if="!current && !error && !loading" description="尚未选择推荐结果。进入页面不会自动生成，请主动点击生成或打开历史记录。" />
      <template v-if="current">
        <header class="batch-head">
          <div>
            <div class="batch-title">{{ current.title || (current.type === 'short_term' ? '短线推荐' : '长线推荐') }}</div>
            <div class="batch-meta">生成 {{ new Date(current.created_at).toLocaleString('zh-CN', { hour12: false }) }} · 量化版本 {{ current.strategy_version || '未知' }} · Prompt {{ current.prompt_version || '未知' }}</div>
            <div v-if="current.score_profile" class="batch-meta">本批评分侧重：{{ SCORE_PROFILE_LABEL[current.score_profile] || current.score_profile }}</div>
            <div v-if="current.scoring_version" class="batch-meta">排序方式：{{ scoringVersionLabel(current.scoring_version) }}<template v-if="current.scoring_artifact_id"> · 模型 #{{ current.scoring_artifact_id }}</template> · 分值用于排序，不代表获利概率</div>
          </div>
          <n-tag :type="current.status === 'success' ? 'success' : current.status === 'failed' ? 'error' : current.status === 'degraded' ? 'warning' : 'info'" :bordered="false">
            {{ businessStatusLabel(current.status) }}
          </n-tag>
        </header>
        <n-alert v-if="current.status === 'processing'" type="info" :bordered="false">任务仍在后台运行，刷新页面后可以继续查看状态；当前页面不会重复提交。</n-alert>
        <n-alert v-else-if="current.status === 'degraded'" type="warning" :bordered="false">{{ current.error || '部分数据或 AI 结果不可用，已保留可用结果。' }}</n-alert>
        <n-alert v-else-if="current.status === 'failed'" type="error" :bordered="false">{{ current.error || '推荐生成失败，请查看任务状态中的真实原因和下一步。' }}</n-alert>
        <n-empty v-if="!current.items.length && current.status !== 'processing'" description="本批没有有效推荐，候选与排除事实仍可在下方查看。" />
        <div v-if="visible.length" class="rec-list">
          <RecommendationCard
            v-for="item in visible"
            :key="item.id"
            :item="item"
            :type="current.type"
            :candidate="poolMap.get(`${item.market || 'cn'}:${item.symbol}`)"
            :stop-alerting="!!stopAlerting[item.id]"
            @stop-alert="emit('stop-alert', $event)"
            @linked="emit('linked', $event)"
          />
        </div>
        <n-button v-if="ordered.length > visible.length" block tertiary class="more-btn" @click="showAll = true">查看其余 {{ ordered.length - visible.length }} 条</n-button>
        <RecommendationCandidateAudit v-model:sections="sectionModel" :current="current" />
      </template>
    </n-spin>
  </SectionCard>
</template>

<style scoped>
.result-error { margin-bottom: 12px; }
.discovery-band,
.batch-head { display: flex; min-width: 0; justify-content: space-between; align-items: flex-start; flex-wrap: wrap; gap: 8px 16px; }
.discovery-band { margin-bottom: 14px; padding-bottom: 12px; border-bottom: 1px solid rgba(128,128,128,.2); font-size: 12px; }
.discovery-band > div { display: flex; gap: 7px; align-items: center; }
.discovery-band > span { opacity: .66; }
.batch-head { margin-bottom: 12px; }
.batch-title { font-size: 17px; font-weight: 650; }
.batch-meta { margin-top: 4px; font-size: 12px; opacity: .62; overflow-wrap: anywhere; }
/* 推荐卡列表：与上方批次信息/告警、下方「查看其余」按钮和审计区都留出间距，
 * 卡与卡之间的间隔由 RecommendationCard 的 `+` 选择器负责。 */
.rec-list {
  display: block;
  margin: 14px 0;
}
.more-btn {
  margin-bottom: 14px;
}
</style>
