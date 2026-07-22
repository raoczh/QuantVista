# 技术架构设计

## 1. 总体架构

QuantVista 采用前后端分离的全栈架构：

```text
Vue 前端
  |
  | REST 为主；个股问答走 NDJSON 流式（S1），推荐/日报生成为异步任务+轮询（§6.10）
  v
Go API Server
  |
  +-- Auth Service
  +-- Market Data Service
  +-- AI Analysis Service
  +-- Recommendation Service
  +-- Portfolio Service
  +-- Tracking Service
  +-- Settings Service
  +-- Job Service
  |
  +-- MySQL（生产，宝塔托管）/ SQLite（开发）
  +-- Redis
  +-- 外部行情、新闻、财务、宏观数据源
  +-- 外部 LLM Provider
```

## 2. 参考 new-api 的结论（历史决策记录）

后端工程组织（Controller/Service/Model 分层、`router`/`middleware`/`setting`/`common` 划分）、鉴权、系统设置、任务、AI 渠道管理均参照 `new-api`（Go + Gin + GORM + Redis + JWT/OAuth）落地；前端未复用其 React 组件，独立建 Vue 3 项目（API 契约框架无关）。此节仅存档立项决策，当前结构以仓库代码为准。

## 3. 后端技术栈（已定型）

- 语言：Go
- Web 框架：Gin
- ORM：GORM
- 数据库：开发 SQLite，生产 MySQL（宝塔托管，与 new-api 同实例不同库）；PostgreSQL 仅 GORM 兼容，不主推
- 缓存：Redis
- 认证：JWT + GitHub OAuth
- 定时任务：Go 内部任务调度，后续可扩展队列
- 日志：结构化日志
- 配置：环境变量 + 数据库配置

## 4. 前端技术栈（已定型）

- Vue 3
- Vite
- TypeScript
- Pinia
- Vue Router
- ECharts
- Naive UI
- Axios 或 Fetch 封装

页面风格：

- 应用型工作台，不做营销落地页。
- 首页直接展示市场总览和可操作入口。
- 信息密度适中，适合反复查看和对比。
- 图表、表格、筛选器、状态标签、分段控件是主要 UI 形态。

### 4.1 主题系统（硬约束）

内置 **6 套主流主题**，用户可在顶栏一键切换，选择持久化到 `localStorage`（key `qv-theme`），刷新与重开会话保持：

| key | 名称 | 基色调 | 主色 |
| --- | --- | --- | --- |
| `light-blue` | 极简蓝（浅） | 亮 | `#2080f0` |
| `dark-blue` | 深空蓝（深） | 暗 | `#2080f0` |
| `dark-emerald` | 极客绿（深） | 暗 | `#18a058` |
| `light-violet` | 典雅紫（浅） | 亮 | `#7c3aed` |
| `dark-amber` | 暖夜橙（深） | 暗 | `#f0a020` |
| `light-rose` | 樱桃红（浅） | 亮 | `#d03050` |

实现：主题预设集中在 `web/src/theme/presets.ts`，全局状态在 `stores/theme.ts`，由根部 `n-config-provider` 统一下发 `theme` + `theme-overrides`，并配 `n-global-style` 联动 body 背景。应用外壳在 `components/AppShell.vue`——**必须位于 `n-config-provider` 内部**（`useThemeVars()` 只在 provider 子树内能取到 override 后的变量，App.vue 顶层取不到）。每套预设除主色四件套外还定义**背景分层**：浅色 = 带主色倾向的浅灰底 + 纯白卡片，深色 = 品牌色调深底 + 浮起一档的卡片（同时对齐 `tableColor`/`tableHeaderColor`/`codeColor`，避免 Naive 暗色固定灰黑与带色调卡片打架）；另统一 `borderRadius: 8px` 控件圆角。

**后续所有页面 / 组件样式必须兼容全部 6 套主题（强制规则）：**

- **禁止硬编码颜色**（文字、背景、边框）。颜色一律取自 Naive UI 主题：组件优先用 Naive 组件自带样式；确需取色时用 `useThemeVars()` 拿主题变量，或用 `n-config-provider` 的 override。
- **明暗都要可读**：6 套里有亮有暗，任何新页面在亮色和暗色基调下都要对比度达标，不能只在某一种下好看。
- **图表（ECharts）必须主题感知**：按当前 `isDark` 选明/暗主题，主题切换时重建（见 `pages/Home.vue` 的 `watch(isDark)` 范式）；语义色（涨红跌绿等）可固定，但坐标轴/背景/文字跟随主题。
- **第三方/自绘组件**接入前先确认能跟随主题，否则需包一层主题适配。
- 新增主题只在 `presets.ts` 加一项即可，不改页面代码——页面不得对“当前是哪套主题”做硬编码假设。

> 验收口径：任何新页面合并前，至少在 1 套亮色 + 1 套暗色主题下自检通过。

### 4.2 UI 设计系统（全站统一，后续按此走）

风格定位：**专业金融终端 × 现代高颜值站点**。信息密度够、层级清晰、数字对齐不跳动，同时有圆角/留白/主色点缀的现代观感。全站复用同一套基础层与组件，新页面**不再从零堆 `n-card` + 原始 `table`**。

