<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { NSpin, NResult, NButton } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'
import AuthShell from '@/components/AuthShell.vue'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const error = ref('')
const binding = ref(false)

onMounted(async () => {
  const code = route.query.code as string
  const state = route.query.state as string
  // 同一回调页承接两种意图：设置页发起的「绑定」与登录页发起的「登录」。
  // 绑定意图仅在已登录时成立（守卫已在导航前恢复登录态）；未登录时的
  // 残留标记一律按登录处理并清除，防止误打 authed 绑定接口 401 弹回登录页。
  binding.value = auth.pendingGithubBind() && auth.isLoggedIn
  if (!binding.value) auth.clearGithubBindFlag()
  if (!code || !state) {
    // GitHub 侧未完成授权（用户取消等）会带 error/error_description 回跳。
    const ghErr = (route.query.error_description || route.query.error) as string | undefined
    auth.clearGithubBindFlag()
    error.value = ghErr ? `GitHub 授权未完成：${ghErr}` : '回调参数缺失'
    return
  }
  try {
    if (binding.value) {
      await auth.finishGithubBind(code, state)
      router.replace('/settings?tab=account')
    } else {
      await auth.finishGithubLogin(code, state)
      router.replace('/')
    }
  } catch (e) {
    error.value = (e as Error).message
  }
})
</script>

<template>
  <AuthShell>
    <n-spin v-if="!error" :description="binding ? '正在完成 GitHub 绑定 ...' : '正在完成 GitHub 登录 ...'" style="width: 100%; padding: 24px 0" />
    <n-result v-else status="error" :title="binding ? '绑定失败' : '登录失败'" :description="error">
      <template #footer>
        <n-button @click="router.replace(binding ? '/settings' : '/login')">{{ binding ? '返回设置' : '返回登录' }}</n-button>
      </template>
    </n-result>
  </AuthShell>
</template>
