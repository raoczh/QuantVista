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
    :class="{ 'is-hoverable': hoverable, 'has-title': !!title }"
    :size="size"
    :bordered="true"
  >
    <header v-if="title || $slots.extra" class="sc-header">
      <h2 v-if="title" class="sc-title">{{ title }}</h2>
      <div v-if="$slots.extra" class="sc-extra">
        <slot name="extra" />
      </div>
    </header>
    <slot />
  </n-card>
</template>

<style scoped>
.section-card {
  border-radius: var(--qv-radius-card);
  min-width: 0;
  box-shadow: none;
  transition: border-color 0.18s ease;
}
.section-card.is-hoverable:hover {
  border-color: var(--qv-primary);
}
.sc-header {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 18px;
  min-width: 0;
}
.sc-title {
  min-width: 0;
  margin: 0;
  font-weight: 600;
  font-size: 15px;
  line-height: 1.5;
  overflow-wrap: anywhere;
}

/* 使用自己的语义标题，避免组件库 header 的 heading 与 h2 重复嵌套。 */
.section-card :deep(.n-card-content) {
  min-width: 0;
}
.sc-extra {
  display: flex;
  min-width: 0;
  margin-left: auto;
  gap: 8px;
  flex-wrap: wrap;
  justify-content: flex-end;
}

/* 移动端：卡片内容区可横向滚动，宽表格不撑破整页布局；
 * 表格单元格不折行（挤压成一列一字反而没法看），滚动查看。 */
@media (max-width: 768px) {
  .section-card.has-title .sc-header {
    flex-wrap: wrap;
  }
  .section-card.has-title .sc-title {
    flex: 1 1 100%;
    min-width: 0;
  }
  .section-card.has-title .sc-extra {
    width: 100%;
    margin-left: 0;
    justify-content: flex-start;
  }
  .section-card :deep(.n-card-content) {
    overflow-x: auto;
    -webkit-overflow-scrolling: touch;
  }
  .section-card :deep(.n-table th),
  .section-card :deep(.n-table td) {
    white-space: nowrap;
  }
}
</style>
