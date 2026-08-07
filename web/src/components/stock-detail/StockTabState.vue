<script setup lang="ts">
import { NAlert, NButton, NEmpty, NResult, NSkeleton, NSpin } from 'naive-ui'
import type { StockSectionPhase } from './decisionSummary'

defineProps<{
  phase: StockSectionPhase
  error?: string
  hasData: boolean
}>()
const emit = defineEmits<{ (event: 'retry'): void }>()
</script>

<template>
  <div class="tab-state" :aria-busy="phase === 'loading' || phase === 'refreshing'">
    <n-alert v-if="error && hasData" type="warning" :bordered="false" title="部分数据读取失败">
      {{ error }}
      <n-button text type="primary" class="inline-retry" @click="emit('retry')">重试</n-button>
    </n-alert>

    <n-result v-if="phase === 'error' && !hasData" status="warning" title="本分区读取失败" :description="error">
      <template #footer><n-button @click="emit('retry')">重试本分区</n-button></template>
    </n-result>
    <div v-else-if="phase === 'idle'" class="tab-idle">
      <n-empty description="本分区尚未请求" />
      <n-button secondary @click="emit('retry')">加载本分区</n-button>
    </div>
    <div v-else-if="phase === 'loading' && !hasData" class="tab-skeleton" aria-label="分区加载中">
      <n-skeleton height="180px" />
      <n-skeleton text :repeat="3" />
    </div>
    <n-empty v-else-if="phase === 'empty' && !hasData" description="请求成功，当前没有该分区数据">
      <template #extra><n-button secondary @click="emit('retry')">重新读取</n-button></template>
    </n-empty>
    <n-spin v-else :show="phase === 'refreshing' || phase === 'loading'">
      <slot />
    </n-spin>
  </div>
</template>

<style scoped>
.tab-state {
  min-width: 0;
}
.tab-state > .n-alert {
  margin-bottom: 12px;
}
.inline-retry {
  margin-left: 8px;
}
.tab-idle {
  display: flex;
  min-height: 240px;
  align-items: center;
  justify-content: center;
  flex-direction: column;
  gap: 14px;
}
.tab-skeleton {
  display: grid;
  min-height: 280px;
  gap: 14px;
}
</style>
