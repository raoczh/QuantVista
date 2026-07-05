import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  getSetupStatus,
  createAdmin as apiCreateAdmin,
  loginByPassword,
  logout as apiLogout,
  getGithubAuthURL,
  githubCallback,
  bindGithub,
  unbindGithub,
  getSelf,
  type AuthUser,
  type TokenPair,
} from '@/api/auth'
import { getAccessToken, getRefreshToken, setTokens, clearTokens } from '@/api/token'

// 认证 store：登录态、首启状态、各登录方式。
export const useAuthStore = defineStore('auth', () => {
  const user = ref<AuthUser | null>(null)
  const initialized = ref(true)
  const githubEnabled = ref(false)
  const registrationOpen = ref(false)
  const statusLoaded = ref(false)

  const isLoggedIn = computed(() => !!user.value)
  const isAdmin = computed(() => user.value?.role === 'admin')

  // 前端回调页地址，授权与换 token 两端必须一致。
  function redirectURI() {
    return `${location.origin}/login/callback`
  }

  function applyPair(pair: TokenPair) {
    setTokens(pair.access_token, pair.refresh_token)
    user.value = pair.user
  }

  async function fetchSetupStatus() {
    const s = await getSetupStatus()
    initialized.value = s.initialized
    githubEnabled.value = s.github_oauth_enabled
    registrationOpen.value = s.registration_open
    statusLoaded.value = true
    return s
  }

  // 有 token 但无 user（如刷新页面）时拉取自身信息恢复登录态。
  async function loadSelf() {
    if (!getAccessToken()) return
    try {
      user.value = await getSelf()
    } catch {
      clearTokens()
      user.value = null
    }
  }

  async function createAdmin(username: string, password: string) {
    const pair = await apiCreateAdmin(username, password)
    applyPair(pair)
    initialized.value = true
  }

  async function loginPassword(username: string, password: string) {
    applyPair(await loginByPassword(username, password))
  }

  async function startGithubLogin() {
    // 清除历史绑定流程残留的意图标记（如在 GitHub 授权页取消后返回），
    // 避免本次登录在回调页被误判为绑定而打 authed 接口 401 弹回登录页。
    sessionStorage.removeItem(BIND_FLAG)
    const { url } = await getGithubAuthURL(redirectURI())
    location.href = url
  }

  async function finishGithubLogin(code: string, state: string) {
    applyPair(await githubCallback(code, state, redirectURI()))
  }

  // GitHub 绑定：与登录共用回调页 /login/callback，用 sessionStorage 标记区分意图。
  const BIND_FLAG = 'qv_oauth_bind'

  async function startGithubBind() {
    const { url } = await getGithubAuthURL(redirectURI())
    sessionStorage.setItem(BIND_FLAG, '1')
    location.href = url
  }

  function pendingGithubBind() {
    return sessionStorage.getItem(BIND_FLAG) === '1'
  }

  // 清除绑定意图标记。回调页在标记不成立（未登录）或 GitHub 授权失败时调用，
  // 防止残留标记把后续的「登录」误判成「绑定」。
  function clearGithubBindFlag() {
    sessionStorage.removeItem(BIND_FLAG)
  }

  async function finishGithubBind(code: string, state: string) {
    sessionStorage.removeItem(BIND_FLAG)
    user.value = await bindGithub(code, state, redirectURI())
  }

  async function removeGithubBind() {
    user.value = await unbindGithub()
  }

  async function logout() {
    try {
      await apiLogout(getRefreshToken())
    } catch {
      /* 忽略：本地清票即可 */
    }
    clearTokens()
    sessionStorage.removeItem(BIND_FLAG) // 退出后不存在合法的进行中绑定流程
    user.value = null
  }

  return {
    user,
    initialized,
    githubEnabled,
    registrationOpen,
    statusLoaded,
    isLoggedIn,
    isAdmin,
    fetchSetupStatus,
    loadSelf,
    createAdmin,
    loginPassword,
    startGithubLogin,
    finishGithubLogin,
    startGithubBind,
    pendingGithubBind,
    clearGithubBindFlag,
    finishGithubBind,
    removeGithubBind,
    logout,
  }
})
