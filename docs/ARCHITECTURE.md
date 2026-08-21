# 技术架构设计

## 1. 总体架构

QuantVista 采用前后端分离的全栈架构：

```text
Vue 前端
  |
  | REST 为主；个股问答走 NDJSON 流式（S1），作业事件走带 JWT 的 fetch SSE + 轮询回退（§6.10）
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
  +-- Task Center Service
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
- 首页第一层直接展示与当前用户有关的待办、持仓风险/行情缺口、重点自选和最近研究；市场总览与个股速查完整保留在第二层。
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

实现：主题预设集中在 `web/src/theme/presets.ts`，全局状态在 `stores/theme.ts`，由根部 `n-config-provider` 统一下发 `theme` + `theme-overrides`，并配 `n-global-style` 联动 body 背景。应用外壳在 `components/AppShell.vue`——**必须位于 `n-config-provider` 内部**（`useThemeVars()` 只在 provider 子树内能取到 override 后的变量，App.vue 顶层取不到）。每套预设除主色四件套外还定义**背景分层**：浅色 = 带主色倾向的浅灰底 + 纯白分区，深色 = 品牌色调深底 + 可读的内容层（同时对齐 `tableColor`/`tableHeaderColor`/`codeColor`，避免 Naive 暗色固定灰黑与带色调内容层打架）；控件和实体卡片圆角不超过 8px。

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

- `web/src/styles/global.css`（在 `main.ts` 引入）：设计 token（`--qv-content-max: 1440px`、`--qv-radius-card: 8px`、字体栈）、`.qv-tnum`/`.qv-mono`/`.qv-figure` 等宽数字工具类、细滚动条、`qv-fade-up` 入场动画与 `::selection`（主色由 AppShell 注入 `--qv-primary-selection`，裸布局回落中性灰）。只放**与主题无关**的排版；颜色一律不写死。
- `web/src/composables/useUi.ts`：全站取色入口，颜色全部来自 `useThemeVars()`，自动兼容 6 套主题。导出 `pctColor/pctBg`（涨红 `errorColor`/跌绿 `successColor`/平 `textColor3`）、`primaryAlpha(a)`、`withAlpha(color,a)`、`upColor/downColor`、`isDark`、`vars`。**任何涨跌/主色透明度需求走它，禁止硬编码 hex。**
- `web/src/composables/useAutoRefresh.ts`：盘中自动刷新（仅交易时段周一~五 09:15–15:05 + 页面可见时轮询，切后台暂停；数据源有限流，**间隔不得低于 60s**）。Home/Watchlist/Positions 已接入，行情类新页面照用。
- `web/src/composables/useStockActions.ts`：个股快捷动作（跳 AI 分析/问答/对比/设提醒 query 预填、加自选到第一分组）。Home 速查、GlobalSearch 复用；新入口一律走它。
- `web/src/composables/useDisplayMode.ts`：按账号隔离的“简明/专业”显示偏好，统一管理 `localStorage` 键；页面不得自行读写显示模式存储。
- `web/src/components/home/homeWorkspace.ts` + `HomeWorkspaceSection.vue`：首页盘前/盘中/盘后排序、默认展开与个人区块外壳。自动模式只认后端行情新鲜度的 `market_state`；缺失时上海时钟仅用于展示排序且状态保持 `unknown`。模式偏好按用户写本地存储，不落服务端、不改变业务事实。
- `web/src/lib/pageTitle.ts`：标签页标题统一拼装（页面名 + 大盘行情两段互不覆盖），router.afterEach 与 AppShell 轮询各自 set。

**外壳与导航**（`components/AppShell.vue`）

- 整页滚动 + sticky 毛玻璃顶栏（半透明 cardColor + backdrop-filter）+ 路由切换淡入上移过渡；不使用装饰性径向光晕。
- 桌面导航保留首页 / 今日（复用 `/todos` total 徽标）/ 自选 / 选股 / 持仓 5 个高频直达项；情绪、快讯、热力图与 ETF 归“市场”，推荐、分析、日报、问答与对比归“研究”，其余低频功能归“更多”。设置与管理后台只在右上角用户菜单。
- 顶栏「搜股票」按钮或 `Ctrl/Cmd+K` 唤起 `GlobalSearch`，搜索旁提供 AI 快捷操作；≤768px 仍固定为首页 / 自选 / 搜索 / 今日 / 持仓五项，不增加第六项，AI 操作从搜索面板或首页两步内进入。搜索支持名称、代码与拼音模糊匹配，并复用统一股票动作。

**通用组件**（`web/src/components/`）

- `PageContainer`：页面外层，`max-width` 居中 + 标题/副标题 + `#actions` 插槽 + 页头入场动画。**每个业务页最外层都用它。**
- `SectionCard`：克制的平面分区（包 `NCard`），默认无阴影、无装饰性标题条、无 hover 抬升，圆角 8px；只有明确传入 `:hoverable="true"` 的交互对象才改变边框。
- `StatCard`：指标卡（label + 大号数值 + 涨跌），数值色随涨跌，质感语言与 SectionCard 一致。
- `RankList` + `#row` 插槽：带名次徽标的榜单（第 1 名主色渐变徽标），替代原始 `<table>`。
- `ChangeTag`：涨跌幅 pill（`:value` 百分比，自动 +号/配色）。
- `BrandLogo`：主色渐变方块 + 折线 mark + 双色字标，顶栏/认证页共用。
- `AuthShell`：认证页统一外壳（主题感知渐变背景 + 品牌 + 角落主题切换），登录/首启/回调复用。
- `GlobalSearch`：全局速查命令面板（Ctrl+K），挂在 AppShell。
- `StockIdentity`：股票名称为主、代码和市场为次的统一身份展示；缺名称显示“名称待补全”，支持普通/紧凑/表格密度、键盘进入详情及可选 `StockActionMenu`。
- `StockPicker`：基于既有名称/代码/拼音搜索接口的共享选股器，保持 `symbol/market/name` 深链与 API 参数兼容。
- `TermHelp` + `termDictionary.ts`：简明/专业名称和“一句话回答什么问题”的统一术语入口；风险、缺失和时效信息不得被白话化隐藏。
- `AIQuickActions`：统一提供 AI 选股、分析一只股票、AI 检查持仓和个股追问；导航本身不发起 AI 请求，股票类操作先经过 `StockPicker`。

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

### 4.4 AI 推荐与分析工作台（散户体验第三批，2026-08-18）

- **页面编排边界**：`pages/Recommendations.vue` 只负责路由 query、页面级数据加载、任务轮询和动作协调；`components/recommendations/` 分别承载生成参数、今日结果卡、候选池/召回审计、历史追踪和专业审计。`pages/Analysis.vue` 对应拆为 `AnalysisLauncher`、`AnalysisResultWorkspace`、`AnalysisHistory`。页面不再承载大段结果模板或表格细节。
- **状态与调用边界**：`useBusinessTask` 只读统一 JobRun，`useResultPolling` 只 GET 已存在的业务结果；取消、重试、生成和分析都必须由用户按钮触发。进入页面、切换标签、打开历史、导航和刷新只恢复已有 ID/状态，不创建新的 LLM 调用；提交锁在第一次 `await` 前生效，避免重复任务。
- **股票身份规则**：所有工作台动作复用 `StockIdentity`/`StockActionMenu`/`useStockActions`，名称为主、代码和市场为次，名称缺失显示明确状态。推荐复核深链携带 `recommendation_id` 和上下文；持仓卖出决策只接受准确的 `position_id`，禁止按 symbol 猜测。按推荐建仓只预填 `rec_id` 与研究数量，不自动下单或写真实持仓。
- **事实展示规则**：推荐将今日可操作结果、历史事实、追踪成熟度、候选池和排除原因分开；未成熟、数据不足、部分成功和失效不得包装成“准确/失败”。分析首屏先显示结论、行动、依据、风险、数据时点和有效时间，AI 观点与程序事实、行情事实、本人持仓事实分层；AI 复核不得改写程序风险等级、持仓卖出等级或任务状态。
- **术语与新鲜度**：专业指标统一通过 `TermHelp` 和术语字典解释，默认折叠原始指标/快照/审计字段；`as_of`、`partial`、`unknown`、`stale` 均显示白话说明，缺失不当作中性。简明/专业模式复用 `useDisplayMode`，按账号隔离。
- **线上边界**：本批已完成源码、契约测试、构建和本地 Chrome 1440×1000 亮色 / 375×812 暗色检查；生产真实数据、长任务恢复、权限隔离和移动真机仍须线上验收，不得以本地检查代替。

