<script setup lang="ts">
import { NButton, NEmpty, NTable, NTag } from 'naive-ui'
import SectionCard from '@/components/SectionCard.vue'
import { taskStatusLabel } from '@/api/taskCenter'
import type { ScanResult, StrategyRun } from '@/api/screener'

defineProps<{ items: StrategyRun<ScanResult>[] }>()
const emit = defineEmits<{ open: [item: StrategyRun<ScanResult>] }>()
const shortHash = (value?: string) => value ? value.slice(0, 12) : '-'
</script>

<template>
  <SectionCard title="历史扫描" class="block">
    <p class="history-note">历史任务、策略版本和审计快照独立保存，不会覆盖上方当前扫描结果。</p>
    <n-empty v-if="!items.length" description="暂无持久扫描结果" />
    <div v-else class="qv-scroll-x">
      <n-table size="small" :single-line="false">
        <thead><tr><th>策略</th><th>版本</th><th>状态</th><th>数据截止时间</th><th>完成时间</th><th>操作</th></tr></thead>
        <tbody><tr v-for="item in items" :key="item.id">
          <td>{{ item.strategy_name }}</td>
          <td><span v-if="item.strategy_revision">v{{ item.strategy_revision }} · </span><code>{{ shortHash(item.strategy_hash) }}</code></td>
          <td><n-tag size="small" :type="item.status === 'success' ? 'success' : item.status === 'failed' ? 'error' : 'info'">{{ taskStatusLabel(item.status) }}</n-tag></td>
          <td>{{ item.as_of?.slice(0, 10) || '未知' }}</td>
          <td>{{ (item.finished_at || item.created_at).replace('T', ' ').slice(0, 16) }}</td>
          <td><n-button size="tiny" quaternary @click="emit('open', item)">{{ item.status === 'success' ? '查看快照' : '任务详情' }}</n-button></td>
        </tr></tbody>
      </n-table>
    </div>
  </SectionCard>
</template>

<style scoped>
.history-note { margin: 0 0 12px; opacity: .62; }
</style>
