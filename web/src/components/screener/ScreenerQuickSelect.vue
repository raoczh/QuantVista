<script setup lang="ts">
import { NAlert, NButton, NEmpty, NInputNumber, NSpin, NTag } from 'naive-ui'
import SectionCard from '@/components/SectionCard.vue'
import {
  PERIOD_LABEL,
  RISK_LABEL,
  RISK_TAG_TYPE,
  type RetailTemplate,
} from '@/api/screener'

defineProps<{
  templates: RetailTemplate[]
  values: Record<string, Record<string, number>>
  loading: boolean
  error: string
  scanning: string
}>()

const emit = defineEmits<{
  scan: [template: RetailTemplate]
  retry: []
  'update-param': [templateKey: string, paramKey: string, value: number | null]
}>()
</script>

<template>
  <SectionCard title="快速选股" class="block">
    <n-spin :show="loading">
      <n-alert v-if="error" type="error" :bordered="false" class="load-state">
        <span>常用模板读取失败，不能把缺失数据当成可用策略。</span>
        <n-button size="small" @click="emit('retry')">重试</n-button>
      </n-alert>
      <n-empty v-else-if="!loading && !templates.length" description="暂无可用的确定性模板" class="load-state">
        <template #extra><n-button size="small" @click="emit('retry')">重新读取</n-button></template>
      </n-empty>
      <div v-else class="template-grid">
        <section v-for="template in templates" :key="template.key" class="template-item">
          <div class="template-head">
            <strong>{{ template.name }}</strong>
            <span class="template-tags">
              <n-tag size="tiny" :bordered="false">{{ PERIOD_LABEL[template.period] || template.period }}</n-tag>
              <n-tag size="tiny" :bordered="false" :type="RISK_TAG_TYPE[template.risk_level] || 'default'">
                {{ RISK_LABEL[template.risk_level] || template.risk_level }}
              </n-tag>
            </span>
          </div>
          <dl class="template-notes">
            <div><dt>适用场景</dt><dd>{{ template.scenario }}</dd></div>
            <div><dt>主要风险</dt><dd>{{ template.risk }}</dd></div>
            <div><dt>数据要求</dt><dd>{{ template.data_requirements }}</dd></div>
          </dl>
          <div class="template-params">
            <label v-for="param in template.params.slice(0, 3)" :key="param.key">
              <span>{{ param.label }}</span>
              <n-input-number
                :value="values[template.key]?.[param.key]"
                :min="param.min"
                :max="param.max"
                :step="param.step"
                size="small"
                @update:value="emit('update-param', template.key, param.key, $event)"
              >
                <template v-if="param.unit" #suffix>{{ param.unit }}</template>
              </n-input-number>
            </label>
          </div>
          <div class="condition-list" aria-label="模板条件">
            <n-tag v-for="condition in template.conditions" :key="condition" size="small" :bordered="false">
              {{ condition }}
            </n-tag>
          </div>
          <n-button
            size="small"
            type="primary"
            secondary
            :loading="scanning === `retail-${template.key}`"
            :disabled="!!scanning && scanning !== `retail-${template.key}`"
            @click="emit('scan', template)"
          >
            开始扫描
          </n-button>
        </section>
      </div>
    </n-spin>
  </SectionCard>
</template>

<style scoped>
.template-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(100%, 300px), 1fr));
  gap: 12px;
}
.load-state { min-height: 96px; }
.template-item {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 12px;
  padding: 14px;
  border: 1px solid var(--qv-divider);
  border-radius: 8px;
}
.template-head,
.template-tags,
.condition-list {
  display: flex;
  align-items: center;
  gap: 6px;
  flex-wrap: wrap;
}
.template-head { justify-content: space-between; }
.template-notes { display: grid; gap: 6px; margin: 0; }
.template-notes div { display: grid; grid-template-columns: 64px 1fr; gap: 8px; }
.template-notes dt { opacity: .62; }
.template-notes dd { min-width: 0; margin: 0; overflow-wrap: anywhere; }
.template-params {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 8px;
}
.template-params label { display: grid; gap: 4px; font-size: 12px; }
.template-item > .n-button { align-self: flex-end; }
@media (max-width: 768px) {
  .template-params { grid-template-columns: 1fr; }
  .template-item > .n-button { width: 100%; }
}
</style>
