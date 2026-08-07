<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NCollapseTransition,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NRadioButton,
  NRadioGroup,
  NSelect,
  NStep,
  NSteps,
  NSwitch,
  NTag,
  useMessage,
  type SelectOption,
} from 'naive-ui'
import {
  alertRequestMessage,
  createAlert,
  isPositionAlertKind,
  updateAlert,
  type AlertKind,
  type AlertRule,
} from '@/api/alert'
import { listPositions, type Position } from '@/api/position'
import { searchStocks, type StockSearchItem } from '@/api/stockSearch'
import {
  ALERT_KIND_OPTIONS,
  ALERT_TEMPLATE_GROUPS,
  ALERT_TEMPLATES,
  alertCheckTiming,
  alertDataBoundary,
  alertNeedsPeriod,
  alertNeedsThreshold,
  alertPreview,
  inferAlertTemplate,
  inputFromRule,
  isEarnAlertKind,
  makeTemplateInput,
  templateById,
  type AlertStockContext,
  type AlertTemplate,
  type AlertTemplateId,
  type AlertWizardInput,
} from './alertTemplates'

const props = defineProps<{
  editingRule: AlertRule | null
  stockContext: AlertStockContext | null
}>()
const emit = defineEmits<{
  (e: 'saved'): void
  (e: 'cancel-edit'): void
}>()

const message = useMessage()
const root = ref<HTMLElement | null>(null)
const wizardStep = ref(1)
const selectedTemplateId = ref<AlertTemplateId | null>(null)
const expertMode = ref(false)
const expertExpanded = ref(false)
const positionScope = ref<'all' | 'single'>('all')
const defaultTemplate = ALERT_TEMPLATES[0]
const form = ref<AlertWizardInput>(makeTemplateInput(defaultTemplate))

const editingId = computed(() => props.editingRule?.id ?? null)
const selectedTemplate = computed(() => templateById(selectedTemplateId.value))
const isPositionKind = computed(() => isPositionAlertKind(form.value.kind))
const isEarnKind = computed(() => isEarnAlertKind(form.value.kind))
const needsThreshold = computed(() => alertNeedsThreshold(form.value.kind))
const needsPeriod = computed(() => alertNeedsPeriod(form.value.kind))
const dataBoundary = computed(() => alertDataBoundary(form.value.kind))
const checkTiming = computed(() => alertCheckTiming(form.value.kind))
const expertKindOptions = computed<SelectOption[]>(() => {
  const options: SelectOption[] = [...ALERT_KIND_OPTIONS]
  if (!options.some((option) => option.value === form.value.kind)) {
    options.unshift({ label: `旧规则类型：${form.value.kind}`, value: form.value.kind })
  }
  return options
})

function resetWizard() {
  wizardStep.value = 1
  selectedTemplateId.value = null
  expertMode.value = false
  expertExpanded.value = false
  positionScope.value = 'all'
  form.value = makeTemplateInput(defaultTemplate)
  applyStockContext(props.stockContext)
  saveError.value = ''
}

function applyStockContext(context: AlertStockContext | null) {
  if (!context || editingId.value) return
  form.value.symbol = context.symbol.trim()
  form.value.market = context.market.trim() || 'cn'
  form.value.name = context.name.trim()
  selectedStock.value = context.symbol
    ? { symbol: context.symbol, market: context.market || 'cn', name: context.name || context.symbol, as_of: '', in_watchlist: false, has_position: false }
    : null
}

function selectTemplate(template: AlertTemplate) {
  const target = {
    symbol: form.value.symbol,
    market: form.value.market,
    name: form.value.name,
  }
  form.value = { ...makeTemplateInput(template), ...target }
  selectedTemplateId.value = template.id
  expertMode.value = false
  expertExpanded.value = false
  if (isPositionAlertKind(template.kind)) {
    positionScope.value = target.symbol ? 'single' : 'all'
  } else {
    positionScope.value = 'single'
  }
  saveError.value = ''
  wizardStep.value = 2
}

function startExpertMode() {
  selectedTemplateId.value = null
  expertMode.value = true
  expertExpanded.value = true
  wizardStep.value = 2
}

