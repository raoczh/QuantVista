# QuantVista Android 壳应用实施方案（v2，2026-07-15）

> v2 变更：推送主路从 FCM 改为**自建 ntfy**（大陆网络下 FCM 不可靠，见 §2.3 决策记录）；
> 移动 OAuth 修正为**复用现有回调页**（GitHub 单 OAuth App 限制 + 现有 cookie double-submit 机制与移动流冲突）；
> 新增**阶段 D：自选/持仓异动守护推送**；全文补齐现状代码锚点，各阶段可独立开会话执行。

## 1. 目标与边界

### 1.1 目标

- 保留现有 Web，浏览器访问方式不变；不做 Flutter/RN 重写，App 页面复用现有 Vue 页面和 Go API。
- Capacitor Android 壳加载线上 HTTPS Web；页面/后端仍由 Docker 统一发布，改页面无需重发 APK。
- GitHub 系统浏览器授权登录，回跳 App。
- **系统级推送必达**（锁屏/后台/App 被杀均可收到）——这是套壳的首要目的。
- 推送逻辑增强：自选与持仓（建仓）数据的异动主动推送。
- 所有业务页面逐页完成手机布局与触摸交互验收。

### 1.2 暂不做

- 不上应用市场（Google Play / 国内市场均不上）。
- 不做离线行情、离线 AI、后台持续运行（推送接收由伴生 ntfy App 承担，见 §6）。
- 不接入 iOS / APNs。
- 不接入 FCM 与华为/小米/OPPO/vivo 厂商推送（决策见 §2.3，保留升级路径）。
- App 内不做 GitHub 绑定流（仅登录流；绑定提示去电脑浏览器操作，见 §5.6）。

## 2. 现状盘点（新会话开工前必读）

### 2.1 部署链路（已就绪，无需动）

```text
手机/浏览器 ── HTTPS ──> Cloudflare（橙云代理） ──> 东京 VPS 宝塔 nginx 反代 ──> Docker quantvista:3002→3000
```

- HTTPS 域名已可用；GitHub OAuth App 的 Authorization callback URL 已指向 `https://<域名>/login/callback`。
- Cloudflare 免费版支持 WebSocket 代理，空闲超时约 100s——ntfy 默认 45s keepalive 可安全穿过（§6.2）。
- 编排：`deploy/docker-compose.yml`（gitignore，样例 `deploy/docker-compose.example.yml`），外部网络 `baota_net`，同宿主机已有 new-api（占 3001）。
- 镜像构建与推送由用户自理（`编译推送步骤.md`，**该文件勿动勿提交**）。

### 2.2 后端/前端现状锚点

| 领域 | 现状 | 位置 |
|---|---|---|
| 推送通道体系 | `NotifyService.Send(userID,title,content)` 扇出到用户全部启用通道；已有 kind：`serverchan`/`webhook`；target 加密落库 | `server/service/notify.go`、`server/model/notify.go` |
| 推送总闸 | `UserPreference.EnableNotify` + `HasEnabledChannel` 前置判断 | `server/service/notify.go:74-80,191-195` |
| 现有推送触发点（3 处） | 条件提醒命中、财报提醒、收盘日报生成 | `server/service/alert.go:597,758`、`server/service/dailyreport.go:360-361` |
| 提醒评估调度 | 15min 一轮全量评估（启动延迟 60s）；财报类由财报 job 每日一评 | `server/service/alert.go:604-634` |
| GitHub OAuth | `GET /api/oauth/github/url?redirect_uri=` 返回授权地址并种 HttpOnly cookie（double-submit）；回调页 POST `/api/oauth/github` {code,state,redirect_uri} 换 JWT 双 token | `server/controller/auth.go:107-150`、`server/service/auth.go:127-138` |
| state 机制 | `common.SignState()` 无状态 HMAC 签名（nonce.ts.mac，TTL 10min），**防重放靠 cookie 一次性**——移动流 cookie 跨浏览器不共享，必须换服务端一次性存储（§5.3） | `server/common/state.go` |
| OAuth 凭证 | DB 系统设置（管理后台配置，env 仅首启种子） | `server/setting/`、`deploy/.env.example:47-52` |
| 前端回调页 | `/login/callback` 单页承接登录+绑定两种意图（sessionStorage 标记区分） | `web/src/pages/OAuthCallback.vue`、`web/src/stores/auth.ts` |
| token 存储 | localStorage 双 token，axios 拦截器自动刷新 | `web/src/api/token.ts`、`web/src/api/client.ts` |
| 静态托管 | Go embed `web/dist`，SPA fallback；**未设任何 Cache-Control**（embed FS 无 modtime，连 Last-Modified 都没有） | `server/main.go:19`、`server/router/web.go` |
| 持仓模型 | 已有 `PlanStopLoss`/`PlanTakeProfit` 计划止损止盈字段（建仓时填写，当前仅页面展示用） | `server/model/portfolio.go:84-85` |
| 自选模型 | 已有 `IsPinned` 重点关注、`ResearchStage` 机会池漏斗阶段（`waiting_price`/`planned` 等） | `server/model/portfolio.go:42-44` |
| 用户偏好风格 | 布尔开关列 + JSON 文本配置列（`BlacklistJSON`/`RecFiltersJSON` 先例） | `server/model/user.go:34-62` |
| 移动端适配基础 | **已有一轮全站 768px 适配**：AppShell 抽屉导航、宽表横滚、`useIsMobile`、弹窗 max-width、ECharts `confine:true`+resize | ARCHITECTURE §4.2/§4.3；防回归：**页面必须单根**（多根组件路由离开白屏） |
| Redis | 可选依赖（失败仅告警），生产 compose 内置必有 | `server/main.go:44-47` |
| 前端栈 | Vue 3.5 + vite 5 + naive-ui + pinia，Node v24 可本地构建 | `web/package.json` |

