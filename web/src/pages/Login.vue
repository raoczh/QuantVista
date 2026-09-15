<script setup lang="ts">
import { onUnmounted, ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { NForm, NFormItem, NInput, NButton, NDivider, NIcon, useMessage } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'
import { isNativeApp } from '@/config/runtime'
import AuthShell from '@/components/AuthShell.vue'
import { safeInternalRoute } from '@/lib/internalRoute'
import { authErrorText } from '@/lib/authError'

const router = useRouter()
const route = useRoute()
const message = useMessage()
const auth = useAuthStore()

const username = ref('')
const password = ref('')
const loading = ref(false)
const githubLoading = ref(false)
let active = true
onUnmounted(() => { active = false })
const isCurrent = () => active && route.name === 'login'

function go() {
  const redirect = safeInternalRoute(route.query.redirect)
  router.replace(redirect)
}

async function submit() {
  if (loading.value || githubLoading.value || !isCurrent()) return
  if (!username.value.trim() || !password.value) return message.error('请输入用户名和密码')
  loading.value = true
  try {
    await auth.loginPassword(username.value.trim(), password.value, isCurrent)
    if (!isCurrent()) return
    message.success('登录成功')
    go()
  } catch (e) {
    if (isCurrent()) message.error(authErrorText(e, '登录失败，请检查账号和密码'))
  } finally {
    loading.value = false
  }
}

async function github() {
  if (loading.value || githubLoading.value || !isCurrent()) return
  githubLoading.value = true
  try {
    sessionStorage.setItem('qv_login_redirect', safeInternalRoute(route.query.redirect))
    // App 内走移动流（系统浏览器授权 + 深链回跳，阶段 B）；浏览器走原 Web 流。
    await (isNativeApp ? auth.startMobileGithubLogin(isCurrent) : auth.startGithubLogin(isCurrent))
  } catch (e) {
    if (isCurrent()) message.error(authErrorText(e, 'GitHub 登录暂不可用，请稍后重试'))
  } finally {
    githubLoading.value = false
  }
}
</script>

<template>
  <AuthShell subtitle="欢迎回来" description="登录以继续你的研究。">
    <n-form>
      <n-form-item label="用户名">
        <n-input v-model:value="username" placeholder="用户名" :input-props="{ 'aria-label': '用户名', autocomplete: 'username' }" />
      </n-form-item>
      <n-form-item label="密码">
        <n-input
          v-model:value="password"
          type="password"
          :input-props="{ 'aria-label': '密码', autocomplete: 'current-password' }"
          show-password-on="click"
          @keyup.enter="submit"
        />
      </n-form-item>
      <n-button type="primary" block :loading="loading" :disabled="githubLoading" @click="submit">登录</n-button>
    </n-form>

    <template v-if="auth.githubEnabled">
      <n-divider style="margin: 16px 0">或</n-divider>
      <n-button block :loading="githubLoading" :disabled="loading" @click="github">
        <template #icon>
          <n-icon>
            <svg viewBox="0 0 16 16" width="16" height="16" fill="currentColor">
              <path
                d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82.64-.18 1.32-.27 2-.27.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"
              />
            </svg>
          </n-icon>
        </template>
        使用 GitHub 登录
      </n-button>
    </template>
  </AuthShell>
</template>
