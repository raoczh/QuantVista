<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { NForm, NFormItem, NInput, NButton, NAlert, useMessage } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'
import AuthShell from '@/components/AuthShell.vue'

const router = useRouter()
const message = useMessage()
const auth = useAuthStore()

const username = ref('')
const password = ref('')
const confirm = ref('')
const loading = ref(false)

async function submit() {
  if (username.value.trim().length < 3) return message.error('用户名至少 3 个字符')
  if (password.value.length < 8) return message.error('密码至少 8 个字符')
  if (password.value !== confirm.value) return message.error('两次输入的密码不一致')
  loading.value = true
  try {
    await auth.createAdmin(username.value.trim(), password.value)
    message.success('管理员账号已创建，欢迎使用 QuantVista')
    router.replace('/')
  } catch (e) {
    message.error((e as Error).message)
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <AuthShell subtitle="首次启动 · 初始化系统">
    <h2 class="auth-title">创建管理员</h2>
    <n-alert type="info" :show-icon="false" :bordered="false" class="setup-note">
      这是系统首次启动。第一个账号即系统拥有者，后续可在后台开启 GitHub 登录与注册。
    </n-alert>
    <n-form>
      <n-form-item label="管理员用户名">
        <n-input v-model:value="username" placeholder="至少 3 个字符" />
      </n-form-item>
      <n-form-item label="密码">
        <n-input
          v-model:value="password"
          type="password"
          show-password-on="click"
          placeholder="至少 8 个字符"
        />
      </n-form-item>
      <n-form-item label="确认密码">
        <n-input
          v-model:value="confirm"
          type="password"
          show-password-on="click"
          @keyup.enter="submit"
        />
      </n-form-item>
      <n-button type="primary" block :loading="loading" @click="submit">创建并进入</n-button>
    </n-form>
  </AuthShell>
</template>

<style scoped>
.auth-title {
  margin: 0 0 12px;
  font-size: 20px;
  font-weight: 700;
}
.setup-note {
  margin-bottom: 16px;
  border-radius: 10px;
}
</style>