### 2.3 决策记录：推送为什么是自建 ntfy

**需求**：系统级推送必达；手机 VPN 不常开；免费或少量付费；单人/极少数用户自部署场景。

| 方案 | 结论 | 原因 |
|---|---|---|
| FCM | ❌ 不采用 | 大陆网络下手机端到 Google 的推送长连接必须常态代理，与"VPN 不常开"冲突；东京服务器发送侧没问题，但收不到等于没做 |
| 厂商推送聚合（极光/个推） | ⏸ 备选二期 | 真系统级通道，但要原生 SDK 接入 + 逐厂商开放平台注册（OPPO/vivo 需企业认证）+ 隐私合规初始化时机，对单人自用过重 |
| 壳内自造前台服务长连接 | ❌ 不采用 | Doze/厂商杀后台/断网重连退避全是深坑，自造大概率"能收但不稳" |
| **自建 ntfy + 官方 ntfy App 接收** | ✅ **主路** | 开源免费全自控；东京小鸡加一个 Docker 容器，走自有 CF 域名，大陆直连可达；ntfy App 的 instant delivery 前台服务模式专为无 GMS 场景打磨多年；后端只是 `NotifyChannel` 新增一个 kind，**天然复用总闸/扇出/触发点，架构侵入最小** |

代价与说明：手机需装两个 App（QuantVista 壳 + ntfy 接收器）；通知由 ntfy App 弹出，点击经 App Links 直达 QuantVista 壳内对应页面（§6.4）。若日后 ntfy 保活体验不满意，再升级厂商通道（NotifyService 多通道架构不变，只加 kind）。

## 3. 总体架构

```text
浏览器 ─────────────────┐
                         │
Android 壳(Capacitor) ───┼── HTTPS/CF ──> 宝塔 nginx ──> quantvista 容器 (Go API + embed Web)
   │                     │                    │              │
   │  quantvista:// 深链  │                    ├──> ntfy 容器（自建推送服务）
   │  App Links(https)   │                    │        ▲ POST 发布
ntfy App(接收器) ── WebSocket 长连接(经 CF) ───┘        │
                                              NotifyService（serverchan/webhook/ntfy 三通道扇出）
                                                       ▲
                                     条件提醒 / 财报提醒 / 收盘日报 / 守护推送(新)
```

- 壳加载线上 Web（`server.url`），Docker 发布页面后 App 打开即新版；APK 仅在原生层变化时重发。
- WebView 内只允许本站域名导航，其余 host 自动交系统浏览器（Capacitor `allowNavigation` 默认行为）。

## 4. 阶段 A：Capacitor 壳跑通（预计 1～2 天）

### 4.1 工程创建

新增独立 `mobile/` 目录（勿混入 `web/`）：

```text
mobile/
  capacitor.config.ts     # 包名 com.quantvista.app、server.url、errorPath
  package.json            # @capacitor/core cli android app browser preferences
  www/                    # 占位目录：仅 error.html（远程模式必需 webDir 存在）
  android/                # npx cap add android 生成
  resources/              # 图标、启动图
  README.md               # 构建、签名、安装说明
```

命令序列（Node v24 已满足 Capacitor 7 要求）：

```bash
cd mobile && npm init -y
npm i @capacitor/core @capacitor/cli @capacitor/android @capacitor/app @capacitor/browser @capacitor/preferences
npx cap init QuantVista com.quantvista.app --web-dir=www
npx cap add android
```

### 4.2 壳配置要点

```ts
// capacitor.config.ts
const config: CapacitorConfig = {
  appId: 'com.quantvista.app',
  appName: 'QuantVista',
  webDir: 'www',
  server: {
    url: 'https://<正式域名>',
    errorPath: 'error.html',        // 网络不可用时的本地兜底页（含重试按钮 location.reload）
  },
}
```

