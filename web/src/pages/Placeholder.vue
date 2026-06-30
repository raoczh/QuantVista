<script setup lang="ts">
import { useRoute } from 'vue-router'
import { NResult, NCard } from 'naive-ui'
import { computed } from 'vue'

const route = useRoute()
const title = computed(() => (route.meta.title as string) || '页面')
</script>

<template>
  <n-card size="small">
    <n-result
      status="info"
      :title="title"
      description="该模块尚未实现，将随实施路线图阶段推进逐步上线（见 docs/ROADMAP.md）。"
    />
  </n-card>
</template>
