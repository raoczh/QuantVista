<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
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
  NSpin,
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
import { getSessionEpoch } from '@/api/token'
import InvestmentPreferenceGuide from '@/components/InvestmentPreferenceGuide.vue'

const route = useRoute()
const router = useRouter()
const message = useMessage()
const auth = useAuthStore()
const { user, isLoggedIn } = storeToRefs(auth)

const show = ref(false)
const preferenceShow = ref(false)
const loading = ref(false)
const writing = ref(false)
const requestError = ref('')
const progress = ref<OnboardingProgress | null>(null)
const preference = ref<UserPreference | null>(null)
const current = ref(1)
let loadSequence = 0
let disposed = false
let preferenceCurrent = () => false
onBeforeUnmount(() => { disposed = true; loadSequence++ })

function captureScope() {
  const owner = user.value?.id
  const session = getSessionEpoch()
  const path = route.fullPath
  return () => !disposed && !!owner && user.value?.id === owner && getSessionEpoch() === session && route.fullPath === path
}

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
  if (disposed || !isLoggedIn.value || writing.value) return
  const sequence = ++loadSequence
  const currentScope = captureScope()
  loading.value = true
  requestError.value = ''
  if (openExplicitly) show.value = true
  try {
    const value = await getOnboardingProgress()
    if (!currentScope() || sequence !== loadSequence) return
    progress.value = value
    current.value = stepNumber[value.suggested_step]
    const onHome = route.name === 'home'
    if (openExplicitly || (onHome && value.should_prompt)) show.value = true
  } catch (error) {
    if (currentScope() && sequence === loadSequence) requestError.value = (error as Error).message
  } finally {
    if (currentScope() && sequence === loadSequence) loading.value = false
  }
}