- **targetSdk 钉在 34**：不上架无下限压力；targetSdk 35 强制 edge-to-edge，而 Android WebView 里 `env(safe-area-inset-*)` 默认全 0，会引入一整块无收益的适配面。
- `windowSoftInputMode=adjustResize`（AndroidManifest），防软键盘遮挡输入框。
- 深链 intent-filter：`quantvista://` 自定义 scheme（OAuth 回跳用，阶段 B）+ `https://<域名>` App Links（通知点击用，阶段 C，`android:autoVerify="true"`）。
- 状态栏颜色跟主题；返回键：`@capacitor/app` 的 `backButton` 事件——先关弹窗/抽屉/键盘，`canGoBack` 则 `history.back()`，到根则 `App.minimizeApp()`（不退出）。

### 4.3 前端接线（web/ 侧，随 Docker 发布）

```text
web/src/config/runtime.ts        # 新增：isNativeApp = Boolean(window.Capacitor)
web/src/components/AppShell.vue  # 壳内隐藏不适用元素（如"下载 App"入口，若有）
web/src/main.ts 或 AppShell      # 壳内注册 backButton / appUrlOpen 监听（动态 import @capacitor/app，浏览器不加载）
```

- **所有 `@capacitor/*` 调用必须动态 import + `isNativeApp` 守卫**：浏览器端不加载、web 包不增重。
- **版本耦合纪律（写进 mobile/README.md）**：web bundle 里打包的 `@capacitor/*` JS 端与 APK 原生端必须兼容——**升级任何 `@capacitor/*` npm 包 = 必须同步发新 APK，且先装 APK 再发 web**。

### 4.4 顺带加固：静态资源缓存头（保证"发布即生效"）

`server/router/web.go` 当前零缓存头。改造 NoRoute：

- `index.html` 响应加 `Cache-Control: no-cache`（每次协商，保证发布即生效——App 里没有"刷新"按钮，全靠这条）；
- `/assets/` 前缀（vite 带 hash 产物）加 `Cache-Control: public, max-age=31536000, immutable`（弱网 App 体验直接受益）。

### 4.5 release 签名（首次就做，不留到后面）

```bash
keytool -genkeypair -v -keystore quantvista-release.keystore -alias quantvista -keyalg RSA -keysize 2048 -validity 36500
```

- keystore 与口令**存放在仓库外**（丢失 = 已装设备永远无法覆盖升级）；`mobile/android/` 里只留 `key.properties` 模板（gitignore 真实文件）。
- 后续 App Links 的 assetlinks.json 需要此 keystore 的 SHA256 指纹（`keytool -list -v`），阶段 C 用。
- versionCode 单调递增、versionName 语义化，每次发 APK +1。

### 4.6 开发调试

- 调试本地后端：debug 构建临时改 `server.url` 为 `http://<局域网IP>:5173`（vite dev server，`npm run dev -- --host`）并开 `server.cleartext: true`——**仅 debug 构建，release 永远 HTTPS**。
- WebView 远程调试：debug 构建默认可用 `chrome://inspect`。

### 4.7 验收清单

- [ ] APK 安装启动，加载线上站点，登录/行情/ECharts/AI 页/持仓/登出与浏览器行为一致。
- [ ] 返回键：弹窗→抽屉→路由→最小化的顺序正确；外部链接（新闻源等）跳系统浏览器。
- [ ] 断网打开 App 显示本地错误页，恢复网络后重试成功。
- [ ] Docker 发布一次前端改动，App 冷启动即见新版（缓存头生效验证）。
- [ ] 浏览器 Web 无回归（`npm run build` 零类型错误；Capacitor 代码未进浏览器加载路径）。

## 5. 阶段 B：移动 GitHub OAuth（预计 1～2 天）

### 5.1 硬约束（为什么不能照抄常见方案）

1. **GitHub 只注册了一个 OAuth App**，其 Authorization callback URL = `https://<域名>/login/callback`；GitHub 校验 redirect_uri 必须与之同 host 且 path 为其子路径 → **不能新增 `/api/...` 后端回调地址**，必须复用该前端回调页。redirect_uri 允许追加 query 参数（GitHub 会原样保留并附加 code/state）。
2. 现有 state 防重放靠 **HttpOnly cookie double-submit**（`auth.go:112-121`）：cookie 种在发起方。移动流发起在 App WebView、回调落在系统浏览器，**cookie 不共享，该机制对移动流必然失败** → 移动流 state 改服务端一次性存储。

### 5.2 目标流程