function selectExpertKind(value: string) {
  const kind = value as AlertKind
  const template = ALERT_TEMPLATES.find((item) => item.kind === kind)
  form.value.kind = kind
  if (template) {
    form.value.op = template.op
    form.value.threshold = template.threshold
    form.value.period = template.period ?? 20
    form.value.once = template.once
  }
  if (isPositionAlertKind(kind)) {
    form.value.op = 'gte'
    form.value.once = false
    positionScope.value = form.value.symbol ? 'single' : 'all'
  } else if (isEarnAlertKind(kind)) {
    form.value.op = 'gte'
  }
}

// ---------- 股票搜索：失败时保留上一批可用结果 ----------
const stockSearchLoading = ref(false)
const stockSearchError = ref('')
const stockResults = ref<StockSearchItem[]>([])
const selectedStock = ref<StockSearchItem | null>(null)
const lastStockQuery = ref('')
let stockSearchSeq = 0

function stockKey(stock: Pick<StockSearchItem, 'market' | 'symbol'>): string {
  return `${stock.market}:${stock.symbol}`
}

const selectedStockKey = computed(() => (form.value.symbol ? `${form.value.market}:${form.value.symbol}` : null))
const stockOptions = computed<SelectOption[]>(() => {
  const rows = [...stockResults.value]
  if (selectedStock.value && !rows.some((item) => stockKey(item) === stockKey(selectedStock.value!))) {
    rows.unshift(selectedStock.value)
  }
  return rows.map((item) => ({
    label: `${item.name || item.symbol} · ${item.symbol}`,
    value: stockKey(item),
  }))
})

async function runStockSearch(query: string) {
  const keyword = query.trim()
  if (!keyword) return
  const seq = ++stockSearchSeq
  lastStockQuery.value = keyword
  stockSearchLoading.value = true
  stockSearchError.value = ''
  try {
    const result = await searchStocks(keyword, 20)
    if (seq !== stockSearchSeq) return
    stockResults.value = result.items || []
  } catch (error) {
    if (seq !== stockSearchSeq) return
    stockSearchError.value = alertRequestMessage('stocks', error)
  } finally {
    if (seq === stockSearchSeq) stockSearchLoading.value = false
  }
}

function chooseStock(value: string | number | null) {
  if (!value) {
    form.value.symbol = ''
    form.value.name = ''
    selectedStock.value = null
    return
  }
  const key = String(value)
  const stock = stockResults.value.find((item) => stockKey(item) === key) ||
    (selectedStock.value && stockKey(selectedStock.value) === key ? selectedStock.value : null)
  if (!stock) return
  selectedStock.value = stock
  form.value.symbol = stock.symbol
  form.value.market = stock.market
  form.value.name = stock.name
}

// ---------- 当前持仓：失败时保留上一批可用结果 ----------
const positions = ref<Position[]>([])
const positionsLoading = ref(false)
const positionsError = ref('')

const positionOptions = computed<SelectOption[]>(() => {
  const seen = new Set<string>()
  const options: SelectOption[] = []
  for (const position of positions.value) {
    const key = `${position.market}:${position.symbol}`
    if (seen.has(key)) continue
    seen.add(key)
    options.push({ label: `${position.name || position.symbol} · ${position.symbol}`, value: key })
  }
  if (form.value.symbol) {
    const key = `${form.value.market}:${form.value.symbol}`
    if (!seen.has(key)) options.unshift({ label: `${form.value.name || form.value.symbol} · ${form.value.symbol}（当前列表未找到）`, value: key })
  }
  return options
})

const selectedPositionKey = computed(() => (form.value.symbol ? `${form.value.market}:${form.value.symbol}` : null))

async function loadPositions() {
  positionsLoading.value = true
  positionsError.value = ''
  try {
    positions.value = await listPositions('holding')
  } catch (error) {
    positionsError.value = alertRequestMessage('positions', error)
  } finally {
    positionsLoading.value = false
  }
}

function choosePosition(value: string | number | null) {
  const key = value == null ? '' : String(value)
  const position = positions.value.find((item) => `${item.market}:${item.symbol}` === key)
  if (position) {
    form.value.symbol = position.symbol
    form.value.market = position.market
    form.value.name = position.name
    return
  }
  if (!key) {
    form.value.symbol = ''
    form.value.name = ''
  }
}

function changePositionScope(value: string | number) {
  positionScope.value = value === 'single' ? 'single' : 'all'
  if (positionScope.value === 'all') {
    form.value.symbol = ''
    form.value.name = '我的全部持仓'
    form.value.market = 'cn'
  } else if (props.stockContext?.symbol && !editingId.value) {
    form.value.symbol = props.stockContext.symbol
    form.value.market = props.stockContext.market || 'cn'
    form.value.name = props.stockContext.name || props.stockContext.symbol
  } else {
    form.value.symbol = ''
    form.value.name = ''
  }
}