### 4.5 剩余页面组件边界与全站收口（2026-08-18）

- `Screener.vue` 保留 API 编排、任务轮询、批量幂等/撤销和策略编辑；快速模板、当前结果、历史扫描分别下沉 `components/screener/`。AI 白话解析仍只由按钮触发。
- `StockDetail.vue` 保留按路由股票加载、数据编排和图表控制；研究页签状态与移动操作下沉 `components/stock-detail/`。本人持仓风险只从 `Position.exit_assessment` 的统一 `PositionExitAssessment` 生成展示项。
- `PortfolioRisk.vue` 保留账户、参数、计算请求、图表与现金流/目标草案编排；白话结论和专业指标下沉 `components/portfolio-risk/`，不改变任何风险公式、阈值和来源。
- 全站路由矩阵、统一股票身份/术语/状态/AI 规则和验收边界见 `docs/RETAIL_FINAL_PAGE_MATRIX.md`。

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
- **`daily_bars` 索引与保留期（2026-08-21）**：三个索引各有不可替代的职责，**都不能删**——`idx_bar_symbol_date (symbol, market, trade_date) UNIQUE` 既服务单股取序列、也是两处 upsert 的 `ON CONFLICT` 冲突目标（缺失或丢掉 UNIQUE 时每日同步不报错、而是静默插入重复行，属正确性依赖）；`idx_bar_market_date (market, trade_date)` 服务**不带 symbol** 的按市场+日期查询（数据健康缺口检查、宇宙 join、候选审计、保留期清理），这些用不上下面那个索引；`idx_market_symbol_date (market, symbol, trade_date)` 给四处全市场流式读（`buildFactorTable` / `RunFactorIC` / `streamCNDailyBars` / M2 回测）消除 filesort——线上 EXPLAIN 实测 `WHERE market='cn' ORDER BY symbol, trade_date` 走 `idx_bar_market_date` 过滤后仍 `Using filesort` 排 27 万+ 行、单次 4~9 秒（此前代码注释误称「恰合唯一索引序免 filesort」，唯一索引把 `market` 放在中列，等值条件在中列时 MySQL 无法用它满足排序）。三索引现状可用 `SQL_DSN=... go run ./cmd/barscheck` 只读核对（含唯一性、自然键重复、保留期跨度），可直接对生产库执行。
- 保留期 **400 天**（`model.DailyBarRetentionDays`，约 270 个交易日），每日 03:50 按市场分批清理（`StartDailyBarRetentionJob`，每批 5000 行、先取 id 再按主键删以兼容三种方言）。此前无任何按日期的删除路径，每日增约 5500 行、每年约 135 万行只增不减。**400 不是随手取的整数**：系统多处硬依赖 250 个交易日历史（`ma250`/`pos_250`/年内新高、`boardValHistWindow`、`indicatorMaxLimit`、`fflowBarLimit`、`asOfBarLimit`、除权重锚的 `wideBarLimit`），250 交易日按每年约 243 个交易日折算需约 375 自然日，400 天留出长假与停牌余量。**下调此值会静默改变上述因子口径**（窗口不足时按现有样本算，不报错），`TestDailyBarRetentionCutoffCovers250TradingDays` 为此设了防回归下界。
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

- 管理已购入持仓（**带流水的账本**：分批加仓/减仓、加权成本重算、已实现盈亏结转）。
- 管理用户级命名真实/模拟账户、默认账户、归档状态及旧事实的幂等归属迁移。
- 记录买入和卖出。
- 记录真实账户不可变外部现金流，按资金流调整口径构造完整净值。
- 计算当前盈亏。
- 关联 AI 推荐记录。
- 每交易日盘后落资产快照，供资产曲线。
- 组合层暴露分析（行业 / 市值风格 / 估值风格三维分布，读时计算不落库）。
- 组合层历史风险、相关性、风险贡献、压力测试与版本化目标配置/再平衡草案。

**账本口径铁律（B5，改代码前必读）**：`positions.buy_price` 恒为**当前持仓**的加权平均成本、
`positions.quantity` 恒为**当前持仓**数量（全部卖出后为 0）、`buy_fee/buy_tax` 恒为当前持仓尚未
结转的买入费税——全部既有消费方（tracking 的 actual_position 标签、todo 止损、guard 事件、组合
总览）读法零改动。累计口径（一共买过多少 / 卖回多少 / 赚了多少）走 `total_buy_qty` /
`total_buy_cost` / `total_sell_net` / `realized_pnl`；当前剩余仓位的金额权威走
`remaining_cost`，**绝不再用四位 `buy_price × quantity` 结转大额仓位**。升级前已有流水的
持仓按卖出流水恒等式迁移余额，`adjust` 现金分红不参与成本反推。
`position_trades` 是唯一明细来源，汇总值在同一事务内回写 positions。

**相关表（B5~B7 + P2）**：

- `portfolio_accounts`：`user_id / name / kind(real|paper) / currency / status(active|archived) /
  is_default / default_key / archived_at`。`default_key` 只在默认账户上取 `user:kind`，唯一索引保证
  每用户每 kind 至多一个默认账户；严禁 `user_id=0`。首次访问幂等创建默认账户，启动归属迁移只
  回填 `account_id IS NULL/0` 的旧事实，重复启动不新建账户、不改已有归属。归档后历史可读、事实写入
  拒绝；有持仓、流水、快照、现金流、导入批次或目标 revision 时禁止硬删除。
- `portfolio_cash_flows`：`user_id / account_id / type(deposit|withdrawal|fee_adjustment|reversal) /
  amount / trade_date / note / idempotency_key / reversal_of_id`。唯一键 `(user_id, account_id,
  idempotency_key)`；行创建后禁止更新/删除，只能新增金额相反的 reversal。仅 real 账户可写。
- `target_allocation_revisions`：`user_id / account_id / revision / content_hash / items_json`，唯一键
  `(user_id, account_id, revision)`。每次编辑追加不可变 revision，历史重算严格读取该 revision 的
  JSON，不得拿当前配置覆盖。

- `position_trades`：`user_id / account_id / position_id / side(buy|sell|adjust) / price / quantity / fee / tax /
  trade_date / note / realized_pnl / avg_cost_after / quantity_after / backfilled`
  + B8 折算审计列 `avg_cost_before / quantity_before / corporate_action_id / adjust_id`，
  索引 `(user_id, position_id)`。单位：price=元/股、quantity=股、fee/tax=元。
  旧持仓读取时**惰性补建**等价首笔 buy（`backfilled=true`），幂等 + 行锁并发安全 +
  不改动任何既有汇总值。`side=adjust` 是**除权除息折算**（不是买卖）：price/fee/tax 恒 0，
  quantity 记数量变化量，realized_pnl 记到手税前现金分红；前后账面在
  `*_before/*_after` 四列，来源在 `corporate_action_id`——**审计信息一律成列，不塞进 note**。
- `portfolio_snapshots`：`user_id / account_id / kind(real|paper) / trade_date / market_value / cost /
  unrealized_pnl / realized_cum / cash / position_count / partial / missing_count / note`，
  唯一键 `(user_id, account_id, trade_date)`。交易日 16:20 job 按活跃账户幂等 upsert（错峰：16:10 全市场日线、
  16:35 涨停池、18:45 龙虎榜）。**fail-closed**：市值走 `FreshQuotesFor`，stale/失败的标的
  既不进市值也不进成本，该日快照标 `partial` 并记缺口数——绝不用旧价冒充。

**公司行动与事件日历相关表（B8~B9 新增）**：

