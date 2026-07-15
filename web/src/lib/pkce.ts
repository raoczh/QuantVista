// PKCE（RFC 7636）工具：纯 WebCrypto，无 Capacitor 依赖。
// crypto.subtle 仅安全上下文可用（HTTPS / localhost）——App WebView 加载正式
// HTTPS 域名满足；局域网 http 调试下移动 OAuth 本就不可用（GitHub 回调域不符）。

function base64url(bytes: Uint8Array): string {
  let bin = ''
  for (const b of bytes) bin += String.fromCharCode(b)
  return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '')
}

// 生成 code_verifier：32 字节熵 → base64url 定长 43 字符（RFC 下限）。
export function generateCodeVerifier(): string {
  const bytes = new Uint8Array(32)
  crypto.getRandomValues(bytes)
  return base64url(bytes)
}

// S256 方法：code_challenge = base64url(SHA256(verifier))，无填充。
export async function codeChallengeS256(verifier: string): Promise<string> {
  if (!crypto.subtle) throw new Error('当前环境不支持安全加密上下文（需 HTTPS）')
  const digest = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(verifier))
  return base64url(new Uint8Array(digest))
}