```text
App(WebView) 点 GitHub 登录
  │ 生成 code_verifier(随机 43+ 字符, 存 @capacitor/preferences)
  │ challenge = base64url(SHA256(verifier))
  ├─ GET /api/oauth/github/url?redirect_uri=https://<域名>/login/callback?mode=mobile
  │       &mode=mobile&code_challenge=<challenge>
  │   后端：SignState() 照旧 + Redis 存一次性记录 state_nonce → {challenge}（TTL 10min，不种 cookie）
  ├─ @capacitor/browser 打开授权 URL（系统浏览器，不内嵌）
系统浏览器
  ├─ GitHub 授权 → 302 https://<域名>/login/callback?mode=mobile&code=..&state=..
  └─ OAuthCallback.vue 检测 query.mode==='mobile' 走移动分支：
        POST /api/oauth/github/mobile-callback {code, state, redirect_uri}
          后端：VerifyState + 消费一次性记录（原子删除，防重放）
                → ExchangeToken → GetUser → 查/建用户（复用 LoginByGitHub 用户段逻辑，抽公共函数）
                → 生成一次性短码 auth_code（60s TTL、单次、绑定 challenge，存 Redis）
          返回 {auth_code}
        页面 location.href = 'quantvista://oauth/callback?code=<auth_code>'
        （页面留"未自动跳转？点此返回 App"按钮 + 授权失败时的可重试错误态）
App 收到 appUrlOpen
  └─ POST /api/oauth/github/mobile-exchange {auth_code, code_verifier}
        后端：短码取记录并删除 → 校验 SHA256(verifier)==challenge → 签发现有 JWT 双 token
     WebView 内 setTokens()（沿用 web/src/api/token.ts 的 localStorage）→ 跳首页
```

### 5.3 后端改动

```text
server/controller/auth.go   # GitHubURL 加 mode/code_challenge 参数分支；新增 MobileCallback/MobileExchange
server/service/auth.go      # LoginByGitHub 拆出"GitHub 用户→本地用户"公共段；新增移动流两方法
server/common/state.go      # 不动（SignState/VerifyState 复用）
server/router/api.go        # 两条新路由（匿名区，与现有 /oauth/github 并列，挂限流 middleware.RateLimit）
```

一次性存储：优先 Redis（`SETEX` + `GETDEL` 原子消费）；Redis 不可用时退进程内 `sync.Map`+TTL（单实例部署成立，注释写明前提）。

### 5.4 安全要求

- state：HMAC 签名 + 10min 时效（现有）+ 服务端一次性消费（新增）。
- 短码：60s TTL、单次消费、**必须绑定 PKCE challenge**——`quantvista://` scheme 可被恶意 App 抢注，verifier 校验保证截获短码也换不到 token。
- access/refresh token、GitHub secret 绝不进 URL/深链/日志；mobile-exchange 失败不泄露短码是否存在（统一错误文案）。
- 授权取消/超时/重复回调：回调页均显示可重试错误，不留半程状态。

### 5.5 前端改动

```text
web/src/pages/OAuthCallback.vue  # 新增 mobile 分支（换短码→深链跳转→兜底按钮）
web/src/stores/auth.ts           # 新增 startMobileGithubLogin（PKCE 生成）/ finishMobileExchange
web/src/pages/Login.vue          # isNativeApp 时 GitHub 按钮走移动流
```

### 5.6 边界

- App 内**隐藏设置页"GitHub 绑定"入口**（`isNativeApp` 条件渲染 + 提示"请在电脑浏览器操作"）——绑定流需要已登录态跨浏览器传递，第一版不做。
- 密码登录在 App 内不受影响（同 WebView 同源，原样可用）。

### 5.7 验收清单

- [ ] App 内 GitHub 授权：系统浏览器完成 → 自动回 App → 登录态就绪；Web 浏览器原 OAuth 流无回归。
- [ ] 授权取消/超时/短码过期/短码重放/verifier 错误：均给出明确可重试错误。
- [ ] 抓包检查：token 不出现在任何 URL；短码只能成功兑换一次。
- [ ] App 被系统回收后重开：refresh token 续期正常；退出登录后深链重放无效。

## 6. 阶段 C：ntfy 推送通道（预计 2～3 天）

### 6.1 服务端部署（东京小鸡）

`deploy/docker-compose.yml` 加一个服务（样例同步进 `docker-compose.example.yml`）：

```yaml
  quantvista-ntfy:
    image: binwiederhier/ntfy
    container_name: quantvista-ntfy
    restart: always
    command: serve
    environment:
      NTFY_BASE_URL: https://ntfy.<域名>
      NTFY_BEHIND_PROXY: "true"
      NTFY_AUTH_FILE: /var/lib/ntfy/auth.db
      NTFY_AUTH_DEFAULT_ACCESS: deny-all        # 私有实例：默认全拒，显式授权
      NTFY_CACHE_FILE: /var/lib/ntfy/cache.db
      NTFY_ATTACHMENT_CACHE_DIR: /var/lib/ntfy/attachments
      TZ: Asia/Shanghai
    ports:
      - "3003:80"        # 避开 3001(new-api)/3002(quantvista)
    volumes:
      - ${APP_PATH:-/www/wwwroot/quantvista}/ntfy:/var/lib/ntfy
    networks:
      - baota_net
```