// ---------- 参数与校验 ----------
const directionOptions = computed(() => {
  switch (form.value.kind) {
    case 'price':
      return [{ label: '向上到价', value: 'gte' }, { label: '向下到价', value: 'lte' }]
    case 'pct_change':
      return [{ label: '上涨', value: 'gte' }, { label: '下跌', value: 'lte' }]
    case 'ma':
      return [{ label: '站上均线', value: 'gte' }, { label: '跌破均线', value: 'lte' }]
    case 'breakout':
      return [{ label: '创新高', value: 'gte' }, { label: '创新低', value: 'lte' }]
    case 'volume_surge':
      return [{ label: '达到倍数', value: 'gte' }, { label: '低于倍数', value: 'lte' }]
    case 'amplitude':
      return [{ label: '达到阈值', value: 'gte' }, { label: '低于阈值', value: 'lte' }]
    default:
      return [{ label: '大于等于', value: 'gte' }, { label: '小于等于', value: 'lte' }]
  }
})

const pctMagnitude = computed<number | null>({
  get: () => form.value.threshold == null ? null : Math.abs(Number(form.value.threshold)),
  set: (value) => {
    if (value == null) {
      form.value.threshold = undefined
    } else {
      form.value.threshold = form.value.op === 'lte' ? -Math.abs(value) : Math.abs(value)
    }
  },
})

function setDirection(value: string | number) {
  form.value.op = value === 'lte' ? 'lte' : 'gte'
  if (form.value.kind === 'pct_change' && form.value.threshold != null) {
    form.value.threshold = form.value.op === 'lte'
      ? -Math.abs(form.value.threshold)
      : Math.abs(form.value.threshold)
  }
}

const thresholdLabel = computed(() => {
  switch (form.value.kind) {
    case 'price': return '目标价（元）'
    case 'pct_change': return '涨跌幅（%）'
    case 'volume_surge': return '20 日均量倍数'
    case 'amplitude': return '振幅（%）'
    case 'earn_date': return '提前天数'
    case 'cost_gain': return '相对成本上涨（%）'
    case 'cost_drawdown': return '相对成本下跌（%）'
    case 'peak_drawdown': return '从峰值回撤（%）'
    default: return '阈值'
  }
})

const thresholdMax = computed(() => {
  if (form.value.kind === 'earn_date') return 30
  if (form.value.kind === 'cost_gain') return 1000
  if (['pct_change', 'volume_surge', 'amplitude'].includes(form.value.kind)) return 100
  if (['cost_drawdown', 'peak_drawdown'].includes(form.value.kind)) return 99.99
  return undefined
})

function validateTarget(): boolean {
  if (isPositionKind.value && positionScope.value === 'all') return true
  if (!form.value.symbol.trim()) {
    message.warning(isPositionKind.value ? '请选择一只当前持仓' : '请选择股票')
    return false
  }
  return true
}

function validateParameters(): boolean {
  if (needsThreshold.value) {
    const value = Number(form.value.threshold)
    if (!Number.isFinite(value) || value === 0 || (form.value.kind !== 'pct_change' && value < 0)) {
      message.warning(`请填写${thresholdLabel.value}`)
      return false
    }
    const magnitude = Math.abs(value)
    if (thresholdMax.value != null && magnitude > thresholdMax.value) {
      message.warning(`${thresholdLabel.value}不能超过 ${thresholdMax.value}`)
      return false
    }
  }
  if (needsPeriod.value && (!form.value.period || form.value.period < 2 || form.value.period > 250)) {
    message.warning('周期须在 2 到 250 个交易日之间')
    return false
  }
  return true
}

function goToParameters() {
  if (!validateTarget()) return
  wizardStep.value = 3
}

function goToConfirm() {
  if (!validateTarget() || !validateParameters()) return
  wizardStep.value = 4
}

const targetLabel = computed(() => {
  if (isPositionKind.value && positionScope.value === 'all') return '我的全部当前持仓（逐笔判断）'
  return form.value.name
    ? `${form.value.name}（${form.value.symbol}）`
    : form.value.symbol || '尚未选择股票'
})
const preview = computed(() => alertPreview(form.value, targetLabel.value))

