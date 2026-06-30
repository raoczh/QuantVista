<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { NCard, NSpin, NResult, NButton } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const error = ref('')

onMounted(async () => {
  const code = route.query.code as string
  const state = route.query.state as string
  if (!code || !state) {
    error.value = '回调参数缺失'
    return
  }
  try {
    await auth.finishGithubLogin(code, state)
    router.replace('/')
  } catch (e) {
    error.value = (e as Error).message
  }
})
</script>

<template>
  <div class="auth-wrap">
    <n-card style="max-width: 420px; width: 100%">
      <n-spin v-if="!error" description="正在完成 GitHub 登录 ..." style="width: 100%; padding: 24px 0" />
      <n-result v-else status="error" title="登录失败" :description="error">
        <template #footer>
          <n-button @click="router.replace('/login')">返回登录</n-button>
        </template>
      </n-result>
    </n-card>
  </div>
</template>

<style scoped>
.auth-wrap {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 100vh;
  padding: 24px;
}
</style>
