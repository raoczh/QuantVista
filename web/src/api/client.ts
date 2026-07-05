import axios, { type AxiosInstance } from 'axios'
import { getAccessToken, getRefreshToken, setTokens, clearTokens } from './token'

// 后端统一响应包络：{ success, message, data }
export interface ApiEnvelope<T> {
  success: boolean
  message: string
  data: T
}

const http: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 20000,
})

// AI 类接口（分析/推荐/问答/对比点评）服务端合法耗时可达数分钟（LLM 单次 90s、
// 校验失败最多重试 2 次）；全局 20s 会把仍在执行并扣配额的任务在前端掐断。
export const AI_TIMEOUT = 300000

// 请求拦截：自动附带 access token。
http.interceptors.request.use((config) => {
  const token = getAccessToken()
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// 单飞刷新：并发 401 只触发一次 refresh。
let refreshing: Promise<boolean> | null = null

async function tryRefresh(): Promise<boolean> {
  const refresh = getRefreshToken()
  if (!refresh) return false
  try {
    const resp = await axios.post('/api/auth/refresh', { refresh_token: refresh })
    const body = resp.data as ApiEnvelope<{ access_token: string; refresh_token: string }>
    if (body?.success) {
      setTokens(body.data.access_token, body.data.refresh_token)
      return true
    }
  } catch {
    /* 落到下方返回 false */
  }
  return false
}

// 响应拦截：401 时尝试刷新一次并重放原请求；刷新失败则清票并跳登录。
http.interceptors.response.use(
  (resp) => resp,
  async (error) => {
    const original = error.config
    const status = error.response?.status
    if (status === 401 && original && !original._retried) {
      original._retried = true
      if (!refreshing) {
        refreshing = tryRefresh().finally(() => {
          refreshing = null
        })
      }
      const ok = await refreshing
      if (ok) {
        original.headers = original.headers || {}
        original.headers.Authorization = `Bearer ${getAccessToken()}`
        return http.request(original)
      }
      clearTokens()
      // /login 前缀（含 /login/callback）豁免整页跳转：登录页自身无需跳；回调页
      // 正在用 code 换令牌，整页跳转会取消飞行中的 OAuth 请求，导致 GitHub 登录失败。
      if (!location.pathname.startsWith('/login')) {
        // 带上当前位置，登录后由路由守卫送回原页面。
        location.href = '/login?redirect=' + encodeURIComponent(location.pathname + location.search)
      }
    }
    return Promise.reject(error)
  },
)

// 统一拆包：success=false 时抛出带 message 的错误，组件只处理 data。
export async function request<T>(config: Parameters<AxiosInstance['request']>[0]): Promise<T> {
  let resp
  try {
    resp = await http.request<ApiEnvelope<T>>(config)
  } catch (e) {
    if (axios.isAxiosError(e) && e.code === 'ECONNABORTED') {
      throw new Error('请求超时：任务可能仍在后台执行，请稍后刷新查看结果')
    }
    throw e
  }
  const body = resp.data
  if (!body || body.success !== true) {
    throw new Error(body?.message || '请求失败')
  }
  return body.data
}

export default http