// ---------- 保存：失败保留全部字段 ----------
const saving = ref(false)
const saveError = ref('')

async function submit() {
  if (!validateTarget() || !validateParameters() || saving.value) return
  saving.value = true
  saveError.value = ''
  try {
    if (editingId.value) await updateAlert(editingId.value, form.value)
    else await createAlert(form.value)
    message.success(editingId.value ? '提醒已更新' : '提醒已创建')
    emit('saved')
    resetWizard()
  } catch (error) {
    saveError.value = alertRequestMessage('save', error)
  } finally {
    saving.value = false
  }
}

function cancelEditing() {
  emit('cancel-edit')
  resetWizard()
}

watch(
  () => props.editingRule,
  (rule) => {
    if (!rule) {
      resetWizard()
      return
    }
    form.value = inputFromRule(rule)
    const inferred = inferAlertTemplate(rule)
    selectedTemplateId.value = inferred
    expertMode.value = inferred == null
    expertExpanded.value = inferred == null
    positionScope.value = isPositionAlertKind(rule.kind) && !rule.symbol ? 'all' : 'single'
    selectedStock.value = rule.symbol
      ? { symbol: rule.symbol, market: rule.market, name: rule.name || rule.symbol, as_of: '', in_watchlist: false, has_position: isPositionAlertKind(rule.kind) }
      : null
    wizardStep.value = 3
    saveError.value = ''
    void nextTick(() => root.value?.scrollIntoView({ behavior: 'smooth', block: 'start' }))
  },
  { immediate: true },
)

watch(
  () => props.stockContext,
  (context) => applyStockContext(context),
  { deep: true },
)

onMounted(() => void loadPositions())
</script>

