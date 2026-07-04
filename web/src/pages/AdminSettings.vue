<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import {
  NSpace,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSwitch,
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
  type SystemSettings,
  type SyncLog,
} from '@/api/admin'
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
  } catch (e) {
    message.error((e as Error).message)
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

onMounted(() => {
  load()
  loadUsers()
  loadLogs()
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
</style>