**基础层**

- `web/src/styles/global.css`（在 `main.ts` 引入）：设计 token（`--qv-content-max: 1440px`、`--qv-radius-card: 14px`、字体栈）、`.qv-tnum`/`.qv-mono`/`.qv-figure` 等宽数字工具类、细滚动条、`qv-fade-up` 入场动画与 `::selection`（主色由 AppShell 注入 `--qv-primary-selection`，裸布局回落中性灰）。只放**与主题无关**的排版；颜色一律不写死。
- `web/src/composables/useUi.ts`：全站取色入口，颜色全部来自 `useThemeVars()`，自动兼容 6 套主题。导出 `pctColor/pctBg`（涨红 `errorColor`/跌绿 `successColor`/平 `textColor3`）、`primaryAlpha(a)`、`withAlpha(color,a)`、`upColor/downColor`、`isDark`、`vars`。**任何涨跌/主色透明度需求走它，禁止硬编码 hex。**
- `web/src/composables/useAutoRefresh.ts`：盘中自动刷新（仅交易时段周一~五 09:15–15:05 + 页面可见时轮询，切后台暂停；数据源有限流，**间隔不得低于 60s**）。Home/Watchlist/Positions 已接入，行情类新页面照用。
- `web/src/composables/useStockActions.ts`：个股快捷动作（跳 AI 分析/问答/对比/设提醒 query 预填、加自选到第一分组）。Home 速查、GlobalSearch 复用；新入口一律走它。
- `web/src/lib/pageTitle.ts`：标签页标题统一拼装（页面名 + 大盘行情两段互不覆盖），router.afterEach 与 AppShell 轮询各自 set。

**外壳与导航**（`components/AppShell.vue`）

- 整页滚动 + sticky 毛玻璃顶栏（半透明 cardColor + backdrop-filter）+ 顶部主色氛围光晕 + 路由切换淡入上移过渡。
- 导航收敛 7 项：市场首页 / 今日待办（数字徽标，`/todos` total）/ 自选 / 持仓 / 推荐追踪 / AI 研究▾（分析·问答·对比）/ 更多▾（模拟盘·条件提醒·提示词模板）；设置与管理后台只在右上角用户菜单。菜单激活态为主色胶囊。
- 顶栏「搜代码」按钮 + `Ctrl/Cmd+K` 唤起 `GlobalSearch` 命令面板：精确代码查行情 + 快捷动作直达（后端无模糊搜索，仅精确代码）。

**通用组件**（`web/src/components/`）

- `PageContainer`：页面外层，`max-width` 居中 + 标题/副标题 + `#actions` 插槽 + 页头入场动画。**每个业务页最外层都用它。**
- `SectionCard`：带主色渐变标题条 + 静态质感阴影（浅色柔和投影/深色顶部内高光）+ hover 抬升的卡片（包 `NCard`），`title` / `#extra` 插槽 / `:hoverable`。替代裸 `n-card`。
- `StatCard`：指标卡（label + 大号数值 + 涨跌），数值色随涨跌，质感语言与 SectionCard 一致。
- `RankList` + `#row` 插槽：带名次徽标的榜单（第 1 名主色渐变徽标），替代原始 `<table>`。
- `ChangeTag`：涨跌幅 pill（`:value` 百分比，自动 +号/配色）。
- `BrandLogo`：主色渐变方块 + 折线 mark + 双色字标，顶栏/认证页共用。
- `AuthShell`：认证页统一外壳（主题感知渐变背景 + 品牌 + 角落主题切换），登录/首启/回调复用。
- `GlobalSearch`：全局速查命令面板（Ctrl+K），挂在 AppShell。

**约定**

- 新页面骨架 = `PageContainer` → 若干 `SectionCard`；列表优先 `RankList`，指标优先 `StatCard`，涨跌用 `ChangeTag`/`useUi().pctColor`。
- 数字加 `.qv-tnum`（或 `.qv-figure` 用于大号），保证对齐。
- 图表主题感知照 §4.1（`isDark` 初始化 + `watch` 重建，语义色取 `vars.errorColor/successColor`）。
- 仍受 §4.1 硬约束约束：组件内所有色值来自主题变量，中性描边/阴影可用 `rgba(128,128,128,…)` / `rgba(0,0,0,…)`。

### 4.3 页面单根与移动端适配（硬约束，2026-07-04）

- **页面模板必须单根**：每个业务页顶层只有一个节点（`PageContainer`/`AuthShell`），`n-modal` 等浮层也放进它内部。外壳 `RouterView` 包着 `Transition mode="out-in"`，多根组件（fragment）无法执行 leave 过渡、`afterLeave` 永不触发，**离开该页时整个应用白屏**（Settings.vue 曾双根踩坑）。`router/index.ts` 已加 `onError`：懒加载 chunk 拉取失败（部署后旧 hash）整页 `location.assign` 兜底。
- **移动端断点全站统一 768px**：
  - `AppShell` ≤768px 用汉堡按钮 + `n-drawer` 抽屉导航（水平菜单隐藏），顶栏只留图标；桌面 `n-menu` 加 `responsive` 溢出收纳。
  - `SectionCard` 内容区 `overflow-x:auto` + `n-table` 单元格 nowrap（宽表横滚）；`global.css` 提供 `.qv-scroll-x` 工具类与 `.n-modal { max-width: calc(100vw - 24px) }` 全局限制。
  - `composables/useIsMobile.ts`（matchMedia 768）用于切 Naive 布局 props——左标签表单手机切 `label-placement="top"`（Settings/AdminSettings 范式）。
  - 行式列表（自选条目/待办/提醒规则）手机上 `flex-wrap`、操作按钮组 `flex-basis:100%` 换行；弹窗内 `n-grid` 一律 `cols="1 s:N" responsive="screen"`。
  - ECharts tooltip 加 `confine: true`（否则被卡片横滚容器剪裁），并挂 window resize → `chart.resize()`。

