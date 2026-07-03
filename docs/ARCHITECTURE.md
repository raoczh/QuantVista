# 技术架构设计

## 1. 总体架构

QuantVista 采用前后端分离的全栈架构：

```text
Vue 前端
  |
  | REST（当前全部为 REST；SSE/WebSocket 为可扩展方向，未使用）
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

## 2. 参考 new-api 的结论

`new-api` 参考项目当前是 Go 后端项目，核心技术包括：

- Gin
- GORM
- Redis
- JWT / OAuth
- Controller / Service / Model 分层
- 配置管理
- 日志
- 中间件
- AI Provider / Channel 管理
- 前端静态资源托管

它的新前端位于 `web/default`，技术栈是 React + Rsbuild + TanStack Router，不是 Vue。

本项目建议：

- 后端工程组织、鉴权、设置、日志、任务、缓存、AI 渠道管理参考 `new-api`。
- 前端重新建立 Vue 3 项目，不直接复用 React 组件。
- API 契约保持框架无关，因此 Vue 前端没有技术栈兼容问题。
- 如果后端沿用 Go + Gin，只要返回 JSON / SSE，Vue、React 或其他前端都可以正常接入。

## 3. 后端技术栈

推荐：

- 语言：Go
- Web 框架：Gin
- ORM：GORM
- 数据库：开发 SQLite，生产 MySQL（宝塔托管，与 new-api 同实例不同库）；PostgreSQL 仅 GORM 兼容，不主推
- 缓存：Redis
- 认证：JWT + GitHub OAuth
- 定时任务：Go 内部任务调度，后续可扩展队列
- 日志：结构化日志
- 配置：环境变量 + 数据库配置

可从 `new-api` 借鉴：

- `router/`：路由组织
- `middleware/`：鉴权、限流、日志、CORS
- `controller/`：请求处理
- `service/`：业务逻辑
- `model/`：GORM 模型
- `setting/`：系统配置
- `oauth/`：OAuth Provider 设计
- `common/`：缓存、工具、校验、HTTP 客户端

## 4. 前端技术栈

推荐：

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
- 记录数据更新时间和数据源状态（数据源健康状态落库为**规划项**：`data_source_configs` 表已建，manager 回写与管理端查询未接，当前排查靠日志）。

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
- **MVP 只实现一个市场、一个源**，端到端跑通后再扩展（见 ROADMAP 阶段 0/1）。
- 每条数据携带 `source` 与 `data_time`；同步失败写 `data_sync_logs`。
- 日线行情写入 `daily_bars`（OHLC），供追踪与回撤计算使用；公司行为写 `corporate_actions` 用于复权。
- **Tushare 分档接入**：第一阶段以东财 + 新浪为主，Tushare 非前置；免费档（120，股票清单/日线/交易日历）与低 cost 档（2000，财务三表/复权因子/指数日线，长线财务深度来源）按需启用，高级档（5000，分钟线/融资融券明细等）暂不实现。详见 [数据源选型](docs/DATA_SOURCES.md)。

接口示例（与实际路由一致；行情为公开端点、带宽松限流）：

- `GET /api/markets/:market/overview`
- `GET /api/markets/:market/stocks/:symbol/quote`
- `GET /api/markets/:market/stocks/:symbol/bars`
- `GET /api/markets/:market/stocks/:symbol/score`
-（fundamentals / news 待外部数据源，未实现）

### 5.3 Watchlist Service

职责：

- 自选股增删改查。
- 自选股分组。
- 重点关注和备注。

接口示例：

- `GET /api/watchlists`
- `POST /api/watchlists`
- `POST /api/watchlists/:id/items`
- `PATCH /api/watchlist-items/:id`
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
-（数据源配置管理端为规划项：`data_source_configs` 表已建、未接管理端）

### 5.8.1 条件提醒与命中事件（阶段 7 + 批次 H）

- `GET/POST /api/alerts`，`PUT/DELETE /api/alerts/:id`，`PUT /api/alerts/:id/status`（暂停/恢复），`POST /api/alerts/evaluate`（手动评估，限流 20/min）
- 提醒类型 kind：price（到价）/ pct_change（异动）/ ma（均线）/ breakout（突破）/ **volume_surge**（当日量≥N 倍 20 日均量）/ **amplitude**（当日振幅，优先腾讯估值源、缺则 (high-low)/prev_close 回退）
- 命中明细状态机（`alert_events`，unread/read/dismissed）：`GET /api/alerts/events?status=&limit=`，`PUT /api/alerts/events/:id/status`，`PUT /api/alerts/events/read-all`
- 今日待办（`GET /api/todos`）的提醒条目即 unread 事件，`ref_id` 为事件 id，可就地标记已读/忽略「完成待办」

### 5.9 Job Service

职责：

- 定时刷新市场快照。
- 更新股票行情缓存。
- 更新推荐追踪状态。
- 清理过期缓存。
- 生成每日市场摘要，后续可选。

## 6. AI 调用设计

### 6.1 推荐流程

```text
用户发起推荐
  |
