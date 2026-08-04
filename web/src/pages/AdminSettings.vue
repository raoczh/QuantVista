<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import {
  NSpace,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSwitch,
  NSelect,
  NButton,
  NTable,
  NTag,
  NAlert,
  NPopconfirm,
  NModal,
  NCheckbox,
  NEmpty,
  useMessage,
} from 'naive-ui'
import {
  getSystemSettings,
  updateSystemSettings,
  listUsers,
  setUserStatus,
  getUserQuota,
  updateUserQuota,
  listSyncLogs,
  getDataHealth,
  triggerWideSync,
  triggerWideInit,
  triggerSyncBars,
  triggerSnapshot,
  triggerFactorRebuild,
  triggerBackfillCalendar,
  listLLMRoutes,
  upsertLLMRoute,
  deleteLLMRoute,
  resetLLMRoute,
  type SystemSettings,
  type SyncLog,
  type DataHealthReport,
  type LLMRouteView,
  type LLMRouteModuleOption,
} from '@/api/admin'
import { listLLMConfigs, type LLMConfig } from '@/api/llm'
import type { AuthUser } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'
import { useIsMobile } from '@/composables/useIsMobile'
import PageContainer from '@/components/PageContainer.vue'
import SectionCard from '@/components/SectionCard.vue'

const message = useMessage()
// 手机上左标签表单太挤，切换为上下堆叠。
const { isMobile } = useIsMobile()
const auth = useAuthStore()

const settings = ref<SystemSettings | null>(null)
const savingReg = ref(false)
const savingGithub = ref(false)

// GitHub 表单（secret 留空表示保留原值）。
const gh = reactive({ client_id: '', client_secret: '', enabled: false })

async function load() {
  try {
    settings.value = await getSystemSettings()
    gh.client_id = settings.value.github_client_id
    gh.enabled = settings.value.github_oauth_enabled
    gh.client_secret = ''
    news.interval = settings.value.news_collect_interval_min
    news.auto_llm = settings.value.news_auto_llm
    fb.enabled = settings.value.llm_fallback_enabled
    fb.config_id = settings.value.llm_fallback_config_id
    acEnabled.value = settings.value.llm_accuracy_contract
    evRefsEnabled.value = settings.value.llm_evidence_refs
    semanticEnabled.value = settings.value.llm_semantic_validator
    capRoutingEnabled.value = settings.value.llm_capability_routing
    debateEnabled.value = settings.value.llm_conditional_debate
    reflectionEnabled.value = settings.value.llm_reflection_shadow
    challengerEnabled.value = settings.value.llm_challenger
    layeredCtxEnabled.value = settings.value.llm_layered_context
    modelRoutingEnabled.value = settings.value.llm_model_routing
    siteBaseURL.value = settings.value.site_base_url
  } catch (e) {
    message.error((e as Error).message)
  }
}

