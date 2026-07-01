<script setup lang="ts">
import { computed } from 'vue'
import { NCard } from 'naive-ui'
import { useUi } from '@/composables/useUi'

withDefaults(
  defineProps<{
    title?: string
    hoverable?: boolean
    size?: 'small' | 'medium' | 'huge'
  }>(),
  { hoverable: true, size: 'medium' },
)

const { vars, primaryAlpha } = useUi()
const styleVars = computed(() => ({
  '--sc-primary': vars.value.primaryColor,
  '--sc-shadow': primaryAlpha(0.16),
}))
</script>

<template>
  <n-card
    class="section-card"
    :class="{ 'is-hoverable': hoverable }"
    :size="size"
    :bordered="true"
    :style="styleVars"
  >
    <template v-if="title || $slots.extra" #header>
      <div class="sc-header">
        <span class="sc-bar" />
        <span class="sc-title">{{ title }}</span>
      </div>
    </template>
    <template v-if="$slots.extra" #header-extra>
      <slot name="extra" />
    </template>
    <slot />
  </n-card>
</template>

<style scoped>
.section-card {
  border-radius: 14px;
  transition:
    border-color 0.2s ease,
    box-shadow 0.2s ease,
    transform 0.2s ease;
}
.section-card.is-hoverable:hover {
  border-color: var(--sc-primary);
  box-shadow: 0 8px 24px var(--sc-shadow);
  transform: translateY(-2px);
}
.sc-header {
  display: flex;
  align-items: center;
  gap: 9px;
}
.sc-bar {
  width: 3px;
  height: 14px;
  border-radius: 2px;
  background: var(--sc-primary);
}
.sc-title {
  font-weight: 600;
  font-size: 15px;
}
</style>
