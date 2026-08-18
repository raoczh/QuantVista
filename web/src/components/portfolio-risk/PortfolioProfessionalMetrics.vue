<script setup lang="ts">
import { NAlert } from 'naive-ui'
import StatCard from '@/components/StatCard.vue'
import TermHelp from '@/components/TermHelp.vue'
import { TERM_DICTIONARY, type TermKey } from '@/components/termDictionary'
import { useDisplayMode } from '@/composables/useDisplayMode'
import type { PortfolioRisk, RiskMetric } from '@/api/portfolio'

defineProps<{ risk: PortfolioRisk | null; reasons: string[] }>()
const { isPlain } = useDisplayMode()
function label(term: TermKey) {
  const definition = TERM_DICTIONARY[term]
  return isPlain.value ? definition.plain : definition.professional
}
function value(metric: RiskMetric | undefined, suffix = '') {
  return metric?.status === 'available' && metric.value != null ? `${metric.value.toFixed(2)}${suffix}` : '不可用'
}
</script>

<template>
  <section class="professional-metrics" aria-labelledby="professional-risk-title">
    <div class="metric-title">
      <div><h3 id="professional-risk-title">专业指标和计算说明</h3><p>以下指标只在样本满足要求时展示数值；不可用不等于风险为零。</p></div>
      <div><TermHelp term="twr" /> · <TermHelp term="sharpe" /> · <TermHelp term="sortino" /> · <TermHelp term="beta" /> · <TermHelp term="alpha" /></div>
    </div>
    <div class="metric-grid">
      <StatCard :label="label('twr')" :value="value(risk?.twr_pct, '%')" />
      <StatCard label="年化波动" :value="value(risk?.annualized_volatility_pct, '%')" />
      <StatCard label="预测波动" :value="value(risk?.risk_contribution.predicted_volatility_pct, '%')" />
      <StatCard label="下行波动" :value="value(risk?.downside_volatility_pct, '%')" />
      <StatCard :label="label('sharpe')" :value="value(risk?.sharpe)" />
      <StatCard :label="label('sortino')" :value="value(risk?.sortino)" />
      <StatCard :label="label('beta')" :value="value(risk?.beta)" />
      <StatCard :label="label('alpha')" :value="value(risk?.alpha_pct, '%')" />
      <StatCard label="最大回撤" :value="value(risk?.max_drawdown.metric, '%')" />
    </div>
    <n-alert v-if="reasons.length" type="warning" title="部分指标不可用">{{ reasons.join('；') }}</n-alert>
    <p class="calculation-note">数据截止时间 {{ risk?.as_of || '未知' }} · 样本范围 {{ risk?.window_days || '未知' }} 个交易日 · 参数版本 {{ risk?.data_version || '未知' }}</p>
  </section>
</template>

<style scoped>
.professional-metrics { display: grid; gap: 12px; }
.metric-title { display: flex; justify-content: space-between; align-items: flex-end; gap: 16px; }
h3, p { margin: 0; }
.metric-title p, .calculation-note { margin-top: 4px; opacity: .62; }
.metric-grid { display: grid; grid-template-columns: repeat(auto-fit, minmax(150px, 1fr)); gap: 12px; }
@media (max-width: 768px) { .metric-title { align-items: flex-start; flex-direction: column; } }
</style>