/* 站点地址：推送通知（ntfy）拼点击跳转链接用；空 = 通知不带跳转 */
const savingSite = ref(false)
const siteBaseURL = ref('')
async function saveSite() {
  savingSite.value = true
  try {
    settings.value = await updateSystemSettings({ site_base_url: siteBaseURL.value.trim() })
    siteBaseURL.value = settings.value.site_base_url
    message.success('站点地址已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingSite.value = false
  }
}

/* LLM 回退：开关 + 指定回退配置（下拉列出当前管理员自己的 LLM 配置） */
const savingFb = ref(false)
const fb = reactive({ enabled: true, config_id: 0 })
const myConfigs = ref<LLMConfig[]>([])
const fbOptions = computed(() => {
  const opts: { label: string; value: number }[] = [{ label: '自动（首个管理员的默认配置）', value: 0 }]
  for (const c of myConfigs.value) {
    opts.push({ label: `${c.name}（${c.model}${c.is_default ? ' · 默认' : ''}）`, value: c.id })
  }
  // 指定的配置不在本人列表里（其他管理员的）也要能回显，避免下拉显示成裸数字。
  if (fb.config_id > 0 && !opts.some((o) => o.value === fb.config_id)) {
    opts.push({ label: `配置 #${fb.config_id}（其他管理员的）`, value: fb.config_id })
  }
  return opts
})
async function loadMyConfigs() {
  try {
    myConfigs.value = await listLLMConfigs()
  } catch {
    /* 列表拉不到时仍可保存"自动" */
  }
}
async function saveFallback() {
  savingFb.value = true
  try {
    settings.value = await updateSystemSettings({
      llm_fallback_enabled: fb.enabled,
      llm_fallback_config_id: fb.config_id || 0,
    })
    fb.enabled = settings.value.llm_fallback_enabled
    fb.config_id = settings.value.llm_fallback_config_id
    message.success('LLM 回退设置已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingFb.value = false
  }
}

/* LLM 准确性契约（P0-1 ac1）：中央契约注入/低温钳制/repair 归零/流式完整性门禁的总开关 */
const savingAc = ref(false)
const acEnabled = ref(true)
async function toggleAccuracy(v: boolean) {
  savingAc.value = true
  try {
    settings.value = await updateSystemSettings({ llm_accuracy_contract: v })
    acEnabled.value = settings.value.llm_accuracy_contract
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingAc.value = false
  }
}

/* P0-3 字段路径证据链（ev4）：快照结构化数据缺口注入 + 证据链 evidence_id/source 标注 */
const savingEv = ref(false)
const evRefsEnabled = ref(true)
async function toggleEvidenceRefs(v: boolean) {
  savingEv.value = true
  try {
    settings.value = await updateSystemSettings({ llm_evidence_refs: v })
    evRefsEnabled.value = settings.value.llm_evidence_refs
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingEv.value = false
  }
}

/* P0-4 跨模块语义校验：rating/action/计划价/风险闸门跨字段一致性（仅新增规则的回滚开关） */
const savingSv = ref(false)
const semanticEnabled = ref(true)
async function toggleSemanticValidator(v: boolean) {
  savingSv.value = true
  try {
    settings.value = await updateSystemSettings({ llm_semantic_validator: v })
    semanticEnabled.value = settings.value.llm_semantic_validator
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingSv.value = false
  }
}

/* P0-5 能力矩阵声明化路由：已声明/观察到不支持 json_object 的模型直接按纯文本请求 */
const savingCapRoute = ref(false)
const capRoutingEnabled = ref(true)
async function toggleCapabilityRouting(v: boolean) {
  savingCapRoute.value = true
  try {
    settings.value = await updateSystemSettings({ llm_capability_routing: v })
    capRoutingEnabled.value = settings.value.llm_capability_routing
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingCapRoute.value = false
  }
}

/* P1-3 条件式辩论：低置信/证据冲突/风险临界的个股分析追加 bull/bear/judge 独立复核 */
const savingDebate = ref(false)
const debateEnabled = ref(true)
async function toggleDebate(v: boolean) {
  savingDebate.value = true
  try {
    settings.value = await updateSystemSettings({ llm_conditional_debate: v })
    debateEnabled.value = settings.value.llm_conditional_debate
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingDebate.value = false
  }
}

/* P1-5 反思记忆影子层：成熟推荐教训生成 + 推荐生成时影子检索（不注入不改写） */
const savingReflection = ref(false)
const reflectionEnabled = ref(true)
async function toggleReflection(v: boolean) {
  savingReflection.value = true
  try {
    settings.value = await updateSystemSettings({ llm_reflection_shadow: v })
    reflectionEnabled.value = settings.value.llm_reflection_shadow
    message.success('已保存，下一轮任务生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingReflection.value = false
  }
}

/* P2-1/S3-6C 统一影子实验：prompt 与 score-blind 互斥，每批最多一次额外调用（缺省关）。 */
const savingChallenger = ref(false)
const challengerEnabled = ref(false)
async function toggleChallenger(v: boolean) {
  savingChallenger.value = true
  try {
    settings.value = await updateSystemSettings({ llm_challenger: v })
    challengerEnabled.value = settings.value.llm_challenger
    message.success('已保存，下一次推荐生成生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingChallenger.value = false
  }
}

/* P2-3 多层上下文：QA 被裁剪历史的分层注入 + 反思影子检索分层（纯程序化零额外调用） */
const savingLayeredCtx = ref(false)
const layeredCtxEnabled = ref(true)
async function toggleLayeredContext(v: boolean) {
  savingLayeredCtx.value = true
  try {
    settings.value = await updateSystemSettings({ llm_layered_context: v })
    layeredCtxEnabled.value = settings.value.llm_layered_context
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingLayeredCtx.value = false
  }
}

/* P2-4 模型路由：按模块把 AI 调用改走指定配置（缺省关；自动回退须显式恢复） */
const savingModelRouting = ref(false)
const modelRoutingEnabled = ref(false)
async function toggleModelRouting(v: boolean) {
  savingModelRouting.value = true
  try {
    settings.value = await updateSystemSettings({ llm_model_routing: v })
    modelRoutingEnabled.value = settings.value.llm_model_routing
    message.success('已保存，下一次 AI 调用生效')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingModelRouting.value = false
  }
}

const routes = ref<LLMRouteView[]>([])
const routeModules = ref<LLMRouteModuleOption[]>([])
const routeSaving = ref(false)
const routeForm = reactive({ module: '', config_id: 0, enabled: true, note: '', max_cost_ratio: 0 })
const routeModuleOptions = computed(() =>
  routeModules.value.map((m) => ({ label: `${m.label}（${m.module}）`, value: m.module })),
)
const routeConfigOptions = computed(() =>
  myConfigs.value.map((c) => ({ label: `${c.name}（${c.model}）`, value: c.id })),
)
async function loadRoutes() {
  try {
    const res = await listLLMRoutes()
    routes.value = res.routes
    routeModules.value = res.modules
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function saveRoute() {
  if (!routeForm.module || !routeForm.config_id) {
    message.warning('请选择模块与目标配置')
    return
  }
  routeSaving.value = true
  try {
    await upsertLLMRoute({
      module: routeForm.module,
      config_id: routeForm.config_id,
      enabled: routeForm.enabled,
      note: routeForm.note,
      max_cost_ratio: routeForm.max_cost_ratio || 0,
    })
    message.success('路由已保存（自动回退状态已清除）')
    routeForm.module = ''
    routeForm.config_id = 0
    routeForm.note = ''
    routeForm.max_cost_ratio = 0
    routeForm.enabled = true
    await loadRoutes()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    routeSaving.value = false
  }
}
function editRoute(r: LLMRouteView) {
  routeForm.module = r.module
  routeForm.config_id = r.config_id
  routeForm.enabled = r.enabled
  routeForm.note = r.note
  routeForm.max_cost_ratio = r.max_cost_ratio || 0
}
async function removeRoute(r: LLMRouteView) {
  try {
    await deleteLLMRoute(r.id)
    message.success('路由已删除，恢复默认配置链路')
    await loadRoutes()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function recoverRoute(r: LLMRouteView) {
  try {
    await resetLLMRoute(r.id)
    message.success('已恢复该路由（自动回退状态清除）')
    await loadRoutes()
  } catch (e) {
    message.error((e as Error).message)
  }
}
function routeHealthText(r: LLMRouteView): string {
  const h = r.health
  const parts: string[] = []
  if (h.routed.total > 0) {
    parts.push(`近24h ${h.routed.total} 次（失败 ${h.routed.errors}）`)
  } else {
    parts.push('近24h 无调用')
  }
  if (h.cost_ratio > 0) parts.push(`成本比 ${h.cost_ratio}`)
  if (h.calib_brier != null && h.calib_best_peer != null)
    parts.push(`Brier ${h.calib_brier}（最优层 ${h.calib_best_peer}）`)
  return parts.join(' · ')
}

/* 新闻采集配置：间隔分钟数 + 自动 LLM 情绪分析开关 */
const savingNews = ref(false)
const news = reactive({ interval: 5, auto_llm: true })
async function saveNews() {
  savingNews.value = true
  try {
    settings.value = await updateSystemSettings({
      news_collect_interval_min: news.interval || 5,
      news_auto_llm: news.auto_llm,
    })
    news.interval = settings.value.news_collect_interval_min
    news.auto_llm = settings.value.news_auto_llm
    message.success('新闻采集设置已保存，间隔在下一轮采集生效')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingNews.value = false
  }
}

async function toggleRegistration(v: boolean) {
  savingReg.value = true
  try {
    settings.value = await updateSystemSettings({ registration_open: v })
    await auth.fetchSetupStatus()
    message.success('已保存')
  } catch (e) {
    message.error((e as Error).message)
    await load()
  } finally {
    savingReg.value = false
  }
}

async function saveGithub() {
  savingGithub.value = true
  try {
    settings.value = await updateSystemSettings({
      github_client_id: gh.client_id,
      github_client_secret: gh.client_secret || undefined,
      github_oauth_enabled: gh.enabled,
    })
    gh.client_secret = ''
    await auth.fetchSetupStatus()
    message.success('GitHub 设置已保存')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    savingGithub.value = false
  }
}

/* 用户管理 */
const users = ref<AuthUser[]>([])
async function loadUsers() {
  try {
    users.value = await listUsers()
  } catch (e) {
    message.error((e as Error).message)
  }
}
async function toggleStatus(u: AuthUser) {
  const next = u.status === 'enabled' ? 'disabled' : 'enabled'
  try {
    await setUserStatus(u.id, next)
    message.success(next === 'disabled' ? '已禁用（并强制登出）' : '已启用')
    await loadUsers()
  } catch (e) {
    message.error((e as Error).message)
  }
}

const callbackHint = `${location.origin}/login/callback`

/* 用户 AI 配额管理（批次 J） */
const quotaModal = ref(false)
const quotaUser = ref<AuthUser | null>(null)
const quotaLoading = ref(false)
const quotaSaving = ref(false)
const quotaForm = reactive({ action_limit: 0, action_used: 0, token_used: 0, request_count: 0, reset_used: false })
async function openQuota(u: AuthUser) {
  quotaUser.value = u
  quotaForm.reset_used = false
  quotaModal.value = true
  quotaLoading.value = true
  try {
    const q = await getUserQuota(u.id)
    quotaForm.action_limit = q.action_limit
    quotaForm.action_used = q.action_used
    quotaForm.token_used = q.token_used
    quotaForm.request_count = q.request_count
  } catch (e) {
    message.error((e as Error).message)
    quotaModal.value = false
  } finally {
    quotaLoading.value = false
  }
}
async function saveQuota() {
  if (!quotaUser.value) return
  quotaSaving.value = true
  try {
    const q = await updateUserQuota(quotaUser.value.id, {
      action_limit: quotaForm.action_limit || 0,
      reset_used: quotaForm.reset_used,
    })
    quotaForm.action_used = q.action_used
    quotaForm.token_used = q.token_used
    quotaForm.request_count = q.request_count
    quotaForm.reset_used = false
    message.success('配额已更新')
    quotaModal.value = false
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    quotaSaving.value = false
  }
}

/* 数据源同步日志（批次 J：现有 sync-logs 端点接入后台页） */
const logs = ref<SyncLog[]>([])
const logsLoading = ref(false)
async function loadLogs() {
  logsLoading.value = true
  try {
    logs.value = await listSyncLogs(50)
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    logsLoading.value = false
  }
}
function logStatusType(s: string) {
  return s === 'success' ? 'success' : s === 'failed' ? 'error' : 'warning'
}
const taskLabel: Record<string, string> = {
  sync_daily_bars: '日线批量同步',
  backfill_calendar: '交易日历回填',
  snapshot_market: '市场情绪快照',
}
function fmtLogTime(t: string) {
  return t ? new Date(t).toLocaleString('zh-CN', { hour12: false }) : ''
}

/* P1 数据健康总览：各数据域 expected/observed 对账 + 补跑入口 */
const health = ref<DataHealthReport | null>(null)
const healthLoading = ref(false)
async function loadHealth() {
  healthLoading.value = true
  try {
    health.value = await getDataHealth()
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    healthLoading.value = false
  }
}
const healthStatusMeta: Record<string, { label: string; type: 'success' | 'error' | 'warning' | 'default' }> = {
  ok: { label: '正常', type: 'success' },
  behind: { label: '落后', type: 'error' },
  empty: { label: '无数据', type: 'warning' },
  unknown: { label: '无法判定', type: 'default' },
}
const rerunning = ref('')
async function rerun(key: string, fn: () => Promise<unknown>) {
  rerunning.value = key
  try {
    await fn()
    message.success('已触发，稍后刷新查看结果')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    rerunning.value = ''
  }
}

onMounted(() => {
  load()
  loadUsers()
  loadLogs()
  loadMyConfigs()
  loadHealth()
  loadRoutes()
})
</script>

<template>
  <PageContainer title="管理后台" subtitle="系统设置与用户管理">
    <div class="admin-stack">
      <!-- 注册开关 -->
      <SectionCard title="注册策略" :hoverable="false">
        <n-space align="center">
          <span>开放 GitHub 注册：</span>
          <n-switch
            :value="settings?.registration_open ?? false"
            :loading="savingReg"
            @update:value="toggleRegistration"
          />
          <span style="opacity: 0.6">关闭时，仅已存在的账号可登录，新 GitHub 用户无法注册。</span>
        </n-space>
      </SectionCard>

      <!-- 新闻采集 -->
      <SectionCard title="新闻采集" :hoverable="false">
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 120" style="max-width: 560px" :show-feedback="false">
          <n-form-item label="采集间隔">
            <n-input-number v-model:value="news.interval" :min="1" :max="120" :step="1" style="width: 140px">
              <template #suffix>分钟</template>
            </n-input-number>
            <span style="opacity: 0.6; margin-left: 12px; font-size: 12px">快讯轮询间隔（1~120），改动在下一轮采集生效，无需重启。</span>
          </n-form-item>
          <n-form-item label="自动 LLM 分析">
            <n-switch v-model:value="news.auto_llm" />
            <span style="opacity: 0.6; margin-left: 12px; font-size: 12px">
              开启：采集后自动调用管理员 LLM 做新闻情绪增强；关闭：只做关键词规则分析，不消耗 token。
            </span>
          </n-form-item>
          <n-button type="primary" :loading="savingNews" style="margin-top: 8px" @click="saveNews">保存新闻设置</n-button>
        </n-form>
      </SectionCard>

      <!-- LLM 回退 -->
      <SectionCard title="LLM 回退" :hoverable="false">
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 120" style="max-width: 620px" :show-feedback="false">
          <n-form-item label="允许回退">
            <n-switch v-model:value="fb.enabled" />
            <span style="opacity: 0.6; margin-left: 12px; font-size: 12px">
              开启：未配置 LLM 的用户自动使用下方指定的配置（次数配额仍按其本人计）；关闭：用户必须自己配置 LLM 才能使用 AI 功能。
            </span>
          </n-form-item>
          <n-form-item label="回退配置">
            <n-select v-model:value="fb.config_id" :options="fbOptions" :disabled="!fb.enabled" style="max-width: 340px" />
          </n-form-item>
          <n-form-item label=" ">
            <span style="font-size: 12px; opacity: 0.55">
              该配置同时作为系统后台任务（新闻情绪分析等）的默认 LLM；后台任务不受"允许回退"开关影响。指定配置失效时自动回落"首个管理员的默认配置"。
            </span>
          </n-form-item>
          <n-button type="primary" :loading="savingFb" @click="saveFallback">保存回退设置</n-button>
        </n-form>
      </SectionCard>

      <!-- LLM 准确性契约（P0-1 ac1）+ P0-3 证据链 + P0-4 语义校验 -->
      <SectionCard title="LLM 准确性契约" :hoverable="false">
        <n-space vertical size="large">
          <n-space align="center">
            <span>启用准确性契约：</span>
            <n-switch :value="acEnabled" :loading="savingAc" @update:value="toggleAccuracy" />
            <span style="opacity: 0.6; font-size: 12px">
              开启：所有 AI 调用注入不可覆盖的准确性契约（ac1）、结构化调用低温钳制、repair 温度归零、拒收被截断或无终止标记的半截响应。仅当上游网关兼容性异常（正常请求被误判拒收）时才临时关闭回退旧路径。
            </span>
          </n-space>
          <n-space align="center">
            <span>字段路径证据链：</span>
            <n-switch :value="evRefsEnabled" :loading="savingEv" @update:value="toggleEvidenceRefs" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P0-3）：个股快照对缺失数据段注入结构化 unknowns（模型可区分「没有数据」与「数据为零」），数值核验明细带证据链 ID 与数据源。关闭回退 ev3 行为。
            </span>
          </n-space>
          <n-space align="center">
            <span>跨模块语义校验：</span>
            <n-switch :value="semanticEnabled" :loading="savingSv" @update:value="toggleSemanticValidator" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P0-4）：评级/动作/计划价与风险闸门的跨字段一致性（如 ST 禁买级标的不得评偏多、短线买入盈亏比不足降观察）。关闭仅回退新增跨字段规则，各模块既有校验不受影响。
            </span>
          </n-space>
          <n-space align="center">
            <span>模型能力声明化路由：</span>
            <n-switch :value="capRoutingEnabled" :loading="savingCapRoute" @update:value="toggleCapabilityRouting" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P0-5）：已声明或观察到不支持 JSON 结构化输出的模型，业务调用直接按纯文本请求（省一次注定失败的请求；LLM 配置页「测试连接」会顺带探测并记录）。关闭回退每次在线试错的隐式回落。
            </span>
          </n-space>
          <n-space align="center">
            <span>条件式多空辩论：</span>
            <n-switch :value="debateEnabled" :loading="savingDebate" @update:value="toggleDebate" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P1-3）：个股标准分析在低置信度/结论与数据矛盾/风险闸门临界时，追加独立的看多/看空/裁判三角色辩论复核（最多 4 次额外调用，触发条件与预算记录进运行清单）。高置信分析不触发零成本；辩论失败自动回退单路结果。
            </span>
          </n-space>
          <n-space align="center">
            <span>反思记忆影子层：</span>
            <n-switch :value="reflectionEnabled" :loading="savingReflection" @update:value="toggleReflection" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P1-5）：成熟推荐样本 ≥30 后，后台按批结算结果生成历史教训（LLM 反思，走系统默认配置）；推荐生成时检索适用教训随批次记录（影子：不注入提示词、不改写结果）。关闭停止生成与检索，已生成教训保留。
            </span>
          </n-space>
          <n-space align="center">
            <span>推荐影子实验采样：</span>
            <n-switch :value="challengerEnabled" :loading="savingChallenger" @update:value="toggleChallenger" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P2-1/S3-6C，缺省关）：存在 running 的 prompt challenger 或 score-blind 输入实验时，仅对实验创建者本人每批推荐追加一次影子调用；两类实验互斥、合计最多一次。纯影子、不影响推荐：成功、失败、空 picks、越池结果均只落实验与审计事实，不改业务 picks、action、confidence、候选池、推荐批次状态或 l2 标签。
            </span>
          </n-space>
          <n-space align="center">
            <span>多层上下文检索：</span>
            <n-switch :value="layeredCtxEnabled" :loading="savingLayeredCtx" @update:value="toggleLayeredContext" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P2-3）：问答被裁剪的更早轮次以程序化「索引 + 按相关性检索摘录」注入（此前静默丢弃），每轮回答记录上下文分层快照（各层条数/粗估 token/不可见轮数）；反思影子检索同步分层。纯程序化零额外 LLM 调用；关闭回退旧的静默截断（快照观测保留）。
            </span>
          </n-space>
          <n-space align="center">
            <span>模型路由：</span>
            <n-switch :value="modelRoutingEnabled" :loading="savingModelRouting" @update:value="toggleModelRouting" />
            <span style="opacity: 0.6; font-size: 12px">
              开启（P2-4，缺省关）：按下方「模型路由」表把指定模块的 AI 调用改走指定配置（如小模型跑新闻情绪省成本）。结构化任务不会路由到已知不支持 JSON 输出的目标；失败率/成本比/校准 Brier 恶化会自动回退并停用该路由（须显式恢复）。配额仍记发起用户；审计记录路由后真实目标。
            </span>
          </n-space>
        </n-space>
      </SectionCard>

      <!-- P2-4 模型路由表 -->
      <SectionCard title="模型路由" :hoverable="false">
        <n-alert type="info" :show-icon="false" :bordered="false" class="note">
          一行 = 一个模块的调用改走指定 LLM 配置（目标配置须属启用状态的管理员）。推荐影子实验（prompt challenger / score-blind）恒跟随推荐主调的路由，不可单独配置。总开关在上方「LLM 准确性契约」卡的「模型路由」。
        </n-alert>
        <n-table :bordered="false" :single-line="false" v-if="routes.length">
          <thead>
            <tr>
              <th>模块</th>
              <th>目标配置</th>
              <th>状态</th>
              <th>健康</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="r in routes" :key="r.id">
              <td>{{ r.module }}</td>
              <td>
                <template v-if="r.config_missing"><n-tag size="small" type="error">配置已删</n-tag></template>
                <template v-else>{{ r.config_name }} · {{ r.config_model }}</template>
              </td>
              <td>
                <n-tag v-if="r.auto_fallback_at" size="small" type="warning">自动回退</n-tag>
                <n-tag v-else-if="r.enabled" size="small" type="success">生效中</n-tag>
                <n-tag v-else size="small">已停用</n-tag>
                <div v-if="r.auto_fallback_reason" style="font-size: 12px; opacity: 0.65; max-width: 320px">{{ r.auto_fallback_reason }}</div>
              </td>
              <td style="font-size: 12px; opacity: 0.8">{{ routeHealthText(r) }}</td>
              <td>
                <n-space size="small">
                  <n-button size="tiny" @click="editRoute(r)">编辑</n-button>
                  <n-button v-if="r.auto_fallback_at" size="tiny" type="warning" @click="recoverRoute(r)">恢复</n-button>
                  <n-popconfirm @positive-click="removeRoute(r)">
                    <template #trigger><n-button size="tiny" type="error">删除</n-button></template>
                    删除后该模块恢复默认配置链路？
                  </n-popconfirm>
                </n-space>
              </td>
            </tr>
          </tbody>
        </n-table>
        <n-empty v-else description="暂无路由：全部模块走默认配置链路" style="margin: 12px 0" />
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 120" style="max-width: 560px; margin-top: 12px" :show-feedback="false">
          <n-form-item label="模块">
            <n-select v-model:value="routeForm.module" :options="routeModuleOptions" placeholder="选择业务模块" filterable />
          </n-form-item>
          <n-form-item label="目标配置">
            <n-select v-model:value="routeForm.config_id" :options="routeConfigOptions" placeholder="选择本人的 LLM 配置" />
          </n-form-item>
          <n-form-item label="成本回退阈值">
            <n-input-number v-model:value="routeForm.max_cost_ratio" :min="0" :step="0.05" style="width: 160px" />
            <span style="font-size: 12px; opacity: 0.55; margin-left: 8px">路由目标平均 token / 其他配置平均 token 超过该比值自动回退；0 = 默认 1.35</span>
          </n-form-item>
          <n-form-item label="备注">
            <n-input v-model:value="routeForm.note" placeholder="为什么路由（可选）" />
          </n-form-item>
          <n-form-item label="启用">
            <n-switch v-model:value="routeForm.enabled" />
          </n-form-item>
          <n-button type="primary" :loading="routeSaving" @click="saveRoute">保存路由</n-button>
        </n-form>
      </SectionCard>

      <!-- GitHub OAuth -->
      <SectionCard title="GitHub 登录" :hoverable="false">
        <n-alert type="info" :show-icon="false" :bordered="false" class="note">
          在 GitHub OAuth App 中将「Authorization callback URL」设置为：<strong>{{ callbackHint }}</strong>
        </n-alert>
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 120" style="max-width: 560px">
          <n-form-item label="Client ID">
            <n-input v-model:value="gh.client_id" placeholder="GitHub OAuth App Client ID" />
          </n-form-item>
          <n-form-item label="Client Secret">
            <n-input
              v-model:value="gh.client_secret"
              type="password"
              show-password-on="click"
              :placeholder="settings?.has_github_secret ? '已配置，留空表示保留原值' : '请输入 Client Secret'"
            />
          </n-form-item>
          <n-form-item label="启用 GitHub 登录">
            <n-switch v-model:value="gh.enabled" />
          </n-form-item>
          <n-button type="primary" :loading="savingGithub" @click="saveGithub">保存 GitHub 设置</n-button>
        </n-form>
      </SectionCard>

      <!-- 站点地址（推送通知点击跳转） -->
      <SectionCard title="站点地址" :hoverable="false">
        <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 120" style="max-width: 560px" :show-feedback="false">
          <n-form-item label="站点基础 URL">
            <n-input v-model:value="siteBaseURL" placeholder="https://app.example.com（本站对外访问地址）" />
          </n-form-item>
          <n-form-item label=" ">
            <span style="font-size: 12px; opacity: 0.55">
              App 推送通知（ntfy 通道）的点击跳转链接 = 该地址 + 站内路由；留空则通知不带跳转链接。须为 http/https 完整地址，尾部斜杠自动去除。
            </span>
          </n-form-item>
          <n-button type="primary" :loading="savingSite" @click="saveSite">保存站点地址</n-button>
        </n-form>
      </SectionCard>

      <!-- 用户管理 -->
      <SectionCard title="用户管理" :hoverable="false">
        <n-table :bordered="false" :single-line="false">
          <thead>
            <tr>
              <th>ID</th>
              <th>用户名</th>
              <th>角色</th>
              <th>状态</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="u in users" :key="u.id">
              <td>{{ u.id }}</td>
              <td>{{ u.display_name || u.username }}</td>
              <td>
                <n-tag :type="u.role === 'admin' ? 'info' : 'default'" size="small" round>{{ u.role }}</n-tag>
              </td>
              <td>
                <n-tag :type="u.status === 'enabled' ? 'success' : 'error'" size="small" round>{{ u.status }}</n-tag>
              </td>
              <td>
                <n-space size="small">
                  <n-button size="tiny" quaternary @click="openQuota(u)">配额</n-button>
                  <n-popconfirm v-if="u.id !== auth.user?.id" @positive-click="toggleStatus(u)">
                    <template #trigger>
                      <n-button size="tiny" :type="u.status === 'enabled' ? 'error' : 'primary'">
                        {{ u.status === 'enabled' ? '禁用' : '启用' }}
                      </n-button>
                    </template>
                    {{ u.status === 'enabled' ? '禁用该用户并强制登出？' : '重新启用该用户？' }}
                  </n-popconfirm>
                  <span v-else style="opacity: 0.5">当前账号</span>
                </n-space>
              </td>
            </tr>
          </tbody>
        </n-table>
      </SectionCard>

      <!-- P1 数据健康总览：expected/observed 对账 + 补跑入口 -->
      <SectionCard title="数据健康" :hoverable="false">
        <template #extra>
          <n-button size="tiny" quaternary :loading="healthLoading" @click="loadHealth">刷新</n-button>
        </template>
        <div class="health-actions">
          <n-button size="tiny" tertiary :loading="rerunning === 'wide'" @click="rerun('wide', triggerWideSync)"
            >全市场增量同步</n-button
          >
          <n-button size="tiny" tertiary :loading="rerunning === 'init'" @click="rerun('init', triggerWideInit)"
            >历史初始化</n-button
          >
          <n-button size="tiny" tertiary :loading="rerunning === 'bars'" @click="rerun('bars', triggerSyncBars)"
            >日线批量同步</n-button
          >
          <n-button size="tiny" tertiary :loading="rerunning === 'snap'" @click="rerun('snap', triggerSnapshot)"
            >情绪快照</n-button
          >
          <n-button
            size="tiny"
            tertiary
            :loading="rerunning === 'factor'"
            @click="rerun('factor', triggerFactorRebuild)"
            >重建因子宽表</n-button
          >
          <n-button
            size="tiny"
            tertiary
            :loading="rerunning === 'cal'"
            @click="rerun('cal', triggerBackfillCalendar)"
            >回填日历</n-button
          >
        </div>
        <n-empty v-if="!health?.items?.length" description="暂无数据" size="small" style="padding: 20px 0" />
        <n-table v-else :bordered="false" :single-line="false" size="small">
          <thead>
            <tr>
              <th>数据域</th>
              <th>状态</th>
              <th>库内最新</th>
              <th>应有日期</th>
              <th>落后</th>
              <th>覆盖</th>
              <th>最近任务</th>
              <th>说明</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="it in health.items" :key="it.key">
              <td>{{ it.name }}</td>
              <td>
                <n-tag :type="healthStatusMeta[it.status]?.type || 'default'" size="small" round>{{
                  healthStatusMeta[it.status]?.label || it.status
                }}</n-tag>
              </td>
              <td class="qv-tnum">{{ it.observed_date || '—' }}</td>
              <td class="qv-tnum">{{ it.expected_date || '—' }}</td>
              <td class="qv-tnum">
                <template v-if="it.lag_open_days > 0">{{ it.lag_open_days }} 开市日</template>
                <template v-else-if="it.lag_open_days < 0">?</template>
                <template v-else>—</template>
              </td>
              <td>{{ it.coverage || '—' }}</td>
              <td class="log-time">
                <template v-if="it.last_run"
                  >{{ fmtLogTime(it.last_run.created_at) }}
                  <n-tag :type="logStatusType(it.last_run.status)" size="tiny" round>{{ it.last_run.status }}</n-tag>
                  <span v-if="it.last_run.status !== 'success'" class="log-msg">{{ it.last_run.message }}</span>
                </template>
                <template v-else>—</template>
              </td>
              <td class="log-msg">{{ it.note }}</td>
            </tr>
          </tbody>
        </n-table>
        <div style="font-size: 12px; opacity: 0.55; margin-top: 8px">
          生成于 {{ health?.generated_at || '—' }}。「落后」按交易日历对照应有日期计算（不与库内自身最大日期比较）；超过容忍值的域会以「落后」标红，请用上方补跑入口处理。
        </div>
      </SectionCard>

      <!-- 数据源同步日志 -->
      <SectionCard title="数据源同步日志" :hoverable="false">
        <template #extra>
          <n-button size="tiny" quaternary :loading="logsLoading" @click="loadLogs">刷新</n-button>
        </template>
        <n-empty v-if="!logs.length" description="暂无同步记录" size="small" style="padding: 20px 0" />
        <n-table v-else :bordered="false" :single-line="false" size="small">
          <thead>
            <tr>
              <th>时间</th>
              <th>任务</th>
              <th>状态</th>
              <th>成功/总数</th>
              <th>耗时</th>
              <th>摘要</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="lg in logs" :key="lg.id">
              <td class="log-time">{{ fmtLogTime(lg.created_at) }}</td>
              <td>{{ taskLabel[lg.task] || lg.task }}</td>
              <td>
                <n-tag :type="logStatusType(lg.status)" size="small" round>{{ lg.status }}</n-tag>
              </td>
              <td>{{ lg.succeeded }}/{{ lg.total }}<span v-if="lg.failed"> · 失败 {{ lg.failed }}</span></td>
              <td>{{ (lg.duration_ms / 1000).toFixed(1) }}s</td>
              <td class="log-msg">{{ lg.message }}</td>
            </tr>
          </tbody>
        </n-table>
      </SectionCard>
    </div>

    <!-- 用户配额编辑 -->
    <n-modal v-model:show="quotaModal" preset="card" :title="`AI 配额 · ${quotaUser?.display_name || quotaUser?.username || ''}`" style="max-width: 460px">
      <n-form :label-placement="isMobile ? 'top' : 'left'" :label-width="isMobile ? undefined : 110" :show-feedback="false">
        <n-form-item label="已用次数">
          <span class="qv-tnum">{{ quotaForm.action_used }}</span>
          <span style="opacity: 0.5; margin-left: 8px"
            >（累计 {{ quotaForm.token_used.toLocaleString() }} token / {{ quotaForm.request_count }} 轮调用）</span
          >
        </n-form-item>
        <n-form-item label="次数上限">
          <n-input-number v-model:value="quotaForm.action_limit" :min="0" :step="50" style="width: 100%" />
        </n-form-item>
        <n-form-item label=" ">
          <span style="font-size: 12px; opacity: 0.55"
            >0 表示不限；按用户手动发起的 AI 动作计次（分析/推荐/问答/点评各 1 次，内部多轮请求不重复计），用尽后熔断。</span
          >
        </n-form-item>
        <n-form-item label="清零已用量">
          <n-checkbox v-model:checked="quotaForm.reset_used">同时清零已用次数与 token 审计（周期性重置）</n-checkbox>
        </n-form-item>
      </n-form>
      <template #footer>
        <div style="display: flex; justify-content: flex-end; gap: 10px">
          <n-button @click="quotaModal = false">取消</n-button>
          <n-button type="primary" :loading="quotaSaving || quotaLoading" @click="saveQuota">保存</n-button>
        </div>
      </template>
    </n-modal>
  </PageContainer>
</template>

<style scoped>
.admin-stack {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.note {
  margin-bottom: 16px;
  border-radius: 10px;
}
.log-time {
  white-space: nowrap;
  font-size: 12px;
}
.log-msg {
  font-size: 12px;
  opacity: 0.75;
  max-width: 360px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.health-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-bottom: 12px;
}
</style>
