<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NSelect,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NDatePicker,
  NAlert,
  useMessage,
} from 'naive-ui'
import {
  listThesisCards,
  upsertThesisCard,
  setThesisStatus,
  deleteThesisCard,
  checkupThesisCards,
  type ThesisCard,
  type ThesisCheckItem,
} from '@/api/thesis'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { vars, pctColor, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

// ---------- 列表 ----------
const cards = ref<ThesisCard[]>([])
const loading = ref(false)
const statusFilter = ref<'active' | 'invalidated' | 'archived' | ''>('active')
const statusOptions = [
  { label: '跟踪中', value: 'active' },
  { label: '已失效', value: 'invalidated' },
  { label: '已归档', value: 'archived' },
  { label: '全部', value: '' },
]

async function load() {
  loading.value = true
  try {
    cards.value = await listThesisCards(statusFilter.value)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}

// ---------- 体检 ----------
const checking = ref(false)
const checkResults = ref<ThesisCheckItem[] | null>(null)
const checkBySymbol = computed(() => {
  const m = new Map<string, ThesisCheckItem>()
  checkResults.value?.forEach((it) => m.set(it.card.symbol + ':' + it.card.market, it))
  return m
})
async function runCheckup() {
  checking.value = true
  try {
    checkResults.value = await checkupThesisCards()
    const warn = checkResults.value.filter((r) => r.signals.length).length
    message.success(warn ? `体检完成：${warn} 张卡需要注意` : '体检完成：暂无需要注意的信号')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    checking.value = false
  }
}

// ---------- 表单 ----------
const showForm = ref(false)
const saving = ref(false)
const form = ref({
  symbol: '',
  market: 'cn',
  thesis: '',
  key_evidence: '',
  risks: '',
  kill_switches: '',
  track_metrics: '',
  next_review_ts: null as number | null,
})
function resetForm() {
  form.value = { symbol: '', market: 'cn', thesis: '', key_evidence: '', risks: '', kill_switches: '', track_metrics: '', next_review_ts: null }
}
function editCard(c: ThesisCard) {
  form.value = {
    symbol: c.symbol,
    market: c.market,
    thesis: c.thesis,
    key_evidence: c.key_evidence,
    risks: c.risks,
    kill_switches: c.kill_switches,
    track_metrics: c.track_metrics,
    next_review_ts: c.next_review_date ? new Date(c.next_review_date + 'T00:00:00').getTime() : null,
  }
  showForm.value = true
}

async function submit() {
  if (!form.value.symbol.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (!form.value.thesis.trim()) {
    message.warning('请填写核心逻辑——没有逻辑就没有这张卡')
    return
  }
  saving.value = true
  try {
    const d = form.value.next_review_ts
    await upsertThesisCard({
      symbol: form.value.symbol.trim(),
      market: form.value.market,
      thesis: form.value.thesis,
      key_evidence: form.value.key_evidence,
      risks: form.value.risks,
      kill_switches: form.value.kill_switches,
      track_metrics: form.value.track_metrics,
      next_review_date: d ? localDate(d) : '',
    })
    message.success('逻辑卡已保存')
    showForm.value = false
    resetForm()
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

// ---------- 状态操作 ----------
const invalidatingId = ref<number | null>(null)
const invalidReason = ref('')
async function doSetStatus(c: ThesisCard, status: string, reason = '') {
  try {
    await setThesisStatus(c.id, status, reason)
    message.success('状态已更新')
    invalidatingId.value = null
    invalidReason.value = ''
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function doDelete(c: ThesisCard) {
  try {
    await deleteThesisCard(c.id)
    message.success('已删除')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}

// ---------- 展示辅助 ----------
function lines(s: string): string[] {
  return s ? s.split('\n').map((l) => l.trim()).filter(Boolean) : []
}
// 本地日历日 YYYY-MM-DD：toISOString 走 UTC，在 UTC+8 深夜/清晨会整体退一天，
// 逻辑卡复盘日按本地日历判断，必须用本地年月日拼接。
function localDate(d: string | number | Date): string {
  const dt = new Date(d)
  const y = dt.getFullYear()
  const mm = String(dt.getMonth() + 1).padStart(2, '0')
  const dd = String(dt.getDate()).padStart(2, '0')
  return `${y}-${mm}-${dd}`
}
const today = localDate(new Date())
function isDue(c: ThesisCard) {
  return c.status === 'active' && !!c.next_review_date && c.next_review_date <= today
}
const statusTag: Record<string, { label: string; type: 'success' | 'error' | 'default' }> = {
  active: { label: '跟踪中', type: 'success' },
  invalidated: { label: '已失效', type: 'error' },
  archived: { label: '已归档', type: 'default' },
}

onMounted(() => {
  load()
  // 深链预填：/thesis?add=1&symbol=&market=（自选/持仓行内入口）
  if (route.query.add === '1' && route.query.symbol) {
    form.value.symbol = String(route.query.symbol)
    form.value.market = String(route.query.market || 'cn')
    showForm.value = true
    router.replace({ query: {} })
  }
})
</script>

<template>
  <PageContainer title="投资逻辑卡" subtitle="核心逻辑 · 关键证据 · 失效条件 · 定期复盘——先想清楚，再谈买卖">
    <div class="thesis" :style="styleVars">
      <SectionCard title="逻辑卡">
        <template #extra>
          <div class="toolbar">
            <n-select v-model:value="statusFilter" :options="statusOptions" size="small" style="width: 110px" @update:value="load" />
            <n-button size="small" secondary :loading="checking" @click="runCheckup">一键体检</n-button>
            <n-button size="small" type="primary" @click="showForm = !showForm; if (showForm) resetForm()">
              {{ showForm ? '收起表单' : '＋ 新建逻辑卡' }}
            </n-button>
          </div>
        </template>

        <!-- 新建/编辑表单 -->
        <div v-if="showForm" class="form">
          <div class="form-row">
            <n-input v-model:value="form.symbol" placeholder="股票代码，如 600000" style="max-width: 180px" />
            <n-date-picker v-model:value="form.next_review_ts" type="date" placeholder="下次复盘日期（可选）" clearable style="max-width: 200px" />
          </div>
          <n-input v-model:value="form.thesis" type="textarea" :rows="2" placeholder="核心逻辑（必填）：为什么值得关注/持有？一句到三句说清" />
          <n-input v-model:value="form.key_evidence" type="textarea" :rows="2" placeholder="关键证据（可选，一行一条）：支撑逻辑的事实或数据" />
          <n-input v-model:value="form.risks" type="textarea" :rows="2" placeholder="主要风险（可选，一行一条）" />
          <n-input v-model:value="form.kill_switches" type="textarea" :rows="2" placeholder="失效条件（强烈建议填写，一行一条）：出现什么情况说明逻辑不成立，应复盘或放弃" />
          <n-input v-model:value="form.track_metrics" type="textarea" :rows="1" placeholder="跟踪指标（可选，一行一条）：需要持续验证的数据点" />
          <div class="form-actions">
            <n-button type="primary" :loading="saving" @click="submit">保存（同标的自动覆盖）</n-button>
          </div>
        </div>

        <n-spin :show="loading">
          <n-empty v-if="!cards.length" description="还没有逻辑卡——从自选或持仓页的「逻辑卡」入口为标的建立第一张" />
          <div v-else class="cards">
            <div v-for="c in cards" :key="c.id" class="card" :class="{ due: isDue(c) }">
              <div class="card-head">
                <div class="card-title">
                  <span class="name">{{ c.name || c.symbol }}</span>
                  <span class="symbol qv-mono">{{ c.symbol }}</span>
                  <n-tag size="tiny" :type="statusTag[c.status]?.type" :bordered="false" round>
                    {{ statusTag[c.status]?.label }}
                  </n-tag>
                  <n-tag v-if="isDue(c)" size="tiny" type="warning" :bordered="false" round>复盘日已到</n-tag>
                </div>
                <div class="card-ops">
                  <n-button size="tiny" quaternary @click="editCard(c)">编辑</n-button>
                  <n-button v-if="c.status === 'active'" size="tiny" quaternary type="warning" @click="invalidatingId = invalidatingId === c.id ? null : c.id">
                    置失效
                  </n-button>
                  <n-button v-if="c.status === 'active'" size="tiny" quaternary @click="doSetStatus(c, 'archived')">归档</n-button>
                  <n-button v-if="c.status !== 'active'" size="tiny" quaternary type="info" @click="doSetStatus(c, 'active')">恢复</n-button>
                  <n-popconfirm @positive-click="doDelete(c)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error">删除</n-button>
                    </template>
                    确认删除这张逻辑卡？
                  </n-popconfirm>
                </div>
              </div>

              <!-- 置失效原因输入 -->
              <div v-if="invalidatingId === c.id" class="invalid-input">
                <n-input v-model:value="invalidReason" size="small" placeholder="失效原因（如：核心假设被财报证伪）" />
                <n-button size="small" type="warning" @click="doSetStatus(c, 'invalidated', invalidReason)">确认失效</n-button>
              </div>

              <p class="thesis-text">{{ c.thesis }}</p>

              <div v-if="lines(c.key_evidence).length" class="block">
                <span class="block-label">关键证据</span>
                <ul><li v-for="(l, i) in lines(c.key_evidence)" :key="i">{{ l }}</li></ul>
              </div>
              <div v-if="lines(c.risks).length" class="block">
                <span class="block-label">主要风险</span>
                <ul><li v-for="(l, i) in lines(c.risks)" :key="i">{{ l }}</li></ul>
              </div>
              <div v-if="lines(c.kill_switches).length" class="block kill">
                <span class="block-label">失效条件</span>
                <ul><li v-for="(l, i) in lines(c.kill_switches)" :key="i">{{ l }}</li></ul>
              </div>
              <div v-if="lines(c.track_metrics).length" class="block">
                <span class="block-label">跟踪指标</span>
                <ul><li v-for="(l, i) in lines(c.track_metrics)" :key="i">{{ l }}</li></ul>
              </div>
              <div v-if="c.invalid_reason" class="block kill">
                <span class="block-label">失效原因</span>
                <p class="invalid-reason">{{ c.invalid_reason }}</p>
              </div>

              <!-- 体检结果富化 -->
              <div v-if="checkBySymbol.get(c.symbol + ':' + c.market)" class="check">
                <template v-if="checkBySymbol.get(c.symbol + ':' + c.market)!.quote_ok">
                  <span class="check-quote qv-tnum">
                    现价 {{ checkBySymbol.get(c.symbol + ':' + c.market)!.price.toFixed(2) }}
                    <span :style="{ color: pctColor(checkBySymbol.get(c.symbol + ':' + c.market)!.change_pct) }">
                      {{ checkBySymbol.get(c.symbol + ':' + c.market)!.change_pct.toFixed(2) }}%
                    </span>
                    · 近20日
                    <span :style="{ color: pctColor(checkBySymbol.get(c.symbol + ':' + c.market)!.change_pct_20d) }">
                      {{ checkBySymbol.get(c.symbol + ':' + c.market)!.change_pct_20d.toFixed(2) }}%
                    </span>
                  </span>
                </template>
                <n-alert
                  v-for="(sig, i) in checkBySymbol.get(c.symbol + ':' + c.market)!.signals"
                  :key="i"
                  type="warning"
                  :bordered="false"
                  size="small"
                  style="margin-top: 6px"
                >
                  {{ sig }}
                </n-alert>
              </div>

              <div class="card-foot">
                <span v-if="c.next_review_date">下次复盘：{{ c.next_review_date }}</span>
                <span>更新于 {{ c.updated_at?.slice(0, 10) }}</span>
              </div>
            </div>
          </div>
        </n-spin>
      </SectionCard>
    </div>
  </PageContainer>
</template>

<style scoped>
.thesis {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.toolbar {
  display: flex;
  gap: 10px;
  align-items: center;
  /* 卡头 extra 无横滚兜底，360px 下下拉会被两个按钮挤到不可用 */
  flex-wrap: wrap;
  justify-content: flex-end;
}
.form {
  display: flex;
  flex-direction: column;
  gap: 10px;
  padding-bottom: 14px;
  margin-bottom: 14px;
  border-bottom: 1px solid var(--qv-divider);
}
.form-row {
  display: flex;
  gap: 10px;
  flex-wrap: wrap;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
}
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(min(340px, 100%), 1fr));
  gap: 14px;
}
.card {
  border: 1px solid var(--qv-divider);
  border-radius: 10px;
  padding: 14px 16px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.card.due {
  border-color: v-bind('withAlpha(vars.warningColor, 0.55)');
}
.card-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
  flex-wrap: wrap;
}
.card-title {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.name {
  font-weight: 600;
}
.symbol {
  font-size: 12px;
  opacity: 0.65;
}
.card-ops {
  display: flex;
  gap: 2px;
  flex-wrap: wrap;
}
.invalid-input {
  display: flex;
  gap: 8px;
}
.thesis-text {
  margin: 0;
  font-size: 13.5px;
  line-height: 1.65;
}
.block {
  font-size: 12.5px;
}
.block-label {
  font-weight: 600;
  opacity: 0.75;
}
.block ul {
  margin: 4px 0 0;
  padding-left: 18px;
  line-height: 1.6;
}
.block.kill .block-label {
  color: v-bind('vars.warningColor');
}
.invalid-reason {
  margin: 4px 0 0;
}
.check {
  border-top: 1px dashed var(--qv-divider);
  padding-top: 8px;
  font-size: 12.5px;
}
.card-foot {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  opacity: 0.6;
  margin-top: auto;
}
</style>
