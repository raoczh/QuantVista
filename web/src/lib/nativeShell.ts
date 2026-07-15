import type { Router } from 'vue-router'

// Android 壳（Capacitor）原生事件接线：仅在 isNativeApp 时由 main.ts 动态 import 本模块，
// 浏览器端不会加载（@capacitor/* 相关 chunk 均不进浏览器加载路径）。
//
// 版本耦合纪律：本文件 import 的 @capacitor/* JS 端与 APK 原生端必须兼容——
// 升级 web/package.json 里任何 @capacitor/* = 必须同步升级 mobile/ 并重发 APK，
// 且先装新 APK 再发 web（见 mobile/README.md「版本耦合纪律」）。

// naive-ui 弹层遮罩（弹窗/抽屉/图片预览）。返回键先关最上层弹层，与 Android 用户直觉一致。
const overlayMaskSelector = '.n-modal-mask, .n-drawer-mask, .n-image-preview-container'

function closeTopOverlay(): boolean {
  if (!document.querySelector(overlayMaskSelector)) return false
  // naive-ui 弹层默认 close-on-esc：派发合成 Escape 让其自行关闭一层。
  // 个别 :close-on-esc="false" 的弹窗返回键关不掉，用页面上的关闭按钮即可。
  document.dispatchEvent(
    new KeyboardEvent('keydown', { key: 'Escape', code: 'Escape', bubbles: true }),
  )
  return true
}

export async function setupNativeShell(router: Router): Promise<void> {
  const { App } = await import('@capacitor/app')

  // Android 返回键：弹层 → 路由后退 → 最小化（不退出，保留 WebView 状态下次秒开）。
  await App.addListener('backButton', ({ canGoBack }) => {
    if (closeTopOverlay()) return
    if (canGoBack) {
      window.history.back()
      return
    }
    void App.minimizeApp()
  })

  // 深链进入：https App Links（阶段 C 通知点击）直达站内路由。
  // quantvista:// OAuth 回跳（阶段 B）届时在此扩展分支处理。
  await App.addListener('appUrlOpen', ({ url }) => {
    try {
      const target = new URL(url)
      if (target.protocol === 'https:') {
        void router.push(target.pathname + target.search + target.hash)
      }
    } catch {
      /* 非法深链忽略 */
    }
  })
}
