<script setup lang="ts">
import { ref, onMounted, computed } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import {
  NButton,
  NInput,
  NInputNumber,
  NSelect,
  NSwitch,
  NForm,
  NFormItem,
  NTag,
  NSpin,
  NEmpty,
  NPopconfirm,
  NGrid,
  NGi,
  useMessage,
} from 'naive-ui'
import {
  listAlerts,
  createAlert,
  updateAlert,
  setAlertStatus,
  deleteAlert,
  evaluateAlerts,
  type AlertRule,
  type AlertInput,
} from '@/api/alert'
import {
  listChannels,
  createChannel,
  updateChannel,
  deleteChannel,
  testChannel,
  type NotifyChannel,
  type NotifyKind,
} from '@/api/notify'
import { useUi } from '@/composables/useUi'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
const route = useRoute()
const router = useRouter()
const { upColor, vars, withAlpha } = useUi()
const styleVars = computed(() => ({ '--qv-divider': vars.value.dividerColor }))

const marketOptions = [
  { label: 'A 股', value: 'cn' },
]
const kindOptions = [
  { label: '到价提醒', value: 'price' },
  { label: '涨跌幅异动', value: 'pct_change' },
  { label: '均线（站上/跌破）', value: 'ma' },
  { label: '突破（新高/新低）', value: 'breakout' },
]
// op 选项随 kind 变化，文案更贴切。
const opOptions = computed(() => {
  switch (form.value.kind) {
    case 'ma':
      return [
        { label: '站上均线', value: 'gte' },
        { label: '跌破均线', value: 'lte' },
      ]
    case 'breakout':
      return [
        { label: '创新高', value: 'gte' },
        { label: '创新低', value: 'lte' },
      ]
    default:
      return [
        { label: '大于等于 ≥', value: 'gte' },
        { label: '小于等于 ≤', value: 'lte' },
      ]
  }
})
const needThreshold = computed(() => form.value.kind === 'price' || form.value.kind === 'pct_change')
const needPeriod = computed(() => form.value.kind === 'ma' || form.value.kind === 'breakout')

// ---------- 表单 ----------
const editingId = ref<number | null>(null)
const form = ref<AlertInput & { symbol: string; market: string }>({
  symbol: '',
  market: 'cn',
  name: '',
  kind: 'price',
  op: 'gte',
  threshold: undefined,
  period: 20,
  once: true,
  note: '',
})
function resetForm() {
  editingId.value = null
  form.value = { symbol: '', market: 'cn', name: '', kind: 'price', op: 'gte', threshold: undefined, period: 20, once: true, note: '' }
}
function editRule(r: AlertRule) {
  editingId.value = r.id
  form.value = {
    symbol: r.symbol,
    market: r.market,
    name: r.name,
    kind: r.kind,
    op: r.op,
    threshold: r.threshold || undefined,
    period: r.period || 20,
    once: r.once,
    note: r.note,
  }
}

const saving = ref(false)
async function submit() {
  if (!editingId.value && !form.value.symbol.trim()) {
    message.warning('请输入股票代码')
    return
  }
  if (needThreshold.value && (form.value.threshold == null || (form.value.kind === 'price' && form.value.threshold <= 0))) {
    message.warning(form.value.kind === 'price' ? '请输入目标价' : '请输入涨跌幅阈值')
    return
  }
  saving.value = true
  try {
    if (editingId.value) await updateAlert(editingId.value, form.value)
    else await createAlert(form.value)
    message.success('已保存')
    resetForm()
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    saving.value = false
  }
}