## 5. 后端模块

### 5.1 Auth Service

职责：

- GitHub OAuth 登录。
- JWT 签发与刷新。
- 用户资料同步。
- 用户权限判断。

> **凭证管理（阶段 1 落地后修订）**：GitHub OAuth 的 `client_id` / `client_secret` **存数据库系统设置**（`options` 表，secret 经 AES-256-GCM 加密），由管理员后台「GitHub 登录」运行时可配可改。`deploy/.env` 的 `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` 仅作**首启种子**（DB 无值且 env 有则回填），之后以 DB 为准。此处与最初"只走 env"的设计已调整——改为参照 new-api 的 DB 设置项做法（换 token、拉用户、建号流程同样可参考）。
> 鉴权用 JWT：access token（HS256，2h，无状态）+ refresh token（`refresh_tokens` 表落库、可吊销、换发轮换）。首启无用户时走密码方式创建首个管理员（admin），解 GitHub 凭证配置前的"鸡生蛋"问题。

接口示例（与实际路由一致）：

- `GET /api/oauth/github/url`（返回授权地址，同时种 state cookie）
- `POST /api/oauth/github`（前端回调页用 code 换令牌，double-submit 校验 state）
- `POST /api/user/github/bind` / `DELETE /api/user/github/bind`（已登录用户绑定/解绑 GitHub，2026-07-03；绑定复用同一授权地址与回调页，前端 sessionStorage 标记区分意图；github_id 被他人占用拒绝；未设密码的纯 OAuth 账号拒绝解绑防锁死）
- `GET /api/user/self`
- `POST /api/auth/login` / `POST /api/auth/refresh` / `POST /api/auth/logout`

### 5.2 Market Data Service

职责：

- 拉取行情、指数、板块、新闻、财务和宏观数据。
- 标准化不同数据源格式。
- 缓存热点行情。
- 记录数据更新时间和数据源健康状态（S1 落地：每（源,能力）健康滑窗，empty/error 超阈值冷却踢出轮询，`GET /api/admin/datasources` 查看；早期规划的 `data_source_configs` 配置表已确认为死表删除）。

**数据适配/标准化层（关键设计）**：

数据源是本项目工程量最大的部分，必须抽象成可插拔的适配层：

```text
内部标准数据结构 (Quote / Bar / Fundamental / News ...)
        ^
        | normalize()
   DataSourceAdapter 接口
        ^
   +----+----+----+
  东方财富  新浪 ...     <- MVP 先实现东财，新浪做备份/校验
```

- 上层（缓存、AI、追踪）只依赖**内部标准结构**，不感知具体数据源。
- 新增数据源 = 新增一个 Adapter 实现，不改上层。
- **MVP 只实现一个市场、一个源**的原则已按此落地：当前东财→腾讯→新浪三源互备，仅覆盖 A 股沪深（含沪深 ETF/LOF 场内基金）。
- 每条数据携带 `source` 与 `data_time`；同步失败写 `data_sync_logs`。
- 日线行情写入 `daily_bars`（OHLC，东财前复权主源），供追踪、因子宽表与回撤计算使用；`corporate_actions` 复权因子表未建，现行方案为除权检测+整股重锚（见 ROADMAP 边界区）。
- **Tushare 分档接入**：第一阶段以东财 + 新浪为主，Tushare 非前置；免费档（120，股票清单/日线/交易日历）与低 cost 档（2000，财务三表/复权因子/指数日线，长线财务深度来源）按需启用，高级档（5000，分钟线/融资融券明细等）暂不实现。详见 [数据源选型](docs/DATA_SOURCES.md)。

接口示例（与实际路由一致；行情为公开端点、带宽松限流）：

- `GET /api/markets/:market/overview`
- `GET /api/markets/:market/stocks/:symbol/{quote,bars,score,indicators,chips,finance,fundflow,lhb,orgview}`（行情/日线/评分/技术指标/筹码/财务/主力资金/龙虎榜/机构观点）
- `GET /api/markets/:market/boards`、`GET /api/markets/:market/boards/:code{,/fundflow}`（板块热度/详情/资金流）
- `GET /api/news?symbol=&source=&limit=`、`GET /api/announcements`（新闻与公告）

### 5.3 Watchlist Service

职责：

- 自选股增删改查。
- 自选股分组。
- 重点关注和备注。

接口示例：

