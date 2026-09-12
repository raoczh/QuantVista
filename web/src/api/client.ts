import axios, { type AxiosInstance, type InternalAxiosRequestConfig } from 'axios'
import { getAccessToken, getRefreshToken, rotateTokens, clearTokens, getSessionEpoch } from './token'

// 后端统一响应包络：{ success, message, data }
export interface ApiEnvelope<T> {
  success: boolean
  message: string
  data: T
  code?: string
}

export class ApiRequestError extends Error {
  readonly code?: string
  readonly status?: number

  constructor(message: string, code?: string, status?: number) {
    super(message)
    this.name = 'ApiRequestError'
    this.code = code || undefined
    this.status = status
  }
}

export function getApiErrorCode(error: unknown): string | undefined {
  if (error instanceof ApiRequestError) return error.code
  if (error && typeof error === 'object') {
    const value = error as { code?: unknown; refusalCode?: unknown }
    if (typeof value.refusalCode === 'string' && value.refusalCode) return value.refusalCode
    if (typeof value.code === 'string' && value.code) return value.code
  }
  return undefined
}

/**
 * 请求是否因 AbortController 主动取消而失败（切标的/组件卸载时的正常路径）。
 * 取消不是故障：页面据此跳过错误提示与状态回填，避免用户看到「加载失败」的假报错。
 */
export function isAbortError(error: unknown): boolean {
  if (axios.isCancel(error)) return true
  if (error instanceof DOMException && error.name === 'AbortError') return true
  const code = getApiErrorCode(error)
  return code === 'ERR_CANCELED'
}

const http: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 20000,
})

// AI 类接口（分析/推荐/问答/对比点评）服务端合法耗时可达数分钟（LLM 单次 90s、
// 校验失败最多重试 2 次）；全局 20s 会把仍在执行并扣配额的任务在前端掐断。
export const AI_TIMEOUT = 300000

// 全市场重算类接口（walk-forward / 因子 IC / 召回评估）合法耗时数十秒，
// 远超默认 20s 但不涉及 LLM，用独立的中等超时。
export const HEAVY_TIMEOUT = 120000

// 请求拦截：自动附带 access token。
type SessionRequestConfig = InternalAxiosRequestConfig & { _retried?: boolean; _sessionEpoch?: number }
const sessionChangedError = () => new ApiRequestError('登录状态已变化，请重新执行操作', 'auth_session_changed')

http.interceptors.request.use((config) => {
  const original = config as SessionRequestConfig
  if (original._sessionEpoch !== undefined && original._sessionEpoch !== getSessionEpoch()) throw sessionChangedError()
  original._sessionEpoch = getSessionEpoch()
  const token = getAccessToken()
  if (token) {
    config.headers.Authorization = `Bearer ${token}`
  }
  return config
})

// 单飞刷新：并发 401 只触发一次 refresh。
let refreshing: { epoch: number; promise: Promise<boolean> } | null = null
const REFRESH_TIMEOUT = 20000

function waitForPeerRefresh(epoch: number, refresh: string): Promise<boolean> {
  if (typeof window === 'undefined') return Promise.resolve(false)
  // 不支持 Web Locks 的旧浏览器/HTTP 页面仍可能同时消费旧令牌。
  // 401 可能先于成功标签的响应到达；给另一页完整的请求超时窗口发布新凭证。
  return new Promise((resolve) => {
    const finish = (ok: boolean) => {
      clearTimeout(timer)
      window.removeEventListener('storage', check)
      resolve(ok)
    }
    const check = () => {
      if (epoch !== getSessionEpoch()) finish(false)
      else if (refresh !== getRefreshToken()) finish(!!(getAccessToken() && getRefreshToken()))
    }
    const timer = setTimeout(() => finish(false), REFRESH_TIMEOUT)
    window.addEventListener('storage', check)
    check()
  })
}