- `corporate_actions`：`symbol / market / name / report_date / ex_date / record_date / plan_notice_date / notice_date /
  bonus_ratio / transfer_ratio / dividend_pretax / dividend_yield / progress / plan_profile`，
  逻辑稳定身份为上游 `PLAN_NOTICE_DATE`（连同 symbol/market/report_date）；`ex_date` 会从空值
  推进到实施日，也可能被订正，不能承担业务身份。存储护栏索引额外含 `ex_date` 以兼容
  `plan_notice_date` 为空的历史分次实施行，写入统一由 service 按稳定身份就地更新。
  **送转与派息为「每 10 股」口径原值**
  （`bonus_ratio=2` = 每 10 股送 2 股，B8 折算公式直接按此写，**不要预先除以 10**）；
  `dividend_yield` 为百分比数值（上游小数已 ×100）。**该列为 0 表示「该期方案没有股息率」
  （预案/纯送转，上游不给），不是「股息率为零」**——取值一律走 `pickLatestDividendYield`（见 C10 段）。
  同一方案随进度（预案 → 实施分配）更新而非堆积；除权日确定前 `ex_date` 为空串。
- `restricted_releases`：`symbol / market / name / free_date / free_type / free_shares /
  lift_market_cap / free_ratio / total_ratio`，唯一键 `(symbol, market, free_date, free_type)`
  ——**同股同日可有多批不同类型的限售股同时解禁**，只按 `(symbol, free_date)` 去重会少算规模。
  单位：`free_shares`=股、`lift_market_cap`=元、两个 ratio=百分比数值。
  **`free_shares` 是「本次」解禁量**（上游 `CURRENT_FREE_SHARES`），上游同名的 `FREE_SHARES`
  是「解禁后已流通股数」，错用会把规模高估数倍（见 `datasource/emcorpaction.go` 头注的实测验算）。
  完整窗口同步成功后把返回集合视为权威，删除窗口内已改期或取消的旧行；网络、分页或解析失败
  不执行清理。
- `ipo_subscriptions`：`kind(stock|cb) / code / name / apply_code / apply_date / issue_price /
  apply_upper / pay_date / ballot_date / list_date / board / stock_code / stock_name / rating /
  issue_scale_yi`，存储唯一键 `(kind, code, apply_date)`，逻辑稳定身份为 `(kind, code)`：改期
  就地更新申购日，完整窗口成功后按 kind 分源清理取消项和历史重复；股票源失败不清转债（反之亦然）。
  新股与可转债两源合表；
  **`apply_code` 不进唯一键**（沪市新股与可转债的申购代码都与标的代码不同，上游订正应就地更新）。
  `issue_price=0` 表示尚未定价——**不用预估价冒充**。
- `position_corp_adjusts`：除权除息**待确认**调整建议 + 审计。
  `user_id / position_id / corporate_action_id`（唯一键）` / symbol / ex_date / record_date / 方案快照四列 /
  qty_before / entitled_qty / qty_after / cost_before / cost_after / cash_dividend / manual_review /
  review_reason / status / trade_id / confirmed_at / reverted_at`。状态机
  `pending → confirmed → reverted`（可再确认）/ `dismissed`。
  **程序绝不静默改写用户真实账本**：只有用户显式确认才落 `position_trades{side:adjust}` 并改写
  `positions`；建议回补近 30 天漏跑方案，pending/reverted 按当前账面刷新，confirmed/dismissed
  保持终态；撤销仅在「当前账面仍等于折算结果且其后无任何新交易」时被接受，否则明确拒绝。
- `paper_corp_adjusts`：模拟盘**自动**折算审计，唯一键 `(user_id, corporate_action_id)`
  ——虚拟账户无真实后果故自动执行，按 action 唯一键保证重跑不重复发钱。

**持仓卖出决策相关（D14~D17 新增）**：

- `positions` 新增四列 `peak_price / peak_date / peak_from / peak_backfilled`
  ——**持仓期最高价**（D15 移动止盈的地基）。口径全文见 `model.Position.PeakPrice` 注释，
  三条不可回退：①**加仓重置**峰值为加仓价与加仓日（成本已变，加仓前的高点不再是这本账
  赚到过的利润；不重置会让回调加仓当场误报大幅回撤）；②**减仓不重置**（剩余仓位持有期连续）；
  ③**除权除息按价格侧公式同步折算**（见下），撤销时按落库原值还原。
  `peak_backfilled=true` 表示由本地日线（**前复权口径**）回填而非逐日累积——偏差方向偏低
  （偏保守少触发），UI 与 AI 快照必须标注，不得当作精确账面历史最高价。
  更新时点：交易日 16:25 job 读当日 `daily_bars` 的 High **只抬不降**（零上游请求）；
  盘中评估用 `max(落库峰值, 当日 fresh 行情 High)` 参与判定但**不落库**。
- `position_corp_adjusts` 新增 `peak_before / peak_after` 两列：折算前后的峰值。
  **撤销按 `peak_before` 原值还原而非反算公式**——反算留舍入漂移，破坏「撤销后与折算前
  逐字节一致」这条不变式。`peak_before=0` 表示折算时尚无峰值记录，撤销时不动峰值。
- `sell_reviews`：D16 卖出复核待办。`user_id / position_id / symbol / market / name /
  trigger / trade_date / severity / title / detail / buy_price / quantity / price /
  profit_pct / quote_ok / status / resolved_at`，唯一键 `(user_id, position_id, trigger, trade_date)`。
  与另两张表的边界：`guard_events` 是**推送去重台账**（无状态机、不进页面）、
  `alert_events` 是**用户手配规则的命中明细**、`sell_reviews` 是**系统自动发现的持仓利空**
  （用户无需配置任何规则）。**去重维度按 position 而非 symbol**——同一个利空对不同成本的
  两笔仓位含义完全不同。幂等靠 `OnConflict{DoNothing}`：已 resolved/dismissed 的行绝不能被
  下轮扫描拉回 open（同 B8 先例）。`detail` 必须回答「这件事对**我这笔持仓**意味着什么」
  （含我的成本与浮盈亏）；`quote_ok=false` 时 `price/profit_pct` 恒 0 且 detail 如实声明
  行情不可用——绝不用旧价冒充。
- 浏览器通知分为事实、设备和投递三层：`browser_notification_events` 用
  `(user_id, fact_key)` 固化稳定来源类型、来源 ID、分类、等级、站内路由和创建时间；
  `browser_notification_devices` 与 `web_push_subscriptions` 以用户+本地随机设备标识隔离多设备，
  endpoint/p256dh/auth 全部用 `ENCRYPTION_KEY` 加密且 API 不回显；
  `browser_notification_deliveries` 用 `(user_id, device_id, event_id)` 保证同一事实同一设备只投递一次。
  review 升 urgent 使用包含新评估 ID/等级/事实 hash 的新 fact key，因此允许再次提醒。任一设备失败不影响
  其他设备或业务事实，Web Push 返回 404/410 会禁用对应订阅。

**除权除息折算公式（B8，改动前先读；D15 起含峰值）**：

```
有权数量 = 股权登记日收盘数量（流水按业务日期重建）
新数量 = 当前数量 + 有权数量 × (送股 + 转增) / 10
新成本 = 原成本 × 当前数量 / 新数量
现金分红 = 有权数量 × 每10股派息 / 10
新峰值 = (原峰值 − 每10股派息 / 10) / (1 + (送股 + 转增) / 10)
```

现金分红只计入 `realized_pnl`（真金到账，与卖出兑现同属「已实现」），**不再同时冲减
剩余成本**，否则最终收益会重复计算一次；`remaining_cost` 保持不变，仅由新数量摊薄每股成本。
`total_buy_cost / total_buy_qty` **保持不变**——送转不是「又买了一次」，改动它们会让
「一共投入多少 / 已平仓收益率」全部失真。
峰值按市场价格的标准除权公式移动，避免除权日出现假回撤；它会扣除每股派息，而账本成本
不扣现金分红，因此两者不再是同一公式。