初始化（容器内执行一次）：

```bash
docker exec -it quantvista-ntfy ntfy user add --role=user quantvista   # 交互设密码
docker exec -it quantvista-ntfy ntfy access quantvista "qv-*" rw       # 仅 qv- 前缀 topic
docker exec -it quantvista-ntfy ntfy token add quantvista              # 得 tk_xxx，后端发布用
```

宝塔/CF 接线：

- 宝塔新建站点 `ntfy.<域名>` 反代 `http://127.0.0.1:3003`，**必须带 WebSocket 支持**（`Upgrade`/`Connection` 头），`proxy_read_timeout` ≥ 120s（ntfy 服务端默认 45s keepalive，足够刷新空闲计时）。
- CF 加 `ntfy` 子域 A 记录（橙云代理即可，免费版 WebSocket 可用；国内直连 CF 可达是本方案成立的关键）。

### 6.2 手机端（一次性配置，写进 mobile/README.md 使用说明）

- 安装 ntfy Android App：**GitHub Releases APK 或 F-Droid 版**（纯长连接 instant delivery，不依赖 GMS；Play 版对自建服务器同样走长连接，但装 Play 版的前提本身不成立）。
- App 内添加自建服务器 `https://ntfy.<域名>` + quantvista 用户凭证，订阅个人 topic（如 `qv-u1`）。
- **国产 ROM 必做**：给 ntfy App 设电池"无限制"+ 允许自启动/后台运行；ntfy 设置里开 instant delivery（常驻前台服务）。

### 6.3 后端：ntfy 作为 NotifyChannel 第三 kind（复用全部现有体系）

这是本方案架构收益最大的一步：**总闸（`EnableNotify`）、`HasEnabledChannel`、三个现有触发点全部零改动生效**。

```text
server/model/notify.go    # 加 NotifyKindNtfy = "ntfy"；target 明文格式定义为 JSON：
                          #   {"url":"https://ntfy.<域名>","topic":"qv-u1","token":"tk_xxx"}（整串加密落库，512 够用）
server/service/notify.go  # validate 加 ntfy 分支（解析 JSON、校验 https + topic 非空）
                          # sendTo 加 ntfy 分支：POST {url}/ JSON {topic,title,message,click,priority,tags}
                          #   Authorization: Bearer <token>；走 SafeHTTPClient（target 是公网 CF 域名，不触发内网拦截）
web/src/pages/Settings.vue# 推送通道表单 kind 选项加"ntfy（App 推送）"，target 三字段拼 JSON 提交
```

### 6.4 点击跳转：NotifyMessage 扩展 + App Links

`Send(title, content)` 纯文本签名承载不了跳转路由，扩展为：

```go
type NotifyMessage struct {
    Title, Content string
    Route          string // 站内路由（/stock/600519、/alerts、/daily-reports）；ntfy 通道拼 click=<SiteBaseURL>+Route
    Kind           string // alert / earn / report / guard —— 映射 ntfy tags
    Priority       int    // 0=default；止损触达等给 4(high)
}
func (s *NotifyService) SendMsg(userID int64, msg NotifyMessage)   // 新主入口
func (s *NotifyService) Send(userID int64, title, content string)  // 保留为薄包装
```

- 系统设置加 `SiteBaseURL`（管理后台配置，风格对齐 GitHub OAuth 凭证项）；为空则 ntfy 消息不带 click。
- 现有三触发点迁移 `SendMsg` 并补 Route：提醒命中→`/alerts`（单标的命中可 `/stock/<symbol>`）、财报提醒→`/alerts`、收盘日报→`/daily-reports`。
- **App Links**：新建 `web/public/.well-known/assetlinks.json`（vite 原样拷进 dist，Go embed 托管即可服务；main.go 已是 `all:web/dist` 会包含点开头目录）：

```json
[{
  "relation": ["delegate_permission/common.handle_all_urls"],
  "target": {
    "namespace": "android_app",
    "package_name": "com.quantvista.app",
    "sha256_cert_fingerprints": ["<release keystore SHA256，阶段 A keytool -list -v 获取>"]
  }
}]
```

壳 App manifest 对 `https://<域名>` 开 `autoVerify` intent-filter；`appUrlOpen` 事件里 `router.push(path)`。点击 ntfy 通知的 https 链接 → 已装壳则直入对应页面，未装/验证失败则开浏览器（优雅降级，不阻塞验收）。