<template>
  <div ref="root" class="alert-wizard">
    <n-steps :current="wizardStep" size="small" class="wizard-steps">
      <n-step title="选模板" />
      <n-step title="选范围" />
      <n-step title="填参数" />
      <n-step title="确认" />
    </n-steps>

    <section v-if="wizardStep === 1" class="wizard-pane">
      <div v-for="group in ALERT_TEMPLATE_GROUPS" :key="group.id" class="template-section">
        <div class="template-section-title">{{ group.label }}</div>
        <div class="template-grid">
          <button
            v-for="template in ALERT_TEMPLATES.filter((item) => item.group === group.id)"
            :key="template.id"
            type="button"
            class="template-option"
            @click="selectTemplate(template)"
          >
            <strong>{{ template.title }}</strong>
            <span>{{ template.summary }}</span>
          </button>
        </div>
      </div>
      <div class="expert-entry">
        <n-button quaternary size="small" @click="startExpertMode">直接使用专家模式</n-button>
      </div>
    </section>

    <section v-else-if="wizardStep === 2" class="wizard-pane">
      <div class="step-heading">
        <div>
          <strong>{{ selectedTemplate?.title || '专家规则' }}</strong>
          <span>选择监控对象</span>
        </div>
        <n-tag v-if="editingId" size="small" type="info">编辑时不可更换对象</n-tag>
      </div>

      <template v-if="editingId">
        <div class="locked-target">
          <span>监控对象</span>
          <strong>{{ targetLabel }}</strong>
        </div>
      </template>

      <template v-else-if="isPositionKind">
        <n-radio-group :value="positionScope" @update:value="changePositionScope">
          <n-radio-button value="all">我的全部持仓</n-radio-button>
          <n-radio-button value="single">指定股票</n-radio-button>
        </n-radio-group>
        <n-form-item v-if="positionScope === 'single'" label="当前持仓" class="target-field">
          <n-select
            :value="selectedPositionKey"
            :options="positionOptions"
            :loading="positionsLoading"
            filterable
            placeholder="选择一只当前持仓"
            @update:value="choosePosition"
          />
        </n-form-item>
        <n-alert v-if="positionsError" type="warning" title="持仓加载失败" :bordered="false">
          {{ positions.length ? '仍展示上次取得的持仓，刷新失败。' : positionsError }}
          <div class="recovery-action"><n-button size="small" @click="loadPositions">重试加载持仓</n-button></div>
        </n-alert>
        <n-alert v-else-if="!positionsLoading && !positions.length" type="default" title="当前没有持仓" :bordered="false">
          “我的全部持仓”规则可以保存，但在账本出现当前持仓前不会产生判断。
        </n-alert>
      </template>

      <template v-else>
        <n-form-item label="股票" class="target-field">
          <n-select
            :value="selectedStockKey"
            :options="stockOptions"
            :loading="stockSearchLoading"
            filterable
            remote
            clearable
            placeholder="输入名称、拼音或代码搜索"
            @search="runStockSearch"
            @update:value="chooseStock"
          />
        </n-form-item>
        <n-alert v-if="stockSearchError" type="warning" title="股票加载失败" :bordered="false">
          {{ stockResults.length ? '仍展示上次搜索结果，刷新失败。' : stockSearchError }}
          <div class="recovery-action">
            <n-button size="small" :disabled="!lastStockQuery" @click="runStockSearch(lastStockQuery)">重试当前搜索</n-button>
          </div>
        </n-alert>
      </template>

      <div class="wizard-actions">
        <n-button @click="wizardStep = 1">返回</n-button>
        <n-button type="primary" @click="goToParameters">下一步</n-button>
      </div>
    </section>

    <section v-else-if="wizardStep === 3" class="wizard-pane">
      <div class="step-heading">
        <div>
          <strong>{{ selectedTemplate?.title || '专家规则' }}</strong>
          <span>{{ targetLabel }}</span>
        </div>
        <n-tag v-if="expertMode" size="small" type="warning">专家模式</n-tag>
      </div>

      <n-form label-placement="top" :show-feedback="false" class="parameter-form">
        <n-form-item v-if="expertMode" label="规则类型">
          <n-select :value="form.kind" :options="expertKindOptions" @update:value="selectExpertKind" />
        </n-form-item>

        <n-form-item v-if="!isEarnKind && !isPositionKind && (expertMode || ['price', 'pct_change', 'ma', 'breakout'].includes(form.kind))" label="触发方向">
          <n-radio-group :value="form.op" size="small" @update:value="setDirection">
            <n-radio-button v-for="option in directionOptions" :key="option.value" :value="option.value">
              {{ option.label }}
            </n-radio-button>
          </n-radio-group>
        </n-form-item>

        <n-form-item v-if="needsThreshold" :label="thresholdLabel">
          <n-input-number
            v-if="form.kind === 'pct_change'"
            v-model:value="pctMagnitude"
            :min="0.01"
            :max="thresholdMax"
            :precision="2"
            style="width: 100%"
          />
          <n-input-number
            v-else
            v-model:value="form.threshold"
            :min="form.kind === 'earn_date' ? 1 : 0.01"
            :max="thresholdMax"
            :precision="form.kind === 'earn_date' ? 0 : 2"
            style="width: 100%"
          />
        </n-form-item>

        <n-form-item v-if="needsPeriod" label="周期（交易日）">
          <n-input-number v-model:value="form.period" :min="2" :max="250" :precision="0" style="width: 100%" />
        </n-form-item>

        <n-form-item v-if="expertMode && !needsThreshold" label="原始阈值（当前类型不使用）">
          <n-input-number v-model:value="form.threshold" style="width: 100%" />
        </n-form-item>
        <n-form-item v-if="expertMode && !needsPeriod" label="原始周期（当前类型不使用）">
          <n-input-number v-model:value="form.period" :min="0" :max="250" :precision="0" style="width: 100%" />
        </n-form-item>

        <div v-if="!editingId && selectedTemplate" class="default-note">
          模板值只是可编辑的观察起点，不构成交易建议。
        </div>

        <div class="trust-note">
          <strong>数据来源与失效条件</strong>
          <span>{{ dataBoundary }}</span>
          <span>检查时机：{{ checkTiming }}</span>
        </div>

        <n-button v-if="!expertMode" quaternary size="small" class="expert-toggle" @click="expertExpanded = !expertExpanded">
          {{ expertExpanded ? '收起专家参数' : '展开专家参数' }}
        </n-button>
        <n-collapse-transition :show="expertMode || expertExpanded">
          <div class="expert-fields">
            <n-form-item v-if="!isPositionKind" label="命中后自动暂停">
              <n-switch v-model:value="form.once" />
              <span class="field-hint">关闭后规则持续生效；财报类仍按具体事实去重。</span>
            </n-form-item>
            <n-form-item v-else label="规则生命周期">
              <span class="field-value">持续生效，由后端按每笔持仓和交易日去重</span>
            </n-form-item>
            <n-form-item label="备注">
              <n-input v-model:value="form.note" maxlength="256" placeholder="可选" />
            </n-form-item>
          </div>
        </n-collapse-transition>
      </n-form>

      <div class="wizard-actions">
        <n-button @click="wizardStep = 2">返回</n-button>
        <n-button v-if="editingId" quaternary @click="cancelEditing">取消编辑</n-button>
        <n-button type="primary" @click="goToConfirm">查看确认</n-button>
      </div>
    </section>

    <section v-else class="wizard-pane confirm-pane">
      <n-alert type="info" title="保存前确认" :bordered="false">
        <div class="preview-sentence">{{ preview }}</div>
      </n-alert>
      <div class="confirm-source">
        <strong>数据边界</strong>
        <span>{{ dataBoundary }}</span>
      </div>
      <n-alert v-if="saveError" type="error" title="提醒保存失败" :bordered="false">
        {{ saveError }}
        <div class="recovery-action"><n-button size="small" :loading="saving" @click="submit">重试保存</n-button></div>
      </n-alert>
      <div class="wizard-actions">
        <n-button @click="wizardStep = 3">返回修改</n-button>
        <n-button v-if="editingId" quaternary @click="cancelEditing">取消编辑</n-button>
        <n-button type="primary" :loading="saving" @click="submit">
          {{ editingId ? '保存修改' : '创建提醒' }}
        </n-button>
      </div>
    </section>
  </div>
