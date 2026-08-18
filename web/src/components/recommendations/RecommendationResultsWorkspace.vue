<script setup lang="ts">
import { computed, ref } from 'vue'
import { NAlert, NButton, NEmpty, NSpin, NTag } from 'naive-ui'
import type { DiscoveryStatusView, PoolCandidate, RecommendationItem, RecommendationView } from '@/api/recommendation'
import SectionCard from '@/components/SectionCard.vue'
import RecommendationCard from './RecommendationCard.vue'
import RecommendationCandidateAudit from './RecommendationCandidateAudit.vue'
import { businessStatusLabel, recommendationDecisionState } from './recommendationPresentation'

const props = defineProps<{
  current: RecommendationView | null
  discovery: DiscoveryStatusView | null
  loading: boolean
  tracking: boolean
  stopAlerting: Record<number, boolean>
  sections: string[]
}>()
const emit = defineEmits<{
  (event: 'refresh-tracking'): void
  (event: 'stop-alert', item: RecommendationItem): void
  (event: 'update:sections', value: string[]): void
}>()

const showAll = ref(false)
const sectionModel = computed({
  get: () => props.sections,
  set: (value: string[]) => emit('update:sections', value),
})
const poolMap = computed(() => {
  const map = new Map<string, PoolCandidate>()
  if (!props.current?.candidate_pool) return map
  try {
    const rows = JSON.parse(props.current.candidate_pool) as PoolCandidate[]
    if (Array.isArray(rows)) rows.forEach((item) => map.set(`${item.market || 'cn'}:${item.symbol}`, item))
  } catch { /* 历史坏快照按缺失显示 */ }
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

    <n-spin :show="loading && !current">
      <n-empty v-if="!current" description="尚未选择推荐结果。进入页面不会自动生成，请主动点击生成或打开历史记录。" />
      <template v-else>
        <header class="batch-head">
          <div>
            <div class="batch-title">{{ current.title || (current.type === 'short_term' ? '短线推荐' : '长线推荐') }}</div>
            <div class="batch-meta">生成 {{ new Date(current.created_at).toLocaleString('zh-CN', { hour12: false }) }} · 策略 {{ current.strategy_version || current.strategy }} · Prompt {{ current.prompt_version || '未知' }}</div>
          </div>
          <n-tag :type="current.status === 'success' ? 'success' : current.status === 'failed' ? 'error' : current.status === 'degraded' ? 'warning' : 'info'" :bordered="false">
            {{ businessStatusLabel(current.status) }}
          </n-tag>
        </header>
        <n-alert v-if="current.status === 'processing'" type="info" :bordered="false">任务仍在后台运行，刷新页面后可以继续查看状态；当前页面不会重复提交。</n-alert>
        <n-alert v-else-if="current.status === 'degraded'" type="warning" :bordered="false">{{ current.error || '部分数据或 AI 结果不可用，已保留可用结果。' }}</n-alert>
        <n-alert v-else-if="current.status === 'failed'" type="error" :bordered="false">{{ current.error || '推荐生成失败，请查看任务状态中的真实原因和下一步。' }}</n-alert>
        <n-empty v-if="!current.items.length && current.status !== 'processing'" description="本批没有有效推荐，候选与排除事实仍可在下方查看。" />
        <RecommendationCard
          v-for="item in visible"
          :key="item.id"
          :item="item"
          :type="current.type"
          :candidate="poolMap.get(`${item.market || 'cn'}:${item.symbol}`)"
          @stop-alert="emit('stop-alert', $event)"
        />
        <n-button v-if="ordered.length > visible.length" block tertiary @click="showAll = true">查看其余 {{ ordered.length - visible.length }} 条</n-button>
        <RecommendationCandidateAudit v-model:sections="sectionModel" :current="current" />
      </template>
    </n-spin>
  </SectionCard>
</template>

<style scoped>
.discovery-band,
.batch-head { display: flex; min-width: 0; justify-content: space-between; align-items: flex-start; flex-wrap: wrap; gap: 8px 16px; }
.discovery-band { margin-bottom: 14px; padding-bottom: 12px; border-bottom: 1px solid rgba(128,128,128,.2); font-size: 12px; }
.discovery-band > div { display: flex; gap: 7px; align-items: center; }
.discovery-band > span { opacity: .66; }
.batch-head { margin-bottom: 12px; }
.batch-title { font-size: 17px; font-weight: 650; }
.batch-meta { margin-top: 4px; font-size: 12px; opacity: .62; overflow-wrap: anywhere; }
</style>