- `GET /api/watchlists`
- `POST /api/watchlists`
- `POST /api/watchlists/:id/items`
- `PUT /api/watchlist-items/:id`（另有 `/:id/stage` 研究阶段、`/missed` 错过复盘）
- `DELETE /api/watchlist-items/:id`

### 5.4 Portfolio Service

职责：

- 管理已购入持仓。
- 记录买入和卖出。
- 计算当前盈亏。
- 关联 AI 推荐记录。

接口示例：

- `GET /api/positions`（含止损/分析时效富化）/ `GET /api/positions/overview`（组合总览+风控信号）
- `POST /api/positions`
- `POST /api/positions/import`（CSV 批量导入，multipart，逐行校验+错误行报告，上限 500 行，限流 10/min）
- `PUT /api/positions/:id`
- `POST /api/positions/:id/close`
- `DELETE /api/positions/:id`
-（复盘内容随 close 落库，无独立 review 端点）

### 5.5 AI Analysis Service

职责：

- 根据用户选择的模块组装上下文。
- 调用用户或系统配置的 LLM。
- 输出结构化分析报告。
- 保存分析历史。
- 统计调用消耗。

接口示例（与实际路由一致）：

- `POST /api/analysis`（发起分析，限流 20/min；`mode=panel` 为个股多角色观点——technical/momentum/risk/contrarian 四角色独立评级+共识+分歧）
- `GET /api/analysis?module=&limit=`（历史，支持模块筛选）
- `GET /api/analysis/:id` / `DELETE /api/analysis/:id`
- `GET /api/analysis/:id/diff`（变化检测：与上一份同对象成功分析对比 rating/confidence/summary/highlights/risks）
-（rerun / 从历史创建推荐为规划项，未实现）

### 5.6 Recommendation Service

职责：

- 根据策略模板和用户偏好筛选候选池。
- 调用 AI 生成推荐解释。
- 生成短线或长线推荐。
- 保存推荐记录。

接口示例（与实际路由一致）：

- `POST /api/recommendations`（生成，限流 15/min）
- `GET /api/recommendations?type=&limit=` / `GET /api/recommendations/:id`
- `GET /api/recommendations/strategies?type=`
-（「一键建仓」由前端跳持仓页预填并带 `rec_id`，落库 `positions.recommendation_id` 血缘；推荐详情回显「已建仓」与推荐价 vs 实际买价对比）

### 5.7 Tracking Service

职责：

- 跟踪推荐表现。
- 更新短线止盈、止损、过期和重新分析状态。
- 计算推荐成功率、收益率和最大回撤。

**追踪数据建模（两层）**：

- **当前状态层**（与推荐 1:1）：保存最新收益率、最高价、最低价、最大涨幅、最大回撤、当前状态，定时任务覆盖更新。
- **价格序列层**：复用 `daily_bars`（按 stock + 日期），追踪只引用，不重复存全量行情。
- 止盈/止损判断使用当日 **high/low**，而非单点收盘价，避免漏判盘中触达；且**仅在有效期窗口内判定**，过期后触达不改写结局。
- 收益/回撤计算的目标态为**复权后**价格（结合 `corporate_actions`）；当前实现基于东财前复权日线（重锚型，除权后历史重刷、与生成时点快照价可能错位，note 标注），复权因子表待补。
- 表现统计同时计算**相对基准（指数）的超额收益**并附**样本量 n**。

接口示例（与实际路由一致）：

- `GET /api/recommendations/performance?type=`
- `POST /api/recommendations/:id/track`（手动刷新单批追踪）

### 5.8 Settings Service

职责：

- 用户 LLM 配置。
- 数据源配置。
- 风险偏好。
- 策略模板。
- 通知设置。

接口示例（与实际路由一致）：

- `GET/POST /api/llm-configs`，`PUT/DELETE /api/llm-configs/:id`，`POST /api/llm-configs/:id/test`
- `GET/PUT /api/user/preference`，`GET /api/user/quota`
- `GET/POST /api/notify-channels`，`PUT/DELETE /api/notify-channels/:id`，`POST /api/notify-channels/:id/test`
- `GET/PUT /api/admin/users/:id/quota`（管理员查看/调整用户 AI 次数上限、手工清零已用量；2026-07-03 起配额为次数制，token 仅审计）
- `GET /api/export/:kind`（kind=positions|watchlist|recommendations|analyses，CSV 带 BOM，限流 10/min）
- `GET /api/admin/datasources`（数据源健康滑窗状态，S1；`data_source_configs` 死表已删，数据源无用户级配置）

### 5.8.1 条件提醒与命中事件（阶段 7 + 批次 H）

- `GET/POST /api/alerts`，`PUT/DELETE /api/alerts/:id`，`PUT /api/alerts/:id/status`（暂停/恢复），`POST /api/alerts/evaluate`（手动评估，限流 20/min）
- 提醒类型 kind：price（到价）/ pct_change（异动）/ ma（均线）/ breakout（突破）/ **volume_surge**（当日量≥N 倍 20 日均量）/ **amplitude**（当日振幅，优先腾讯估值源、缺则 (high-low)/prev_close 回退）/ **earn_date**（财报披露临近 ≤N 日，F1）/ **earn_fcst**（新业绩预告发布，F1）——财报两类不走盘中 15min 评估，由 finance job 每日一评
- 命中明细状态机（`alert_events`，unread/read/dismissed）：`GET /api/alerts/events?status=&limit=`，`PUT /api/alerts/events/:id/status`，`PUT /api/alerts/events/read-all`
- 今日待办（`GET /api/todos`）的提醒条目即 unread 事件，`ref_id` 为事件 id，可就地标记已读/忽略「完成待办」