**时序边界**：纯现金分红不改变数量与成本，登记日有权的仓位即使后来已平仓，仍可生成
零数量建议，确认后只增加 `realized_pnl`；送转必须先于除权日后的卖出入账。真实盘卖出前会
阻断未处理送转，模拟盘下单前先自动结算；若历史上已经先卖出或清仓，当前版本不会拿现有
聚合态倒序套公式，而是落一条 `manual_review=true` 的待处理记录并明确说明原因。此类记录禁止
自动确认，用户明确确认已自行核对后可忽略并解除交易拦截；忽略不会自动修正账本（完整自动
修复需要按业务日期重放全部流水）。

**行业 / 风格暴露（C13，第六十二批；读时计算零落库、零新表）**：

`GET /api/positions/overview` 的 `exposure` 段给出三维分布（`service/positionexposure.go`）：

- **行业**走宇宙快照 `stock_universe_dailies.industry`（`industriesFor`）；**市值风格**与
  **估值风格**走 `MarketService.ValuationsFor` 的 `TotalCap` / `PETTM`（腾讯免费估值，缓存 60s）。
- 分档：大盘 ≥500 亿 / 中盘 100~500 亿 / 小盘 <100 亿（元）；PE ≤15 低 / 15~30 中 / >30 高 /
  **严格负值**为亏损。**`PE==0` 是估值缺失不是亏损**（全项目铁律）。
- **口径三条**：①占比基数 = 已定价（fresh 行情）持仓市值合计，**含「未知」桶**，各桶之和恒 100%
  ——按「已知部分」做分母会让 20% 覆盖率的组合显示成「银行 100%」；②某维一条数据都没有时
  `available=false` 整块缺席（「不知道」≠「分布均匀」），部分缺失显式成「未知」桶且**恒排最后**；
  ③集中度提示须过覆盖率闸门（≥60%）——只查到一小部分持仓的行业就断言赛道集中是拿局部当整体。
- 行情 stale/失败的持仓既不进市值也不进分布（与 `Overview.TotalValue` 同口径），未计入笔数在
  `base_note` 里如实声明。

**组合风险、压力测试与再平衡（P2，2026-08-12）**：

- `service/portfoliorisk_core.go` 只含稳定纯函数；`portfoliorisk.go` 负责按 `user_id + account_id`
  读取快照、现金流、当前持仓和本地 `daily_bars`。无插值、partial 点不参与完整区间，样本不足和
  基准缺失均返回 `unavailable + reason`，不得用 0 冒充。参数快照固定
  `annualization/risk_free_rate_pct/window_days/benchmark_code/as_of/version` 并计算稳定 hash。
- 区间收益为 `(V_end - external_flow_at_end) / V_start - 1`，TWR 链式相乘；真实账户现金由外部
  现金流加买卖/费税/现金分红重建，缺有效初始入金即拒绝完整总资产、TWR 与相关派生指标。
  Beta/年化 Jensen Alpha 只在组合与指定基准共同交易日上计算。
- 风险贡献按全部持仓共同收益样本协方差矩阵计算：`sigma_p=sqrt(w'Σw)`、
  `MRC_i=(Σw)_i/sigma_p`、`CRC_i=w_i*MRC_i`、`PCR_i=CRC_i/sigma_p`；权重分母为完整组合总资产，
  现金保留为零波动权重。任一持仓缺 fresh 价格、真实现金不完整或共同样本不足时整块 fail-closed。
- 压力测试只读地应用市场、行业、单票或计划止损冲击并返回损失金额/比例、逐持仓贡献和未知项。
  目标配置草案按 symbol/industry 比较当前与目标，以 fresh 价格换算 A 股整百股，程序估算费税；
  stale、停牌、涨停和缺价返回 unavailable。两者均不写 `positions/position_trades`、不提醒、
  不调用 LLM、不创建 JobRun 或 ResearchArtifact。它们是即时可重算的有界研究响应，不是异步业务事实。

账户接口：

- `GET/POST /api/portfolios`、`PUT/DELETE /api/portfolios/:id`
- `POST /api/portfolios/:id/archive`、`POST /api/portfolios/:id/default`
- `GET /api/portfolios/:id/{overview,risk,holdings,cash-flows,targets,rebalance}`
- `POST /api/portfolios/:id/cash-flows`、`POST /api/portfolios/:id/cash-flows/:flow_id/reverse`
- `POST /api/portfolios/:id/stress-tests`、`POST /api/portfolios/:id/targets`

旧 positions/paper/curve/stats/import 接口继续兼容可选 `account_id`；未传时只解析本人同 kind 默认账户。
显式 ID 必须同时匹配本人和 kind，跨用户、跨账户、real/paper 混用均按不存在处理。

**股息率（C10，第六十二批）**：`corporate_actions.dividend_yield` 的**唯一**取值口径是
`service/dividendyield.go` 的 `pickLatestDividendYield`——取最近一期 `dividend_yield > 0`
且报告期在 800 天窗口内的方案，随值透出报告期 as-of。三个消费方（个股详情估值区 /
AI 个股快照 `corp_events.latest_dividend_yield_pct` / 选股因子 `div_yield`）共用，
不得各自从 `actions` 里挑。**取不到就整项缺席，绝不回退 0**（上游对预案与纯送转方案不给
股息率、落库为 0；把 0 当答案会被读成「这家公司不分红」）。

接口示例：

- `GET /api/positions`（含止损/分析时效富化）/ `GET /api/positions/overview`（组合总览+风控信号+**行业/风格暴露**）
- `POST /api/positions`
- `POST /api/positions/import`（CSV 批量导入，multipart，逐行校验+错误行报告，上限 500 行，限流 10/min）
- `PUT /api/positions/:id`
- `POST /api/positions/:id/close`（= 卖出全部剩余数量的减仓笔，走同一流水逻辑）
- `GET /api/positions/:id/trades` / `POST /api/positions/:id/trades`（B5 流水明细 / 加仓·减仓）
- `GET /api/positions/stats?range=`（B6 个人交易复盘统计，纯读时聚合）
- `GET /api/positions/curve?days=` / `GET /api/paper/curve?days=`（B7 资产曲线，读盘后快照）
- `GET /api/positions/corp-adjusts?status=` / `POST /api/positions/corp-adjusts/:id/:action`
  （B8 除权除息待确认折算；action = confirm | revert | dismiss）
- `GET /api/positions/sell-reviews?status=` / `PUT /api/positions/sell-reviews/:id/status`
  （D16 卖出复核清单与状态机；status = open | resolved | dismissed）
- `POST /api/positions/advice`（D17 AI 逐笔卖出建议，**走 llm_tasks 后台任务秒回任务 id**，
  前端 `GET /api/llm-tasks/:id` 轮询；限流 15/min）。当前契约为 `pa2 / position_advice.v2`：
  prompt 给每笔仓位显式 `position_id`，模型必须原样回传 `position_id + symbol`，服务端双重
  校验并按 position ID 去重，同代码多仓不得合并。
- `GET /api/events/calendar?days=`（B9 未来 N 天事件日历：持仓/自选的解禁·除权·财报 + 全市场打新）
- `GET /api/markets/:market/stocks/:symbol/corp-events`（B9 个股解禁 / 分红，公开信息无用户隔离）
- `DELETE /api/positions/:id`（级联删流水）
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
- `GET /api/browser-notifications/config`，`PUT /api/browser-notifications/settings`，
  `POST/DELETE /api/browser-notifications/subscriptions`，`GET /api/browser-notifications/events`，
  `PUT /api/browser-notifications/events/:id/ack`，`POST /api/browser-notifications/test`
- `GET/PUT /api/admin/users/:id/quota`（管理员查看/调整用户 AI 次数上限、手工清零已用量；2026-07-03 起配额为次数制，token 仅审计）
- `GET /api/export/:kind`（kind=positions|watchlist|recommendations|analyses，CSV 带 BOM，限流 10/min）
- `GET /api/admin/datasources`（数据源健康滑窗状态，S1；`data_source_configs` 死表已删，数据源无用户级配置）

