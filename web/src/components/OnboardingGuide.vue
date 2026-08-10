<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { storeToRefs } from 'pinia'
import {
  NAlert,
  NButton,
  NModal,
  NSpace,
  NStep,
  NSteps,
  NTag,
  useMessage,
} from 'naive-ui'
import {
  deferOnboarding,
  finishOnboarding,
  getOnboardingProgress,
  restartOnboarding,
  skipOnboardingStep,
  type OnboardingProgress,
  type OnboardingStep,
  type OnboardingStepStatus,
} from '@/api/onboarding'
import { getPreference, type UserPreference } from '@/api/user'
import { useAuthStore } from '@/stores/auth'
import InvestmentPreferenceGuide from '@/components/InvestmentPreferenceGuide.vue'

const route = useRoute()
const router = useRouter()
const message = useMessage()
const auth = useAuthStore()
const { user, isLoggedIn } = storeToRefs(auth)

const show = ref(false)
const preferenceShow = ref(false)
const loading = ref(false)
const progress = ref<OnboardingProgress | null>(null)
const preference = ref<UserPreference | null>(null)
const current = ref(1)
let loadSequence = 0

const stepNumber: Record<OnboardingStep | 'complete', number> = {
  preference: 1,
  portfolio: 2,
  alert: 3,
  complete: 4,
}
const completedOrSkipped = computed(() => {
  const value = progress.value
  return Boolean(
    value &&
      value.preference_status !== 'not_started' &&
      value.portfolio_status !== 'not_started' &&
      value.alert_status !== 'not_started',
  )
})

function statusMeta(status: OnboardingStepStatus) {
  if (status === 'completed') return { label: '已完成', type: 'success' as const }
  if (status === 'skipped') return { label: '已跳过', type: 'warning' as const }
  return { label: '未处理', type: 'default' as const }
}

async function load(openExplicitly = false) {
  if (!isLoggedIn.value) return
  const sequence = ++loadSequence
  loading.value = true
  try {
    const value = await getOnboardingProgress()
    if (sequence !== loadSequence) return
    progress.value = value
    current.value = stepNumber[value.suggested_step]
    const onHome = route.name === 'home'
    if (openExplicitly || (onHome && value.should_prompt)) show.value = true
  } catch (error) {
    if (openExplicitly) message.error((error as Error).message)
  } finally {
    if (sequence === loadSequence) loading.value = false
  }
}

watch(
  [() => user.value?.id, () => route.name, () => route.query.onboarding],
  ([userID, , requested], previous) => {
    if (!userID) {
      show.value = false
      progress.value = null
      return
    }
    const explicit = requested === '1'
    const changedUser = userID !== previous?.[0]
    if (explicit || changedUser || route.name === 'home') void load(explicit)
  },
  { immediate: true },
)

async function clearOpenQuery() {
  if (route.query.onboarding !== '1') return
  const query = { ...route.query }
  delete query.onboarding
  await router.replace({ query })
}

async function defer() {
  loading.value = true
  try {
    if (progress.value?.status !== 'completed') progress.value = await deferOnboarding()
    show.value = false
    await clearOpenQuery()
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    loading.value = false
  }
}

async function skip(step: OnboardingStep) {
  loading.value = true
  try {
    progress.value = await skipOnboardingStep(step)
    current.value = stepNumber[progress.value.suggested_step]
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    loading.value = false
  }
}

async function openPreference() {
  loading.value = true
  try {
    preference.value = await getPreference()
    show.value = false
    preferenceShow.value = true
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    loading.value = false
  }
}

async function preferenceUpdated(value: UserPreference) {
  preference.value = value
  preferenceShow.value = false
  await load(true)
}

function goToAction(name: 'watchlist' | 'positions' | 'alerts', query: Record<string, string>) {
  show.value = false
  void router.push({ name, query: { ...query, onboarding_return: '1', _stock_action: String(Date.now()) } })
}

async function finish() {
  loading.value = true
  try {
    progress.value = await finishOnboarding()
    current.value = 4
    message.success('首次使用引导已完成')
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    loading.value = false
  }
}

