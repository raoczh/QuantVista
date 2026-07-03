<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NButton,
  NEmpty,
  NPopconfirm,
  NSelect,
  NSpin,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  listDailyReports,
  getDailyReport,
  generateDailyReport,
  type DailyReportRow,
  type DailyReportView,
} from '@/api/report'
import { useUi } from '@/composables/useUi'
import { useStockActions } from '@/composables/useStockActions'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const router = useRouter()
const { pctColor } = useUi()
const { goDetail } = useStockActions()

const rows = ref<DailyReportRow[]>([])
const current = ref<DailyReportView | null>(null)
const loading = ref(false)
const generating = ref(false)
const selectedId = ref<number | null>(null)

const historyOptions = computed(() =>
  rows.value.map((r) => ({
    label: `${r.trade_date}（${statusText(r.status)}）`,
    value: r.id,
  })),
)

function statusText(s: string) {
  return s === 'success' ? '完整' : s === 'partial' ? '部分成功' : '失败'
}
function statusType(s: string): 'success' | 'warning' | 'error' {
  return s === 'success' ? 'success' : s === 'partial' ? 'warning' : 'error'
}

async function load() {
  loading.value = true
  try {
    rows.value = await listDailyReports(30)
    if (rows.value.length) {
      selectedId.value = rows.value[0].id
      current.value = await getDailyReport(rows.value[0].id)
    } else {
      current.value = null
    }
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function pick(id: number | null) {
  if (!id) return
  loading.value = true
  try {
    current.value = await getDailyReport(id)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

async function doGenerate() {
  generating.value = true
  try {
    current.value = await generateDailyReport()
    message.success('日报已生成')
    rows.value = await listDailyReports(30)
    selectedId.value = current.value.id
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    generating.value = false
  }
}

const recItems = computed(() => current.value?.recommendation?.items ?? [])

onMounted(load)
</script>

<template>
  <PageContainer title="收盘日报" subtitle="交易日 15:35 后自动生成：今日复盘 + 明日选股推荐（可在设置-偏好开启）">
    <template #actions>
      <div class="toolbar">
        <n-select
          v-model:value="selectedId"
          :options="historyOptions"
          placeholder="历史日报"
          size="small"
          style="width: 220px"
          @update:value="pick"
        />
        <n-popconfirm @positive-click="doGenerate">
          <template #trigger>
            <n-button size="small" type="primary" ghost :loading="generating">生成 / 重生成今日</n-button>
          </template>
          将调用你的 LLM 生成当日复盘与明日推荐（计 1 次配额），已有今日日报会被覆盖，继续？
        </n-popconfirm>
      </div>
    </template>

    <n-spin :show="loading">
      <n-empty
        v-if="!current"
        description="还没有日报。交易日收盘后自动生成（需在 设置→偏好 开启「收盘日报」），或点右上角立即生成。"
        style="padding: 48px 0"
      />
      <div v-else class="report">
        <SectionCard :hoverable="false">
          <div class="head">
            <span class="head-date qv-figure">{{ current.trade_date }}</span>
            <n-tag :type="statusType(current.status)" round :bordered="false">{{ statusText(current.status) }}</n-tag>
            <span class="meta">耗时 {{ (current.latency_ms / 1000).toFixed(1) }}s · {{ current.total_tokens }} tokens</span>
          </div>
          <div v-if="current.error" class="err">{{ current.error }}</div>
        </SectionCard>

        <!-- 今日复盘 -->
        <SectionCard v-if="current.review" title="今日复盘">
          <div class="review">
            <p class="summary">{{ current.review.summary }}</p>
            <div class="block"><span class="bk">大盘</span><p>{{ current.review.market_review }}</p></div>
            <div class="block"><span class="bk">持仓</span><p>{{ current.review.position_review }}</p></div>
            <div class="block"><span class="bk">自选</span><p>{{ current.review.watch_review }}</p></div>
            <div v-if="current.review.risk_warnings?.length" class="block warn">
              <span class="bk">风险</span>
              <ul>
                <li v-for="(w, i) in current.review.risk_warnings" :key="i">{{ w }}</li>
              </ul>
            </div>
            <div class="block plan"><span class="bk">明日计划</span><p>{{ current.review.tomorrow_plan }}</p></div>
          </div>
        </SectionCard>
        <SectionCard v-else title="今日复盘">
          <n-empty description="复盘生成失败（见上方错误），可点「生成 / 重生成今日」重试" />
        </SectionCard>

        <!-- 明日推荐 -->
        <SectionCard title="明日选股推荐（短线）">
          <template #extra>
            <n-button v-if="current.recommendation" size="tiny" quaternary type="primary" @click="router.push('/recommendations')"
              >完整详情与追踪 →</n-button
            >
          </template>
          <div v-if="recItems.length" class="recs">
            <div v-for="it in recItems" :key="it.id" class="rec">
              <div class="rec-head">
                <span class="rec-name" @click="goDetail({ symbol: it.symbol, market: it.market, name: it.name })"
                  >{{ it.name }} <span class="qv-mono rec-sym">{{ it.symbol }}</span></span
                >
                <n-tag size="small" round :bordered="false" :type="it.action === 'buy' ? 'error' : 'default'">
                  {{ it.action === 'buy' ? '买入关注' : '观察' }}
                </n-tag>
                <span class="rec-conf">置信 {{ it.confidence }}%</span>
              </div>
              <div v-if="it.detail" class="rec-prices">
                <span
                  >买点 <b class="qv-tnum">{{ it.detail.buy_zone_low }} ~ {{ it.detail.buy_zone_high }}</b></span
                >
                <span
                  >止盈 <b class="qv-tnum" :style="{ color: pctColor(1) }">{{ it.detail.take_profit }}</b></span
                >
                <span
                  >止损 <b class="qv-tnum" :style="{ color: pctColor(-1) }">{{ it.detail.stop_loss }}</b></span
                >
                <span>有效期 {{ it.detail.valid_days }} 交易日</span>
              </div>
              <p class="rec-summary">{{ it.summary }}</p>
            </div>
            <p class="sell-hint">
              已按止盈/止损价自动创建到价卖点提醒（见「条件提醒」，note 标注「收盘日报」；命中即进今日待办并推送）。
            </p>
          </div>
          <n-empty v-else description="推荐未生成（候选池为空或 LLM 失败），可重生成重试" />
        </SectionCard>

        <p class="disclaimer">本内容为 AI 生成的研究参考，不构成投资建议；数据可能延迟或不完整，决策风险自担。</p>
      </div>
    </n-spin>
  </PageContainer>
</template>

<style scoped>
.toolbar {
  display: flex;
  gap: 10px;
  align-items: center;
}
.report {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.head {
  display: flex;
  align-items: center;
  gap: 12px;
}
.head-date {
  font-size: 22px;
  font-weight: 700;
}
.meta {
  font-size: 12px;
  opacity: 0.55;
}
.err {
  margin-top: 8px;
  font-size: 12px;
  opacity: 0.7;
}
.review .summary {
  font-size: 15px;
  font-weight: 600;
  line-height: 1.7;
  margin: 0 0 12px;
}
.block {
  display: flex;
  gap: 10px;
  margin-bottom: 10px;
}
.block p,
.block ul {
  margin: 0;
  line-height: 1.7;
  flex: 1;
}
.block ul {
  padding-left: 18px;
}
.bk {
  flex-shrink: 0;
  width: 62px;
  font-size: 12px;
  font-weight: 600;
  opacity: 0.6;
  padding-top: 3px;
}
.recs {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.rec {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.rec-head {
  display: flex;
  align-items: center;
  gap: 10px;
}
.rec-name {
  font-weight: 600;
  cursor: pointer;
}
.rec-sym {
  font-size: 12px;
  opacity: 0.55;
}
.rec-conf {
  font-size: 12px;
  opacity: 0.55;
}
.rec-prices {
  display: flex;
  gap: 18px;
  flex-wrap: wrap;
  font-size: 13px;
}
.rec-summary {
  margin: 0;
  font-size: 13px;
  line-height: 1.6;
  opacity: 0.85;
}
.sell-hint {
  font-size: 12px;
  opacity: 0.55;
  margin: 4px 0 0;
}
.disclaimer {
  font-size: 12px;
  opacity: 0.5;
  margin: 0;
}
</style>