前端唯一可编辑通知入口为 `/settings?tab=notifications`：推送总闸、智能守护和 Server酱/Webhook/ntfy 通道 CRUD、启停、删除、测试均复用以上接口。通道管理只是从提醒页集中到设置页，**编辑能力没有删除**；同类型编辑时敏感字段留空保留原密文，ntfy 三项全部留空保留整套原配置，重新填写时校验完整地址与 Topic。`/alerts` 只负责提醒规则、立即检查和命中历史，并提供“通知接收设置”跳转；不得再放第二套通道表单。

### 5.8.0 浏览器通知（2026-08-18）

- `GET /api/browser-notifications/config` 返回 VAPID 可用状态、分类偏好和脱敏设备列表；
  `POST/DELETE /subscriptions` 新增、更新或移除本人设备，`GET /events` + `PUT /events/:id/ack`
  为前台事件轮询与逐设备确认，`POST /test` 只测试当前设备。全部接口按当前用户与设备归属校验。
- 网站打开或后台标签页时，AppShell 每 20 秒轮询待投递事件并用 Notification API 发系统通知，
  同时给出克制的站内提示；权限请求只允许设置页按钮触发。VAPID 未配置或浏览器不支持 PushManager
  不影响此前台降级路径。
- 网站关闭后的投递由 `/sw.js` 的 `push`/`notificationclick` 处理。点击只接受同源相对路径；未登录时
  路由守卫带 `redirect` 进入登录，密码/GitHub 登录完成后经同一站内路径校验恢复目标。
- VAPID 只从 `VAPID_PUBLIC_KEY/VAPID_PRIVATE_KEY/VAPID_SUBJECT` 读取，并校验 P-256 密钥对与
  `mailto:/https:` subject；三项缺失或无效时服务端正常启动且不临时生成密钥。生产 Web Push 要求 HTTPS
  （localhost 可用于本地），iOS/iPadOS 通常还要求先添加到主屏幕。
- 浏览器事件至少消费 `PositionExitAssessment` review/urgent、`AlertEvent` 手工规则命中和 `GuardEvent`
  智能守护。卖出风险、手工提醒默认开启，智能守护分类默认关闭；三者继续受 `UserPreference.EnableNotify`
  总闸控制。浏览器设备本身就是有效目的地，不要求先配置外部通道。

### 5.8.1 条件提醒与命中事件（阶段 7 + 批次 H；D14/D15 起含持仓类）

- `GET/POST /api/alerts`，`PUT/DELETE /api/alerts/:id`，`PUT /api/alerts/:id/status`（暂停/恢复），`POST /api/alerts/evaluate`（手动评估，限流 20/min）
- 提醒类型 kind：price（到价）/ pct_change（异动）/ ma（均线）/ breakout（突破）/ **volume_surge**（当日量≥N 倍 20 日均量）/ **amplitude**（当日振幅，优先腾讯估值源、缺则 (high-low)/prev_close 回退）/ **earn_date**（财报披露临近 ≤N 日，F1）/ **earn_fcst**（新业绩预告发布，F1）——财报两类不走盘中 15min 评估，由 finance job 每日一评
- **持仓卖出决策类（D14/D15）**：`cost_gain`（现价相对我的成本涨 ≥N%）/ `cost_drawdown`（跌 ≥N%）/ `peak_drawdown`（自持仓期最高价回撤 ≥N%）。三条与上面各类的结构性差异（改代码前必读）：
  - **评估单元是「一笔持仓」不是「一个 symbol」**：`symbol` 可为空 = 绑定「我的全部持仓」，评估时按当前 holding 逐仓展开（同一标的多笔仓位各自判定各自的成本）；走 `evaluatePositionRules` 独立编排，不能混进按 `rule.Symbol` 拉行情的 `evaluateRules`（空代码会拉挂）。
  - **成本恒取 `positions.buy_price`**（B5 口径铁律），用户无需手工填任何价位——这正是旧「持仓止损」待办形同虚设的原因（必须先填 `plan_stop_loss`）。
  - `op` 无方向语义统一存 gte；**`Once` 强制 false**（一条规则覆盖多笔持仓，命中一笔就暂停整条会让其余持仓失联）；threshold 一律正数百分比。
  - fail-closed：无 fresh 行情的持仓本轮跳过；盘中用当日 high 抬升峰值参与判定但**不落库**（落库交给 16:25 盘后那一次）。
- 命中明细状态机（`alert_events`，unread/read/dismissed）：`GET /api/alerts/events?status=&limit=`，`PUT /api/alerts/events/:id/status`，`PUT /api/alerts/events/read-all`
- **P0-5 命中上下文（代码已实现、待线上验收）**：新事件写 `context_version=1` 与最大 4096 字节的白名单 JSON；普通行情只保留实际用于判定的 quote、最新 bar 摘要和 MA/前高前低/量比/振幅指标，财报只保留预约披露或业绩预告结构化字段，持仓只保留命中持仓 ID、加权成本、峰值与 fresh quote。缺失来源/时点进入 `unknown` 或省略；不得放 token/cookie/密钥、请求/响应正文或上游原始对象。旧事件保持 `0/空串`，API 显示上下文不可用，不追溯猜测。
- **详情与深链**：`GET /api/alerts/events/:id` 必须同时按 `id+user_id` 查询，返回解析后的 `context/context_available` 与稳定 `deep_link=/alerts?event_id=<id>`，不返回原始 ContextJSON。列表与状态更新使用同一安全视图。Today 的提醒待办返回同一 deep_link；仅打开详情不改变 unread 状态。主动推送正文最多保留 5 条有界摘要及各自事件深链，不复制完整快照。
- **持仓类事件去重键 `(rule_id, position_id, trade_date)`（D14 起下沉）**：绑「全部持仓」的规则同一天会逐仓命中，同一代码也可能有多笔不同成本的持仓；按 symbol 判重会吞掉后面的仓位。`alert_events` 因此新增 `trade_date`（旧行空串，历史不追溯）与 `position_id`（持仓类命中的那一笔，其余恒 0）。非持仓类仍由规则行 `triggered_at` 做同日去重。
- **模板向导只是一层无损映射**：`AlertWizard.vue` 与 `alertTemplates.ts` 将 11 个现有 kind 组织为模板→范围→参数→确认，不新增规则类型或第二套评估语义。编辑旧规则时，只有 kind/op 能被模板完整表达才反推模板；缩量、低振幅或未知组合进入专家模式并保留原字段。持仓模板只能选当前持仓或空 symbol 的“我的全部持仓”，最终约束仍以后端 `AlertService.validate` 为权威。
- Today 收件箱的提醒来源仍是 `alert_events`，`GuardEvent` 只保存主动推送去重台账，绝不直接作为页面事实。提醒“收下”调用 `AlertService.SetEventStatus`，推荐复盘调用 `TrackingService.AckReview`，卖出复核调用 `SetSellReviewStatus`；持仓止损、定期复盘、逻辑卡和公司行动不能由收件箱伪造完成，只能稍后或深链回原页面处理。

### 5.8.2 Today 统一收件箱（D18 / Top50 U13/U18）

`GET /api/todos?scope=&status=&source=&page=&page_size=&history_days=&limit=` 继续由现有 `TodoService` 聚合，不建立平行提醒引擎。`scope` 保持 `ledger/research/market/all` 兼容；`status` 为 `needs_action/awareness/completed/open/all`。Home 请求 `scope=all&status=needs_action&limit=3`，直接消费后端前三条，前端不复制排序。

来源至少包括 `AlertEvent`、`RecommendationStatus`、持仓当前风险/复盘状态、`ThesisCard`、`PositionCorpAdjust`、`IpoSubscription`、`SellReview` 和脱敏 `JobFailureNotification`。任务失败投影只按白名单 `error_code` 重建摘要，不读取 `JobRun.Error/RequestSnapshot`，旧通知行即使含异常正文也不会透传。

`todo_inbox_states` 是唯一新增状态表，字段只含 `user_id/source_kind/source_id/source_version/read/snoozed_until/muted_date/created_at/updated_at`。唯一键为用户+来源类型+来源 ID；标题、正文、股票和任务请求始终从原业务事实即时读取。状态更新与详情均按 `user_id` 隔离。`PUT /api/todos/actions` 支持最多 100 个来源引用的 `read/snooze/mute_today`，请求必须携当前来源版本，旧版本操作会被拒绝。

