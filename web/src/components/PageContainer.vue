<script setup lang="ts">
defineProps<{ title?: string; subtitle?: string }>()
</script>

<template>
  <div class="page">
    <header v-if="title || subtitle || $slots.actions" class="page-head">
      <div class="page-head-text">
        <h1 v-if="title" class="page-title">{{ title }}</h1>
        <p v-if="subtitle" class="page-sub">{{ subtitle }}</p>
      </div>
      <div v-if="$slots.actions" class="page-actions">
        <slot name="actions" />
      </div>
    </header>
    <slot />
  </div>
</template>

<style scoped>
.page {
  max-width: var(--qv-content-max);
  margin: 0 auto;
  width: 100%;
}
.page-head {
  display: flex;
  align-items: flex-end;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 20px;
  flex-wrap: wrap;
}
.page-title {
  font-size: 22px;
  font-weight: 700;
  margin: 0;
  letter-spacing: -0.01em;
}
.page-sub {
  margin: 4px 0 0;
  font-size: 13px;
  opacity: 0.6;
}
.page-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
</style>
