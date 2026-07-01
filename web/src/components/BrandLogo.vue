<script setup lang="ts">
import { computed } from 'vue'
import { useThemeVars } from 'naive-ui'
import { withAlpha } from '@/composables/useUi'

const props = withDefaults(defineProps<{ size?: number; showText?: boolean }>(), {
  size: 30,
  showText: true,
})

const vars = useThemeVars()

const markStyle = computed(() => ({
  width: `${props.size}px`,
  height: `${props.size}px`,
  borderRadius: `${Math.round(props.size * 0.28)}px`,
  background: `linear-gradient(135deg, ${vars.value.primaryColor}, ${vars.value.primaryColorSuppl})`,
  boxShadow: `0 4px 12px ${withAlpha(vars.value.primaryColor, 0.35)}`,
}))
const fontSize = computed(() => `${Math.round(props.size * 0.6)}px`)
</script>

<template>
  <div class="brand" :style="{ gap: `${Math.round(size * 0.34)}px` }">
    <div class="brand-mark" :style="markStyle">
      <svg :width="size * 0.62" :height="size * 0.62" viewBox="0 0 32 32" fill="none">
        <path
          d="M5 22 L13 14 L18 18 L27 8"
          stroke="rgba(255,255,255,0.96)"
          stroke-width="2.8"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
        <path
          d="M21 8 H27 V14"
          stroke="rgba(255,255,255,0.96)"
          stroke-width="2.8"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
    </div>
    <span v-if="showText" class="brand-text" :style="{ fontSize }">
      <span :style="{ color: vars.textColorBase }">Quant</span
      ><span :style="{ color: vars.primaryColor }">Vista</span>
    </span>
  </div>
</template>

<style scoped>
.brand {
  display: inline-flex;
  align-items: center;
  user-select: none;
}
.brand-mark {
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}
.brand-text {
  font-weight: 700;
  letter-spacing: -0.02em;
  line-height: 1;
  white-space: nowrap;
}
</style>
