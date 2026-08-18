<script setup lang="ts">
import { computed } from 'vue'
import {
  NButton,
  NCollapse,
  NCollapseItem,
  NForm,
  NFormItem,
  NInputNumber,
  NRadioButton,
  NRadioGroup,
  NSelect,
  NSwitch,
  NTag,
} from 'naive-ui'
import type { RecFilters, RecommendRequest } from '@/api/recommendation'
import type { UserPreference } from '@/api/user'
import SectionCard from '@/components/SectionCard.vue'

const form = defineModel<RecommendRequest>('form', { required: true })
const filters = defineModel<RecFilters>('filters', { required: true })
const pricePreset = defineModel<number>('pricePreset', { required: true })
const capPreset = defineModel<number>('capPreset', { required: true })

const props = defineProps<{
  pref: UserPreference | null
  strategyOptions: Array<{ label: string; value: string }>
  marketOptions: Array<{ label: string; value: string }>
  pricePresetOptions: Array<{ label: string; value: number }>
  capPresetOptions: Array<{ label: string; value: number }>
  llmOptions: Array<{ label: string; value: number }>
  llmConfigured: boolean
  running: boolean
  savingFilters: boolean
}>()
const emit = defineEmits<{
  (event: 'generate'): void
  (event: 'save-filters'): void
  (event: 'preferences'): void
  (event: 'onboarding'): void
}>()

const riskLabel = computed(() => ({ conservative: '保守', aggressive: '激进', balanced: '均衡' })[props.pref?.risk_level || 'balanced'])
const horizonLabel = computed(() => ({ short_term: '短线', mid_term: '中线', long_term: '长线' })[props.pref?.horizon_pref || 'long_term'])
const callBudget = computed(() => 1 + (form.value.verify ? 1 : 0) + (form.value.bear_check ? 1 : 0))
</script>

<template>
  <SectionCard title="推荐生成">
    <template #extra>
      <n-button size="tiny" quaternary @click="emit('preferences')">投资偏好</n-button>
      <n-button size="tiny" quaternary @click="emit('onboarding')">首次使用引导</n-button>
    </template>

    <div class="preference-line">
      <n-tag size="small" :bordered="false">{{ horizonLabel }}</n-tag>
      <n-tag size="small" :bordered="false">{{ riskLabel }}风险</n-tag>
      <span>本次最多启用 {{ callBudget }} 个 AI 角色；结构修复仍遵循服务端原预算。</span>
    </div>

    <n-form label-placement="top" :show-feedback="false" class="form">
      <div class="form-grid">
        <n-form-item label="推荐周期">
          <n-radio-group v-model:value="form.type">
            <n-radio-button value="short_term">短线</n-radio-button>
            <n-radio-button value="long_term">长线</n-radio-button>
          </n-radio-group>
        </n-form-item>
        <n-form-item label="策略">
          <n-select v-model:value="form.strategy" :options="strategyOptions" />
        </n-form-item>
        <n-form-item label="市场">
          <n-select v-model:value="form.market" :options="marketOptions" />
        </n-form-item>
        <n-form-item label="结果数量">
          <n-input-number v-model:value="form.count" :min="3" :max="5" />
        </n-form-item>
      </div>

      <n-collapse class="advanced">
        <n-collapse-item title="数据要求与硬筛选" name="filters">
          <div class="filter-grid">
            <n-select v-model:value="pricePreset" :options="pricePresetOptions" size="small" />
            <n-select v-model:value="capPreset" :options="capPresetOptions" size="small" />
            <template v-if="pricePreset === -1">
              <n-input-number v-model:value="filters.price_min" :min="0" size="small" placeholder="价格下限" />
              <n-input-number v-model:value="filters.price_max" :min="0" size="small" placeholder="价格上限，0=不限" />
            </template>
            <template v-if="capPreset === -1">
              <n-input-number v-model:value="filters.float_cap_min_yi" :min="0" size="small" placeholder="流通市值下限（亿）" />
              <n-input-number v-model:value="filters.float_cap_max_yi" :min="0" size="small" placeholder="流通市值上限（亿）" />
            </template>
            <n-input-number v-model:value="filters.turnover_min" :min="0" :max="25" size="small" placeholder="换手率下限%" />
            <n-input-number v-model:value="filters.turnover_max" :min="0" :max="30" size="small" placeholder="换手率上限%" />
            <n-input-number v-model:value="filters.max_gain_5d_pct" :min="0" :max="100" size="small" placeholder="近5日涨幅上限%" />
          </div>
          <label class="switch-line"><span>排除已涨停</span><n-switch v-model:value="filters.exclude_limit_up" size="small" /></label>
          <label class="switch-line"><span>排除创业板 / 科创板</span><n-switch v-model:value="filters.exclude_gem_star" size="small" /></label>
          <div class="data-note">停牌、流动性不足、数据过期、黑名单和不满足策略的股票会保留排除原因，不会混入最终推荐。</div>
          <n-button size="small" tertiary :loading="savingFilters" @click="emit('save-filters')">保存为默认筛选</n-button>
        </n-collapse-item>
      </n-collapse>

      <div class="ai-options">
        <label class="switch-line">
          <span><b>AI 复核</b><small>独立核对证据和价位，多一次调用</small></span>
          <n-switch v-model:value="form.verify" />
        </label>
        <label class="switch-line">
          <span><b>反方研究员</b><small>仅展示反方意见，不改写原结论，多一次调用</small></span>
          <n-switch v-model:value="form.bear_check" />
        </label>
      </div>

      <n-form-item label="模型配置">
        <n-select v-model:value="form.llm_config_id" :options="llmOptions" :placeholder="llmConfigured ? '选择模型配置' : '未配置将使用系统默认配置'" />
      </n-form-item>
      <n-button class="generate-button" type="primary" block :loading="running" :disabled="running" @click="emit('generate')">
        {{ running ? '任务处理中' : '明确生成推荐' }}
      </n-button>
      <p class="submit-note">进入页面、切换周期、打开历史和导航到其他页面都不会创建 AI 任务。任何操作都不会自动下单或修改真实持仓。</p>
    </n-form>
  </SectionCard>
</template>

<style scoped>
.preference-line,
.switch-line {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
}
.preference-line {
  margin-bottom: 14px;
  flex-wrap: wrap;
  font-size: 12px;
  opacity: 0.72;
}
.form-grid,
.filter-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 12px;
}
.advanced {
  margin-bottom: 12px;
}
.filter-grid {
  gap: 8px;
  margin-bottom: 10px;
}
.switch-line {
  justify-content: space-between;
  margin: 8px 0;
}
.switch-line > span {
  display: grid;
  min-width: 0;
}
.switch-line small,
.data-note,
.submit-note {
  font-size: 12px;
  line-height: 1.55;
  opacity: 0.66;
}
.data-note {
  margin: 8px 0;
}
.ai-options {
  margin: 10px 0 14px;
}
.submit-note {
  margin: 8px 0 0;
}
@media (max-width: 560px) {
  .form-grid,
  .filter-grid {
    grid-template-columns: 1fr;
  }
  .generate-button {
    position: sticky;
    z-index: 7;
    bottom: calc(64px + env(safe-area-inset-bottom));
  }
}
</style>