读取用户偏好和策略
  |
拉取市场、行情、财务、新闻数据
  |
规则筛选候选池
  |
构建 Prompt 和结构化输入
  |
调用 LLM
  |
校验结构化输出
  |
保存分析报告
  |
保存推荐记录
  |
初始化推荐追踪
```

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

>（原设计的第 3 级「系统默认 LLM 配置」未实现：当前无系统级共享渠道，用户未配置任何 LLM 时直接提示先去设置。个人自用下可接受，多用户开放前再评估。）

### 6.4 上下文预算（硬约束）

- 每次调用设定 **token 预算上限**，按模块**分级注入**：核心数据全量、辅助数据摘要、长文本（新闻/公告）先摘要再注入。
- 大列表先**规则裁剪/排序/截断**，只注入 top-N 并在 prompt 标注“已截断”。
- 注入内容与原始输入快照写入 `ai_analysis_reports.data_snapshot_json`，保证可复现。

### 6.5 结构化输出可靠性（硬约束）

- 优先 provider 的 **function calling / JSON mode / response schema**；不支持的 provider fallback 到“prompt 约束 + 文本解析”。
- 统一 **JSON Schema 校验**；失败做**有限次 repair 重试**（错误回灌），仍失败则**优雅降级**（文本可读、recommendations 置空、标记 `partial`），不写脏数据。
- 校验结果、重试次数、降级状态写 `ai_call_logs`。

### 6.6 版本与可复现

- 每次分析/推荐保存 **prompt 版本、策略版本、评分方法版本**；prompt/策略/评分迭代后，历史记录仍可定位当时方法、可横向比较。

### 6.7 成本控制

- **每用户 AI 次数配额与熔断**：按用户手动动作计次（一次分析/推荐/问答/点评=1 次，内部 repair/panel 多轮请求不重复计，后台任务不计次），超额拒绝调用；token 消耗仍全量累计作审计。
- AI 调用做频率限制；首页等高频页面不得每次刷新触发 LLM（走缓存）。

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
- AI 调用做频率限制、每用户配额和成本统计。
- 敏感操作（改 API Key、改数据源 URL）写**审计日志**（`audit_logs` 未建，个人自用降级为规划项；多用户开放前须补）。
- 股票推荐页面固定展示免责声明。

## 9. 前端页面结构

实际路由（`web/src/router/index.ts`）：

- `/`：市场首页
- `/stocks/:market/:symbol`：个股详情（行情/日K/估值/评分 + 快捷动作，2026-07-03；首页榜单行与个股速查可点击进入，`useStockActions.goDetail` 供各处复用）
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
- `/prompt-templates`：自定义分析提示词模板（用户菜单进入）
- `/settings`：设置（LLM/偏好/AI 用量/账号安全）
- `/admin`：管理员后台

## 10. MVP 边界

第一阶段必须实现：

- Vue 前端骨架。
- Go 后端骨架。
- GitHub 登录。
- 用户、设置、自选股、已购入持仓持久化。
- 市场首页基础数据。
- LLM 配置和测试连接。
- AI 分析。
- 短线/长线推荐。
- 推荐历史。
- 用户查询时显示短线止盈、止损、过期状态。

第一阶段暂不强制实现：

- 主动推送提醒。
- 完整回测系统。
- 模拟交易排行榜。
- 多租户计费。
- 复杂管理员权限系统。
