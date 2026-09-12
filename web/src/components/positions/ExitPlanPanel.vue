<script setup lang="ts">
import { computed } from 'vue'
import { NTag, useThemeVars } from 'naive-ui'
import type { ExitPlan, ExitPlanSeed } from '@/api/position'
import { formatPrice } from '@/lib/formatPrice'

const props = defineProps<{ seed?: ExitPlanSeed | null; plan?: ExitPlan | null; compact?: boolean }>()
const theme = useThemeVars()
const initial = computed(() => props.plan?.initial || props.seed)
const status = computed(() => props.plan?.data_status || initial.value?.data_status)
function messages(value: unknown): string[] { return Array.isArray(value) ? value.filter((item): item is string => typeof item === 'string') : [] }
const gaps = computed(() => messages(props.plan?.data_gaps || initial.value?.data_gaps))
const evidence = computed(() => messages(props.plan?.evidence || initial.value?.evidence))
const stages: Record<string, string> = {
  initial: '初始保护', tightened: '风险保护上移', breakeven: '净保本保护', profit_lock: '盈利保护',
  first_target: '第一目标已触达', extended_target: '延伸目标已触达', stop_triggered: '保护价已触发',
  time_review: '持有期需要复核',
}
const sources: Record<string, string> = {
  preview: '买入前预览', entry: '建仓时规划', recorded_later: '补录时规划', first_assessment: '首次评估规划',
  plan_edit: '编辑后规划', add_buy: '加仓后规划', corporate_adjustment: '公司行动折算', recommendation: '推荐时规划',
  data_recovery: '数据核验后重建',
}
function price(value: unknown) { return typeof value === 'number' && Number.isFinite(value) && value > 0 ? formatPrice(value) : '待计算' }
function number(value: unknown, digits = 2) { return typeof value === 'number' && Number.isFinite(value) ? value.toFixed(digits) : '—' }
</script>

<template>
  <section v-if="initial" class="exit-plan-panel" :class="{ compact }" :style="{ '--exit-divider': theme.dividerColor, '--exit-muted': theme.textColor3, '--exit-warning': theme.warningColor }" aria-label="持仓退出规划">
    <div class="exit-plan-heading">
      <strong>退出规划</strong>
      <n-tag size="small" :bordered="false" :type="status === 'ready' ? 'default' : 'warning'">
        {{ status === 'unavailable' ? '数据不足' : plan ? stages[plan.stage] || '持续跟踪' : sources[initial.source] || '初始规划' }}
      </n-tag>
    </div>
    <dl class="exit-plan-prices">
      <div><dt>初始止损</dt><dd>{{ price(initial.stop_price) }}</dd></div>
      <div><dt>{{ plan ? '最近保护价' : initial.estimated_reward > 0 ? '第一止盈目标' : '第一价格复核点' }}</dt><dd>{{ price(plan ? plan.current_stop : initial.target_price) }}</dd></div>
      <div v-if="plan"><dt>{{ initial.estimated_reward > 0 ? '第一止盈目标' : '第一价格复核点' }}</dt><dd>{{ price(initial.target_price) }}<small v-if="plan.first_target_at">{{ plan.first_target_handled ? '已登记减仓' : '已触达' }}</small></dd></div>
      <div><dt>{{ initial.estimated_extended_reward > 0 ? '延伸止盈目标' : '延伸价格复核点' }}</dt><dd>{{ price(initial.extended_target) }}<small v-if="plan?.second_target_at">{{ plan.second_target_handled ? '已登记减仓' : '已触达' }}</small></dd></div>
      <div v-if="plan"><dt>估算净保本价</dt><dd>{{ price(plan.breakeven_price) }}</dd></div>
      <div><dt>初始净收益 / 风险</dt><dd>{{ initial.estimated_risk > 0 ? number(initial.net_reward_risk) + ' 倍' : '待计算' }}</dd></div>
    </dl>
    <p v-if="!plan && initial.estimated_risk > 0" class="exit-plan-note">按 {{ initial.quantity }} 股估算：初始风险 {{ number(initial.estimated_risk) }} 元，第一目标净收益 {{ number(initial.estimated_reward) }} 元；已计估算卖出费税及滑点。</p>
    <p v-if="plan" class="exit-plan-note">账本估算可卖 {{ plan.sellable_quantity == null ? '未知' : number(plan.sellable_quantity, 0) + ' 股' }}<template v-if="plan.suggested_quantity > 0">，本阶段参考处理 {{ number(plan.suggested_quantity, 0) }} 股</template>。到价只生成提醒，成交后请登记减仓或平仓。</p>
    <p v-if="gaps.length" class="exit-plan-gaps">{{ gaps.join('；') }}</p>
    <details class="exit-plan-details">
      <summary>价位依据与执行条件</summary>
      <p>参考买价 {{ price(initial.entry_price) }}；完整日线截至 {{ initial.bars_as_of || '未知' }}；{{ sources[initial.source] || '规划' }} {{ initial.generated_at }}。</p>
      <p v-if="initial.atr14 > 0">初始 ATR {{ price(initial.atr14) }}，支撑 {{ price(initial.support) }}，阻力 {{ price(initial.resistance) }}。{{ initial.review_days }} 个交易日复核持有逻辑。</p>
      <p v-for="(item, i) in evidence" :key="`e-${i}`">{{ item }}</p>
      <p v-for="(item, i) in messages(plan?.execution_notes)" :key="`x-${i}`">{{ item }}</p>
      <p v-if="plan?.stop_effective_at">当前保护价生效于 {{ plan.stop_effective_at }}。保护价与目标价均不保证实际成交。</p>
    </details>
  </section>
</template>

<style scoped>
.exit-plan-panel { display: grid; gap: 10px; min-width: 0; padding: 12px; border: 1px solid var(--exit-divider); border-radius: 6px; }
.exit-plan-heading { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
.exit-plan-prices { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; margin: 0; }
.exit-plan-prices div { min-width: 0; }
.exit-plan-prices dt { color: var(--exit-muted); font-size: 12px; }
.exit-plan-prices dd { margin: 4px 0 0; font-size: 16px; font-weight: 600; font-variant-numeric: tabular-nums; overflow-wrap: anywhere; }
.exit-plan-prices small { display: block; font-size: 11px; color: var(--exit-muted); font-weight: 400; }
.exit-plan-note, .exit-plan-details { color: var(--exit-muted); font-size: 12px; line-height: 1.65; }
.exit-plan-panel p { margin: 0; overflow-wrap: anywhere; }
.exit-plan-gaps { color: var(--exit-warning); font-size: 12px; }
.exit-plan-details summary { cursor: pointer; }
.exit-plan-details p { margin-top: 6px; }
.compact { margin-block: 10px; }
@media (max-width: 560px) { .exit-plan-prices { grid-template-columns: repeat(2, minmax(0, 1fr)); } }
</style>
