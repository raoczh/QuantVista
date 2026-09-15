<script setup lang="ts">
import { ref, useId, watch } from 'vue'
import { NButton } from 'naive-ui'

const props = withDefaults(defineProps<{
  view: 'current' | 'history'
  hasResult: boolean
  busy: boolean
  statusVisible?: boolean
  statusCollapsible?: boolean
}>(), { statusVisible: false, statusCollapsible: false })
const emit = defineEmits<{ 'update:view': [value: 'current' | 'history'] }>()
const workspaceID = useId()
const views = [{ key: 'current', label: '本次结果' }, { key: 'history', label: '历史与复盘' }] as const
const controlsOpen = ref(!props.hasResult)
watch(() => [props.hasResult, props.busy], ([hasResult, busy]) => {
  if (hasResult && !busy) controlsOpen.value = false
  else if (!hasResult) controlsOpen.value = true
})
function toggleControls() {
  if (props.view !== 'current') {
    emit('update:view', 'current')
    controlsOpen.value = true
  } else controlsOpen.value = !controlsOpen.value
}
function onTabKeydown(event: KeyboardEvent) {
  if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(event.key)) return
  event.preventDefault()
  const next = event.key === 'Home' ? 'current' : event.key === 'End' ? 'history' : props.view === 'current' ? 'history' : 'current'
  emit('update:view', next)
  document.getElementById(`${workspaceID}-${next}-tab`)?.focus()
}
</script>

<template>
  <div class="research-workspace">
    <div class="workspace-bar">
      <div class="workspace-tabs" role="tablist" aria-label="研究视图">
        <button v-for="item in views" :id="`${workspaceID}-${item.key}-tab`" :key="item.key" type="button" role="tab" :aria-selected="view === item.key" :aria-controls="`${workspaceID}-${item.key}-panel`" :tabindex="view === item.key ? 0 : -1" @click="emit('update:view', item.key)" @keydown="onTabKeydown">{{ item.label }}</button>
      </div>
      <n-button secondary size="small" :aria-expanded="view === 'current' && controlsOpen" :aria-controls="`${workspaceID}-controls`" @click="toggleControls">
        {{ view === 'current' && controlsOpen ? '收起条件' : '研究条件' }}
      </n-button>
    </div>
    <div class="research-layout" :class="{ 'with-controls': view === 'current' && controlsOpen }">
      <div class="research-main">
        <div v-show="view === 'current'" :id="`${workspaceID}-current-panel`" role="tabpanel" :aria-labelledby="`${workspaceID}-current-tab`" class="research-current">
          <details v-if="statusVisible && statusCollapsible" class="research-task-details">
            <summary>任务已完成 · 查看运行记录</summary>
            <slot name="status" />
          </details>
          <div v-else-if="statusVisible"><slot name="status" /></div>
          <slot name="result" />
        </div>
        <div v-show="view === 'history'" :id="`${workspaceID}-history-panel`" role="tabpanel" :aria-labelledby="`${workspaceID}-history-tab`"><slot name="history" /></div>
      </div>
      <aside v-show="view === 'current' && controlsOpen" :id="`${workspaceID}-controls`" class="research-controls" aria-label="研究条件">
        <slot name="controls" />
      </aside>
    </div>
  </div>
</template>

<style scoped>
.research-workspace { container-type: inline-size; min-width: 0; }
.workspace-bar { display: flex; align-items: center; justify-content: space-between; gap: 16px; margin-bottom: 20px; border-bottom: 1px solid var(--qv-border); }
.workspace-tabs { display: flex; gap: 24px; min-width: 0; }
.workspace-tabs button { padding: 12px 0; border: 0; border-bottom: 2px solid transparent; background: none; color: var(--qv-text-secondary); font: inherit; font-size: 13px; white-space: nowrap; cursor: pointer; }
.workspace-tabs button[aria-selected="true"] { color: var(--qv-primary); border-bottom-color: var(--qv-primary); font-weight: 600; }
.research-layout, .research-current { display: grid; min-width: 0; gap: 20px; align-items: start; }
.research-layout { grid-template-columns: minmax(0, 1fr); }
.research-main, .research-controls { min-width: 0; }
.research-task-details > summary { padding: 2px 0; color: var(--qv-text-secondary); font-size: 12px; cursor: pointer; }
.research-task-details[open] > summary { margin-bottom: 12px; }
.research-controls { grid-row: 1; }
@container (min-width: 980px) {
  .research-layout.with-controls { grid-template-columns: minmax(0, 1fr) 340px; }
  .research-controls { grid-column: 2; grid-row: 1; }
}
@media (max-width: 768px) { .research-layout, .research-current { gap: 16px; } }
</style>
