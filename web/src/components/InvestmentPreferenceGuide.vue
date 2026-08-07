<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NForm,
  NFormItem,
  NInputNumber,
  NModal,
  NRadioButton,
  NRadioGroup,
  NSpace,
  useMessage,
} from 'naive-ui'
import {
  INVESTMENT_GUIDE_VERSION,
  updatePreference,
  type UserPreference,
} from '@/api/user'

const props = defineProps<{
  modelValue: boolean
  preference: UserPreference | null
}>()
const emit = defineEmits<{
  'update:modelValue': [value: boolean]
  updated: [value: UserPreference]
}>()

const message = useMessage()
const saving = ref(false)
const draft = ref({
  horizon_pref: 'long_term',
  risk_level: 'balanced',
  total_capital: 0,
})
const firstRun = computed(
  () => (props.preference?.investment_guide_version || 0) < INVESTMENT_GUIDE_VERSION,
)
const totalCapitalWan = computed({
  get: () => draft.value.total_capital / 1e4,
  set: (value: number | null) => {
    draft.value.total_capital = Math.round((value || 0) * 1e4)
  },
})

watch(
  () => props.modelValue,
  (show) => {
    if (!show || !props.preference) return
    draft.value = {
      horizon_pref: props.preference.horizon_pref || 'long_term',
      risk_level: props.preference.risk_level || 'balanced',
      total_capital: props.preference.total_capital || 0,
    }
  },
  { immediate: true },
)

function close() {
  emit('update:modelValue', false)
}

async function saveCompleted() {
  if (!props.preference || saving.value) return
  if (draft.value.total_capital <= 0) {
    message.warning('请填写大于 0 的总投资资金；暂时不填可选择跳过')
    return
  }
  saving.value = true
  try {
    const updated = await updatePreference({
      ...props.preference,
      ...draft.value,
      investment_guide_version: INVESTMENT_GUIDE_VERSION,
      investment_guide_status: 'completed',
    })
    emit('updated', updated)
    close()
    message.success('投资偏好已保存')
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    saving.value = false
  }
}

async function skip() {
  if (!props.preference || saving.value) return
  if (!firstRun.value) {
    close()
    return
  }
  saving.value = true
  try {
    const updated = await updatePreference({
      ...props.preference,
      investment_guide_version: INVESTMENT_GUIDE_VERSION,
      investment_guide_status: 'skipped',
    })
    emit('updated', updated)
    close()
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <n-modal
    :show="modelValue"
    preset="card"
    title="我的投资偏好"
    :mask-closable="false"
    :close-on-esc="false"
    :closable="false"
    style="width: min(560px, calc(100vw - 24px))"
  >
    <n-form label-placement="top" :show-feedback="false">
      <n-form-item label="1. 通常准备持有多久？">
        <n-radio-group v-model:value="draft.horizon_pref">
          <n-radio-button value="short_term">短线</n-radio-button>
          <n-radio-button value="mid_term">中线</n-radio-button>
          <n-radio-button value="long_term">长线</n-radio-button>
        </n-radio-group>
      </n-form-item>
      <n-form-item label="2. 能承受多大的波动？">
        <n-radio-group v-model:value="draft.risk_level">
          <n-radio-button value="conservative">保守</n-radio-button>
          <n-radio-button value="balanced">均衡</n-radio-button>
          <n-radio-button value="aggressive">激进</n-radio-button>
        </n-radio-group>
      </n-form-item>
      <n-form-item label="3. 用于投资研究的总资金">
        <n-input-number
          v-model:value="totalCapitalWan"
          :min="0"
          :max="100000000"
          :precision="1"
          :step="1"
          style="width: 180px"
        >
          <template #suffix>万元</template>
        </n-input-number>
      </n-form-item>
      <n-alert type="info" :bordered="false">
        总资金仅用于研究预算、仓位和整手数量估算，不读取也不代表券商账户可用现金。
      </n-alert>
    </n-form>
    <template #footer>
      <n-space justify="end">
        <n-button :disabled="saving" @click="skip">{{ firstRun ? '跳过' : '取消' }}</n-button>
        <n-button type="primary" :loading="saving" @click="saveCompleted">保存偏好</n-button>
      </n-space>
    </template>
  </n-modal>
</template>