### 6.5 验收清单

- [ ] `POST /api/notify-channels/:id/test`（现有测试接口）对 ntfy 通道可达：前台、后台、锁屏、**杀掉壳 App 后**均收到。
- [ ] 点击通知直达壳 App 对应页面；未装壳的设备点击开浏览器同路由。
- [ ] 提醒命中/财报提醒/收盘日报三个既有触发点经 ntfy 可收到且带正确跳转。
- [ ] Server酱/Webhook 老通道行为无回归（`go test ./...`，notify_test.go 扩 ntfy 用例）。
- [ ] ntfy 匿名访问被拒（deny-all 生效）；token 不出现在日志。
- [ ] 长连接稳定性：手机息屏 2 小时后发测试推送仍可达（CF/宝塔超时与 keepalive 配置正确的证明）。

## 7. 阶段 D：自选/持仓异动守护推送（预计 2～3 天）

> 目标：不依赖用户手工配条件提醒，系统自动盯"已建仓的"和"重点关注的"。
> 与现有条件提醒的边界：**目标价类诉求仍走 AlertRule**（用户显式配置）；守护只管两类系统性事件——止损止盈触达、异常波动。

### 7.1 触发规则

| 对象 | 规则 | 默认 | 推送样例 |
|---|---|---|---|
| 持仓（holding） | 现价 ≤ `PlanStopLoss`（>0 时） | 开 | ⚠️ 贵州茅台 触及计划止损 1580（现价 1576.00），Route=/positions |
| 持仓（holding） | 现价 ≥ `PlanTakeProfit`（>0 时） | 开 | 🎯 触及计划止盈，同上 |
| 持仓（holding） | 当日涨跌幅 `|pct| ≥ pos_pct` | 开，±5% | 📉 持仓异动：XX 当日 -5.2%，Route=/stock/<symbol> |
| 自选（守护范围内） | 当日涨跌幅 `|pct| ≥ watch_pct` 或涨停/跌停 | 开，±7% | 📈 自选异动/涨停，Route=/stock/<symbol> |

- 守护范围（自选侧）：`IsPinned=true` **或** `ResearchStage ∈ {waiting_price, planned}` 的条目——普通自选不推，防推送疲劳。
- 止损/止盈价与现价比较用当日 low/high 兜底（对齐 alert.go 价格类"盘中触达不漏判"的既有口径，见 `evaluateAlert` price 分支）。

### 7.2 实现设计

```text
server/model/guard.go       # GuardEvent{UserID,Symbol,Market,Kind,TradeDate,Price,Message}
                            #   唯一索引 (user_id,symbol,kind,trade_date) —— 同日同标的同类事件只推一次
                            #   注册进 model.Migrate()
server/service/guard.go     # StartGuardJobs(mgr)：交易时段内（周一~五 09:30-15:05，时段判断参考
                            #   StartIntradayJobs 现有实现）每 15min 一轮，节奏对齐 alert job：
                            #   列举 guard 开启用户 → 收集 holding 持仓 + 守护范围自选 →
                            #   批量行情（复用自选/持仓页的批量 quote 路径，注意数据源限流：单轮单用户一次批量拉取）→
                            #   评估 → INSERT GuardEvent（冲突跳过）→ 新事件 SendMsg（含 Route/Kind=guard，止损类 Priority=4）
server/model/user.go        # UserPreference 加 GuardConfigJSON（沿用 JSON 配置列先例）：
                            #   {"enabled":true,"pos_pct":5,"watch_pct":7,"stop_loss":true,"take_profit":true}
                            #   空串 = 默认全开（服务层给默认值，风格对齐 RecFiltersJSON）
server/controller/          # GET/PUT /api/preferences 现有接口自然带上新字段；GuardEvent 查询接口
                            #   GET /api/guard-events?date= 供页面展示（可并入今日待办数据源，二期）
web/src/pages/Settings.vue  # 偏好区加"智能守护"开关+两个阈值+止损止盈子开关
main.go                     # service.StartGuardJobs(mgr) 注册（与 StartAlertJobs 并列）
```

- 推送前置与所有通道一致：`EnableNotify` 总闸 + `HasEnabledChannel`（ntfy 通道天然计入）。
- 财报类提醒的教训（`alert.go:44-46`）：守护 job 只做行情评估，绝不在循环里做逐 symbol 的重接口调用。

### 7.3 验收清单

- [ ] 建一条带止损价的持仓，模拟触达（阈值设到必命中）：15min 内收到 ntfy 推送，点击直达；同日重复评估不再推。
- [ ] IsPinned 自选异动推送可达；普通自选不推；关闭 GuardConfig 后全部不推。
- [ ] 非交易时段 job 不发起行情请求（日志验证）；单轮行情请求次数 = 用户数量级而非标的数量级（批量接口）。
- [ ] `go test ./...` 通过：guard 评估纯函数单测（止损/止盈/异动/去重各至少 1 例）。

