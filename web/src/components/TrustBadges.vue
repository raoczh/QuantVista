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
// ev3 起主徽章「全绿」只认快照佐证：全部命中但 snapshot_matched=0（引用的数字全是
// AI 计划价/用户输入/上下文复述）不算「被数据证明」，配中性色而非绿色——防「快照
// 佐证为 0 但核验全绿」误导。旧记录（无 snapshot_matched 字段）保持旧口径。
const evAllRestated = computed(
  () =>
    evAllMatched.value &&
    ev.value?.snapshot_matched !== undefined &&
    (ev.value.snapshot_matched ?? 0) === 0 &&
    (ev.value.matched ?? 0) > 0,
)
const evColor = computed(() => {
  if (!evAllMatched.value) return vars.value.warningColor
  if (evAllRestated.value) return flatColor.value
  return upColor.value
})
// 有 items 明细（ev2 起）才可点击展开；旧记录只显示 tooltip。
const evHasItems = computed(() => !!ev.value?.items?.length)
const evMatchedItems = computed(() => (ev.value?.items || []).filter((i) => i.matched))
const evUnmatchedItems = computed(() => (ev.value?.items || []).filter((i) => !i.matched))
// ev4：结构化数据缺口与关键结论段佐证（旧记录无这些字段时 v-if 兜底不渲染）。
const evUnknowns = computed(() => ev.value?.unknowns || [])
const evKeyUnbacked = computed(() => {
  const ks = ev.value?.key_section
  return !!ks && ks.total > 0 && ks.snapshot_matched === 0
})
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
// 命中值域来源标注：区分「被数据快照佐证」与「模型复述自身计划价 / 用户输入 /
// 上下文文本」，后三者不得展示成「已被数据证明」。
function originLabel(o?: string) {
  if (o === 'plan') return 'AI 计划价'
  if (o === 'user') return '用户输入'
  if (o === 'context') return '上下文文本'
  return ''
}
// ev3 来源分类汇总（旧记录无分类字段时回退：不显示分类，不冒充快照佐证数）。
const evHasOriginSplit = computed(() => ev.value?.snapshot_matched !== undefined)
const evOriginSummary = computed(() => {
  const e = ev.value
  if (!e || !evHasOriginSplit.value) return ''
  const parts: string[] = [`数据快照佐证 ${e.snapshot_matched ?? 0}`]
  if (e.plan_matched) parts.push(`AI 计划价复述 ${e.plan_matched}`)
  if (e.user_matched) parts.push(`用户输入复述 ${e.user_matched}`)
  if (e.context_matched) parts.push(`上下文文本复述 ${e.context_matched}`)
  return parts.join(' · ')
})

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
        数值存在性核验：AI 文本提取到 {{ ev.total }} 个可核验数值（去重），{{ ev.matched }} 个找到对应来源（±2%
        容差）<template v-if="evOriginSummary">——{{ evOriginSummary }}</template
        >。<template v-if="evAllRestated"
          >⚠ 本次命中全部为合法复述（AI 计划价/用户输入/上下文），没有任何数字被数据快照佐证，请勿当作「已被数据证明」。</template
        ><template v-if="evKeyUnbacked"
          >⚠ 关键结论（{{ ev?.key_section?.module }}）中的数字无一被数据快照佐证。</template
        ><template v-if="evUnknowns.length"
          >本次数据快照有 {{ evUnknowns.length }} 个缺失数据段（见明细「数据缺口」）。</template
        ><template v-if="ev.skipped_count"
          >另跳过 {{ ev.skipped_count }} 个（小整数/年份/代码等非核验对象）。</template
        >
        仅「数据快照佐证」表示数字有数据支撑；「AI 计划价/用户输入/上下文文本复述」只是合法引用，并非被快照数据证明。只验证数值是否存在，不代表字段语义与整段结论正确。<template
          v-if="evHasItems"
          >点击查看逐项明细。</template
        >
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
        共 {{ ev?.total }} 个可核验数值，{{ ev?.matched }} 个找到对应来源<template v-if="evOriginSummary"
          >（{{ evOriginSummary }}）</template
        ><template v-if="ev?.skipped_count"
          >；另跳过 {{ ev?.skipped_count }} 个（小整数/年份/代码等）</template
        >。仅「数据快照佐证」有数据支撑，「AI 计划价/用户输入/上下文文本」为合法复述而非快照佐证。这是数值存在性核验，不代表字段语义与整段结论正确。
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
              <span v-if="it.evidence_id" class="ev-id">{{ it.evidence_id }}</span>
              <span v-if="it.count && it.count > 1" class="ev-count">×{{ it.count }}</span>
              <span v-if="it.module" class="ev-mod">{{ it.module }}</span>
              <span
                v-if="originLabel(it.origin)"
                class="ev-origin"
                :style="{ color: vars.warningColor, borderColor: withAlpha(vars.warningColor, 0.5) }"
                >{{ originLabel(it.origin) }}</span
              >
            </div>
            <div class="ev-match">
              {{ it.path }} = {{ it.snap_value }}<template v-if="it.tolerance"> （±{{ it.tolerance }}）</template
              ><template v-if="it.source"> · 来源 {{ it.source }}</template
              ><template v-if="it.as_of"> · 数据时间 {{ it.as_of }}</template>
              <template v-if="it.origin === 'plan'">
                · 该数字来自 AI 自身给出的计划价/公式输出，属合法复述，但并非被数据快照佐证</template
              >
              <template v-else-if="it.origin === 'user'">
                · 该数字来自用户输入（提问/设定阈值）的复述，并非被数据快照佐证</template
              >
              <template v-else-if="it.origin === 'context'">
                · 该数字来自新闻/公告标题等上下文文本的复述，并非数值快照佐证</template
              >
              <template v-else-if="!it.origin"> · 数据快照佐证</template>
            </div>
            <div v-if="it.sentence" class="ev-sent">「{{ it.sentence }}」</div>
          </div>
        </n-collapse-item>
        <n-collapse-item
          v-if="evUnknowns.length"
          :title="`数据缺口 ${evUnknowns.length} 项`"
          name="unknowns"
        >
          <div v-for="(u, i) in evUnknowns" :key="'k' + i" class="ev-item">
            <div class="ev-line">
              <span class="ev-raw" :style="{ color: flatColor }">{{ u.field_path }}</span>
            </div>
            <div class="ev-reason">{{ u.reason }}<template v-if="u.impact">——{{ u.impact }}</template></div>
          </div>
          <div class="ev-reason" style="margin-top: 6px">
            以上数据段本次快照缺失（缺失≠为零）：AI 已被要求对应维度只声明数据不足、不得臆测补齐。
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
.ev-id {
  font-size: 11px;
  font-family: monospace;
  color: v-bind('vars.textColor3');
  border: 1px solid v-bind('vars.dividerColor');
  border-radius: 10px;
  padding: 0 6px;
  line-height: 16px;
}
.ev-reason,
.ev-match {
  font-size: 12px;
  color: v-bind('vars.textColor2');
  margin-top: 2px;
}
.ev-origin {
  font-size: 11px;
  font-weight: 600;
  padding: 0 6px;
  border: 1px solid;
  border-radius: 10px;
  line-height: 16px;
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
