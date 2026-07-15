# QuantVista Android 壳（Capacitor）

Capacitor **远程加载模式**：APK 只是一层原生壳，WebView 直接加载线上 HTTPS 站点。
页面/后端由 Docker 统一发布，**改页面无需重发 APK**；只有本目录（原生层）变化才需要重新打包。
总体方案见 `docs/ANDROID_APP_PLAN.md`（§4 阶段 A）。

```text
mobile/
  capacitor.config.ts   # 包名 com.quantvista.app、server.url（正式域名）、errorPath
  www/                  # 占位 webDir：error.html（断网兜底页）+ index.html（占位，正常不渲染）
  android/              # 原生工程（入库；build 产物与签名材料除外）
  resources/            # 图标/启动图源图与生成说明
```

## ⚠️ 域名替换（首次构建前必做）

当前配置使用占位符 `https://app.example.com`，替换为你的线上站点（与 GitHub OAuth 回调同域）：

| 位置 | 内容 |
|---|---|
| `mobile/capacitor.config.ts` → `server.url` | 壳加载的线上站点，**唯一必改处** |

替换后执行 `npm run sync` 将配置同步进 android 工程再构建。
后续阶段会新增域名相关配置（阶段 C：`web/public/.well-known/assetlinks.json`、ntfy 子域），以 `docs/ANDROID_APP_PLAN.md` 各阶段说明为准。

## 前置条件

- Node ≥ 20（本仓库基线 v24）；
- JDK 21 + Android SDK（装 Android Studio 最省事；仅命令行构建也可，`android/local.properties` 写 `sdk.dir`）；
- 首次进入本目录先 `npm install`。

## 构建

```bash
cd mobile
npm install
npm run sync                 # cap sync android：拷贝 www/、同步插件与配置
cd android
./gradlew assembleDebug      # Windows: gradlew.bat assembleDebug
# 产物 android/app/build/outputs/apk/debug/app-debug.apk
./gradlew assembleRelease    # release 需先配置签名（见下节）
# 产物 android/app/build/outputs/apk/release/app-release.apk
```

也可 `npm run open` 用 Android Studio 打开工程构建。

## 签名（release）

首次生成 release keystore（一次性，有效期 100 年）：

```bash
keytool -genkeypair -v -keystore quantvista-release.keystore -alias quantvista \
  -keyalg RSA -keysize 2048 -validity 36500
```

1. **keystore 与口令存放在仓库外并做好备份**——丢失 = 已装设备永远无法覆盖升级，只能卸载重装；
2. 复制 `android/key.properties.example` 为 `android/key.properties`，填入 keystore 路径与口令
   （`key.properties`、`*.keystore` 均已 gitignore，绝不入库）；
3. `key.properties` 存在时 `assembleRelease` 自动签名；不存在时产出未签名 APK（不影响 debug 构建）。

阶段 C 的 App Links（assetlinks.json）需要此 keystore 的 SHA256 指纹：

```bash
keytool -list -v -keystore quantvista-release.keystore -alias quantvista | grep SHA256
```

## 发布纪律

- 每次发 APK：`android/app/build.gradle` 的 `versionCode` 单调 +1，`versionName` 语义化递增；
- 不上应用市场，APK 私有分发（服务器下载页/直接传输）；release 签名保证可覆盖升级。

### 版本耦合纪律（重要）

web 前端 bundle 里打包的 `@capacitor/*` JS 端与 APK 原生端**必须版本兼容**：

- `web/package.json` 与 `mobile/package.json` 的 `@capacitor/*` 版本保持一致（当前 8.x）；
- **升级任何 `@capacitor/*` npm 包 = 必须同步升级两处并重发 APK**；
- 发布顺序：**先装新 APK，再发 web**（旧 web + 新壳兼容窗口远好于反向）。

日常改页面（不动 `@capacitor/*` 版本）不受此限制，Docker 发布即生效。

## 与 web 侧的接线（随 Docker 发布，不在本目录）

- `web/src/config/runtime.ts`：`isNativeApp` 运行时判断（壳注入 `window.Capacitor`）；
- `web/src/lib/nativeShell.ts`：返回键（先关弹窗/抽屉 → 路由后退 → 最小化不退出）与
  `appUrlOpen` 深链路由跳转；由 `main.ts` 在 `isNativeApp` 时动态 import，浏览器端不加载；
- 硬约束：**所有 `@capacitor/*` 调用必须动态 import + `isNativeApp` 守卫**（web 包不增重）；
- 外部链接（新闻源等）由 Capacitor 默认行为交系统浏览器打开，无需 web 侧处理；
- 服务端 `server/router/web.go`：`index.html` no-cache + `/assets/*` immutable，
  保证 Docker 发布后壳内冷启动即见新版（壳内没有刷新按钮，全靠这条）。

## 调试

- **连本地后端**：debug 期临时改 `capacitor.config.ts`：

  ```ts
  server: {
    url: 'http://<局域网IP>:5173',   // vite dev server：cd web && npm run dev -- --host
    cleartext: true,                 // 仅 debug 构建允许 http；release 永远 HTTPS
  }
  ```

  改完 `npm run sync` 再装 debug 包；**调完改回，release 构建前必须核查此处**。
- **WebView 远程调试**：debug 构建默认开启，Chrome 打开 `chrome://inspect` 选中设备上的
  WebView 即可用 DevTools 调试线上页面。
- **断网兜底页**：`www/error.html`，加载失败时呈现，改动它属于原生层变化（需重发 APK）。

## 验收清单（阶段 A）

- [ ] APK 安装启动，加载线上站点，登录/行情/ECharts/AI 页/持仓/登出与浏览器行为一致；
- [ ] 返回键：弹窗 → 抽屉 → 路由后退 → 最小化的顺序正确；外部链接跳系统浏览器；
- [ ] 断网打开 App 显示本地错误页，恢复网络后点「重新加载」成功；
- [ ] Docker 发布一次前端改动，App 冷启动即见新版（缓存头生效验证）；
- [ ] 浏览器 Web 无回归（`npm run build` 零类型错误；Capacitor chunk 未进浏览器加载路径）。