## 8. 阶段 E：全页面移动验收（预计 3～5 天）

> **不是从零适配**：2026-07-04 已完成全站 768px 一轮适配（现状见 §2.2 表末行）。本阶段是壳内逐页验收补漏。
> 改页面必须遵守既有硬约束：**页面单根**（多根组件路由离开白屏）、断点统一 768px（`useIsMobile`）、
> 禁硬编码前景/背景色、复用 PageContainer/SectionCard 体系——权威约定 ARCHITECTURE §4.1/§4.2/§4.3。

### 8.1 一级重点页面（逐页 360px/390px 双宽度过）

市场首页、个股详情、今日待办、自选、持仓、推荐追踪、AI 分析、个股问答、收盘日报、条件提醒。

重点：首屏信息层级、按钮 ≥44px、图表可读性、横向表格、输入框与软键盘（adjustResize 生效后复验）、下拉刷新缺失的替代（关键页提供显式刷新按钮）、加载失败重试。

### 8.2 二级页面

新闻、板块热力图、选股、回测、横向对比、投资逻辑卡、投资笔记、模拟交易、ETF、设置与提示词模板。

### 8.3 管理页面（保可用即可）

管理设置、LLM 调用记录、数据源状态——表格横滚，不为手机重做交互。

### 8.4 壳内专属检查项

- Android 返回键在每个带弹窗/抽屉页面的行为（阶段 A 全局逻辑的逐页验证）。
- AI 流式/异步任务轮询在切后台→恢复前台的正确性（生成链路已异步任务化，恢复时轮询应自动续上）。
- 亮/暗主题、横竖屏、长列表滚动性能、弱网与接口超时表现。
- ECharts：tooltip `confine:true` 已全站有，验证壳内触摸交互与页面隐藏/恢复 resize。

## 9. 发布流程与测试矩阵

### 9.1 发布

