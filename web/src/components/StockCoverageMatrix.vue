<script setup lang="ts">
import { NTag, NTooltip } from 'naive-ui'
import SectionCard from '@/components/SectionCard.vue'
import { useUi } from '@/composables/useUi'
import type { StockCoverageItem, StockCoverageStatus } from './stockCoverage'

defineProps<{ items: StockCoverageItem[] }>()
const { vars } = useUi()

const statusMeta: Record<StockCoverageStatus, { label: string; type: 'success' | 'error' | 'warning' | 'default' }> = {
  available: { label: '可用', type: 'success' },
  missing: { label: '缺失', type: 'warning' },
  error: { label: '错误', type: 'error' },
  stale: { label: '过期', type: 'warning' },
  unknown: { label: '未知', type: 'default' },
}
</script>

<template>
  <SectionCard title="数据覆盖" :hoverable="false">
    <div class="coverage-grid">
      <div v-for="item in items" :key="item.key" class="coverage-item">
        <div class="coverage-head">
          <strong>{{ item.label }}</strong>
          <n-tooltip v-if="item.note" trigger="hover">
            <template #trigger>
              <n-tag :type="statusMeta[item.status].type" size="small" round :bordered="false">
                {{ statusMeta[item.status].label }}
              </n-tag>
            </template>
            {{ item.note }}
          </n-tooltip>
          <n-tag v-else :type="statusMeta[item.status].type" size="small" round :bordered="false">
            {{ statusMeta[item.status].label }}
          </n-tag>
        </div>
        <div class="coverage-source"><span>来源</span>{{ item.source || '—' }}</div>
        <div class="coverage-asof qv-tnum"><span>as_of</span>{{ item.asOf || '—' }}</div>
      </div>
    </div>
  </SectionCard>
</template>

<style scoped>
.coverage-grid {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 1px;
  overflow: hidden;
  border: 1px solid color-mix(in srgb, currentColor 14%, transparent);
  border-radius: 6px;
  background: color-mix(in srgb, currentColor 14%, transparent);
}
.coverage-item {
  min-width: 0;
  min-height: 82px;
  padding: 10px;
  background: v-bind('vars.cardColor');
}
.coverage-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 6px;
}
.coverage-head strong {
  min-width: 0;
  font-size: 13px;
  overflow-wrap: anywhere;
}
.coverage-source,
.coverage-asof {
  display: flex;
  gap: 5px;
  margin-top: 7px;
  font-size: 11px;
  line-height: 1.4;
  opacity: 0.62;
  overflow-wrap: anywhere;
}
.coverage-source span,
.coverage-asof span {
  flex: 0 0 auto;
  opacity: 0.72;
}
.coverage-asof {
  margin-top: 2px;
}
@media (max-width: 900px) {
  .coverage-grid { grid-template-columns: repeat(2, minmax(0, 1fr)); }
}
@media (max-width: 420px) {
  .coverage-grid { grid-template-columns: 1fr; }
}
</style>
