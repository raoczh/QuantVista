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
| `mobile/capacitor.config.ts` → `server.url` | 壳加载的线上站点 |
| `mobile/android/app/src/main/AndroidManifest.xml` → https intent-filter 的 `android:host` | App Links 通知点击直达（阶段 C），与 `server.url` 同域 |

替换后执行 `npm run sync` 将配置同步进 android 工程再构建。
ntfy 推送另需服务端部署（`docs/DEPLOYMENT.md` §8）与下文「App Links 指纹」「手机端 ntfy 配置」两步。

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

## App Links 指纹（阶段 C，通知点击直达）

ntfy 通知点击的是 `https://<域名>/<路由>` 链接；Android 校验通过 App Links 才会直达壳 App，否则降级浏览器打开。两处配置：

1. **站点声明**：`web/public/.well-known/assetlinks.json` 里的
   `REPLACE_WITH_RELEASE_KEYSTORE_SHA256_FINGERPRINT` 替换为上节 `keytool -list -v` 输出的
   SHA256 指纹（形如 `AA:BB:...` 冒号分隔大写十六进制，保留冒号）。改完随 web 正常构建发布
   （vite 原样拷贝，Go embed 托管，`https://<域名>/.well-known/assetlinks.json` 可访问即生效）；
2. **壳声明**：`AndroidManifest.xml` 的 https intent-filter（`android:autoVerify`）的 host
   已随「域名替换」改为正式域名。

验证：装 release 包后 `adb shell pm get-app-links com.quantvista.app` 应显示 domain `verified`；
或系统设置 → 应用 → QuantVista → 默认打开。**debug 包签名与 release 不同，App Links 验证不过是预期行为**——
要么临时把 debug 指纹也加进 assetlinks.json 数组，要么用 release 包验收。

## 手机端 ntfy 配置（推送接收，一次性）

系统级推送由伴生 **ntfy App** 承担（服务端部署见 `docs/DEPLOYMENT.md` §8；为什么不用 FCM 见
`docs/ANDROID_APP_PLAN.md` §2.3）。手机上装两个 App：QuantVista 壳 + ntfy 接收器。

1. **安装 ntfy Android App**：[GitHub Releases APK](https://github.com/binwiederhier/ntfy-android/releases)
   或 F-Droid 版（纯 WebSocket 长连接 instant delivery，不依赖 GMS；**不要装 Play 版**——对无 GMS 场景无意义）；
2. **添加自建服务器**：ntfy App → 设置 → Default server 填 `https://ntfy.<域名>`；
   Manage users 添加用户 `quantvista` + 密码（服务端 `ntfy user add` 设置的那个）；
3. **订阅 topic**：Subscribe to topic，填你在 QuantVista「条件提醒 → 推送通道」里配置的 topic
   （如 `qv-u1`，须 `qv-` 前缀，服务端仅授权该前缀）；
4. **开启 instant delivery**：订阅详情 → Instant delivery 打开（常驻前台服务，锁屏/杀 App 均可达）；
5. **国产 ROM 保活（必做，否则长连接被杀）**：
   - 电池优化：给 ntfy App 设「无限制/不优化」（MIUI：省电策略→无限制；ColorOS/OriginOS 类似）；
   - 自启动/后台运行权限：允许；
   - 最近任务里锁定 ntfy（部分 ROM 上划清理会杀前台服务）；
6. **验收**：QuantVista 推送通道点「测试」，前台/后台/锁屏/杀掉 QuantVista 壳四种状态均应收到；
   点击通知直达壳内对应页面（需 App Links 验证通过 + 管理后台已配「站点地址」）。

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
