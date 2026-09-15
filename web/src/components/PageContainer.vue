<script setup lang="ts">
withDefaults(defineProps<{ title?: string; subtitle?: string; headingLevel?: 1 | 2 }>(), { headingLevel: 1 })
</script>

<template>
  <div class="page" :class="{ 'is-section': headingLevel === 2 }">
    <header v-if="title || subtitle || $slots.title || $slots.actions" class="page-head qv-anim-in">
      <div class="page-head-text">
        <component :is="headingLevel === 2 ? 'h2' : 'h1'" v-if="title || $slots.title" class="page-title"><slot name="title">{{ title }}</slot></component>
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
  min-width: 0;
}
.page-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 26px;
  flex-wrap: wrap;
}
.page-head-text {
  min-width: 0;
  flex: 1 1 280px;
}
.page-title {
  font-size: 27px;
  font-weight: 650;
  margin: 0;
  letter-spacing: -0.025em;
  line-height: 1.35;
  overflow-wrap: anywhere;
}
.page-sub {
  margin: 7px 0 0;
  font-size: 13px;
  line-height: 1.7;
  color: var(--qv-text-muted);
  max-width: 80ch;
}
.is-section .page-title { font-size: 20px; }
.page-actions {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  max-width: 100%;
}

@media (max-width: 768px) {
  .page-head {
    margin-bottom: 16px;
  }
  .page-title {
    font-size: 23px;
  }
  .page-sub {
    font-size: 12px;
  }
  .page-actions {
    flex-wrap: wrap;
    row-gap: 10px;
  }
}
</style>
