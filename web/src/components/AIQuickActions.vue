<script setup lang="ts">
import { ref } from 'vue'
import { NButton, NModal, NPopover } from 'naive-ui'
import { useRouter } from 'vue-router'
import { useStockActions, type StockRef } from '@/composables/useStockActions'
import StockPicker from './StockPicker.vue'

type StockAction = 'analysis' | 'qa'

withDefaults(
  defineProps<{
    variant?: 'toolbar' | 'panel'
  }>(),
  { variant: 'panel' },
)
const emit = defineEmits<{ (event: 'navigated'): void }>()

const router = useRouter()
const { goAnalysis, goQa } = useStockActions(() => emit('navigated'))
const pickerOpen = ref(false)
const pendingAction = ref<StockAction>('analysis')
const pickedStock = ref<StockRef | null>(null)

function openStockAction(action: StockAction) {
  pendingAction.value = action
  pickedStock.value = null
  pickerOpen.value = true
}

async function navigatePlain(name: 'recommendations' | 'positions') {
  if (name === 'positions') await router.push({ name, hash: '#position-advice-panel' })
  else await router.push({ name })
  emit('navigated')
}

async function selectStock(stock: StockRef) {
  pickerOpen.value = false
  if (pendingAction.value === 'analysis') await goAnalysis(stock)
  else await goQa(stock)
}
</script>

<template>
  <span class="ai-quick-actions" :class="`variant-${variant}`">
    <n-popover v-if="variant === 'toolbar'" trigger="click" placement="bottom-end">
      <template #trigger>
        <n-button size="small" secondary aria-label="打开 AI 快捷操作">AI</n-button>
      </template>
      <div class="ai-menu" aria-label="AI 快捷操作">
        <n-button text @click="navigatePlain('recommendations')">AI 选股</n-button>
        <n-button text @click="openStockAction('analysis')">分析一只股票</n-button>
        <n-button text @click="navigatePlain('positions')">AI 检查持仓</n-button>
        <n-button text @click="openStockAction('qa')">个股追问</n-button>
      </div>
    </n-popover>
    <div v-else class="ai-panel" aria-label="AI 快捷操作">
      <n-button secondary @click="navigatePlain('recommendations')">AI 选股</n-button>
      <n-button secondary @click="openStockAction('analysis')">分析一只股票</n-button>
      <n-button secondary @click="navigatePlain('positions')">AI 检查持仓</n-button>
      <n-button secondary @click="openStockAction('qa')">个股追问</n-button>
    </div>

    <n-modal v-model:show="pickerOpen" preset="card" :title="pendingAction === 'analysis' ? '选择要分析的股票' : '选择要追问的股票'" class="ai-picker-modal">
      <StockPicker v-model="pickedStock" :clearable="false" @select="selectStock" />
    </n-modal>
  </span>
</template>

<style scoped>
.ai-quick-actions {
  display: inline-flex;
  min-width: 0;
}
.ai-menu {
  display: grid;
  min-width: 156px;
  gap: 9px;
  padding: 4px 2px;
  justify-items: start;
}
.ai-panel {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 8px;
  width: 100%;
}
:global(.ai-picker-modal) {
  width: min(520px, calc(100vw - 24px));
}
@media (max-width: 680px) {
  .ai-panel {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
  .ai-panel :deep(.n-button) {
    min-width: 0;
    white-space: normal;
  }
}
</style>
