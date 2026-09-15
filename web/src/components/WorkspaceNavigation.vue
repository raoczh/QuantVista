<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { RouterLink } from 'vue-router'
import { workspaceGroups } from '@/navigation/workspace'
import WorkspaceIcon from './WorkspaceIcon.vue'

const props = defineProps<{ activeKey: string; admin: boolean; todoBadge: string | null; todoIncomplete: boolean; idPrefix: string }>()
const emit = defineEmits<{ navigate: [] }>()
const groups = computed(() => workspaceGroups.filter(group => !group.admin || props.admin))
const expanded = ref(new Set(['daily', 'research', 'portfolio']))
watch(() => props.activeKey, key => {
  const group = groups.value.find(value => value.items.some(item => item.key === key))
  if (group) expanded.value.add(group.key)
}, { immediate: true })
function toggle(key: string) {
  if (expanded.value.has(key)) expanded.value.delete(key)
  else expanded.value.add(key)
}
</script>

<template>
  <nav class="workspace-nav" aria-label="功能导航">
    <section v-for="group in groups" :key="group.key" class="nav-group">
      <h2 class="nav-group-title">
        <button type="button" :aria-expanded="expanded.has(group.key)" :aria-controls="`${idPrefix}-${group.key}`" @click="toggle(group.key)">
          <span>{{ group.label }}</span>
          <svg :class="{ expanded: expanded.has(group.key) }" viewBox="0 0 16 16" aria-hidden="true"><path d="m6 4 4 4-4 4" /></svg>
        </button>
      </h2>
      <div v-show="expanded.has(group.key)" :id="`${idPrefix}-${group.key}`" class="nav-group-links">
        <RouterLink v-for="item in group.items" :key="item.key" :to="item.path" class="workspace-nav-link" :class="{ 'is-active': activeKey === item.key }" :aria-current="activeKey === item.key ? 'page' : undefined" @click="emit('navigate')">
          <WorkspaceIcon :name="item.icon" class="nav-icon" />
          <span>{{ item.label }}</span>
          <span v-if="item.key === 'today' && todoBadge" class="nav-badge qv-tnum" :class="{ 'is-incomplete': todoIncomplete }" :title="todoIncomplete ? '待办清单读取不完整' : '待处理事项'">{{ todoBadge }}</span>
        </RouterLink>
      </div>
    </section>
  </nav>
</template>

<style scoped>
.workspace-nav { display: grid; align-content: start; gap: 14px; padding: 4px 12px 16px; }
.nav-group { min-width: 0; }
.nav-group-title { margin: 0 0 5px; font-size: 11px; font-weight: 600; letter-spacing: .06em; }
.nav-group-title button { width: 100%; display: flex; align-items: center; justify-content: space-between; padding: 7px 10px; border: 0; background: none; color: var(--qv-text-muted); font: inherit; cursor: pointer; text-align: left; border-radius: 6px; }
.nav-group-title button:hover { color: var(--qv-text); background: var(--qv-hover); }
.nav-group-title svg { width: 13px; height: 13px; fill: none; stroke: currentColor; stroke-width: 1.5; transition: transform .16s ease; }
.nav-group-title svg.expanded { transform: rotate(90deg); }
.nav-group-links { display: grid; gap: 3px; }
.workspace-nav-link { position: relative; display: flex; align-items: center; gap: 11px; min-height: 37px; padding: 0 11px; box-sizing: border-box; border-radius: 8px; color: var(--qv-text-secondary); font-size: 13px; text-decoration: none; transition: background-color .15s ease, color .15s ease; }
.workspace-nav-link:hover { background: var(--qv-hover); color: var(--qv-text); }
.workspace-nav-link.is-active { background: var(--qv-menu-active); color: var(--qv-menu-active-text); font-weight: 600; }
.workspace-nav-link.is-active::before { content: ''; position: absolute; width: 3px; top: 10px; bottom: 10px; left: 0; border-radius: 2px; background: currentColor; }
.nav-icon { width: 18px; height: 18px; flex: 0 0 18px; }
.nav-badge { margin-left: auto; min-width: 18px; padding: 1px 5px; box-sizing: border-box; border-radius: 5px; background: var(--qv-badge-bg); color: #fff; font-size: 10px; line-height: 16px; text-align: center; }
.nav-badge.is-incomplete { background: var(--qv-badge-incomplete-bg); }
@media (max-width: 768px) { .workspace-nav-link { min-height: 42px; font-size: 14px; } .nav-group-title button { min-height: 36px; } }
</style>