async function performRefresh(epoch: number, refresh: string, crossTabLocked: boolean): Promise<boolean> {
  if (epoch !== getSessionEpoch()) return false
  if (refresh !== getRefreshToken()) return !!(getAccessToken() && getRefreshToken())
  try {
    const resp = await axios.post('/api/auth/refresh', { refresh_token: refresh }, { timeout: REFRESH_TIMEOUT })
    if (epoch !== getSessionEpoch()) return false
    // 另一标签已完成本会话的刷新；迟到响应不能覆盖它的新凭证。
    if (refresh !== getRefreshToken()) return !!(getAccessToken() && getRefreshToken())
    const body = resp.data as ApiEnvelope<{ access_token: string; refresh_token: string }>
    if (body?.success) {
      return rotateTokens(refresh, body.data.access_token, body.data.refresh_token)
    }
  } catch (error) {
    if (epoch !== getSessionEpoch()) return false
    // 同一刷新令牌只能使用一次。另一标签抢先轮换后的 401 不代表会话失效。
    if (refresh !== getRefreshToken()) return !!(getAccessToken() && getRefreshToken())
    if (axios.isAxiosError(error) && error.response?.status === 401) {
      return crossTabLocked ? false : waitForPeerRefresh(epoch, refresh)
    }
    // 网络、限流、数据库等暂时故障保留凭证，由调用方显示错误并允许重试。
    throw error
  }
  return false
}

async function tryRefresh(epoch: number): Promise<boolean> {
  const refresh = getRefreshToken()
  if (!refresh) return false
  if (typeof navigator !== 'undefined' && navigator.locks) {
    // 锁覆盖请求和新凭证发布。等待者拿锁后先检查共享令牌，复用已完成的轮换。
    return navigator.locks.request('qv-auth-refresh', () => performRefresh(epoch, refresh, true))
  }
  return performRefresh(epoch, refresh, false)
}

// refreshAccessToken 供 axios 之外的调用方（如流式 fetch）复用同一单飞刷新。
export function refreshAccessToken(): Promise<boolean> {
  const epoch = getSessionEpoch()
  if (refreshing?.epoch === epoch) return refreshing.promise
  const promise = tryRefresh(epoch).finally(() => {
    if (refreshing?.promise === promise) refreshing = null
  })
  refreshing = { epoch, promise }
  return promise
}

// 响应拦截：401 时尝试刷新一次并重放原请求；刷新失败则清票并跳登录。
http.interceptors.response.use(
  (resp) => {
    if ((resp.config as SessionRequestConfig)._sessionEpoch !== getSessionEpoch()) throw sessionChangedError()
    return resp
  },
  async (error) => {
    const original = error.config as SessionRequestConfig | undefined
    const status = error.response?.status
    if (original?._sessionEpoch !== undefined && original._sessionEpoch !== getSessionEpoch()) throw sessionChangedError()
    if (status === 401 && original && !original._retried) {
      original._retried = true
      const currentToken = getAccessToken()
      if (currentToken && original.headers.Authorization !== `Bearer ${currentToken}`) {
        return http.request(original)
      }
      const ok = await refreshAccessToken()
      if (original._sessionEpoch !== getSessionEpoch()) throw sessionChangedError()
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
  // 在调用时固定会话，axios 的请求拦截器会异步执行，不能到那里才选择身份。
  const sessionConfig = { ...config, _sessionEpoch: getSessionEpoch() }
  let resp
  try {
    resp = await http.request<ApiEnvelope<T>>(sessionConfig)
  } catch (e) {
    if (axios.isAxiosError(e)) {
      if (e.code === 'ECONNABORTED') {
        throw new ApiRequestError('请求超时：任务可能仍在后台执行，请稍后刷新查看结果', 'request_timeout')
      }
      // 后端统一包络在 4xx/5xx 时仍带 message；优先透传真实原因（如 LLM 回退被关、
      // 配额不足等），否则保留原始 axios 错误供上层判断 status/code。
      const backend = e.response?.data as ApiEnvelope<unknown> | undefined
      const backendMsg = backend?.message
      if (backendMsg) {
        throw new ApiRequestError(backendMsg, backend?.code, e.response?.status)
      }
    }
    throw e
  }
  const body = resp.data
  if (!body || body.success !== true) {
    throw new ApiRequestError(body?.message || '请求失败', body?.code, resp.status)
  }
  return body.data
}

export default http
