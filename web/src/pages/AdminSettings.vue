<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import {
  NCard,
  NSpace,
  NForm,
  NFormItem,
  NInput,
  NSwitch,
  NButton,
  NTable,
  NTag,
  NAlert,
  NPopconfirm,
  useMessage,
} from 'naive-ui'
import {
  getSystemSettings,
  updateSystemSettings,
  listUsers,
  setUserStatus,
  type SystemSettings,
} from '@/api/admin'
import type { AuthUser } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'

const message = useMessage()
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

onMounted(() => {
  load()
  loadUsers()
})
</script>

<template>
  <n-space vertical :size="16">
    <!-- 注册开关 -->
    <n-card title="注册策略">
      <n-space align="center">
        <span>开放 GitHub 注册：</span>
        <n-switch
          :value="settings?.registration_open ?? false"
          :loading="savingReg"
          @update:value="toggleRegistration"
        />
        <span style="opacity: 0.7">关闭时，仅已存在的账号可登录，新 GitHub 用户无法注册。</span>
      </n-space>
    </n-card>

    <!-- GitHub OAuth -->
    <n-card title="GitHub 登录">
      <n-alert type="info" :show-icon="false" style="margin-bottom: 16px">
        在 GitHub OAuth App 中将「Authorization callback URL」设置为：<strong>{{ callbackHint }}</strong>
      </n-alert>
      <n-form label-placement="left" label-width="120" style="max-width: 560px">
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
    </n-card>

    <!-- 用户管理 -->
    <n-card title="用户管理">
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
              <n-popconfirm v-if="u.id !== auth.user?.id" @positive-click="toggleStatus(u)">
                <template #trigger>
                  <n-button size="tiny" :type="u.status === 'enabled' ? 'error' : 'primary'">
                    {{ u.status === 'enabled' ? '禁用' : '启用' }}
                  </n-button>
                </template>
                {{ u.status === 'enabled' ? '禁用该用户并强制登出？' : '重新启用该用户？' }}
              </n-popconfirm>
              <span v-else style="opacity: 0.5">当前账号</span>
            </td>
          </tr>
        </tbody>
      </n-table>
    </n-card>
  </n-space>
</template>
