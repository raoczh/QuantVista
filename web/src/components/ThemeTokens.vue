<script setup lang="ts">
import { watchEffect } from 'vue'
import { useUi, withAlpha } from '@/composables/useUi'

const { vars, isDark } = useUi()
watchEffect(() => {
  const root = document.documentElement
  const tokens: Record<string, string> = {
    primary: vars.value.primaryColor,
    'primary-selection': withAlpha(vars.value.primaryColor, .18),
    border: vars.value.dividerColor,
    hover: isDark.value ? 'rgba(255, 255, 255, .055)' : 'rgba(128, 128, 128, .07)',
    text: vars.value.textColorBase,
    'text-secondary': vars.value.textColor2,
    'text-muted': vars.value.textColor3,
    surface: vars.value.cardColor,
    background: vars.value.bodyColor,
  }
  for (const [name, value] of Object.entries(tokens)) root.style.setProperty(`--qv-${name}`, value)
  root.style.colorScheme = isDark.value ? 'dark' : 'light'
})
</script>

<template><slot /></template>