- Web/API：现有流程不变（`npm run build` → Go embed → docker buildx → push → 服务器换镜像；用户自理）。
- APK：仅原生变化时重发（keystore 签名 → versionCode+1 → 私有分发/服务器下载页）；**升级 @capacitor/* npm 包必须同步发 APK（§4.3 纪律）**。
- 可选（低成本，建议第一版顺手做）：`GET /api/app-config` 返回 `{min_version, latest_version, apk_url, notes}`（系统设置表存值），壳启动时比对提示手动更新。

### 9.2 测试矩阵

| 维度 | 覆盖 |
|---|---|
| 设备 | Android 12/14 各一（真机或模拟器）；**无 GMS 国产 ROM 真机必测**（ntfy 保活主战场） |
| 登录 | 密码登录、刷新续期、登出、GitHub 授权成功/拒绝/取消/超时、系统回收后重开、深链重放 |
| 推送 | 前台/后台/锁屏/杀壳 App；多设备同时订阅；通道禁用与总闸关闭；点击跳转（装壳/未装壳）；ntfy 断线重连（飞行模式 10min 恢复） |
| 发布 | Docker 发页面后 App 冷启动见新版；后端升级不破坏旧 APK；OAuth/推送全链路走 CF 正常 |

## 10. 完成标准

- 浏览器 Web 功能零回归；`npm run build` 与 `go build ./... && go test ./...` 干净。
- APK 安装启动，仅加载正式 HTTPS 域名，release 签名可覆盖升级。
- GitHub 授权从系统浏览器自动回 App，token/短码/secret 不出现在 URL 与日志。
- ntfy 推送在锁屏且壳 App 被杀时可达，点击直达对应页面；三个既有触发点 + 守护推送全部走通。
- 守护推送：止损止盈/持仓异动/重点自选异动按配置触发、同日去重、可一键关闭。
- 核心页面 360px 宽度可用；页面改动全部遵守单根/768px/主题约束。
- Docker 发布页面改动无需重发 APK 即生效。

## 11. 分阶段开工提示词（新会话直接粘贴）

> 通用约束已写入各提示词；每阶段独立可开工，但顺序建议 A→B→C→D→E（C 依赖 A 的 keystore 指纹做 App Links，D 依赖 C 的通道）。

### 阶段 A

```text
读 docs/ANDROID_APP_PLAN.md 的 §2 现状盘点与 §4 阶段 A，按其设计实施：创建 mobile/ Capacitor 壳工程（远程加载线上站点）、
返回键/外链/错误页处理、web 侧 isNativeApp 运行时判断（@capacitor/* 一律动态 import）、server/router/web.go 缓存头加固、
release keystore 生成说明与 key.properties 模板（真实 keystore 不入库）、mobile/README.md（构建/签名/版本耦合纪律/调试方法）。
约束：正式域名先用占位符 https://app.example.com 并在 README 标注替换位置；遵守 docs/ARCHITECTURE.md §4 的 UI 约定与页面单根硬约束；
完成后 cd web && npm run build 与 cd server && go build ./... && go test ./... 验证；commit 但不要 push；
「编译推送步骤.md」是用户自管文件，勿动勿提交。
```

### 阶段 B

```text
读 docs/ANDROID_APP_PLAN.md 的 §2 现状盘点与 §5 阶段 B，按其设计实施移动 GitHub OAuth：复用现有 /login/callback 回调页
（redirect_uri 追加 ?mode=mobile，GitHub OAuth App 配置不动），移动流 state 走服务端一次性存储（Redis 优先、进程内 TTL map 兜底），
一次性短码 + PKCE（code_challenge 绑定）换现有 JWT 双 token，quantvista:// 深链回 App，App 内隐藏 GitHub 绑定入口。
注意：现有 cookie double-submit 机制（server/controller/auth.go:112-121）对 Web 流保持原样零回归；
LoginByGitHub 的"GitHub 用户→本地用户"段抽公共函数复用，勿复制粘贴。
完成后为新端点补 go test（state 消费一次性、短码重放、verifier 错误三个用例），cd web && npm run build 与
cd server && go build ./... && go test ./... 验证；commit 但不要 push；「编译推送步骤.md」勿动勿提交。
```

### 阶段 C

```text
读 docs/ANDROID_APP_PLAN.md 的 §2 现状盘点与 §6 阶段 C，按其设计实施 ntfy 推送通道：
deploy/docker-compose.example.yml 加 quantvista-ntfy 服务并在 docs/DEPLOYMENT.md 补部署段（宝塔反代 WebSocket + CF 子域 + 用户/token 初始化命令）；
server 侧 NotifyChannel 加 kind=ntfy（target 为 JSON {url,topic,token} 整串加密），NotifyService 加 SendMsg(NotifyMessage{Title,Content,Route,Kind,Priority})
新主入口（旧 Send 保留为薄包装），三个既有触发点（server/service/alert.go:597,758、server/service/dailyreport.go:360-361）迁移 SendMsg 并补 Route；
系统设置加 SiteBaseURL（管理后台可配，风格对齐 GitHub OAuth 凭证项）；web/public/.well-known/assetlinks.json（指纹占位符）+
mobile 壳 App Links intent-filter 与 appUrlOpen 路由跳转；Settings.vue 通道表单加 ntfy 类型；手机端 ntfy App 配置说明写进 mobile/README.md。
约束：Server酱/Webhook 老通道零回归，notify_test.go 扩 ntfy 用例；完成后 cd web && npm run build 与
cd server && go build ./... && go test ./... 验证；commit 但不要 push；「编译推送步骤.md」勿动勿提交。
```

### 阶段 D

```text
读 docs/ANDROID_APP_PLAN.md 的 §2 现状盘点与 §7 阶段 D，按其设计实施自选/持仓异动守护推送：
GuardEvent 模型（唯一索引同日去重）、StartGuardJobs 交易时段 15min 评估（批量行情，严禁逐 symbol 重接口调用，
时段判断参考 StartIntradayJobs）、止损/止盈触达用当日 low/high 兜底（口径对齐 alert.go evaluateAlert 的 price 分支）、
UserPreference.GuardConfigJSON 偏好（默认开、阈值 pos±5%/watch±7%、自选侧仅 IsPinned 或 stage∈{waiting_price,planned}）、
Settings.vue 智能守护配置区、推送走 SendMsg（Route=/stock/<symbol> 或 /positions，止损类 Priority=4）。
完成后 guard 评估纯函数补单测（止损/止盈/异动/去重至少各 1 例），cd web && npm run build 与
cd server && go build ./... && go test ./... 验证；commit 但不要 push；「编译推送步骤.md」勿动勿提交。
```

### 阶段 E

```text
读 docs/ANDROID_APP_PLAN.md 的 §8 阶段 E，在 Android 壳内按清单逐页验收移动布局并修补：
一级页面（市场首页/个股详情/今日待办/自选/持仓/推荐追踪/AI 分析/个股问答/收盘日报/条件提醒）逐页 360px/390px 检查，
二级页面与管理页保可用。硬约束：改页面必须保持单根模板（多根组件路由离开白屏，见 docs/ARCHITECTURE.md §4.3）、
断点统一 768px 用 useIsMobile、禁硬编码颜色、复用 PageContainer/SectionCard 体系；修补集中在 CSS 与响应式 props，
不重做交互不建第二套页面。每修完一批 cd web && npm run build 验证零类型错误；commit 但不要 push；「编译推送步骤.md」勿动勿提交。
```
