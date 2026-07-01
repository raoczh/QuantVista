<script setup lang="ts">
import { useUi } from '@/composables/useUi'

defineProps<{ items: any[] }>()

const { vars, primaryAlpha } = useUi()

function badgeStyle(i: number) {
  if (i === 0) return { background: vars.value.primaryColor, color: '#fff' }
  if (i <= 2) return { background: primaryAlpha(0.16), color: vars.value.primaryColor }
  return { background: 'transparent', color: vars.value.textColor3 }
}
</script>

<template>
  <div class="rank-list">
    <div
      v-for="(item, i) in items"
      :key="i"
      class="rank-row"
      :style="{ '--row-hover': primaryAlpha(0.07) }"
    >
      <span class="rank-badge qv-tnum" :style="badgeStyle(i)">{{ i + 1 }}</span>
      <div class="rank-body">
        <slot name="row" :item="item" :index="i" />
      </div>
    </div>
  </div>
</template>

<style scoped>
.rank-list {
  display: flex;
  flex-direction: column;
}
.rank-row {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 8px 6px;
  border-radius: 8px;
  transition: background 0.15s ease;
}
.rank-row:hover {
  background: var(--row-hover);
}
.rank-badge {
  flex-shrink: 0;
  width: 20px;
  height: 20px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  font-size: 12px;
  font-weight: 700;
  border-radius: 6px;
}
.rank-body {
  flex: 1;
  min-width: 0;
}
</style>