</template>

<style scoped>
.alert-wizard {
  scroll-margin-top: 76px;
}
.wizard-steps {
  margin-bottom: 18px;
}
.wizard-pane {
  min-width: 0;
}
.template-section + .template-section {
  margin-top: 16px;
}
.template-section-title {
  margin-bottom: 7px;
  font-size: 12px;
  font-weight: 600;
  opacity: 0.62;
}
.template-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
}
.template-option {
  display: flex;
  min-width: 0;
  min-height: 76px;
  flex-direction: column;
  align-items: flex-start;
  justify-content: center;
  gap: 4px;
  padding: 11px 12px;
  border: 1px solid var(--qv-divider);
  border-radius: 6px;
  background: transparent;
  color: inherit;
  font: inherit;
  text-align: left;
  cursor: pointer;
}
.template-option:hover,
.template-option:focus-visible {
  border-color: var(--qv-primary);
  outline: none;
}
.template-option strong {
  font-size: 13px;
}
.template-option span {
  font-size: 11px;
  line-height: 1.45;
  opacity: 0.58;
}
.expert-entry {
  display: flex;
  justify-content: flex-end;
  margin-top: 12px;
}
.step-heading {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  margin-bottom: 16px;
}
.step-heading > div:first-child {
  display: flex;
  min-width: 0;
  flex-direction: column;
  gap: 3px;
}
.step-heading strong {
  font-size: 15px;
}
.step-heading span {
  overflow-wrap: anywhere;
  font-size: 12px;
  opacity: 0.58;
}
.locked-target,
.confirm-source,
.trust-note {
  display: flex;
  flex-direction: column;
  gap: 5px;
  padding: 12px;
  border: 1px solid var(--qv-divider);
  border-radius: 6px;
}
.default-note {
  margin-top: -2px;
  font-size: 12px;
  opacity: 0.58;
}
.locked-target span,
.confirm-source span,
.trust-note span {
  font-size: 12px;
  line-height: 1.55;
  opacity: 0.68;
}
.target-field {
  margin-top: 14px;
}
.recovery-action {
  margin-top: 9px;
}
.parameter-form {
  display: flex;
  flex-direction: column;
  gap: 11px;
}
.trust-note {
  margin-bottom: 2px;
  background: rgba(128, 128, 128, 0.05);
}
.trust-note strong,
.confirm-source strong {
  font-size: 12px;
}
.expert-toggle {
  align-self: flex-start;
}
.expert-fields {
  padding-top: 8px;
  border-top: 1px solid var(--qv-divider);
}
.field-hint {
  margin-left: 10px;
  font-size: 12px;
  opacity: 0.55;
}
.field-value {
  font-size: 13px;
  opacity: 0.78;
}
.wizard-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 18px;
}
.confirm-pane {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.preview-sentence {
  font-size: 14px;
  line-height: 1.7;
}
@media (max-width: 480px) {
  .template-grid {
    grid-template-columns: 1fr;
  }
  .template-option {
    min-height: 66px;
  }
  .wizard-actions {
    flex-wrap: wrap;
  }
  .field-hint {
    display: block;
    margin: 6px 0 0;
  }
}
</style>
