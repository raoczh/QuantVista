import type { CapacitorConfig } from '@capacitor/cli'

// QuantVista Android 壳：远程加载线上站点，页面/后端由 Docker 统一发布，
// 改页面无需重发 APK；APK 仅在原生层（本目录）变化时重发。
const config: CapacitorConfig = {
  appId: 'com.quantvista.app',
  appName: 'QuantVista',
  webDir: 'www',
  server: {
    // ⚠️ 正式域名占位符：替换为线上 HTTPS 站点（与 GitHub OAuth 回调同域），
    //    替换位置清单见 mobile/README.md「域名替换」节。
    url: 'https://app.example.com',
    // 远程站点加载失败（断网/服务不可达）时的本地兜底页，含重试按钮。
    errorPath: 'error.html',
  },
}

export default config