async function restart() {
  loading.value = true
  try {
    progress.value = await restartOnboarding()
    current.value = stepNumber[progress.value.suggested_step]
  } catch (error) {
    message.error((error as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <n-modal
    v-model:show="show"
    preset="card"
    title="首次使用引导"
    :closable="false"
    :close-on-esc="false"
    :mask-closable="false"
    :style="{ width: 'min(720px, calc(100vw - 24px))' }"
  >
    <div v-if="progress" class="onboarding">
      <div class="run-meta">
        <span>流程 v{{ progress.version }} · 第 {{ progress.run }} 次</span>
        <n-tag v-if="progress.status === 'completed'" size="small" type="success" :bordered="false">已完成</n-tag>
      </div>

      <n-steps v-model:current="current" :vertical="false" size="small" class="steps">
        <n-step title="投资偏好" />
        <n-step title="第一项关注" />
        <n-step title="提醒检查" />
        <n-step title="完成" />
      </n-steps>

      <section v-if="progress.status === 'completed'" class="step-panel">
        <h3>本轮引导已完成</h3>
        <div class="status-list">
          <span>投资偏好 <n-tag size="tiny" :type="statusMeta(progress.preference_status).type">{{ statusMeta(progress.preference_status).label }}</n-tag></span>
          <span>第一项关注 <n-tag size="tiny" :type="statusMeta(progress.portfolio_status).type">{{ statusMeta(progress.portfolio_status).label }}</n-tag></span>
          <span>提醒检查 <n-tag size="tiny" :type="statusMeta(progress.alert_status).type">{{ statusMeta(progress.alert_status).label }}</n-tag></span>
        </div>
        <n-space justify="end">
          <n-button :disabled="loading" @click="defer">关闭</n-button>
          <n-button type="primary" secondary :loading="loading" @click="restart">重新开始</n-button>
        </n-space>
      </section>

      <section v-else-if="current === 1" class="step-panel">
        <div class="step-heading">
          <div><h3>设置投资偏好</h3><p>填写持有周期、风险承受和研究资金。</p></div>
          <n-tag size="small" :type="statusMeta(progress.preference_status).type">{{ statusMeta(progress.preference_status).label }}</n-tag>
        </div>
        <n-space justify="end">
          <n-button :disabled="loading" @click="skip('preference')">跳过此步</n-button>
          <n-button type="primary" :loading="loading" @click="openPreference">填写偏好</n-button>
        </n-space>
      </section>

      <section v-else-if="current === 2" class="step-panel">
        <div class="step-heading">
          <div><h3>建立第一项关注</h3><p>添加一只自选，或通过统一导入向导导入持仓。</p></div>
          <n-tag size="small" :type="statusMeta(progress.portfolio_status).type">{{ statusMeta(progress.portfolio_status).label }}</n-tag>
        </div>
        <div class="choice-actions">
          <n-button type="primary" @click="goToAction('watchlist', { add: '1' })">添加第一只自选</n-button>
          <n-button secondary @click="goToAction('positions', { import: '1' })">导入持仓</n-button>
        </div>
        <n-space justify="end"><n-button :disabled="loading" @click="skip('portfolio')">跳过此步</n-button></n-space>
      </section>

      <section v-else-if="current === 3" class="step-panel">
        <div class="step-heading">
          <div><h3>创建并检查提醒</h3><p>使用现有提醒模板保存一条规则，再点“立即检查”完成测试。</p></div>
          <n-tag size="small" :type="statusMeta(progress.alert_status).type">{{ statusMeta(progress.alert_status).label }}</n-tag>
        </div>
        <n-alert v-if="progress.alert_rule_id && !progress.alert_tested_at" type="info" :bordered="false">
          提醒已创建，完成一次“立即检查”后返回这里。
        </n-alert>
        <div class="choice-actions">
          <n-button type="primary" @click="goToAction('alerts', { add: '1' })">打开提醒模板</n-button>
        </div>
        <n-space justify="end"><n-button :disabled="loading" @click="skip('alert')">跳过此步</n-button></n-space>
      </section>

      <section v-else class="step-panel">
        <h3>确认完成</h3>
        <p>每一步都已明确完成或跳过，可以结束本轮引导。</p>
        <n-space justify="end">
          <n-button :disabled="loading" @click="current = 1">返回查看</n-button>
          <n-button type="primary" :loading="loading" :disabled="!completedOrSkipped" @click="finish">完成引导</n-button>
        </n-space>
      </section>

      <div v-if="progress.status !== 'completed'" class="later-action">
        <n-button text :disabled="loading" @click="defer">稍后继续</n-button>
      </div>
    </div>
  </n-modal>

  <InvestmentPreferenceGuide
    v-model="preferenceShow"
    :preference="preference"
    @updated="preferenceUpdated"
  />
</template>

<style scoped>
.onboarding {
  display: flex;
  flex-direction: column;
  gap: 18px;
}
.run-meta,
.step-heading,
.choice-actions,
.status-list {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}
.run-meta {
  justify-content: space-between;
  font-size: 12px;
  opacity: 0.72;
}
.steps {
  overflow-x: auto;
  padding-bottom: 4px;
}
.step-panel {
  min-height: 190px;
  padding: 16px 0 0;
  border-top: 1px solid var(--n-border-color);
  display: flex;
  flex-direction: column;
  gap: 18px;
}
.step-heading {
  justify-content: space-between;
  align-items: flex-start;
}
.step-panel h3,
.step-panel p {
  margin: 0;
}
.step-panel h3 {
  font-size: 16px;
}
.step-panel p {
  margin-top: 5px;
  font-size: 13px;
  opacity: 0.72;
}
.choice-actions {
  align-items: stretch;
}
.status-list {
  flex-direction: column;
  align-items: flex-start;
  font-size: 13px;
}
.later-action {
  display: flex;
  justify-content: center;
}
@media (max-width: 480px) {
  .step-panel {
    min-height: 230px;
  }
  .choice-actions {
    flex-direction: column;
  }
  .choice-actions .n-button {
    width: 100%;
  }
}
</style>
