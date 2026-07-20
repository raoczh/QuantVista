<script setup lang="ts">
import { computed } from 'vue'
import { NTooltip } from 'naive-ui'
import { useUi } from '@/composables/useUi'

// 全站统一行情过期徽标：stale=警示「截至 <时刻>」、unknown=灰「时效未核验」、fresh 不渲染。
// 展示行保留最近有效价时必须挂本徽标（原则：可展示旧价，但不得冒充实时）。
const props = defineProps<{
  status?: string // fresh | stale | unknown
  asOf?: string // 行情数据源时刻（YYYY-MM-DD HH:mm）
  reason?: string // 过期原因说明（tooltip）
}>()

const { vars, flatColor, withAlpha } = useUi()

const show = computed(() => !!props.status && props.status !== 'fresh')
const isStale = computed(() => props.status === 'stale')
const label = computed(() => {
  if (isStale.value) {
    const t = (props.asOf || '').slice(5, 16) // MM-DD HH:mm
    return t ? `截至 ${t}` : '行情已过期'
  }
  return '时效未核验'
})
const color = computed(() => (isStale.value ? vars.value.warningColor : flatColor.value))
const tip = computed(() => {
  if (props.reason) return props.reason
  if (isStale.value)
    return `行情数据已过期（数据源时刻 ${props.asOf || '未知'}），非当前有效盘面；价格仅为最近已知值，请勿据此判断当前涨跌与盈亏`
  return '该市场无交易日历，无法核验行情时效，数据可能滞后'
})
</script>

<template>
  <n-tooltip v-if="show" trigger="hover" style="max-width: 320px">
    <template #trigger>
      <span class="fresh-tag" :style="{ color, background: withAlpha(color, 0.12) }">{{ label }}</span>
    </template>
    {{ tip }}
  </n-tooltip>
</template>

<style scoped>
.fresh-tag {
  display: inline-block;
  font-size: 11px;
  font-weight: 600;
  line-height: 18px;
  padding: 0 8px;
  border-radius: 10px;
  white-space: nowrap;
  cursor: default;
}
</style>