// ---------- 列表 ----------
const rules = ref<AlertRule[]>([])
const loading = ref(false)
async function load() {
  loading.value = true
  try {
    rules.value = await listAlerts()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
const evaluating = ref(false)
async function runEvaluate() {
  evaluating.value = true
  try {
    const { hits } = await evaluateAlerts()
    message.success(hits > 0 ? `本次命中 ${hits} 条` : '暂无命中')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    evaluating.value = false
  }
}
async function toggle(r: AlertRule) {
  try {
    await setAlertStatus(r.id, r.status === 'paused' ? 'active' : 'paused')
    await load()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function remove(r: AlertRule) {
  try {
    await deleteAlert(r.id)
    if (editingId.value === r.id) resetForm()
    await load()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}

// ---------- 展示辅助 ----------
function describe(r: AlertRule) {
  const p = (n: number) => n.toFixed(2)
  switch (r.kind) {
    case 'price':
      return `现价 ${r.op === 'gte' ? '≥' : '≤'} ${p(r.threshold)}`
    case 'pct_change':
      return `当日涨跌幅 ${r.op === 'gte' ? '≥' : '≤'} ${p(r.threshold)}%`
    case 'ma':
      return `${r.op === 'gte' ? '站上' : '跌破'} MA${r.period}`
    case 'breakout':
      return `创近 ${r.period} 日${r.op === 'gte' ? '新高' : '新低'}`
    default:
      return ''
  }
}
function todayString() {
  const d = new Date()
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${mm}-${dd}`
}

function isHitToday(r: AlertRule) {
  if (r.status === 'triggered') return true
  // 与后端 TriggeredForUser 同口径：非 once 规则只看 triggered_at 是否为今天。
  // 不能用 last_check_date——它每次评估（15 分钟一轮）都会刷成今天，
  // 历史命中会永久显示「已命中」。已暂停的规则不再提示。
  if (r.status === 'paused' || !r.triggered_at) return false
  const d = new Date(r.triggered_at)
  const mm = String(d.getMonth() + 1).padStart(2, '0')
  const dd = String(d.getDate()).padStart(2, '0')
  return `${d.getFullYear()}-${mm}-${dd}` === todayString()
}

function statusTag(r: AlertRule) {
  if (isHitToday(r)) return { text: '已命中', type: 'warning' as const }
  if (r.status === 'paused') return { text: '已暂停', type: 'default' as const }
  return { text: '生效中', type: 'success' as const }
}
function fmtTime(t: string | null) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

onMounted(async () => {
  // 从自选/持仓「设提醒」跳转预填。
  if (route.query.add === '1') {
    form.value.symbol = String(route.query.symbol || '')
    form.value.market = String(route.query.market || 'cn')
    form.value.name = String(route.query.name || '')
    router.replace({ name: 'alerts' })
  }
  await Promise.all([load(), loadChannels()])
})

// ---------- 推送通道 ----------
const channels = ref<NotifyChannel[]>([])
const chForm = ref<{ kind: NotifyKind; name: string; target: string; enabled: boolean }>({
  kind: 'serverchan',
  name: '',
  target: '',
  enabled: true,
})
const kindNotifyOptions = [
  { label: 'Server酱', value: 'serverchan' },
  { label: '自定义 Webhook', value: 'webhook' },
]
async function loadChannels() {
  try {
    channels.value = await listChannels()
  } catch (e) {
    message.error((e as Error).message)
  }
}
const chAdding = ref(false)
async function addChannel() {
  if (chAdding.value) return
  if (!chForm.value.target.trim()) {
    message.warning(chForm.value.kind === 'serverchan' ? '请输入 Server酱 SendKey' : '请输入 Webhook 地址')
    return
  }
  chAdding.value = true
  try {
    await createChannel({ ...chForm.value })
    chForm.value.target = ''
    chForm.value.name = ''
    await loadChannels()
    message.success('已添加推送通道')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    chAdding.value = false
  }
}
async function toggleChannel(ch: NotifyChannel) {
  try {
    await updateChannel(ch.id, { kind: ch.kind, name: ch.name, enabled: !ch.enabled })
    await loadChannels()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function testCh(ch: NotifyChannel) {
  try {
    await testChannel(ch.id)
    message.success('测试推送已发送，请查收')
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function removeChannel(ch: NotifyChannel) {
  try {
    await deleteChannel(ch.id)
    await loadChannels()
    message.success('已删除')
  } catch (e) {
    message.error((e as Error).message)
  }
}
function channelKindLabel(k: string) {
  return k === 'serverchan' ? 'Server酱' : 'Webhook'
}
</script>

<template>
  <PageContainer title="条件提醒" subtitle="到价 / 异动 / 均线 / 突破 · 命中按当日 OHLC 判定 · 可配推送通道主动通知">
    <template #actions>
      <n-button size="small" quaternary :loading="evaluating" @click="runEvaluate">立即检查</n-button>
      <n-button size="small" quaternary :loading="loading" @click="load">刷新</n-button>
    </template>

    <div class="alerts" :style="styleVars">
      <!-- 左：新建/编辑 -->
      <div class="col-form">
        <SectionCard :title="editingId ? '编辑提醒' : '新建提醒'">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-grid cols="2" :x-gap="12">
              <n-gi>
                <n-form-item label="股票代码">
                  <n-input v-model:value="form.symbol" placeholder="如 600000" :disabled="!!editingId" />
                </n-form-item>
              </n-gi>
              <n-gi>
                <n-form-item label="市场">
                  <n-select v-model:value="form.market" :options="marketOptions" :disabled="!!editingId" />
                </n-form-item>
              </n-gi>
            </n-grid>
            <n-form-item label="提醒类型">
              <n-select v-model:value="form.kind" :options="kindOptions" />
            </n-form-item>
            <n-form-item label="条件方向">
              <n-select v-model:value="form.op" :options="opOptions" />
            </n-form-item>
            <n-form-item v-if="needThreshold" :label="form.kind === 'price' ? '目标价' : '涨跌幅阈值（%）'">
              <n-input-number v-model:value="form.threshold" :precision="2" style="width: 100%" />
            </n-form-item>
            <n-form-item v-if="needPeriod" label="周期（交易日）">
              <n-input-number v-model:value="form.period" :min="2" :max="250" style="width: 100%" />
            </n-form-item>
            <n-form-item label="命中后自动暂停">
              <n-switch v-model:value="form.once" />
              <span class="switch-hint">开启后命中一次即暂停，避免重复提示</span>
            </n-form-item>
            <n-form-item label="备注">
              <n-input v-model:value="form.note" placeholder="可选" maxlength="256" />
            </n-form-item>
            <div class="form-actions">
              <n-button v-if="editingId" quaternary @click="resetForm">取消编辑</n-button>
              <n-button type="primary" :loading="saving" @click="submit">{{ editingId ? '保存修改' : '添加提醒' }}</n-button>
            </div>
          </n-form>
        </SectionCard>

        <SectionCard title="推送通道">
          <n-form label-placement="top" :show-feedback="false" class="form">
            <n-form-item label="通道类型">
              <n-select v-model:value="chForm.kind" :options="kindNotifyOptions" />
            </n-form-item>
            <n-form-item :label="chForm.kind === 'serverchan' ? 'SendKey' : 'Webhook 地址'">
              <n-input
                v-model:value="chForm.target"
                :placeholder="chForm.kind === 'serverchan' ? 'Server酱 SendKey' : 'https://...'"
              />
            </n-form-item>
            <n-button type="primary" ghost block :loading="chAdding" @click="addChannel">添加通道</n-button>
            <div class="hint">提醒命中时会主动推送到已启用的通道（同一提醒每天最多推一次）。密钥加密存储、不回显。</div>
          </n-form>

          <div v-if="channels.length" class="channels">
            <div v-for="ch in channels" :key="ch.id" class="channel">
              <div class="ch-main">
                <div class="ch-title">
                  <n-tag size="tiny" round :bordered="false" :type="ch.kind === 'serverchan' ? 'info' : 'default'">{{
                    channelKindLabel(ch.kind)
                  }}</n-tag>
                  <span class="ch-name">{{ ch.name }}</span>
                  <n-tag size="tiny" round :bordered="false" :type="ch.enabled ? 'success' : 'default'">{{
                    ch.enabled ? '启用' : '停用'
                  }}</n-tag>
                </div>
                <div v-if="ch.last_error" class="ch-err">上次推送失败：{{ ch.last_error }}</div>
              </div>
              <div class="ch-actions">
                <n-button size="tiny" quaternary @click="testCh(ch)">测试</n-button>
                <n-button size="tiny" quaternary @click="toggleChannel(ch)">{{ ch.enabled ? '停用' : '启用' }}</n-button>
                <n-popconfirm @positive-click="removeChannel(ch)">
                  <template #trigger>
                    <n-button size="tiny" quaternary type="error">删</n-button>
                  </template>
                  删除该推送通道？
                </n-popconfirm>
              </div>
            </div>
          </div>
        </SectionCard>
      </div>

      <!-- 右：规则列表 -->
      <div class="col-list">
        <SectionCard title="我的提醒">
          <n-spin :show="loading && !rules.length">
            <n-empty v-if="!rules.length" description="暂无提醒规则，在左侧添加一条" />
            <div v-else class="rules">
              <div v-for="r in rules" :key="r.id" class="rule" :class="{ hit: isHitToday(r) }">
                <div class="rule-main">
                  <div class="rule-title">
                    <span class="rule-name">{{ r.name || r.symbol }}</span>
                    <span class="rule-symbol qv-mono">{{ r.symbol }}</span>
                    <n-tag size="tiny" round :bordered="false" :type="statusTag(r).type">{{
                      statusTag(r).text
                    }}</n-tag>
                  </div>
                  <div class="rule-cond">{{ describe(r) }}</div>
                  <div v-if="isHitToday(r) && r.trigger_msg" class="rule-hit" :style="{ color: upColor }">
                    ⚡ {{ r.trigger_msg }}<span class="rule-hit-time"> · {{ fmtTime(r.triggered_at) }}</span>
                  </div>
                  <div v-else-if="r.last_check_date" class="rule-sub">
                    最近检查 {{ r.last_check_date }}<span v-if="r.last_value"> · 观测值 {{ r.last_value.toFixed(2) }}</span>
                  </div>
                  <div v-if="r.note" class="rule-note">{{ r.note }}</div>
                </div>
                <div class="rule-actions">
                  <n-button size="tiny" quaternary @click="toggle(r)">{{ r.status === 'paused' ? '恢复' : '暂停' }}</n-button>
                  <n-button size="tiny" quaternary @click="editRule(r)">编辑</n-button>
                  <n-popconfirm @positive-click="remove(r)">
                    <template #trigger>
                      <n-button size="tiny" quaternary type="error">删除</n-button>
                    </template>
                    删除提醒「{{ r.name || r.symbol }}」？
                  </n-popconfirm>
                </div>
              </div>
            </div>
          </n-spin>
        </SectionCard>
      </div>
    </div>
  </PageContainer>
</template>

<style scoped>
.alerts {
  display: grid;
  grid-template-columns: 340px 1fr;
  gap: 16px;
  align-items: start;
}
@media (max-width: 900px) {
  .alerts {
    grid-template-columns: 1fr;
  }
}
.col-form,
.col-list {
  display: flex;
  flex-direction: column;
  gap: 16px;
  min-width: 0;
}
.form {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.switch-hint {
  font-size: 12px;
  opacity: 0.5;
  margin-left: 10px;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
  gap: 10px;
  margin-top: 4px;
}
.rules {
  display: flex;
  flex-direction: column;
}
.rule {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  padding: 12px 6px;
  border-bottom: 1px solid var(--qv-divider);
}
.rule:last-child {
  border-bottom: none;
}
.rule.hit {
  background: v-bind('withAlpha(upColor, 0.06)');
  border-radius: 8px;
}
.rule-main {
  flex: 1;
  min-width: 0;
}
.rule-title {
  display: flex;
  align-items: center;
  gap: 8px;
}
.rule-name {
  font-size: 14px;
  font-weight: 600;
}
.rule-symbol {
  font-size: 12px;
  opacity: 0.5;
}
.rule-cond {
  font-size: 13px;
  margin-top: 4px;
  opacity: 0.85;
}
.rule-hit {
  font-size: 12px;
  font-weight: 500;
  margin-top: 4px;
}
.rule-hit-time {
  opacity: 0.6;
  font-weight: 400;
}
.rule-sub {
  font-size: 12px;
  opacity: 0.5;
  margin-top: 4px;
}
.rule-note {
  font-size: 12px;
  opacity: 0.55;
  margin-top: 3px;
}
.rule-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
/* 推送通道 */
.channels {
  display: flex;
  flex-direction: column;
  margin-top: 8px;
  border-top: 1px solid var(--qv-divider);
}
.channel {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 10px 4px;
  border-bottom: 1px solid var(--qv-divider);
}
.channel:last-child {
  border-bottom: none;
}
.ch-main {
  flex: 1;
  min-width: 0;
}
.ch-title {
  display: flex;
  align-items: center;
  gap: 6px;
}
.ch-name {
  font-size: 13px;
  font-weight: 500;
}
.ch-err {
  font-size: 11px;
  color: v-bind('upColor');
  margin-top: 3px;
}
.ch-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  flex-shrink: 0;
}
</style>