### 5.9 Job Service

职责：

- 定时刷新市场快照。
- 更新股票行情缓存。
- 更新推荐追踪状态。
- 清理过期缓存。
- 生成每日市场摘要，后续可选。

### 5.10 扩展模块（N/F/T/S/M/P3 批次 + 2026-07 杂项批）

各批次陆续落地的独立 service 模块（批次交付记录见 [DEVELOPMENT_PLAN](DEVELOPMENT_PLAN.md)，接口速查见 REFERENCE_ANALYSIS §6）：

- **news / newsai / newsevent**（N1/N2）：7×24 快讯采集（采集间隔管理后台可配 1~120 分钟，自调度循环下一轮生效）+ LLM 情绪增强（个股关联/利好利空/当日聚合情绪分；「自动 LLM 分析」总闸关闭时走纯关键词规则零 token）+ 事件抽取；`/news` 页与个股详情消息面卡。
- **finance / finance_f10**（F1/F2）：财报日历/业绩预告/快报增量刷新 + F10 主要财务指标与三大报表关键科目按需缓存 + 公告采集，入个股详情财务块、长线推荐 fin 因子、财报提醒。
- **indicator / chip**（T1）：MACD/BOLL/RSI/ATR 纯函数指标库（Wilder 口径）、筹码峰三角衰减复算、五维技术评分升级，供个股详情副图与推荐量化评分。
- **riskgate / breaker / health**（S1）：风险闸门（ST/一字板/流动性/小市值进 AI prompt 与前端标签）、东财 push2 族域名断路器、数据源健康滑窗；问答流式输出同批。
- **marketwide / factortable / screener**（M1）：全市场日线地基（宇宙字典/历史初始化/除权双层检测重锚）、52 因子列式宽表、条件树 DSL 选股（21 内置白话策略+自定义策略），`/screener` 页；策略信号进推荐候选池。
- **backtest / analysis_asof**（M2）：回测时光机（A 股约束五件套/无未来泄露切片复算）、历史推荐批次回验 α 分布、分析 as_of 回溯诊断与 hindsight 事后核验，`/backtest` 页。
- **mood / fundflow / emlhb**（M3a）：龙虎榜、涨停池/炸板率情绪聚合、股吧人气榜、主力资金流（排行+单股历史），入推荐加分项、市场分析情绪段与个股详情。
- **intraday**（M3b）：腾讯 5 分钟线盘中因子（尾盘拉升/跳水/VWAP 偏离/重心上移），入短线推荐加分。
- **board**（M3c/P3b）：东财板块热度榜/成分股/板块指数日线（`/heatmap` 与 `/boards/:code` 页）+ 板块资金流历史透传 + 行业估值聚合（中位 PE/PB 与横截面/时序分位），入板块 AI 分析两段。
- **analysis_trader**（M3c）：个股标准分析自动附加交易计划（买点/止盈/止损/仓位，评级偏空与风险闸门 block 零成本拒绝）。
- **orgview**（P3a）：卖方研报评级（分布/变动检测/目标价偏离）与机构调研密度按需缓存，入个股详情与分析/问答证据链。
- **screener_ai**（P3c）：AI 白话建策略——自然语言解析为条件树（因子字典程序生成、unmatched 兜底禁硬凑、用户确认才落编辑器）。
- **llm_call_log**（2026-07 杂项批）：全用户 LLM 调用审计，见 §6.9。

## 6. AI 调用设计

> **HTTP 客户端加固（2026-07-03；2026-07-22 长流修订）**：`service/ai_client.go` 复用包级连接池（allowPrivate 两态各一个 client，repair/panel 连发请求不再重复 TLS 握手）；全部业务 `chatCompletion` 默认先发真正 SSE 请求，流式 client 不设整体 `Timeout`，只以 90s `ResponseHeaderTimeout` 防建连挂死、以后台任务总 deadline 控制全程。SSE 请求固定携带 `Accept: text/event-stream`、`Cache-Control: no-cache`、`Accept-Encoding: identity`，Transport 禁用自动压缩，避免兼容网关/反代缓冲分片后触发 60s 空闲超时。瞬时失败只重试未达上游的网络错误与 429/500/502/503；504 视为真实超时不自动重试。错误按状态码归类并透传上游 error.message；usage 缺失时按字符粗估，仅用于审计。
>
> **双端点类型（2026-07-09）**：LLM 配置新增 `endpoint_type`（`chat_completions` 默认 / `responses`）。`responses` 走 `ai_client_responses.go` 按 new-api relayconvert 口径适配 `/v1/responses`（system→instructions 合并、messages→input、max_tokens→max_output_tokens、response_format→text.format、output 取 message+assistant 的 output_text、usage input/output_tokens 映射；流式按事件 type 分派）。两端点共用 `chatCompletion`/`chatCompletionStream` 入口，对 caller 透明。

