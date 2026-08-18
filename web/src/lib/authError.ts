/** 认证页只展示用户可执行的错误，不把服务端堆栈、SQL 或内部配置回显到页面。 */
export function authErrorText(error: unknown, fallback = '认证失败，请稍后重试') {
  const message = error instanceof Error ? error.message.trim() : ''
  if (!message || message.length > 240 || /\n|\r|\bat\s+\S+\s*\(|panic|stack trace|sql|token|secret|password/i.test(message)) {
    return fallback
  }
  return message
}
