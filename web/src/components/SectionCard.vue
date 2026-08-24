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

/* Naive 的 .n-card-header__extra 是 display:flex 但没有 gap / flex-wrap / min-width:0，
 * 直接往 #extra 塞多个按钮会贴死、窄屏还会把标题挤没。在这里统一兜住，
 * 各页就不必每处都包一层 xx-toolbar（已包的也不受影响）。 */
.section-card :deep(.n-card-header) {
  gap: 12px;
}
.section-card :deep(.n-card-header__extra) {
  min-width: 0;
  gap: 8px;
  flex-wrap: wrap;
  justify-content: flex-end;
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
