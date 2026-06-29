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
  +-- PostgreSQL / SQLite / MySQL
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
- 数据库：开发 SQLite，生产 PostgreSQL，兼容 MySQL
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
- Naive UI 或 Element Plus
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

## 7. 数据缓存策略

行情类数据：

- 热点行情短缓存。
- 首页市场概览短缓存。
- 历史行情长期保存或按数据源能力查询。

AI 结果：

- 同一用户、同一参数、同一数据快照下可短期缓存。
- 推荐结果必须持久化，不能只缓存。

配置类数据：

- 用户偏好可缓存。
- LLM API Key 不进入前端缓存。

## 8. 安全设计

- GitHub OAuth 登录。
- JWT 鉴权。
- 用户数据按 `user_id` 隔离。
- API Key 加密保存。
- 用户配置 Base URL 时做 SSRF 防护。
- 数据源 URL 需要白名单或管理员审核。
- AI 调用做频率限制和成本统计。
- 股票推荐页面固定展示免责声明。

## 9. 前端页面结构

推荐页面：

- `/`：市场首页
- `/login`：登录页
- `/watchlist`：自选股
- `/positions`：已购入持仓
- `/analysis`：AI 分析中心
- `/recommendations`：推荐历史与追踪
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
