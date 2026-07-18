<script setup lang="ts">
import { computed, ref } from 'vue'
import { NTooltip, NModal, NCollapse, NCollapseItem, NEmpty } from 'naive-ui'
import { useUi } from '@/composables/useUi'
import type { EvidenceCheck, TrustReview, SysConfidence } from '@/api/trust'

// 全站统一信任徽章：量化分/排名 · 一手成本 · 数值核验 · 综合置信 · AI 复核。
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
// 有 items 明细（ev2 起）才可点击展开；旧记录只显示 tooltip。
const evHasItems = computed(() => !!ev.value?.items?.length)
const evMatchedItems = computed(() => (ev.value?.items || []).filter((i) => i.matched))
const evUnmatchedItems = computed(() => (ev.value?.items || []).filter((i) => !i.matched))
const detailShow = ref(false)
function openDetail() {
  if (evHasItems.value) detailShow.value = true
}
function dirLabel(d?: string) {
  if (d === 'up') return '↑'
  if (d === 'down') return '↓'
  return ''
}
function reasonLabel(r?: string) {
  if (r === 'direction_mismatch') return '方向与快照相反（涨跌/流入流出不一致）'
  return '快照中未找到对应数值'
}

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
        <span
          class="trust-chip"
          :class="{ 'trust-clickable': evHasItems }"
          :style="{ background: withAlpha(evColor, 0.12), color: evColor }"
          @click="openDetail"
        >
          数值核验 {{ ev.matched }}/{{ ev.total }}
        </span>
      </template>
      <div style="max-width: 320px">
        数值存在性核验：AI 文本提取到 {{ ev.total }} 个可核验数值（去重），{{ ev.matched }} 个在快照中找到对应数值（±2% 容差）。<template
          v-if="ev.skipped_count"
          >另跳过 {{ ev.skipped_count }} 个（小整数/年份/代码等非核验对象）。</template
        >
        只验证数值是否存在，不代表字段语义与整段结论正确。<template v-if="evHasItems">点击查看逐项明细。</template>
      </div>
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

    <!-- 数值核验逐项明细 -->
    <n-modal
      v-model:show="detailShow"
      preset="card"
      title="数值核验明细"
      style="max-width: 720px"
      class="ev-modal"
    >
      <div class="ev-summary">
        共 {{ ev?.total }} 个可核验数值，{{ ev?.matched }} 个找到对应快照字段<template v-if="ev?.skipped_count"
          >；另跳过 {{ ev?.skipped_count }} 个（小整数/年份/代码等）</template
        >。这是数值存在性核验，不代表字段语义与整段结论正确。
      </div>
      <n-collapse :default-expanded-names="['unmatched']">
        <n-collapse-item
          v-if="evUnmatchedItems.length"
          :title="`未吻合 ${evUnmatchedItems.length} 项`"
          name="unmatched"
        >
          <div v-for="(it, i) in evUnmatchedItems" :key="'u' + i" class="ev-item">
            <div class="ev-line">
              <span class="ev-raw" :style="{ color: vars.warningColor }"
                >{{ it.raw }}{{ it.unit }} {{ dirLabel(it.direction) }}</span
              >
              <span v-if="it.count && it.count > 1" class="ev-count">×{{ it.count }}</span>
              <span v-if="it.module" class="ev-mod">{{ it.module }}</span>
            </div>
            <div class="ev-reason">{{ reasonLabel(it.reason) }}</div>
            <div v-if="it.sentence" class="ev-sent">「{{ it.sentence }}」</div>
          </div>
        </n-collapse-item>
        <n-collapse-item
          v-if="evMatchedItems.length"
          :title="`已吻合 ${evMatchedItems.length} 项`"
          name="matched"
        >
          <div v-for="(it, i) in evMatchedItems" :key="'m' + i" class="ev-item">
            <div class="ev-line">
              <span class="ev-raw" :style="{ color: upColor }"
                >{{ it.raw }}{{ it.unit }} {{ dirLabel(it.direction) }}</span
              >
              <span v-if="it.count && it.count > 1" class="ev-count">×{{ it.count }}</span>
              <span v-if="it.module" class="ev-mod">{{ it.module }}</span>
            </div>
            <div class="ev-match">
              {{ it.path }} = {{ it.snap_value }}<template v-if="it.tolerance"> （±{{ it.tolerance }}）</template
              ><template v-if="it.as_of"> · 数据时间 {{ it.as_of }}</template>
            </div>
            <div v-if="it.sentence" class="ev-sent">「{{ it.sentence }}」</div>
          </div>
        </n-collapse-item>
      </n-collapse>
      <n-empty v-if="!evMatchedItems.length && !evUnmatchedItems.length" description="无明细" size="small" />
      <div v-if="ev?.truncated" class="ev-trunc">明细已截断至 50 项</div>
    </n-modal>
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
.trust-clickable {
  cursor: pointer;
}
.trust-plain {
  border: 1px solid v-bind('vars.dividerColor');
  opacity: 0.85;
  font-weight: 500;
}
.ev-summary {
  font-size: 13px;
  color: v-bind('vars.textColor2');
  margin-bottom: 12px;
  line-height: 1.6;
}
.ev-item {
  padding: 6px 0;
  border-bottom: 1px solid v-bind('vars.dividerColor');
}
.ev-line {
  display: flex;
  align-items: center;
  gap: 8px;
  font-size: 13px;
}
.ev-raw {
  font-weight: 600;
}
.ev-count,
.ev-mod {
  font-size: 12px;
  color: v-bind('vars.textColor3');
}
.ev-reason,
.ev-match {
  font-size: 12px;
  color: v-bind('vars.textColor2');
  margin-top: 2px;
}
.ev-sent {
  font-size: 12px;
  color: v-bind('vars.textColor3');
  margin-top: 2px;
}
.ev-trunc {
  font-size: 12px;
  color: v-bind('vars.textColor3');
  margin-top: 10px;
}
</style>
