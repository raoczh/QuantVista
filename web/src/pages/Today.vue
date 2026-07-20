<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRouter } from 'vue-router'
import { NButton, NSpin, NEmpty, NTag, NGrid, NGi, NAlert, useMessage } from 'naive-ui'
import { getTodos, type TodoItem, type TodoResult } from '@/api/todo'
import { setAlertEventStatus } from '@/api/alert'
import { ackRecommendationReview } from '@/api/recommendation'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'
import StatCard from '@/components/StatCard.vue'

const message = useMessage()
const router = useRouter()
const { upColor, downColor, flatColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const data = ref<TodoResult | null>(null)
const loading = ref(false)
async function load() {
  loading.value = true
  try {
    data.value = await getTodos()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// 类型 → 展示元信息（标签 + 强调色）。
function kindMeta(kind: string) {
  switch (kind) {
    case 'alert':
      return { label: '提醒', color: upColor.value }
    case 'stop_loss':
      return { label: '止损警示', color: vars.value.errorColor }
    case 'rec_review':
      return { label: '推荐复盘', color: downColor.value }
    case 'position_short':
      return { label: '短线持仓', color: vars.value.warningColor }
    case 'position_long':
      return { label: '长线持仓', color: flatColor.value }
    case 'thesis_due':
      return { label: '逻辑卡复盘', color: vars.value.infoColor }
    default:
      return { label: '待办', color: flatColor.value }
  }
}
function fmtTime(t: string | null) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

// 一键跳转到对应页面处理。
function handle(item: TodoItem) {
  if (item.ref_type === 'alerts') {
    router.push({ name: 'alerts' })
  } else if (item.ref_type === 'recommendations') {
    router.push({ name: 'recommendations' })
  } else if (item.ref_type === 'positions') {
    router.push({ name: 'positions' })
  } else if (item.ref_type === 'thesis') {
    router.push({ name: 'thesis' })
  }
}

// 提醒命中待办可就地完成：已读/忽略后从清单消失（ref_id 即 alert_event id）。
const marking = ref<number | null>(null)
async function markAlert(item: TodoItem, status: 'read' | 'dismissed') {
  if (marking.value) return
  marking.value = item.ref_id
  try {
    await setAlertEventStatus(item.ref_id, status)
    message.success(status === 'read' ? '已标记已读' : '已忽略')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    marking.value = null
  }
}

// 推荐复盘待办可就地已读消项（ref_id 即追踪状态行 id；后台追踪刷新不会打回未读）。
async function markRecReview(item: TodoItem) {
  if (marking.value) return
  marking.value = item.ref_id
  try {
    await ackRecommendationReview(item.ref_id)
    message.success('已标记已读')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    marking.value = null
  }
}

onMounted(load)
</script>

<template>
  <PageContainer title="今日待办" subtitle="聚合命中提醒 · 推荐复盘 · 持仓复盘 —— 今天该看的一览">
    <template #actions>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="todo" :style="styleVars">
      <n-grid cols="2 s:3" :x-gap="14" :y-gap="14" responsive="screen">
        <n-gi>
          <StatCard label="待办合计" :value="String(data?.total ?? 0)" />
        </n-gi>
        <n-gi>
          <StatCard label="命中提醒" :value="String(data?.alerts ?? 0)" />
        </n-gi>
        <n-gi>
          <StatCard label="待复盘" :value="String(data?.reviews ?? 0)" />
        </n-gi>
      </n-grid>

      <SectionCard :title="`清单${data?.date ? ' · ' + data.date : ''}`">
        <n-spin :show="loading && !data">
          <n-alert
            v-if="data && data.complete === false"
            type="warning"
            :bordered="false"
            style="margin-bottom: 12px"
            title="待办清单可能不完整"
          >
            <div v-for="(e, i) in data.errors || []" :key="i">{{ e }}</div>
            <div v-if="!data.errors?.length">部分数据读取失败，状态不明的事项未列出，请稍后刷新重试。</div>
          </n-alert>
          <n-empty
            v-if="data && !data.items.length"
            :description="
              data.complete === false
                ? '暂未取到待办事项，但部分数据读取失败，状态不明——不代表一切正常'
                : '今天没有需要处理的事项，一切都在轨道上 👍'
            "
            style="padding: 40px 0"
          />
          <div v-else class="items">
            <div v-for="(it, i) in data?.items || []" :key="i" class="item">
              <div class="item-bar" :style="{ background: kindMeta(it.kind).color }" />
              <div class="item-main">
                <div class="item-head">
                  <n-tag
                    size="tiny"
                    round
                    :bordered="false"
                    :color="{ color: withAlpha(kindMeta(it.kind).color, 0.14), textColor: kindMeta(it.kind).color }"
                    >{{ kindMeta(it.kind).label }}</n-tag
                  >
                  <span class="item-title">{{ it.title }}</span>
                  <span class="item-stock">{{ it.name || it.symbol }}<span class="item-symbol qv-mono"> {{ it.symbol }}</span></span>
                </div>
                <div class="item-detail">{{ it.detail }}</div>
                <div v-if="it.time" class="item-time">{{ fmtTime(it.time) }}</div>
              </div>
              <div class="item-actions">
                <template v-if="it.kind === 'alert'">
                  <n-button size="small" quaternary :loading="marking === it.ref_id" @click="markAlert(it, 'read')"
                    >已读</n-button
                  >
                  <n-button size="small" quaternary @click="markAlert(it, 'dismissed')">忽略</n-button>
                </template>
                <template v-else-if="it.kind === 'rec_review'">
                  <n-button size="small" quaternary :loading="marking === it.ref_id" @click="markRecReview(it)"
                    >已读</n-button
                  >
                </template>
                <n-button size="small" tertiary @click="handle(it)">去处理</n-button>
              </div>
            </div>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.todo {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.items {
  display: flex;
  flex-direction: column;
}
.item {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 12px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
@media (max-width: 768px) {
  .item {
    flex-wrap: wrap;
    row-gap: 4px;
  }
  .item-actions {
    flex-basis: 100%;
    justify-content: flex-end;
  }
}
.item:last-child {
  border-bottom: none;
}
.item-bar {
  width: 3px;
  align-self: stretch;
  border-radius: 3px;
  flex-shrink: 0;
}
.item-main {
  flex: 1;
  min-width: 0;
}
.item-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.item-title {
  font-size: 14px;
  font-weight: 600;
}
.item-stock {
  font-size: 13px;
  opacity: 0.75;
}
.item-symbol {
  opacity: 0.5;
  font-size: 12px;
}
.item-detail {
  font-size: 13px;
  opacity: 0.75;
  margin-top: 4px;
}
.item-time {
  font-size: 11px;
  opacity: 0.5;
  margin-top: 3px;
}
.item-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
</style>
