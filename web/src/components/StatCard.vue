<script setup lang="ts">
import { computed } from 'vue'
import { useUi } from '@/composables/useUi'
import ChangeTag from './ChangeTag.vue'

const props = defineProps<{
  label: string
  value: string | number
  changePct?: number
  sub?: string
}>()

const { vars, pctColor } = useUi()
const valueColor = computed(() =>
  props.changePct === undefined ? vars.value.textColorBase : pctColor(props.changePct),
)
const styleVars = computed(() => ({
  '--stat-border': vars.value.borderColor,
  '--stat-bg': vars.value.cardColor,
}))
</script>

<template>
  <div class="stat-card" :style="styleVars">
    <div class="stat-label">{{ label }}</div>
    <div class="stat-value qv-figure" :title="String(value)" :style="{ color: valueColor }">{{ value }}</div>
    <div class="stat-foot">
      <ChangeTag v-if="changePct !== undefined" :value="changePct" size="small" />
      <span v-if="sub" class="stat-sub">{{ sub }}</span>
    </div>
  </div>
</template>

<style scoped>
.stat-card {
  padding: 19px 20px;
  min-width: 0;
  border-radius: var(--qv-radius-card);
  border: 1px solid var(--stat-border);
  background: var(--stat-bg);
}
.stat-label {
  font-size: 13px;
  color: var(--qv-text-muted);
  margin-bottom: 8px;
}
.stat-value {
  font-size: 28px;
  font-weight: 650;
  letter-spacing: -0.035em;
  line-height: 1.1;
  /* 大额金额（7 位数含千分位）在移动端 2 列网格下会超出卡宽，截断优于溢出压邻卡 */
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.stat-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 8px;
  min-height: 20px;
}
.stat-sub {
  font-size: 12px;
  color: var(--qv-text-muted);
}

@media (max-width: 768px) {
  .stat-card {
    padding: 12px 14px;
  }
  .stat-value {
    font-size: 22px;
  }
}
</style>
