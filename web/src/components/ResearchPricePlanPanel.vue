<script setup lang="ts">
import { computed } from 'vue'
import { NTag } from 'naive-ui'
import type { ResearchPricePlan } from '@/api/pricePlan'
import { formatPrice } from '@/lib/formatPrice'

const props = defineProps<{ plan: ResearchPricePlan; compact?: boolean }>()
const usable = computed(() => props.plan.status !== 'unavailable' && !!props.plan.exit && props.plan.buy_low > 0 && props.plan.buy_high > props.plan.buy_low)
</script>

<template>
  <section class="research-price-plan" aria-label="程序买卖计划">
    <div class="plan-heading">
      <strong>程序买卖计划</strong>
      <n-tag size="small" :type="plan.status === 'ready' ? 'success' : 'warning'" :bordered="false">
        {{ plan.status === 'ready' ? '生成时满足价格条件' : plan.status === 'wait' ? '等待条件' : '数据不足' }}
      </n-tag>
    </div>
    <p class="plan-context">{{ plan.context.strategy_name }} · {{ plan.context.horizon === 'long_term' ? '长线' : '短线' }} · 规划版本 {{ plan.version }}</p>
    <dl v-if="usable" class="price-values">
      <div><dt>买入观察区间</dt><dd>{{ formatPrice(plan.buy_low) }} ～ {{ formatPrice(plan.buy_high) }}</dd></div>
      <div><dt>第一目标</dt><dd>{{ formatPrice(plan.exit!.target_price) }}</dd></div>
      <div><dt>初始止损</dt><dd>{{ formatPrice(plan.exit!.stop_price) }}</dd></div>
      <div><dt>延伸目标</dt><dd>{{ formatPrice(plan.exit!.extended_target) }}</dd></div>
    </dl>
    <p v-if="usable && !compact" class="plan-context">按上沿 {{ formatPrice(plan.buy_high) }}、{{ plan.exit!.quantity }} 股估算，扣费后第一目标收益/风险为 {{ plan.exit!.net_reward_risk.toFixed(2) }}；入场条件最多观察 {{ plan.entry_valid_days }} 个交易日，持有复核窗口 {{ plan.horizon_days }} 个交易日。</p>
    <ul v-if="plan.reasons?.length" class="plan-reasons"><li v-for="reason in plan.reasons" :key="reason">{{ reason }}</li></ul>
    <p class="plan-context">报价截至 {{ plan.quote_as_of || '未知' }} · 完整日线截至 {{ plan.bars_as_of || '未知' }}</p>
    <details v-if="!compact && plan.evidence?.length"><summary>价格依据及与其他页面的差异</summary>
      <ul><li v-for="line in plan.evidence" :key="line">{{ line }}</li></ul>
      <p>个股分析与推荐共用这一算法；策略、周期及输入时点相同才应比较价位。旧记录保留旧算法结果；实际持仓的保护价随成交成本、峰值和减仓阶段变化。</p>
    </details>
    <p class="plan-context">目标是风险规划价。当前是否可执行需结合最新报价和账户状态；到价提醒不代表已成交。</p>
  </section>
</template>

<style scoped>
.research-price-plan { margin: 12px 0; padding: 12px; border: 1px solid rgba(128,128,128,.24); border-radius: 8px; min-width: 0; overflow-wrap: anywhere; }
.plan-heading { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; }
.plan-context { font-size: 12px; opacity: .72; line-height: 1.6; margin: 7px 0; }
.price-values { display: grid; grid-template-columns: repeat(2,minmax(0,1fr)); gap: 10px; margin: 12px 0; }
.price-values dt { font-size: 12px; opacity: .7; }
.price-values dd { margin: 4px 0 0; font-weight: 600; font-variant-numeric: tabular-nums; }
.plan-reasons, details { font-size: 12px; line-height: 1.65; }
ul { padding-left: 18px; }
summary { cursor: pointer; }
@media (max-width: 340px) { .price-values { grid-template-columns: 1fr; } }
</style>
