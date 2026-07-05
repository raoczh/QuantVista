<script setup lang="ts">
import { computed } from 'vue'
import { NTooltip } from 'naive-ui'
import { useUi } from '@/composables/useUi'
import type { EvidenceCheck, TrustReview, SysConfidence } from '@/api/trust'

// 全站统一信任徽章：量化分/排名 · 一手成本 · 证据核验 · 综合置信 · AI 复核。
// 五处 LLM 链路（推荐/分析/问答/对比/日报）共用；所有 props 可选，缺省不渲染对应徽章。
// 配色一律取自 useUi 主题变量，自动兼容 6 套主题（禁止硬编码）。
const props = defineProps<{
  quantScore?: number
  quantRank?: number
  poolSize?: number
  lotCost?: number
  evidenceCheck?: EvidenceCheck
  sysConfidence?: SysConfidence | string
  sysConfidenceWhy?: string
  review?: TrustReview
}>()

const { upColor, downColor, flatColor, vars, withAlpha } = useUi()

const SYS_CONF_LABEL: Record<string, string> = { high: '综合置信 高', medium: '综合置信 中', low: '综合置信 低' }
function sysConfColor(level: string | undefined) {
  if (level === 'high') return upColor.value
  if (level === 'low') return downColor.value
  return flatColor.value
}
const VERDICT_LABEL: Record<string, string> = { pass: '复核通过', warn: '复核提示', reject: '复核否决' }
function verdictColor(v: string) {
  if (v === 'pass') return upColor.value
  if (v === 'reject') return downColor.value
  return vars.value.warningColor
}

const ev = computed(() => props.evidenceCheck)
const evAllMatched = computed(() => !!ev.value && ev.value.matched === ev.value.total)
const evColor = computed(() => (evAllMatched.value ? upColor.value : vars.value.warningColor))
// 全部徽章缺省时整行不渲染：旧记录（信任层上线前）无这些字段，空 flex 容器会留出多余空隙。
const hasAny = computed(
  () =>
    !!props.quantScore ||
    !!props.lotCost ||
    (!!ev.value && ev.value.total > 0) ||
    !!props.sysConfidence ||
    !!props.review,
)
</script>

<template>
  <div v-if="hasAny" class="trust-row">
    <span
      v-if="quantScore"
      class="trust-chip"
      :style="{ background: withAlpha(vars.primaryColor, 0.1), color: vars.primaryColor }"
    >
      量化分 {{ quantScore.toFixed(1) }}<template v-if="quantRank"> · 第{{ quantRank }}/{{ poolSize }}</template>
    </span>
    <span v-if="lotCost" class="trust-chip trust-plain">一手约 ¥{{ lotCost.toFixed(0) }}</span>
    <n-tooltip v-if="ev && ev.total > 0" trigger="hover">
      <template #trigger>
        <span class="trust-chip" :style="{ background: withAlpha(evColor, 0.12), color: evColor }">
          数据核验 {{ ev.matched }}/{{ ev.total }}
        </span>
      </template>
      <span v-if="ev.unmatched?.length">
        AI 引用里这些数字未能与数据快照吻合，可能是推算值或幻觉，建议人工核对：{{ ev.unmatched.join('、') }}
      </span>
      <span v-else>AI 引用的数字已逐一与数据快照程序化比对，全部吻合</span>
    </n-tooltip>
    <n-tooltip v-if="sysConfidence" trigger="hover">
      <template #trigger>
        <span
          class="trust-chip"
          :style="{ background: withAlpha(sysConfColor(sysConfidence), 0.12), color: sysConfColor(sysConfidence) }"
          >{{ SYS_CONF_LABEL[sysConfidence] || sysConfidence }}</span
        >
      </template>
      由程序合成（证据核验×数据完备度×排名等客观信号），与 AI 口头置信度相互独立：{{ sysConfidenceWhy || '—' }}
    </n-tooltip>
    <n-tooltip v-if="review" trigger="hover">
      <template #trigger>
        <span
          class="trust-chip"
          :style="{ background: withAlpha(verdictColor(review.verdict), 0.12), color: verdictColor(review.verdict) }"
          >{{ VERDICT_LABEL[review.verdict] || review.verdict }}</span
        >
      </template>
      {{ review.comment || '复核未给出说明' }}
    </n-tooltip>
  </div>
</template>

<style scoped>
.trust-row {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.trust-chip {
  font-size: 12px;
  font-weight: 600;
  padding: 2px 10px;
  border-radius: 20px;
  cursor: default;
}
.trust-plain {
  border: 1px solid v-bind('vars.dividerColor');
  opacity: 0.85;
  font-weight: 500;
}
</style>