同股同类同日事项按展示组返回，`children` 保留每个子事件的来源身份、正文和原业务深链。组静默会为当前全部子来源版本落轻量状态，因此同日同类的新普通事件可降噪；任何子来源版本变化、新交易日或严重度升级都会重新出现。稍后默认到次日 09:00，到期自动恢复。

排序完全由服务端决定：持仓硬风险与账本错误优先，其次有截止时间的处理项，再到普通处理、研究与市场知晓项；同层依次按截止时间、严重度、scope、事件时间、kind、symbol、来源类型和 ID 打破平局。任一来源失败时保留其它来源并返回 `complete=false/partial=true/errors`，空数组不能冒充全部完成。完成历史只查最近 30 天（调用方可调，后端上限 90 天）并分页。

用户反馈「现在提醒的是没什么用的」，噪音主体是推荐复盘（AI 推过就追踪、没买也天天提示）。
范围只是**消费出口过滤器**，全量数据照常生成，一条不删：

| scope | 含哪些 | 消费方 |
| --- | --- | --- |
| `ledger` | 卖出复核 / 持仓止损 / 持仓复盘 / 除权折算 / **持仓标的**的提醒与逻辑卡 | Today 范围筛选、账本徽标 |
| `research` | 推荐复盘 / **非持仓标的**的提醒与逻辑卡 | 推荐追踪页「待复盘提示」卡 |
| `market` | 打新（与我买没买无关） | Today “仅知晓”筛选；当日不在后续事件区重复展示 |

**计数按过滤后算**（所见即所计）——否则徽标 12 条、点进去 3 条会让用户以为丢了东西；
`scope_counts` 另外给出各范围全量条数供「另有 N 条在别处」提示，`filtered` 为被过滤条数。

### 5.9 Job Service

职责：

- 定时刷新市场快照。
- 更新股票行情缓存。
- 更新推荐追踪状态。
- 清理过期缓存。
- 生成每日市场摘要，后续可选。

**盘后 job 顺序（错峰铁律，改时点前先读）**：16:10 全市场日线 → 16:20 资产快照 →
**16:25 持仓期峰值更新**（读当日 `daily_bars`，零上游）→ 16:35 涨停池+人气 →
17:05 盘中因子 → 18:45 龙虎榜 → 19:05 财报+公告 → 19:25 公司行动与打新 →
19:35 盘后守护轮 → **19:40 卖出复核轮**。
后两者顺序不可交换：两轮消费同一批本地数据，守护轮先落 `guard_events` 台账，
复核轮的 `pos_ma_break` / `pos_lhb_sell` 推送才不会与它交叉。

### 5.9.1 Task Center Service（Top50 P0-2A/P0-2B-2B）

`GET /api/tasks` 是统一作业与 legacy 异步业务表之上的轻量聚合层：默认按当前用户汇总 JobRun 与未迁移的兼容结果；管理员显式传 `include_system=1` 时，再投影 system JobRun，并只补充 `job_run_id IS NULL` 的 legacy DataSyncLog。系统行以 JobRun ID 执行取消/重跑，以 DataSyncLog ID 定位结果，因而不会双行。各来源各执行一次有界、字段白名单查询，不读取请求/结果正文或数据快照。

所有来源统一映射状态、耗时、错误码、恢复建议与仅含记录 ID 的结果深链。15 分钟未更新且**没有 queued/running JobRun 引用**的 legacy 用户侧 `processing` 任务，才由各自业务服务按同一口径惰性收敛为 `failed`；活跃统一作业即使排队超过 15 分钟也由自身 CAS、取消和恢复规则收敛，旧清理器不得抢写业务结果。顶栏最近任务与运行中数量分别查询，达到 100 条上限时只能展示“至少 N 项”，不能把截断值当精确总数。

P0-2B-2B 已将六类系统维护任务接入相同的 JobRun/JobStep/恢复/背压运行时；P0-4 结果阶段又加入 `screener_scan/strategy_backtest`，用户作业共九类。用户作业 owner 为 `user` 且 `user_id>0`；系统作业 owner 为 `system` 且 `user_id IS NULL`，管理员手动触发另记 `triggered_by`。普通用户只能访问自己的 user JobRun；启用管理员还可审计、取消和失败重跑 system JobRun。`GET /api/admin/jobs/metrics` 仅聚合 kind/status/count、最老 queued_at 和进程容量，不选择 request snapshot。用户 SSE 鉴权与续传契约不变；系统任务当前由管理员任务页轮询同一事实，不额外伪造事件进度。

P0-5 为 user owner 的 failed 终态增加 `job_failure_notifications` 幂等事实：`job_run_id` 唯一，system owner 不建通知行；同用户同 kind 以 5 分钟滑动窗口合并，稳定 group key 负责跨实例并发首条唯一，窗口轮换时释放旧 root，首行记录 `merge_count`，后续行只记 `merge_root_id` 而不再次外发。只有现有 `enable_notify` 总闸和至少一个启用通道同时满足时才声明外发，否则记 suppressed；不新增平行通知配置。失败终态事务提交后，通知行再以 `claimed→dispatching→attempted` CAS 抢占唯一发送权并 best-effort 调现有 NotifyService；`attempted` 只证明调用已发生，NotifyService 当前不返回逐通道聚合结果，因此不得把它解释成确认送达。外发失败不得改写 JobRun。正文只含任务 kind、白名单错误码和 `/tasks?job_id=<id>`；任务列表按 `job_id+user_id` 精确返回该行，禁止读取或推送 request snapshot、模型正文及原始错误详情。

### 5.10 扩展模块（N/F/T/S/M/P3 批次 + 2026-07 杂项批）

各批次陆续落地的独立 service 模块（批次交付记录见 [DEVELOPMENT_PLAN](DEVELOPMENT_PLAN.md)，接口速查见 REFERENCE_ANALYSIS §6）：

