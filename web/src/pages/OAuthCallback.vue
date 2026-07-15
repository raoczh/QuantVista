<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { NSpin, NResult, NButton } from 'naive-ui'
import { useAuthStore } from '@/stores/auth'
import { githubMobileCallback } from '@/api/auth'
import { isNativeApp } from '@/config/runtime'
import AuthShell from '@/components/AuthShell.vue'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const error = ref('')
const binding = ref(false)
// 移动流（阶段 B）三态：'' 非移动流 / 'browser' 系统浏览器侧换短码并深链回 App
// / 'app' App WebView 侧（深链经 nativeShell 翻译进本页）兑换 token。
const mobile = ref<'' | 'browser' | 'app'>('')
// 深链地址：换短码成功后自动跳转，并常驻兜底按钮（部分浏览器拦自动跳转）。
const deepLink = ref('')

// GitHub 侧未完成授权（用户取消等）会带 error/error_description 回跳。
function githubSideError(): string {
  const ghErr = (route.query.error_description || route.query.error) as string | undefined
  return ghErr ? `GitHub 授权未完成：${ghErr}` : '回调参数缺失'
}

// mode=mobile：本页运行在系统浏览器（无原生桥、无登录态）。用 code+state 换
// 一次性短码，经 quantvista:// 深链带回 App——token 绝不落在系统浏览器。
async function runMobileBrowserLeg() {
  const code = route.query.code as string
  const state = route.query.state as string
  if (!code || !state) {
    error.value = githubSideError()
    return
  }
  try {
    // redirect_uri 与发起端（auth store mobileRedirectURI）字节一致。
    const { auth_code } = await githubMobileCallback(code, state, `${location.origin}/login/callback?mode=mobile`)
    deepLink.value = `quantvista://oauth/callback?code=${encodeURIComponent(auth_code)}`
    location.href = deepLink.value
  } catch (e) {
    error.value = (e as Error).message
  }
}

// mode=mobile-exchange：本页运行在 App WebView（深链翻译进来），凭短码+verifier
// 兑换双 token。失败短码即作废，只能回登录页整个重来。
async function runMobileAppLeg() {
  if (isNativeApp) {
    // 收掉授权用的 Custom Tab（还停留在回调页），失败不阻塞兑换。
    import('@capacitor/browser').then(({ Browser }) => Browser.close()).catch(() => {})
  }
  const authCode = route.query.code as string
  if (!authCode) {
    error.value = '深链参数缺失，请重新发起 GitHub 登录'
    return
  }
  try {
    await auth.finishMobileExchange(authCode)
    router.replace('/')
  } catch (e) {
    error.value = (e as Error).message
  }
}

onMounted(run)

// 深链可能在本页已挂载时再次进入（如上次失败停在错误页，用户重新授权后
// nativeShell 翻译的路由与当前同路径不同 query，组件复用、onMounted 不重跑）。
watch(
  () => route.fullPath,
  () => {
    if (route.name !== 'oauth-callback') return
    error.value = ''
    deepLink.value = ''
    mobile.value = ''
    void run()
  },
)

async function run() {
  if (route.query.mode === 'mobile') {
    mobile.value = 'browser'
    await runMobileBrowserLeg()
    return
  }
  if (route.query.mode === 'mobile-exchange') {
    mobile.value = 'app'
    await runMobileAppLeg()
    return
  }
  const code = route.query.code as string
  const state = route.query.state as string
  // 同一回调页承接两种意图：设置页发起的「绑定」与登录页发起的「登录」。
  // 绑定意图仅在已登录时成立（守卫已在导航前恢复登录态）；未登录时的
  // 残留标记一律按登录处理并清除，防止误打 authed 绑定接口 401 弹回登录页。
  binding.value = auth.pendingGithubBind() && auth.isLoggedIn
  if (!binding.value) auth.clearGithubBindFlag()
  if (!code || !state) {
    auth.clearGithubBindFlag()
    error.value = githubSideError()
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
}
</script>

<template>
  <AuthShell>
    <!-- 移动流·系统浏览器侧：换短码成功即深链回 App，常驻兜底按钮 -->
    <template v-if="mobile === 'browser' && !error">
      <n-spin v-if="!deepLink" description="正在完成 GitHub 登录 ..." style="width: 100%; padding: 24px 0" />
      <n-result v-else status="success" title="授权成功" description="正在返回 QuantVista App ...">
        <template #footer>
          <n-button tag="a" :href="deepLink" type="primary">未自动跳转？点此返回 App</n-button>
        </template>
      </n-result>
    </template>
    <!-- 移动流·系统浏览器侧失败：重新发起须回 App，本页不提供入口 -->
    <n-result
      v-else-if="mobile === 'browser'"
      status="error"
      title="登录失败"
      :description="error"
    >
      <template #footer>
        <span style="font-size: 13px; opacity: 0.7">请回到 QuantVista App 重新发起 GitHub 登录</span>
      </template>
    </n-result>
    <!-- 非移动流原样 + 移动流·App 侧（错误按钮回 App 内登录页，语义成立） -->
    <template v-else>
      <n-spin v-if="!error" :description="binding ? '正在完成 GitHub 绑定 ...' : '正在完成 GitHub 登录 ...'" style="width: 100%; padding: 24px 0" />
      <n-result v-else status="error" :title="binding ? '绑定失败' : '登录失败'" :description="error">
        <template #footer>
          <n-button @click="router.replace(binding ? '/settings' : '/login')">{{ binding ? '返回设置' : '返回登录' }}</n-button>
        </template>
      </n-result>
    </template>
  </AuthShell>
</template>
