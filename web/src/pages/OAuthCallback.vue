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
  if (!code || !state) {
    error.value = '回调参数缺失'
    return
  }
  // 同一回调页承接两种意图：设置页发起的「绑定」与登录页发起的「登录」。
  binding.value = auth.pendingGithubBind()
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
