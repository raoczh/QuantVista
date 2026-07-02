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

const { vars, isDark, pctColor, primaryAlpha } = useUi()
const valueColor = computed(() =>
  props.changePct === undefined ? vars.value.textColorBase : pctColor(props.changePct),
)
// 与 SectionCard 同一套质感语言：浅色柔和投影、深色顶部内高光。
const styleVars = computed(() => ({
  '--stat-border': vars.value.borderColor,
  '--stat-bg': vars.value.cardColor,
  '--stat-shadow-rest': isDark.value
    ? 'inset 0 1px 0 rgba(255, 255, 255, 0.045)'
    : '0 1px 2px rgba(0, 0, 0, 0.03), 0 3px 12px rgba(0, 0, 0, 0.04)',
  '--stat-glow': primaryAlpha(0.12),
}))
</script>

<template>
  <div class="stat-card" :style="styleVars">
    <div class="stat-label">{{ label }}</div>
    <div class="stat-value qv-figure" :style="{ color: valueColor }">{{ value }}</div>
    <div class="stat-foot">
      <ChangeTag v-if="changePct !== undefined" :value="changePct" size="small" />
      <span v-if="sub" class="stat-sub">{{ sub }}</span>
    </div>
  </div>
</template>

<style scoped>
.stat-card {
  padding: 16px;
  border-radius: 12px;
  border: 1px solid var(--stat-border);
  background: var(--stat-bg);
  box-shadow: var(--stat-shadow-rest);
  transition:
    box-shadow 0.2s ease,
    transform 0.2s ease;
}
.stat-card:hover {
  box-shadow: var(--stat-shadow-rest), 0 6px 18px var(--stat-glow);
  transform: translateY(-1px);
}
.stat-label {
  font-size: 13px;
  opacity: 0.7;
  margin-bottom: 8px;
}
.stat-value {
  font-size: 26px;
  font-weight: 700;
  line-height: 1.1;
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
  opacity: 0.6;
}
</style>
