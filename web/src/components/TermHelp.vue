<script setup lang="ts">
import { computed } from 'vue'
import { NButton, NTooltip } from 'naive-ui'
import { useDisplayMode } from '@/composables/useDisplayMode'
import { TERM_DICTIONARY, type TermKey } from './termDictionary'

const props = defineProps<{
  term: TermKey
  label?: string
}>()

const { isPlain } = useDisplayMode()
const definition = computed(() => TERM_DICTIONARY[props.term])
const displayLabel = computed(() => props.label || (isPlain.value ? definition.value.plain : definition.value.professional))
</script>

<template>
  <span class="term-help">
    <span>{{ displayLabel }}</span>
    <n-tooltip trigger="hover" placement="top">
      <template #trigger>
        <n-button quaternary circle size="tiny" class="term-trigger" :aria-label="`解释：${displayLabel}`">?</n-button>
      </template>
      <span class="term-tip">{{ definition.help }}</span>
    </n-tooltip>
  </span>
</template>

<style scoped>
.term-help {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  min-width: 0;
}
.term-trigger {
  width: 18px;
  height: 18px;
  min-width: 18px;
  font-size: 11px;
  opacity: 0.62;
}
.term-trigger:focus-visible {
  opacity: 1;
}
.term-tip {
  display: inline-block;
  max-width: 280px;
  line-height: 1.55;
}
</style>
