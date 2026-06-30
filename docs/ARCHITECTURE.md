# 技术架构设计

## 1. 总体架构

QuantVista 采用前后端分离的全栈架构：

```text
Vue 前端
  |
  | REST / SSE / WebSocket
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

## 5. 后端模块

### 5.1 Auth Service

职责：

- GitHub OAuth 登录。
- JWT 签发与刷新。
- 用户资料同步。
- 用户权限判断。

> 与 new-api 的差异：本项目定位个人自用，GitHub OAuth 的 `client_id` / `client_secret` 直接从**环境变量**读取（`GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET`，见 `deploy/.env`），不存数据库 option、不做运行时可改。因此 `oauth/github` 的凭证读取逻辑不能照搬 new-api（它存 DB 设置项），其余流程（换 token、拉用户、建号）可参考。

接口示例：

- `GET /api/oauth/github`
- `GET /api/oauth/github/callback`
- `GET /api/user/me`
- `POST /api/auth/logout`

### 5.2 Market Data Service

职责：

- 拉取行情、指数、板块、新闻、财务和宏观数据。
- 标准化不同数据源格式。
- 缓存热点行情。
- 记录数据更新时间和数据源状态。

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

接口示例：

- `GET /api/markets/overview`
- `GET /api/markets/:market/indices`
- `GET /api/sectors/rankings`
- `GET /api/stocks/:symbol/quote`
- `GET /api/stocks/:symbol/fundamentals`
- `GET /api/news`

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

- `GET /api/positions`
- `POST /api/positions`
- `PATCH /api/positions/:id`
- `POST /api/positions/:id/close`
- `GET /api/positions/:id/review`

### 5.5 AI Analysis Service

职责：

- 根据用户选择的模块组装上下文。
- 调用用户或系统配置的 LLM。
- 输出结构化分析报告。
- 保存分析历史。
- 统计调用消耗。

接口示例：

- `POST /api/ai/analyze`
- `GET /api/ai/reports`
- `GET /api/ai/reports/:id`
- `POST /api/ai/reports/:id/rerun`

### 5.6 Recommendation Service

职责：

- 根据策略模板和用户偏好筛选候选池。
- 调用 AI 生成推荐解释。
- 生成短线或长线推荐。
- 保存推荐记录。

接口示例：

- `POST /api/recommendations/generate`
- `GET /api/recommendations`
- `GET /api/recommendations/:id`
- `POST /api/recommendations/:id/add-to-position`

### 5.7 Tracking Service

职责：

- 跟踪推荐表现。
- 更新短线止盈、止损、过期和重新分析状态。
- 计算推荐成功率、收益率和最大回撤。

**追踪数据建模（两层）**：

- **当前状态层**（与推荐 1:1）：保存最新收益率、最高价、最低价、最大涨幅、最大回撤、当前状态，定时任务覆盖更新。
- **价格序列层**：复用 `daily_bars`（按 stock + 日期），追踪只引用，不重复存全量行情。
- 止盈/止损判断使用当日 **high/low**，而非单点收盘价，避免漏判盘中触达。
- 收益/回撤计算使用**复权后**价格（结合 `corporate_actions`），避免除权导致序列断裂。
- 表现统计同时计算**相对基准（指数）的超额收益**并附**样本量 n**。

接口示例：

- `GET /api/recommendations/tracking`
- `GET /api/recommendations/performance`
- `POST /api/recommendations/:id/recalculate`

### 5.8 Settings Service

职责：

- 用户 LLM 配置。
- 数据源配置。
- 风险偏好。
- 策略模板。
- 通知设置。

接口示例：

- `GET /api/settings/llm`
- `POST /api/settings/llm`
- `POST /api/settings/llm/test`
- `GET /api/settings/data-sources`
- `PATCH /api/settings/preferences`

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
3. 系统默认 LLM 配置。

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

- **每用户 token 配额与熔断**：超额拒绝调用，避免系统默认 LLM 被滥用。
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
- 敏感操作（改 API Key、改数据源 URL）写**审计日志**。
- 股票推荐页面固定展示免责声明。

## 9. 前端页面结构

推荐页面：

- `/`：市场首页
- `/login`：登录页
- `/watchlist`：自选股
- `/positions`：已购入持仓
- `/analysis`：AI 分析中心
- `/recommendations`：推荐历史与追踪
- `/compare`：个股横向对比
- `/alerts`：提醒规则与待办/待复盘清单
- `/strategies`：策略模板
- `/reports`：分析历史
- `/settings`：设置
- `/admin`：管理员后台，后续实现

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