- **news / newsai / newsevent**（N1/N2）：7×24 快讯采集（采集间隔管理后台可配 1~120 分钟，自调度循环下一轮生效）+ LLM 情绪增强（个股关联/利好利空/当日聚合情绪分；「自动 LLM 分析」总闸关闭时走纯关键词规则零 token）+ 事件抽取；`/news` 页与个股详情消息面卡。
- **finance / finance_f10**（F1/F2）：财报日历/业绩预告/快报增量刷新 + F10 主要财务指标与三大报表关键科目按需缓存 + 公告采集，入个股详情财务块、长线推荐 fin 因子、财报提醒。
- **indicator / chip**（T1）：MACD/BOLL/RSI/ATR 纯函数指标库（Wilder 口径）、筹码峰三角衰减复算、五维技术评分升级，供个股详情副图与推荐量化评分。
- **riskgate / breaker / health**（S1）：风险闸门（ST/一字板/流动性/小市值进 AI prompt 与前端标签）、东财 push2 族域名断路器、数据源健康滑窗；问答流式输出同批。
- **marketwide / factortable / screener**（M1）：全市场日线地基（宇宙字典/历史初始化/除权双层检测重锚）、**63 因子**列式宽表（C10/C12 起含 `div_yield` 股息率与 10 个 K 线形态布尔因子，见 `kpattern.go`；**形态是描述性因子，不进任何评分权重**）、条件树 DSL 选股（21 内置白话策略+自定义策略），`/screener` 页；推荐候选池的 `strategy_signal` 当前仍只消费代码内置策略映射，自定义 revision 尚未进入推荐。自定义策略以 `screener_strategy_revisions` 追加快照为执行权威，主表只保留当前 revision 指针和兼容投影；升级时幂等补建存量 revision 1。编辑采用 `base_revision_id` 乐观锁并只追加 revision，删除语义为归档，历史快照不随之删除。扫描提交后立即返回 JobRun/结果引用，成功正文进入 `strategy_run_results`，本人可通过 `/api/screener/results/:id` 稳定找回。
- **backtest / analysis_asof**（M2）：回测时光机（A 股约束五件套/无未来泄露切片复算）、历史推荐批次回验 α 分布、分析 as_of 回溯诊断与 hindsight 事后核验，`/backtest` 页。自定义策略扫描/回测在请求开始时固定 `strategy_revision_id` 对应的条件树；未显式指定时只解析一次当时的当前指针，运行期间的策略编辑不会改变本次条件与 hash。策略回测正文进入同一 `strategy_run_results` 事实，本人可通过 `/api/backtest/results/:id` 找回；旧结果和失败重跑只读保存时的规范化请求及 revision/hash，不重新解析当前策略。推荐批次回验接口保持既有同步口径。
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
>
> **思考档位可配（2026-08-20，推翻此前「不暴露用户配置」的定夺）**：LLM 配置新增 `reasoning_effort`——chat 端发平铺 `reasoning_effort`、responses 端发嵌套 `reasoning.effort`（`addReasoningEffortField`）。**空值 = 不发送该参数**，沿用网关/模型默认档位；前端新建配置预填 `max`，下拉 low/medium/high/xhigh/max/ultra 且允许自定义键入，存量配置保持空、未做批量迁移。取值**不做枚举校验**（只校验格式：小写字母数字与 `-_`、≤16 字符），因为各家档位在持续扩且中转网关口径不一，与 provider 同样按「用户自由填写 + 运行时能力观察」处理。
>
> 上游拒绝档位时**无害降级**：四处 fallback 点（chat/responses × 流式/非流式）去参重试，业务照常出结果。**两类拒绝都覆盖**——①参数本身不认（非推理模型）②所配档位不在取值集合内（`max`/`ultra` 不是 OpenAI 官方档位，官方为 none/minimal/low/medium/high/xhigh，o 系列只认 low/medium/high，故这是常态路径）。第 ②类的报错常不含字段名，`looksLikeUnsupportedReasoningEffort` 用排除法归因（档位是唯一取值由用户自由填写的枚举参数）。这与 `max_tokens` **故意排除「值超限」的做法相反**：去掉 token 预算有害，去掉档位只是回到默认档位。去参重试成功后才落 `capReasoningEffort` 观察（12h TTL，声明化路由据此直接不发）；值被拒时另打 `SysWarn` 带上游列出的合法档位，连接测试结果也如实标注「已接受 / 上游不接受」——否则用户会以为档位生效、实际一直在用网关默认档位。
>
> **配置面板（2026-08-20）**：编辑弹框改为右侧抽屉（移动端底部上拉），分「基础连接 / 模型 / 生成参数 / 默认与流式」四段并逐项配说明，测试结果就地展示。模型支持 `POST /api/llm-config-models` 从上游 `/v1/models` 拉取后选择，或勾「自定义模型」手填；密钥三态（入参 → 该配置已存密钥 → 报错）让「改了 Base URL 不必重填 key」成立。列表行加「设为默认」（`POST /api/llm-configs/:id/default`，与增改同一「单默认」事务纪律）。表单撤除「类型」选择（原 openai/other 走的本就是同一条兼容路径），但**后端 `provider` 字段保留**——它仍参与能力矩阵初始声明、审计标签与校准分层键，新建填 `openai`、更新入参空则沿用原值。

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

生成类 LLM 均为异步任务。P0-4 结果阶段起，扫描/条件树回测也加入统一长任务范围，九类用户任务以及六类系统数据维护/因子任务共用 `JobRun` 有界运行时；原业务表、DataSyncLog 和 `strategy_run_results` 继续保存结果事实与历史深链。HTTP 入口只做确定性校验并立即返回，浏览器断开与页面刷新不取消后台任务。定时系统生产者遇队列背压使用去重重试器，拒绝时尚未创建 JobRun 或 processing 结果；批任务继续逐项隔离并在统一终态事务保存计数。

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
- 自定义选股策略 hash 对名称、说明、周期、风险和结构化规范后的完整条件树计算 SHA-256，不直接使用用户原始 JSON 文本；对象键序、空白和等价数字写法不造成 hash 漂移。
- `(strategy_id, revision)` 唯一；应用层通过 GORM 更新/删除 hooks 与字段仅允许创建的权限阻止历史行被改写或删除，当前尚无数据库 trigger。对当前快照的重复保存复用当前 revision，内容变化（包括 A→B→A）按时间顺序继续追加。历史查询和显式旧版本执行均同时校验 `user_id` 与 `strategy_id`。

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

1. **异步任务化**：分析、推荐、日报、问答、AI 对比、持仓建议和白话选股解析均以 `JobRun` 为作业事实；前三类继续以各自业务表的 `processing/终态` 承载结果正文和旧深链，后四类以 `llm_tasks` 保留兼容结果。同步入口不采集行情、不调用模型；统一固定 4 worker、32 总在途，满载稳定返回 `job_queue_busy`，各任务保留独立总 deadline。
2. **真正流式调用**：所有业务 `chatCompletion` 先走 Chat/Responses SSE，`stream=true` 与防缓冲请求头在每个 fallback 请求中都保留；流式 client 无整体 HTTP timeout。只有上游明确以 4xx 拒绝 stream 时才回落整包请求，不能在 504、流中断或调用 Context 取消后静默改走非流式。
3. **单次调用边界**：模块预算取 `min(用户 max_tokens, 模块上限)`；analysis/recommendation/qa/news=2500，trade_plan/rec_review/rec_bear/daily_report=1500，analysis_review/compare=1000，screener_parse=2000。全部结构化模块最多 1 次 repair，坏输出默认只回灌 600 字（日报 800 字）；业务 prompt 不再用字数、句数或性能型数组条数压缩模型回答，JSON schema、推荐数量、固定角色和逐 ID 映射等业务契约仍保留。Chat 端拒绝 `max_tokens` 时必须携同值改用 `max_completion_tokens`；Responses 始终携带 `max_output_tokens`。两种字段都不支持则失败，禁止删除 token 参数退成无上限生成。
4. **业务降级边界**：只有推荐维持既有量化降级（ATR 规则计划价、action 恒 watch、置信度 low、`degraded_source=quant_fallback`）；鉴权/路径/配额类确定性错误直接失败。日报两路并行并保留旧报告回滚语义；其他模块按自身失败/结构化降级契约收尾。

### 6.11 统一作业事实与研究工件（P0-2B-1 / P0-2B-2B / P0-4）

P0-2B-1/P0-2B-2A 覆盖七类用户任务；P0-2B-2B 在同一运行时加入 `sync_daily_bars / backfill_calendar / snapshot_market / sync_market_wide / init_market_history / factor_rebuild`，并建立一期不可变 `ResearchArtifact`。P0-4 结果阶段再接入 `screener_scan / strategy_backtest`，用户作业增至九类，并用 `strategy_run_results` 保存不可变业务结果。本节描述代码契约，生产 MySQL、真实队列压力和长任务恢复仍待线上验收。

