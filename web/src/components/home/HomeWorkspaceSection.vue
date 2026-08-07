<script setup lang="ts">
defineProps<{
  title: string
  summary: string
  expanded: boolean
  primary?: boolean
}>()

defineEmits<{
  (event: 'toggle'): void
}>()
</script>

<template>
  <section class="workspace-section" :class="{ 'is-primary': primary, 'is-expanded': expanded }">
    <header class="workspace-section-head">
      <button
        type="button"
        class="workspace-section-title"
        :aria-expanded="expanded"
        :aria-label="`${expanded ? '收起' : '展开'}${title}`"
        @click="$emit('toggle')"
      >
        <span class="workspace-section-mark" aria-hidden="true" />
        <span class="workspace-section-copy">
          <strong>{{ title }}</strong>
          <small>{{ summary }}</small>
        </span>
      </button>
      <div class="workspace-section-actions">
        <slot name="actions" />
        <button
          type="button"
          class="workspace-section-toggle"
          :aria-label="`${expanded ? '收起' : '展开'}${title}`"
          @click="$emit('toggle')"
        >
          <span aria-hidden="true">{{ expanded ? '−' : '+' }}</span>
        </button>
      </div>
    </header>
    <div v-show="expanded" class="workspace-section-body">
      <slot />
    </div>
  </section>
</template>

<style scoped>
.workspace-section {
  min-width: 0;
  border: 1px solid var(--home-border);
  border-radius: 8px;
  background: var(--home-panel-bg);
  overflow: hidden;
}

.workspace-section.is-primary {
  border-color: var(--home-primary-border);
}

.workspace-section-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  min-height: 66px;
  padding: 8px 10px;
}

.workspace-section-title {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
  flex: 1;
  padding: 4px;
  border: 0;
  color: inherit;
  background: transparent;
  text-align: left;
  cursor: pointer;
}

.workspace-section-title:focus-visible,
.workspace-section-toggle:focus-visible {
  outline: 2px solid var(--home-primary);
  outline-offset: 2px;
}

.workspace-section-mark {
  width: 4px;
  height: 30px;
  flex: 0 0 4px;
  border-radius: 4px;
  background: var(--home-muted-mark);
}

.is-primary .workspace-section-mark {
  background: var(--home-primary);
}

.workspace-section-copy {
  display: flex;
  flex-direction: column;
  gap: 3px;
  min-width: 0;
}

.workspace-section-copy strong {
  font-size: 15px;
  line-height: 1.25;
}

.workspace-section-copy small {
  overflow: hidden;
  color: var(--home-muted);
  font-size: 12px;
  line-height: 1.35;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.workspace-section-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex: 0 0 auto;
}

.workspace-section-toggle {
  display: grid;
  place-items: center;
  width: 30px;
  height: 30px;
  padding: 0;
  border: 0;
  border-radius: 6px;
  color: var(--home-muted);
  background: var(--home-soft-bg);
  font-size: 20px;
  line-height: 1;
  cursor: pointer;
}

.workspace-section-body {
  min-height: 128px;
  padding: 0 14px 14px;
  border-top: 1px solid var(--home-divider);
}

@media (max-width: 768px) {
  .workspace-section-head {
    min-height: 60px;
  }

  .workspace-section-copy small {
    white-space: normal;
  }

  .workspace-section-body {
    min-height: 112px;
    padding: 0 12px 12px;
  }
}
</style>
