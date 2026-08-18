<script setup lang="ts">
import { computed } from 'vue'
import { NAlert, NTag } from 'naive-ui'
import SectionCard from '@/components/SectionCard.vue'
import type { PortfolioAccount, PortfolioOverview, PortfolioRisk } from '@/api/portfolio'

const props = defineProps<{
  account: PortfolioAccount | null
  overview: PortfolioOverview | null
  risk: PortfolioRisk | null
  reasons: string[]
}>()

const isPartial = computed(() => !!props.overview?.partial_reasons.length || !!props.reasons.length)
const conclusion = computed(() => {
  if (!props.overview || !props.risk) return '组合数据尚未完成读取，当前不能判断风险。'
  if (props.overview.coverage_pct < 60) return '已定价持仓覆盖不足，先补齐行情数据，再判断组合风险。'
  if (props.overview.top_n_weight_pct >= 80) return '组合集中度较高，单一或少数持仓波动可能明显影响总资产。'
  const drawdown = props.risk.max_drawdown.metric
  if (drawdown.status === 'available' && Math.abs(drawdown.value || 0) >= 15) return '历史窗口内回撤较大，需要核对高风险贡献持仓和相关性。'
  return '当前没有单项程序指标触发高优先级结论，仍需检查集中度、样本范围和数据缺口。'
})
const nextCheck = computed(() => {
  if (!props.overview) return '请重试读取组合数据。'
  if (props.overview.coverage_pct < 100) return `先处理未定价持仓；当前覆盖 ${props.overview.coverage_pct}%。`
  if (props.overview.top_n_weight_pct >= 80) return '检查 Top 5 持仓权重和行业暴露，不会自动生成调仓交易。'
  return '查看个股风险贡献与历史回撤，再决定是否需要人工调整研究计划。'
})
</script>

<template>
  <div class="risk-intro">
    <SectionCard title="当前组合结论">
      <div class="conclusion-head">
        <strong>{{ conclusion }}</strong>
        <n-tag :type="isPartial ? 'warning' : 'info'">{{ isPartial ? '数据不完整' : '研究结论' }}</n-tag>
      </div>
      <p>{{ nextCheck }}</p>
      <small>
        {{ account?.kind === 'paper' ? '模拟账户' : '真实账户' }} · 数据截止时间 {{ overview?.as_of || risk?.as_of || '未知' }} ·
        样本窗口 {{ risk?.window_days || '未知' }} 个交易日
      </small>
    </SectionCard>
    <SectionCard title="最需要处理的风险">
      <n-alert v-if="isPartial" type="warning" :bordered="false" title="部分数据不可判定">
        {{ [...(overview?.partial_reasons || []), ...reasons].slice(0, 4).join('；') }}
      </n-alert>
      <p v-else>优先检查集中度最高的持仓、行业暴露和风险贡献；这里仅提供研究提示，不会自动调仓。</p>
    </SectionCard>
  </div>
</template>

<style scoped>
.risk-intro { display: grid; grid-template-columns: 1.4fr 1fr; gap: 12px; margin: 0 0 16px; }
.conclusion-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; }
.conclusion-head strong { line-height: 1.55; }
p { margin: 10px 0 0; line-height: 1.6; }
small { display: block; margin-top: 10px; opacity: .62; }
@media (max-width: 768px) { .risk-intro { grid-template-columns: 1fr; } }
</style>