watch(
  [() => user.value?.id, () => route.fullPath],
  ([userID], previous) => {
    loadSequence++
    loading.value = false
    writing.value = false
    show.value = false
    preferenceShow.value = false
    requestError.value = ''
    const changedUser = userID !== previous?.[0]
    if (changedUser) {
      progress.value = null
      preference.value = null
    }
    if (!userID) {
      progress.value = null
      return
    }
    const explicit = route.query.onboarding === '1'
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
  if (writing.value || !show.value) return
  const currentScope = captureScope()
  if (loading.value || !progress.value || progress.value.status === 'completed') {
    loadSequence++
    loading.value = false
    show.value = false
    if (currentScope()) await clearOpenQuery()
    return
  }
  if (await mutate(deferOnboarding) && currentScope()) {
    show.value = false
    await clearOpenQuery()
  }
}

async function mutate(action: (progressID: number) => Promise<OnboardingProgress>, successMessage = '') {
  if (loading.value || !show.value || !progress.value || disposed || !isLoggedIn.value) return false
  const currentScope = captureScope()
  const sequence = ++loadSequence
  const id = progress.value.id
  loading.value = true
  writing.value = true
  requestError.value = ''
  try {
    const value = await action(id)
    if (!currentScope() || sequence !== loadSequence) return false
    progress.value = value
    current.value = stepNumber[value.suggested_step]
    if (successMessage) message.success(successMessage)
    return true
  } catch (error) {
    if (currentScope() && sequence === loadSequence) requestError.value = (error as Error).message
    return false
  } finally {
    if (currentScope() && sequence === loadSequence) {
      loading.value = false
      writing.value = false
    }
  }
}

async function skip(step: OnboardingStep) {
  await mutate(id => skipOnboardingStep(step, id))
}

async function openPreference() {
  if (loading.value || !show.value || !progress.value || disposed || !isLoggedIn.value) return
  const currentScope = captureScope()
  const sequence = ++loadSequence
  loading.value = true
  requestError.value = ''
  try {
    const value = await getPreference()
    if (!currentScope() || sequence !== loadSequence) return
    preference.value = value
    preferenceCurrent = currentScope
    show.value = false
    preferenceShow.value = true
  } catch (error) {
    if (currentScope() && sequence === loadSequence) requestError.value = (error as Error).message
  } finally {
    if (currentScope() && sequence === loadSequence) loading.value = false
  }
}

function preferenceUpdated(value: UserPreference) {
  if (preferenceCurrent()) preference.value = value
}

function updatePreferenceShow(value: boolean) {
  const wasOpen = preferenceShow.value
  preferenceShow.value = value
  // 完成或取消三问都返回原引导；离页、换会话后的关闭不重开旧流程。
  if (!value && wasOpen && preferenceCurrent()) void load(true)
}

function goToAction(name: 'watchlist' | 'positions' | 'alerts', query: Record<string, string>) {
  if (loading.value || !show.value || !progress.value || disposed || !isLoggedIn.value) return
  show.value = false
  void router.push({ name, query: { ...query, onboarding_return: '1', _stock_action: String(Date.now()) } })
}

async function finish() {
  if (!completedOrSkipped.value) return
  await mutate(finishOnboarding, '首次使用引导已完成')
}

async function restart() {
  await mutate(restartOnboarding)
}
</script>

<template>
  <n-modal
    v-model:show="show"
    preset="card"
    class="onboarding-modal"
    title="首次使用引导"
    :closable="false"
    :close-on-esc="false"
    :mask-closable="false"
  >
    <n-alert v-if="requestError" type="error" :bordered="false" class="request-error">
      {{ requestError }}
      <div><n-button size="small" :disabled="loading" @click="load(true)">重试加载引导</n-button></div>
    </n-alert>
    <div v-if="loading && !progress" class="loading-state"><n-spin size="small" />正在加载引导进度…</div>
    <div v-if="progress" class="onboarding">
      <div class="run-meta">
        <span>第 {{ progress.run }} 次引导</span>
        <n-tag v-if="progress.status === 'completed'" size="small" type="success" :bordered="false">已完成</n-tag>
      </div>

      <n-steps :current="current" :vertical="false" size="small" class="steps" @update:current="!loading && (current = $event)">
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
          <n-button type="primary" :disabled="loading" @click="goToAction('watchlist', { add: '1' })">添加第一只自选</n-button>
          <n-button secondary :disabled="loading" @click="goToAction('positions', { import: '1' })">导入持仓</n-button>
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
          <n-button type="primary" :disabled="loading" @click="goToAction('alerts', { add: '1' })">打开提醒模板</n-button>
        </div>
        <n-space justify="end"><n-button :disabled="loading" @click="skip('alert')">跳过此步</n-button></n-space>
      </section>

      <section v-else class="step-panel">
        <h3>确认完成</h3>
        <p>{{ completedOrSkipped ? '每一步都已明确完成或跳过，可以结束本轮引导。' : '请先完成或跳过前面的步骤，再结束本轮引导。' }}</p>
        <n-space justify="end">
          <n-button :disabled="loading" @click="current = 1">返回查看</n-button>
          <n-button type="primary" :loading="loading" :disabled="!completedOrSkipped" @click="finish">完成引导</n-button>
        </n-space>
      </section>

    </div>
    <template #footer>
      <n-space justify="end">
        <n-button :disabled="writing" @click="defer">{{ loading || !progress || progress.status === 'completed' ? '关闭' : '稍后继续' }}</n-button>
      </n-space>
    </template>
  </n-modal>

  <InvestmentPreferenceGuide
    :model-value="preferenceShow"
    :preference="preference"
    @updated="preferenceUpdated"
    @update:model-value="updatePreferenceShow"
  />
</template>

<style scoped>
:global(.onboarding-modal) {
  width: min(720px, calc(100vw - 24px));
  max-height: calc(100dvh - 24px);
  display: flex;
  flex-direction: column;
  overflow: hidden;
}
:global(.onboarding-modal > .n-card-content) { min-height: 0; overflow-y: auto; }
:global(.onboarding-modal > .n-card__footer) { flex-shrink: 0; }
.request-error { margin-bottom: 14px; }
.request-error .n-button { margin-top: 8px; }
.loading-state { display: flex; align-items: center; gap: 10px; }
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