### 6.1 推荐流程（四阶段流水线，2026-07-04 重构；2026-07-06 来源随策略组合）

LLM 的角色从「海选者」降级为「解释者/否决者」（listwise 海选存在位置偏差，学术共识不让 LLM 做大池排序）：

```text
用户发起推荐（或收盘日报 GenerateAuto 同链路）
  |
1. 多源建池：自选 ∪ 按策略组合的榜单来源（strategySources：涨幅/成交额/换手率/回调/低PB 榜，
  |   每源 20~100 深度取数；升序榜行级过滤极端值——回调榜只收当日 -9%~0、低PB榜滤负PB）
  |   黑名单/ST/北交所/流动性/ETF基金 前置排除；来源可叠加落库；新浪 PB/流通市值作腾讯估值缺失兜底
  |
2. 用户筛选硬过滤：股价/流通市值/换手率区间、排除当日涨停、近5日追高保护（20cm 板放大）、换手>30% 极端换手硬拦
  |     （被筛掉的标的保留在池快照并标注原因；条件快照 filters_json 落库可回显）
  |
3. 本地量化评分（零 LLM 成本）：Top48 拉 90 根日线算技术因子 + 五维评分 + 策略加分，池内排名；
  |   换手 20~30% 在此按位置分档：60日区间 ≥65% 高位＝死亡换手排除，低位保留并扣分标注风险
  |
4. LLM 精选：只见量化 Top10，强制引用字段名+数值、禁先验记忆、合格标的充足时给足数量/不足可 0 只；越池/非 Top10 标的一律丢弃
  |
校验结构化输出（有限次 repair）→ 信任层回填（见 §6.8）→ 批次+条目事务落库 → 初始化追踪
```

关键常量：`maxScanCandidates=48 / maxLLMCandidates=10 / maxPoolIntake=240 / poolSnapshotMax=150`。

生成类 LLM 均为**异步任务**（推荐/日报自 2026-07-14，分析/问答/AI 对比/白话选股自 2026-07-22）：HTTP 入口只做确定性校验、配置/配额解析并落 `processing` 后立即返回；数据采集与全部 LLM 阶段在服务端 `context.Background()` 派生的有界 Context 中执行，前端轮询业务记录或 `llm_tasks` 直到终态。浏览器断开、反代 60s 超时和页面刷新都不会取消后台模型调用。推荐主调失败时既有量化降级语义保持不变；其他模块按各自失败/降级契约收尾，不用量化结果掩盖上游错误。

策略-来源映射（`strategySources`，对冲「热度榜供给的票恰是风控规则最想排除的票」的结构性矛盾）：
momentum=涨幅+换手+成交额；pullback=**回调榜**(跌幅升序过滤温和回调)+成交额+涨幅；active=成交额+换手+涨幅；
value=**低PB榜**(升序滤负PB)+成交额；growth=涨幅+换手+成交额；leader=成交额(深捞80)+涨幅。

### 6.2 Prompt 原则

- 明确数据时间范围。
- 明确不得编造不存在的数据。
- 推荐必须来自候选池。
- 短线必须输出止盈、止损、有效期和失效条件。
- 长线必须输出基本面逻辑、估值区间和复盘条件。
- 所有输出必须包含风险提示。

### 6.3 LLM 配置优先级

1. 用户指定的本次调用配置。
2. 用户默认 LLM 配置。
3. 系统回退配置（2026-07-09 落地）：用户一个配置都没有时，回退到管理后台指定的回退配置（`llm_fallback_config_id`，0=自动取首个启用管理员的默认配置）。受「LLM 回退」总闸（`llm_fallback_enabled`，缺省开）控制，关闸时保持「请先在设置中添加」引导；次数配额仍按发起用户计；内网放行按配置所有者判定（`llmAllowPrivate`）。
   - `resolveSystemFallbackConfig` 统一「系统默认 LLM」语义：用户回退与新闻情绪分析等后台任务共用；指定配置须仍存在且所有者为启用管理员，失效静默回落自动档。后台任务不受回退总闸控制（新闻有自己的 `news_auto_llm` 总闸），token 记账归配置所有者。

### 6.4 上下文预算（硬约束）

- 每次调用设定 **token 预算上限**，按模块**分级注入**：核心数据全量、辅助数据摘要、长文本（新闻/公告）先摘要再注入。
- 大列表先**规则裁剪/排序/截断**，只注入 top-N 并在 prompt 标注“已截断”。
- 注入内容与原始输入快照写入 `ai_analysis_reports.data_snapshot_json`，保证可复现。

### 6.5 结构化输出可靠性（硬约束）

- 优先 provider 的 **function calling / JSON mode / response schema**；不支持的 provider fallback 到“prompt 约束 + 文本解析”。
- 统一 **JSON Schema 校验**；失败做**有限次 repair 重试**（错误回灌），仍失败则**优雅降级**（文本可读、recommendations 置空、标记 `partial`），不写脏数据。
- 每次真实上游调用（含 repair 各轮）落 `llm_call_logs` 审计（见 §6.9）。

### 6.6 版本与可复现

- 每次分析/推荐保存 **prompt 版本、策略版本、评分方法版本**；prompt/策略/评分迭代后，历史记录仍可定位当时方法、可横向比较。

