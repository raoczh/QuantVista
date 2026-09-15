<script setup lang="ts">
import { computed } from 'vue'

const props = defineProps<{ name: string }>()
const paths: Record<string, string[]> = {
  home: ['m3 10 9-7 9 7', 'M5 9v12h14V9', 'M9 21v-7h6v7'],
  inbox: ['M4 4h16l2 10v6H2v-6L4 4Z', 'M2 14h6l2 3h4l2-3h6'],
  star: ['m12 3 2.8 5.7 6.2.9-4.5 4.4 1.1 6.2-5.6-3-5.6 3 1.1-6.2L3 9.6l6.2-.9L12 3Z'],
  activity: ['M2 12h4l3-8 6 16 3-8h4'],
  filter: ['M4 5h16M7 12h10M10 19h4'],
  compass: ['M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z', 'm16 8-3 5-5 3 3-5 5-3Z'],
  sparkles: ['m12 3 2.5 6.5L21 12l-6.5 2.5L12 21l-2.5-6.5L3 12l6.5-2.5L12 3Z', 'M20 2v4M18 4h4'],
  message: ['M21 11a8 8 0 0 1-8 8H6l-4 3V11a9 9 0 0 1 19 0Z', 'M7 10h9M7 14h5'],
  columns: ['M4 4h16v16H4z', 'M12 4v16'],
  news: ['M4 3h16v18H4z', 'M8 7h8M8 11h3M8 15h8M8 18h8'],
  grid: ['M3 3h7v7H3zM14 3h7v7h-7zM3 14h7v7H3zM14 14h7v7h-7z'],
  chart: ['M4 3v17h17', 'm7 14 4-5 4 3 5-7'],
  briefcase: ['M3 7h18v14H3z', 'M8 7V3h8v4M3 12h18M10 12v3h4v-3'],
  shield: ['m12 2 8 4v6c0 5-8 10-8 10S4 17 4 12V6l8-4Z', 'm8 12 3 3 5-6'],
  bell: ['M18 8a6 6 0 0 0-12 0c0 7-3 7-3 9h18c0-2-3-2-3-9', 'M10 21h4'],
  report: ['M5 3h10l4 4v14H5z', 'M14 3v5h5M8 12h8M8 16h6'],
  layers: ['m12 3 10 5-10 5L2 8l10-5Z', 'm2 12 10 5 10-5M2 16l10 5 10-5'],
  note: ['M16 3H4v18h16V9', 'm13 11 7-7-2-2-7 7-1 4 3-2Z', 'M8 17h7'],
  flask: ['M9 3h6M10 3v7l-6 9a2 2 0 0 0 2 3h12a2 2 0 0 0 2-3l-6-9V3', 'M7 16h10'],
  history: ['M3 11a9 9 0 1 1 2 7', 'M3 4v7h7', 'M12 7v5l3 2'],
  sliders: ['M4 6h16M4 12h16M4 18h16', 'M8 3v6M16 9v6M10 15v6'],
  settings: ['M12 8a4 4 0 1 1 0 8 4 4 0 0 1 0-8Z', 'M9 3h6l1 3 3 1 2 5-2 5-3 1-1 3H9l-1-3-3-1-2-5 2-5 3-1 1-3Z'],
}
const strokes = computed(() => paths[props.name] || paths.grid)
</script>

<template>
  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.65" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">
    <path v-for="(path, index) in strokes" :key="index" :d="path" />
  </svg>
</template>
