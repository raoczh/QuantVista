<script setup lang="ts">
import { NRadioButton, NRadioGroup } from 'naive-ui'
import { useDisplayMode, type DisplayMode } from '@/composables/useDisplayMode'

const { displayMode, setMode } = useDisplayMode()
</script>

<template>
  <n-radio-group :value="displayMode" size="small" aria-label="显示模式" @update:value="setMode($event as DisplayMode)">
    <n-radio-button value="plain">简明</n-radio-button>
    <n-radio-button value="professional">专业</n-radio-button>
  </n-radio-group>
</template>
