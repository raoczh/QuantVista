<script setup lang="ts">
import { ref } from 'vue'
import { useRouter } from 'vue-router'
import { NCard, NForm, NFormItem, NInput, NButton, NAlert, useMessage } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'

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
  <div class="auth-wrap">
    <n-card title="初始化 QuantVista" style="max-width: 420px; width: 100%">
      <n-alert type="info" :show-icon="false" style="margin-bottom: 16px">
        这是系统首次启动。请创建管理员账号——第一个账号即系统拥有者，后续可在后台开启 GitHub 登录与注册。
      </n-alert>
      <n-form>
        <n-form-item label="管理员用户名">
          <n-input v-model:value="username" placeholder="至少 3 个字符" />
        </n-form-item>
        <n-form-item label="密码">
          <n-input v-model:value="password" type="password" show-password-on="click" placeholder="至少 8 个字符" />
        </n-form-item>
        <n-form-item label="确认密码">
          <n-input v-model:value="confirm" type="password" show-password-on="click" @keyup.enter="submit" />
        </n-form-item>
        <n-button type="primary" block :loading="loading" @click="submit">创建并进入</n-button>
      </n-form>
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
