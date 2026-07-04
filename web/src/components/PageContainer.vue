<script setup lang="ts">
defineProps<{ title?: string; subtitle?: string }>()
</script>

<template>
  <div class="page">
    <header v-if="title || subtitle || $slots.actions" class="page-head qv-anim-in">
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
  margin-bottom: 22px;
  flex-wrap: wrap;
}
.page-title {
  font-size: 24px;
  font-weight: 700;
  margin: 0;
  letter-spacing: -0.02em;
  line-height: 1.25;
}
.page-sub {
  margin: 5px 0 0;
  font-size: 13px;
  opacity: 0.58;
}
.page-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}

@media (max-width: 768px) {
  .page-head {
    margin-bottom: 16px;
  }
  .page-title {
    font-size: 20px;
  }
  .page-sub {
    font-size: 12px;
  }
  .page-actions {
    flex-wrap: wrap;
  }
}
</style>
