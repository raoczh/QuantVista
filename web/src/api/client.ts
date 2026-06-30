import axios, { type AxiosInstance } from 'axios'

// 后端统一响应包络：{ success, message, data }
export interface ApiEnvelope<T> {
  success: boolean
  message: string
  data: T
}

const http: AxiosInstance = axios.create({
  baseURL: '/api',
  timeout: 15000,
})

// 统一拆包：success=false 时抛出带 message 的错误，组件只处理 data。
export async function request<T>(config: Parameters<AxiosInstance['request']>[0]): Promise<T> {
  const resp = await http.request<ApiEnvelope<T>>(config)
  const body = resp.data
  if (!body || body.success !== true) {
    throw new Error(body?.message || '请求失败')
  }
  return body.data
}

export default http
