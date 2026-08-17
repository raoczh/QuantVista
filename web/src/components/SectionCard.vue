<script setup lang="ts">
import { NCard } from 'naive-ui'

withDefaults(
  defineProps<{
    title?: string
    hoverable?: boolean
    size?: 'small' | 'medium' | 'huge'
  }>(),
  { hoverable: false, size: 'medium' },
)
</script>

<template>
  <n-card
    class="section-card"
    :class="{ 'is-hoverable': hoverable }"
    :size="size"
    :bordered="true"
  >
    <template v-if="title || $slots.extra" #header>
      <div class="sc-header">
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
  border-radius: 8px;
  box-shadow: none;
  transition: border-color 0.18s ease;
}
.section-card.is-hoverable:hover {
  border-color: var(--qv-primary);
}
.sc-header {
  display: flex;
  align-items: center;
  gap: 9px;
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