- **owner 与事实模型**：`job_runs.owner_type` 只允许应用层语义上的 user/system；user 必须绑定正数 `user_id`，system 必须为 SQL NULL，`triggered_by` 只记录管理员手动触发者。JobEvent 继承相同 owner。JobRun 保存 kind、版本化请求、状态、父作业、错误、trace、业务计数和结果引用；步骤只记录真实发生阶段，状态仅允许 `queued/running/success/degraded/failed/canceled`，终态以 CAS 收敛且不可回退。
- **快照、权限与结果绑定**：request snapshot 最大 16KiB，拒绝密钥、Authorization、prompt/消息和数据/结果正文。`result_type` 支持 `llm_task/analysis/recommendation/daily_report/data_sync/strategy_run`。扫描/回测的 JobRun 快照只含 schema 版本、稳定策略/revision 引用、content hash 和规范化请求 hash；包含实际执行条件树的完整规范化请求只在本人 `strategy_run_results` 中保存，结果正文最大 2MiB。worker 从该业务事实读取冻结树并剥离 key/id/current pointer 后执行，内置策略代码升级或 A→B 编辑都不能改变旧作业。用户作业执行时重判账号状态/角色；普通用户无法读取、取消或重跑系统 ID，管理员只扩展到 system owner，不借此读取其他用户结果。DataSyncLog 或 StrategyRunResult 占位、JobRun 和引用同事务创建；终态事务只更新已绑定的同一结果行，不产生第二条结果事实。
- **幂等与容量**：用户按 owner/kind/request hash 防重，系统按 kind 排他防重；active key 仅 queued/running 持有。管理员重复手动触发同 kind 系统任务时返回既有 JobRun 与 `started=false`，不得把旧任务伪报成新计划已启动。运行时固定 4 worker、最多 32 总在途。用户入口满载返回 `job_queue_busy`；系统定时器对该错误去重重试，接受前不建事实行。管理员指标按 kind/status 聚合并显示最老排队与容量，查询禁止选择请求快照。
- **取消与恢复**：queued 取消用 CAS 直接进入 canceled；running 先持久化 `cancel_requested`，本实例立即取消 Context，其他实例/恢复路径通过数据库轮询协作取消。成功持久化与取消在同一事务条件上竞争，只有一个终态获胜。启动时遗留 running 明确收敛为 `job_interrupted`（已请求取消者收敛 canceled），queued 按 ID 升序恢复，容量外的 queued 保持排队并随槽位释放续排。
- **重跑**：失败且已注册的用户或系统 handler 可重跑。新 run 从旧快照创建并写 `parent_id`，旧 run 不修改；扫描/回测子作业同时复制父结果的完整规范化请求、稳定策略身份和 revision/hash，绝不查询当前策略指针。系统重跑的 `triggered_by` 是执行该动作的管理员。已有等价用户请求或同 kind 系统任务在途时返回 `job_already_running`。
- **用户失败通知**：仅 failed 且 owner=user 的已提交 JobRun 可进入通知声明。`job_failure_notifications.job_run_id` 保证 CAS、恢复和重复观察下每个 run 至多一次；同用户同 kind 的短窗失败合并。通知开关复用 UserPreference/NotifyChannel，错误正文不进入推送。通知投递是终态之后的旁路，任何数据库或网络失败都只能记录告警，不能回滚或覆盖 JobRun 终态。
- **事件与前端**：`GET /api/tasks/events` 位于 JWT 中间件后，支持 `Last-Event-ID`、单调 `id:`、`event: job` 与 15s heartbeat。前端用 `fetch` 的 `Authorization` 请求头读取流，token 从不进入 URL；断线指数重连并保留串行轮询回退。任务中心优先列 JobRun，查询旧 `llm_tasks` 与三类业务表时排除已被 `result_type/result_id` 引用的行，从而避免双行；扫描/回测深链使用业务 `result_id` 跳到 `/screener?result_id=` 或 `/backtest?result_id=`。两页历史默认 20 条、最多 100 条，列表不选择请求/结果正文；详情同时按 `id+user_id+kind` 查询，失败刷新保留最近成功视图和历史恢复入口。
- **ResearchArtifact**：只保存 `type/schema_version/subject/as_of/available_at/content_hash/source_refs/storage_ref/job_run_id/owner/created_at`。分析、推荐、日报、策略扫描和策略回测在业务结果校验与 JobRun 成功终态同一事务幂等建索引；扫描/回测的 `storage_ref` 必须是 `strategy_run_results:<id>`，hash 必须来自当次已持久结果正文，source refs 只含作业、结果、策略、revision 与请求 hash。失败、取消、临时响应和历史存量不建工件。正文、候选池、条件树、扫描命中、交易明细和快照不复制；分析 subject 对非个股模块使用真实 target，不能退化成空 symbol。
- **保留策略**：每日 03:35 只删除“对应 JobRun 已终态且事件早于 30 天”的 JobEvent。JobRun/request snapshot 为重跑与审计保留，JobStep 随 JobRun 保留，业务快照由原事实表负责，ResearchArtifact 一期永久保留且拒绝更新/删除。任何后续分层归档必须先证明不会切断 parent 重跑、审计和 source/storage 血缘。

### 6.12 数据覆盖、主动探测与解冷（P0-3A / P0-3B）

数据源支持面只认 `provider × capability × market` 代码注册表，健康滑窗仅提供观测，不得反推未注册能力。`GET /api/admin/datasources` 与 `GET /api/admin/data-health` 都只读取进程观测和本地数据库；后者把交易日窗口钳在 30～60 日，以索引查询和结果硬上限区分休市、停牌、缺失、部分覆盖与未知日历。稀疏事件零行保持 unknown，不能伪装成完整覆盖。

`sync-bars/backfill-calendar/wide-sync` 的新请求正文只接受单个、最大 4KiB 的 JSON 对象；完全无 body 才走旧兼容路径。新路径必须先 dry-run，计划 hash 绑定本地事实并在真正上游调用前重算，事实变化即拒绝。日历计划的 `target_count` 与 `sample_targets` 表示执行时会逐日核对订正的完整范围，`missing_count` 只表示当前缺行数；二者不得混用。`DataSyncLog` 只保存触发来源、管理员、白名单参数摘要与 hash，不保存 token、cookie、密钥或原始正文。

管理员 `POST /api/admin/datasources/probe` 只接受注册表中的精确三元组，直达指定适配器而不自动换源；固定样本与列表 `limit=1`，同时受 12 次/分钟进程窗口、同三元组 15 秒、全局并发 2 和注册能力超时保护。success/empty/error 都写回同一健康滑窗；响应与 `DataSyncLog` 只含归一代码、耗时和白名单参数，不保存原始请求/响应或凭据。

管理员 `POST /api/admin/datasources/uncool` 要求不超过 200 字且不含凭据字段的原因，只把指定三元组的 `cooldownUntil` 清零，保留环形样本、最后观测/成功和冷却次数；审计记录管理员、原因、解冷前剩余秒数与是否实际清除。管理页提供确认、loading、防连点、错误恢复和成功后能力/日志局部刷新；375px 与 1440px、亮暗主题均只使用主题变量。两个副作用都禁止用 GET 或页面刷新替代。

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

- `/`：个人优先分时首页（盘前/盘中/盘后只调整个人区块顺序和默认展开；后端交易状态缺失时显示 unknown，市场全景在第二层）
- `/news`：市场快讯（新闻情绪流，N1/N2）
- `/stocks/:market/:symbol`：个股详情（P0-0C 决策摘要优先，走势/事件/基本面/研究四分区；A 股非基金首屏只预取本地公司行动，公告/新闻按事件页签加载，缺口保持 partial/unknown；stale/unknown 报价只称“最近已知”；首页榜单行与个股速查可点击进入，`useStockActions.goDetail` 供各处复用）
- `/screener`：策略选股（21 策略组合筛选、异步扫描、本人持久历史与 `result_id` 深链）
- `/backtest`：回测时光机（异步条件树回测、本人持久历史与 `result_id` 深链；推荐批次回验保持原接口）
- `/heatmap`：行业热力图（M3b）
- `/mood`：盘面情绪（情绪总览 / 连板梯队 / 龙虎榜 / 人气榜四 tab，A1~A3 散户体验第一批）
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
- `/tasks`：统一任务中心（九类用户 JobRun 与六类 system JobRun 支持真实步骤、取消、父子重跑、恢复与 SSE；原业务结果、策略运行结果和 DataSyncLog 通过引用去重并保留历史深链，legacy 同步日志仅作兼容投影）
- `/alerts`：提醒规则、立即检查和命中历史（通道管理仅跳转设置）
- `/paper`：模拟交易
- `/etf`：指数 ETF 交易（精选指数 ETF 行情 + 复用模拟盘买卖，2026-07-05）
- `/thesis`：投资逻辑卡
- `/notes`：投资笔记
- `/prompt-templates`：自定义分析提示词模板（用户菜单进入）
- `/settings`：设置（LLM/偏好/通知/AI 用量/账号安全；`?tab=notifications` 可直达通知设置）
- `/admin`：管理员后台（注册开关/新闻采集/LLM 回退/GitHub 凭证/用户与配额/同步日志）
- `/admin/llm-calls`：LLM 调用审计记录（筛选+分页+全文详情，2026-07 杂项批）