### 6.7 成本控制

- **每用户 AI 次数配额与熔断**：按用户手动动作计次（一次分析/推荐/问答/点评=1 次，内部 repair/panel 多轮请求不重复计，后台任务不计次），超额拒绝调用；token 消耗仍全量累计作审计。
- AI 调用做频率限制；首页等高频页面不得每次刷新触发 LLM（走缓存）。

### 6.8 信任层（全 AI 链路统一标准，2026-07-05）

用户信任问题的核心认知：**LLM 口头置信度系统性过度自信，不能单独作为信任依据**；信任要靠「程序化核验 + 独立复核 + 数据透明」三件套，且核验结果透明展示而非静默修正。推荐域先落地，其余 LLM 模块（分析/问答/对比/日报复盘）按同一标准推广：

- **程序化证据核验**：LLM 输出文本中的数字逐一与数据快照的数值集合做容差比对（`max(0.02, 2%)`），吻合数/总数与未吻合清单落库并在前端徽章展示（「可能是推算值或幻觉，建议人工核对」）。跳过规则防自伤：无小数点的 ≤99 小整数（rank/天数）、年份、六位代码、常见噪声整数不参与核验；**调用方必须把模型自身输出的计划价与用户设定阈值并入合法值域**（extra 变参），否则模型合法复述自己的结论会被误标幻觉。
- **程序合成置信度**：由客观信号合成（推荐域=量化排名分位×证据吻合率×数据完备度；分析域=证据吻合率×数据完备度×量化分锚点一致性）三档 high/medium/low + 中文依据，与 LLM 口头置信度**并排展示**（不替换，历史 diff 依赖原字段）。
- **AI 复核员（可选 verify）**：同配置二次调用，独立「风控复核员」人设只挑刺不重做（证据核对/风险完整性/价位合理性/置信度校准），输出 pass/warn/reject；**reject 必须级联**（推荐降级为观察、置信度压 ≤25）。best-effort：复核失败不阻断主结果，但 token 累加进同一记录并计入配额。
- **数据透明**：喂给模型的数据快照落库（`data_snapshot`/`candidate_pool`/`filters_json`），前端提供透明面板（候选池全景/分析快照查看），用户可肉眼对照模型引用。
- **prompt 三件套**：①强制 evidence 引用字段名+数值并明示「系统会程序化核对」（威慑）；②禁先验记忆条款（名气/行业地位/新闻记忆都不算数据）；③允许拒选/如实说明数据不足（解析层用指针语义区分「缺字段」与「显式空」，空≠错误）。
- **工程约定**：LLM 输出的整数字段一律 `FlexInt`（容忍 72.5/"80"）；prompt/策略改动递增版本号落库；新旧记录兼容靠 `omitempty` + 前端逐字段 `v-if` 兜底。

### 6.9 调用审计（2026-07 杂项批）

