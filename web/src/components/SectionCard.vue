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

const { vars, isDark, primaryAlpha } = useUi()

// 静态质感：浅色用双层柔和阴影让卡片"浮"在底色上；深色用顶部 1px 内高光模拟受光面。
const restShadow = computed(() =>
  isDark.value
    ? 'inset 0 1px 0 rgba(255, 255, 255, 0.045)'
    : '0 1px 2px rgba(0, 0, 0, 0.04), 0 4px 16px rgba(0, 0, 0, 0.05)',
)
const hoverShadow = computed(() =>
  isDark.value
    ? `inset 0 1px 0 rgba(255, 255, 255, 0.045), 0 8px 24px ${primaryAlpha(0.2)}`
    : `0 1px 2px rgba(0, 0, 0, 0.04), 0 8px 24px ${primaryAlpha(0.16)}`,
)

const styleVars = computed(() => ({
  '--sc-primary': vars.value.primaryColor,
  '--sc-primary-suppl': vars.value.primaryColorSuppl,
  '--sc-shadow-rest': restShadow.value,
  '--sc-shadow-hover': hoverShadow.value,
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
  box-shadow: var(--sc-shadow-rest);
  transition:
    border-color 0.2s ease,
    box-shadow 0.2s ease,
    transform 0.2s ease;
}
.section-card.is-hoverable:hover {
  border-color: var(--sc-primary);
  box-shadow: var(--sc-shadow-hover);
  transform: translateY(-2px);
}
.sc-header {
  display: flex;
  align-items: center;
  gap: 9px;
}
.sc-bar {
  width: 3px;
  height: 15px;
  border-radius: 2px;
  background: linear-gradient(180deg, var(--sc-primary), var(--sc-primary-suppl));
}
.sc-title {
  font-weight: 600;
  font-size: 15px;
}

/* 移动端：卡片内容区可横向滚动，宽表格不撑破整页布局；
 * 表格单元格不折行（挤压成一列一字反而没法看），滚动查看。 */
@media (max-width: 768px) {
  .section-card :deep(.n-card__content) {
    overflow-x: auto;
    -webkit-overflow-scrolling: touch;
  }
  .section-card :deep(.n-table th),
  .section-card :deep(.n-table td) {
    white-space: nowrap;
  }
}
</style>
