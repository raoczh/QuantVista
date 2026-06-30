import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import {
  getSetupStatus,
  createAdmin as apiCreateAdmin,
  loginByPassword,
  logout as apiLogout,
  getGithubAuthURL,
  githubCallback,
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
    const { url } = await getGithubAuthURL(redirectURI())
    location.href = url
  }

  async function finishGithubLogin(code: string, state: string) {
    applyPair(await githubCallback(code, state, redirectURI()))
  }

  async function logout() {
    try {
      await apiLogout(getRefreshToken())
    } catch {
      /* 忽略：本地清票即可 */
    }
    clearTokens()
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
    logout,
  }
})