全用户 LLM 调用明细落 `llm_call_logs` 表：`chatCompletion`/`chatCompletionStream` 是全链路仅有的两个上游出口，defer 埋点全覆盖（responses 端点走同两个入口天然覆盖）。每行记录发起用户/模块（analysis、analysis_review、trade_plan、recommendation、rec_review、qa、compare、daily_report、news、screener_parse、test）/配置与 provider/model/端点类型/流式标记/成功失败与错误信息/token 三项/耗时/**请求与响应全文**（TEXT，>60KB UTF-8 安全截断）。

- repair/panel 的每轮真实调用各落一行（真实调用次数审计，特性而非重复）；新闻等后台任务记配置所有者；测试连接（`module=test`）不走 chat 出口、自行埋点。
- 写失败仅 SysWarn 不影响主流程；`common.DB` 为空直接跳过（直调 ai_client 的单测不受影响）。
- 管理端 `GET /api/admin/llm-calls`（列表显式排除两 TEXT 列防大响应，user/module/status 筛选+分页）与 `/api/admin/llm-calls/:id`（全文详情），仅 AdminAuth；前端 `/admin/llm-calls` 页。**请求正文含用户数据（持仓、自选等），仅管理员可见。**
- 每日 03:25 清理 90 天前记录。
- **stream 列记录实际请求形态**（2026-07-14；2026-07-22 加固）：`chatCompletion` 内部流式优先、上游明确拒绝 stream 时才回落非流式；Chat/Responses 两条 SSE 路径共用无整体超时 client 与防缓存/压缩请求头。审计按实际发出的请求记 stream 值，不按入口意图；`first_chunk_ms` 记首个 data 块到达耗时（非流式恒 0），**≈latency_ms 即上游忽略 stream 整包返回（假流式网关）**，是排查 60s 超时归属层的第一观测。

### 6.10 生成类任务异步化与超时预算（2026-07-14；2026-07-22 全链路修订）

背景：浏览器/入站反代与 LLM 中转站是两层不同的超时。后台任务解决前者取消请求 Context 的问题；真正 SSE 与单次输出预算解决后者的 60s 空闲/整包限制。只改前端轮询不能处理模型网关 504，必须同时满足以下约束：

1. **异步任务化**：推荐/日报/分析复用各自业务表的 `processing` 状态；问答、AI 对比、白话选股复用通用 `llm_tasks`。同步入口不采集行情、不调用模型；后台 goroutine 使用独立 Context + 总 deadline（推荐 6min、日报 8min、分析/问答 10min、对比/白话选股 5min）。前端 `pollUntil` 轮询并可在刷新后恢复；15min 陈旧任务惰性收敛为 failed。
2. **真正流式调用**：所有业务 `chatCompletion` 先走 Chat/Responses SSE，`stream=true` 与防缓冲请求头在每个 fallback 请求中都保留；流式 client 无整体 HTTP timeout。只有上游明确以 4xx 拒绝 stream 时才回落整包请求，不能在 504、流中断或调用 Context 取消后静默改走非流式。
3. **单次调用边界**：模块预算取 `min(用户 max_tokens, 模块上限)`；analysis/recommendation/qa/news=2500，trade_plan/rec_review/rec_bear/daily_report=1500，analysis_review/compare=1000，screener_parse=2000。全部结构化模块最多 1 次 repair，坏输出默认只回灌 600 字（日报 800 字）；业务 prompt 不再用字数、句数或性能型数组条数压缩模型回答，JSON schema、推荐数量、固定角色和逐 ID 映射等业务契约仍保留。Chat 端拒绝 `max_tokens` 时必须携同值改用 `max_completion_tokens`；Responses 始终携带 `max_output_tokens`。两种字段都不支持则失败，禁止删除 token 参数退成无上限生成。
4. **业务降级边界**：只有推荐维持既有量化降级（ATR 规则计划价、action 恒 watch、置信度 low、`degraded_source=quant_fallback`）；鉴权/路径/配额类确定性错误直接失败。日报两路并行并保留旧报告回滚语义；其他模块按自身失败/结构化降级契约收尾。

## 7. 数据缓存策略

行情类数据：

- 热点行情短缓存。
- 首页市场概览短缓存。
- 历史行情长期保存或按数据源能力查询。

AI 结果：

- 同一用户、同一参数、同一数据快照下可短期缓存。
- **缓存 key 必须包含数据快照版本**，数据更新后旧结论自动失效，避免“用旧数据的结论配新行情”。
- 推荐结果必须持久化，不能只缓存。

配置类数据：

- 用户偏好可缓存。
- LLM API Key 不进入前端缓存。

## 8. 安全设计

- GitHub OAuth 登录。
- JWT 鉴权；refresh token 落库，支持**吊销/强制登出**。
- 用户数据按 `user_id` 隔离。
- API Key 加密保存；加密**主密钥**独立管理（环境变量/KMS），支持轮换。
- 用户配置 Base URL 时做 SSRF 防护（禁内网地址、协议白名单、解析后再校验）。
- 数据源 URL 需要白名单或管理员审核。
- AI 调用做频率限制、每用户配额和成本统计；全量调用审计（`llm_call_logs`，§6.9）仅管理员可见。
- 敏感操作（改 API Key、改数据源 URL）写**审计日志**（操作类 `audit_logs` 未建，个人自用降级为规划项；LLM 调用审计已落地，多用户开放前须补操作审计）。
- 股票推荐页面固定展示免责声明。

## 9. 前端页面结构

实际路由（`web/src/router/index.ts`）：

- `/`：市场首页
- `/news`：市场快讯（新闻情绪流，N1/N2）
- `/stocks/:market/:symbol`：个股详情（行情/日K/估值/评分 + 快捷动作，2026-07-03；首页榜单行与个股速查可点击进入，`useStockActions.goDetail` 供各处复用）
- `/screener`：策略选股（21 策略组合筛选，S1）
- `/backtest`：回测时光机（S1）
- `/heatmap`：行业热力图（M3b）
- `/boards/:code`：板块详情（M3c）
- `/daily-report`：收盘日报（今日复盘 + 明日推荐，2026-07-03；`GET/POST /api/daily-reports*` 端点，交易日 15:35 后 `StartDailyReportJobs` 自动生成、手动重生成限流 5/min；首页「AI 今日观点」卡展示最新摘要）
- `/login`（+ `/login/callback` OAuth 回调）、`/setup`：登录 / 首启建管理员
- `/today`：今日待办/待复盘
- `/watchlist`：自选股
- `/positions`：已购入持仓
- `/analysis`：AI 分析中心（含历史，支持模块筛选）
- `/recommendations`：推荐历史与追踪（策略模板为页内下拉，无独立页）
- `/qa`：个股 AI 问答
- `/compare`：个股横向对比
- `/alerts`：提醒规则 + 推送通道
- `/paper`：模拟交易
- `/etf`：指数 ETF 交易（精选指数 ETF 行情 + 复用模拟盘买卖，2026-07-05）
- `/thesis`：投资逻辑卡
- `/notes`：投资笔记
- `/prompt-templates`：自定义分析提示词模板（用户菜单进入）
- `/settings`：设置（LLM/偏好/AI 用量/账号安全）
- `/admin`：管理员后台（注册开关/新闻采集/LLM 回退/GitHub 凭证/用户与配额/同步日志）
- `/admin/llm-calls`：LLM 调用审计记录（筛选+分页+全文详情，2026-07 杂项批）
