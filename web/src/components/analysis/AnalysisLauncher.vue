<script setup lang="ts">
import { computed } from 'vue'
import { NButton, NDatePicker, NForm, NFormItem, NInput, NSelect, NSwitch, NTag } from 'naive-ui'
import type { AnalyzeRequest, AnalysisModule } from '@/api/analysis'
import type { StockRef } from '@/composables/useStockActions'
import SectionCard from '@/components/SectionCard.vue'
import StockIdentity from '@/components/StockIdentity.vue'
import StockPicker from '@/components/StockPicker.vue'

const form = defineModel<AnalyzeRequest>('form', { required: true })
const selectedStock = defineModel<StockRef | null>('selectedStock', { required: true })
const panelMode = defineModel<boolean>('panelMode', { required: true })
const verifyMode = defineModel<boolean>('verifyMode', { required: true })
const asOfTs = defineModel<number | null>('asOfTs', { required: true })

const props = defineProps<{
  moduleOptions: Array<{ label: string; value: AnalysisModule }>
  marketOptions: Array<{ label: string; value: string }>
  llmOptions: Array<{ label: string; value: number }>
  llmLoading: boolean
  running: boolean
  riskLabel: string
  asOf: string
}>()
const emit = defineEmits<{
  (event: 'analyze'): void
  (event: 'stock-change', value: StockRef | null): void
}>()

const needSymbol = computed(() => form.value.module === 'stock')
const needMarket = computed(() => ['stock', 'market', 'sector'].includes(form.value.module))
const needTarget = computed(() => form.value.module === 'sector')
const scopeExplanation = computed(() => ({
  stock: '只分析下方选中的一只股票；名称、代码和市场会一起确认。',
  position: '分析当前账号全部持有中的仓位，不会按相同代码猜测某一笔持仓，也不会修改持仓。',
  watchlist: '分析当前账号的自选组合，不读取其他用户数据。',
  market: '分析 A 股整体市场，不针对单只股票。',
  sector: '分析所填板块；板块名称为空时按全市场板块概览处理。',
} as Record<AnalysisModule, string>)[form.value.module])
const periodExplanation = computed(() => props.asOf
  ? `历史诊断：证据截断到 ${props.asOf}，回溯结果不会混入实时建议。`
  : panelMode.value
    ? '多角色模式：技术、动量、风险和反方同时给观点，不额外生成交易计划。'
    : '标准模式：优先解释当前证据，结论在行情或风险事实变化后失效。')
function dateDisabled(ts: number) {
  const today = new Date()
  today.setHours(0, 0, 0, 0)
  return ts >= today.getTime()
}
</script>

<template>
  <SectionCard title="发起分析">
    <div class="scope-facts">
      <n-tag size="small" :bordered="false">{{ riskLabel }}风险偏好</n-tag>
      <span>{{ scopeExplanation }}</span>
    </div>
    <n-form label-placement="top" :show-feedback="false">
      <n-form-item label="分析范围">
        <n-select v-model:value="form.module" :options="moduleOptions" />
      </n-form-item>
      <n-form-item v-if="needMarket && !needSymbol" label="市场">
        <n-select v-model:value="form.market" :options="marketOptions" />
      </n-form-item>
      <n-form-item v-if="needSymbol" label="目标股票">
        <StockPicker :model-value="selectedStock" @update:model-value="(value) => emit('stock-change', value)" />
      </n-form-item>
      <div v-if="selectedStock && needSymbol" class="selected-stock">
        <StockIdentity :symbol="selectedStock.symbol" :market="selectedStock.market" :name="selectedStock.name" />
      </div>
      <n-form-item v-if="needTarget" label="板块名称（可选）">
        <n-input v-model:value="form.target" placeholder="例如：半导体、银行" />
      </n-form-item>
      <n-form-item v-if="form.module === 'stock'" label="分析时点">
        <n-date-picker v-model:value="asOfTs" type="date" clearable :is-date-disabled="dateDisabled" placeholder="留空为当前分析" />
      </n-form-item>
      <p class="field-help">{{ periodExplanation }}</p>
      <n-form-item v-if="form.module === 'stock' && !asOf" label="多角色观点">
        <n-switch v-model:value="panelMode" />
        <span class="switch-help">技术 / 动量 / 风控 / 反方四个独立视角</span>
      </n-form-item>
      <n-form-item v-if="!(form.module === 'stock' && panelMode)" label="AI 复核">
        <n-switch v-model:value="verifyMode" />
        <span class="switch-help">独立复核员只挑错，不改写程序风险等级或任务状态</span>
      </n-form-item>
      <n-form-item label="模型配置">
        <n-select v-model:value="form.llm_config_id" :options="llmOptions" :loading="llmLoading" placeholder="未配置时使用系统默认" />
      </n-form-item>
      <n-form-item label="附加问题（可选）">
        <n-input v-model:value="form.question" type="textarea" :autosize="{ minRows: 2, maxRows: 5 }" maxlength="500" placeholder="希望重点回答什么" />
      </n-form-item>
      <n-button type="primary" block :loading="running" :disabled="running" @click="emit('analyze')">
        {{ running ? '任务处理中' : '明确开始分析' }}
      </n-button>
      <p class="submit-note">选股、切换范围、打开历史和进入页面都不会发起 AI 请求。失败时已显示的历史或快照数据不会被清空。</p>
    </n-form>
  </SectionCard>
</template>

<style scoped>
.scope-facts { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-bottom: 12px; }
.scope-facts span,
.field-help,
.switch-help,
.submit-note { font-size: 12px; line-height: 1.55; opacity: .68; }
.selected-stock { margin: -4px 0 14px; padding: 9px 10px; border: 1px solid rgba(128,128,128,.24); border-radius: 6px; }
.field-help { margin: -7px 0 12px; }
.switch-help { margin-left: 8px; }
.submit-note { margin: 8px 0 0; }
</style>
