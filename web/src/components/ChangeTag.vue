<script setup lang="ts">
import { computed } from 'vue'
import { useUi } from '@/composables/useUi'

const props = withDefaults(
  defineProps<{
    value: number
    suffix?: string
    showSign?: boolean
    size?: 'small' | 'medium'
    plain?: boolean
  }>(),
  { suffix: '%', showSign: true, size: 'medium', plain: false },
)

const { pctColor, pctBg } = useUi()
const color = computed(() => pctColor(props.value))
const bg = computed(() => (props.plain ? 'transparent' : pctBg(props.value)))
const text = computed(() => {
  const sign = props.showSign && props.value > 0 ? '+' : ''
  return `${sign}${props.value.toFixed(2)}${props.suffix}`
})
</script>

<template>
  <span
    class="change-tag qv-tnum"
    :class="[`is-${size}`, { 'is-plain': plain }]"
    :style="{ color, background: bg }"
  >
    {{ text }}
  </span>
</template>

<style scoped>
.change-tag {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-weight: 600;
  border-radius: 6px;
  white-space: nowrap;
}
.is-medium {
  padding: 2px 8px;
  font-size: 13px;
}
.is-small {
  padding: 1px 6px;
  font-size: 12px;
}
.is-plain {
  padding: 0;
}
</style>
