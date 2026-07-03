# 数据库设计

> **实现差异说明（2026-07-02 审查后补充，2026-07-03 批次 G~J 后修订）**：本文档为**目标态设计**；实际 schema 以
> `server/model/*.go`（GORM AutoMigrate）为准。当前已知差异（多数属实现简化，
> 详见 `ROADMAP.md`「当前实现边界」）：
>
> - **未建的表**：`corporate_actions`（复权，日线暂用东财前复权）、
>   `audit_logs`、`ai_call_logs`（个人自用降级）；`data_source_configs` 表已建但**无读写方**（死表，管理端未接）。
> - **已落地的目标态表**：`alerts` 的命中明细与 unread/read/dismissed 状态机已实现为
>   **`alert_events`** 表（rule_id/user_id/symbol/market/name/kind/message/triggered_at/status，
>   索引 user+status；命中同日去重落一条，今日待办取 unread，删规则时其未读事件转 dismissed）。
> - **字段/结构差异**：`stock_quotes` 实现为 symbol+market 唯一的**最新快照单行覆盖**
>   （非文档的 stock_id+quote_time 历史序列）；`market_snapshots` 实现为涨跌家数列
>   （非 JSON 大字段）；`data_sync_logs` 实现为 Task/Market/Total/Succeeded/Failed/DurationMs；
>   `daily_bars` 用 symbol+market 自然键、无 adj_close；`users.status` 枚举为
>   `enabled/disabled`（非 active/disabled）；`positions` 已加 `recommendation_id`（推荐↔持仓血缘，批次 G）；
>   `user_preferences` 实现为 risk_level/default_market/horizon_pref/default_rec_count/enable_notify
>   /blacklist_json/min_candidate_amount（无 default_llm_config_id——由 `llm_configs.is_default` 承担；无 simulation_enabled——模拟盘恒可用）；
>   `user_token_quotas` 实现为**次数制终身累计**（action_limit/action_used 熔断，token 仅审计；无 period/period_start 周期重置；管理员可经
>   `PUT /api/admin/users/:id/quota` 调整次数上限并手工清零已用量，2026-07-03 标准化）；
>   `alert_rules` 实现为 kind(price/pct_change/ma/breakout/volume_surge/amplitude)+op+threshold+period+once
>   （无 target_type/target_id/cooldown_seconds；同日去重经 triggered_at 实现）；`stock_scores` 实现为五维技术面
>   （trend/momentum/position/volume/risk，估值/成长/财务/情绪待财务数据源）；
>   `analysis_records` 加 `mode` 列（""=标准 / "panel"=多角色观点，批次 I）与
>   `recommendation_status` 加 return_7d/14d/30d 节点收益（批次 G）。
> - 短线追踪状态枚举实现为 `active/take_profit/stop_loss/expired/tracking/no_data`
>   （无 watching/needs_review/closed——用户买卖联动未实现）。

## 1. 设计原则

- 所有用户数据必须通过 `user_id` 隔离。
- 自选股和已购入持仓分开建模。
- AI 分析报告和推荐记录必须持久化。
- 短线推荐必须保存止盈、止损、有效期和状态。
- 长线推荐必须保存复盘周期和关键跟踪指标。
- LLM API Key 加密保存，不明文返回前端；主密钥独立管理。
- 金额、价格、收益率使用 decimal 类型，避免浮点误差。
- 价格序列与回撤计算依赖日线 OHLC（`daily_bars`），并结合公司行为（`corporate_actions`）做复权，不依赖单点快照。
- 分析与推荐记录保存方法版本（prompt/策略/评分），保证历史可复现、可横比。
- 多市场为多币种，金额字段需带币种语义。

## 2. 核心枚举

### market

- `us`
- `cn`
- `hk`

### risk_level

- `conservative`
- `balanced`
- `aggressive`

### horizon

用户投资周期偏好（仅偏好语义）：

- `short_term`
- `mid_term`
- `long_term`

### recommendation_type

推荐只有短线/长线两套逻辑，无独立中线逻辑：

- `short_term`
- `long_term`

> 说明：用户偏好 `mid_term` 不是独立推荐类型，需在业务层映射到 `short_term` 或 `long_term`（默认建议映射到 `long_term`），避免“中线推荐无处安放”。

### currency

- `CNY`
- `USD`
- `HKD`

### position_status

持仓状态不含“观察中”（观察属于自选股/推荐，不属于持仓）：

- `holding`
- `closed`

### short_tracking_status

- `watching`
- `active`
- `take_profit_triggered`
- `stop_loss_triggered`
- `expired`
- `needs_review`
- `closed`

> 状态机归属：`recommendation_status.tracking_status` 是短线追踪状态的**唯一来源**；`positions.status` 只表示持仓生命周期；`recommendation_records.status` 只表示推荐记录本身是否有效。三者各管一段，业务层负责同步，不重复定义同一含义。

## 3. 表设计

### users

用户基础信息。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| github_id | string | GitHub 用户 ID |
| username | string | 用户名 |
| nickname | string | 昵称 |
| email | string | 邮箱 |
| avatar_url | string | 头像 |
| role | string | `user` / `admin` |
| status | string | `active` / `disabled` |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### user_preferences

用户偏好。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| risk_level | string | 风险等级 |
| preferred_market | string | 默认市场 |
| preferred_horizon | string | 默认周期 |
| default_recommendation_count | int | 默认推荐数量，3 到 5 |
| default_llm_config_id | bigint / uuid | 默认 LLM 配置 |
| notification_enabled | bool | 是否启用通知 |
| simulation_enabled | bool | 是否启用模拟交易 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### llm_configs

用户 LLM 连接配置。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID，可为空表示系统配置 |
| name | string | 配置名称 |
| provider | string | OpenAI、Azure、Ollama 等 |
| base_url | string | API Base URL |
| api_key_encrypted | text | 加密后的 API Key |
| model | string | 模型名 |
| temperature | decimal | 温度 |
| max_tokens | int | 最大输出 token |
| stream_enabled | bool | 是否启用流式输出 |
| is_default | bool | 是否默认 |
| last_test_status | string | 最近测试状态 |
| last_test_at | timestamp | 最近测试时间 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### data_source_configs

数据源配置。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| owner_type | string | `system` / `user` |
| user_id | bigint / uuid | 用户 ID，可为空 |
| source_type | string | quote / fundamental / news / macro |
| provider | string | 数据源名称 |
| base_url | string | 地址 |
| api_key_encrypted | text | 加密密钥 |
| refresh_interval_seconds | int | 刷新间隔 |
| enabled | bool | 是否启用 |
| health_status | string | 健康状态 |
| last_success_at | timestamp | 最近成功时间 |
| last_error | text | 最近错误 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### stocks

股票基础信息。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| symbol | string | 股票代码 |
| name | string | 股票名称 |
| market | string | 市场 |
| exchange | string | 交易所 |
| currency | string | 币种 |
| sector | string | 行业 |
| industry | string | 细分行业 |
| status | string | active / suspended / delisted |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

唯一索引：

- `market + symbol`

### stock_quotes

行情快照。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| stock_id | bigint / uuid | 股票 ID |
| quote_time | timestamp | 行情时间 |
| price | decimal | 当前价 |
| open | decimal | 开盘价 |
| high | decimal | 最高价 |
| low | decimal | 最低价 |
| previous_close | decimal | 昨收 |
| change | decimal | 涨跌额 |
| change_percent | decimal | 涨跌幅 |
| volume | decimal | 成交量 |
| turnover | decimal | 成交额 |
| source | string | 数据源 |
| created_at | timestamp | 创建时间 |

索引：

- `stock_id + quote_time`

### stock_fundamentals

基本面数据。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| stock_id | bigint / uuid | 股票 ID |
| report_date | date | 报告日期 |
| pe | decimal | 市盈率 |
| pb | decimal | 市净率 |
| ps | decimal | 市销率 |
| market_cap | decimal | 市值 |
| revenue | decimal | 营收 |
| net_income | decimal | 净利润 |
| gross_margin | decimal | 毛利率 |
| roe | decimal | ROE |
| debt_ratio | decimal | 负债率 |
| dividend_yield | decimal | 股息率 |
| source | string | 数据源 |
| created_at | timestamp | 创建时间 |

> 注：`stock_fundamentals` 填充分两档——轻量版用东财实时估值（PE/PB/市值/股息率及部分实时指标），完整版用 Tushare 低 cost 档（2000 分，财务三表 + 多期财务指标，按需启用）。MVP 不依赖 Tushare，先用轻量版；启用 Tushare 后回填完整字段。

### market_snapshots

市场快照。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| market | string | 市场 |
| snapshot_time | timestamp | 快照时间 |
| indices_json | json | 指数数据 |
| sector_rankings_json | json | 板块排行 |
| sentiment_json | json | 市场情绪 |
| fund_flow_json | json | 资金流向 |
| hot_stocks_json | json | 热门股票 |
| source | string | 数据源 |
| created_at | timestamp | 创建时间 |

### watchlists

自选股分组。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| name | string | 分组名称 |
| sort_order | int | 排序 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### watchlist_items

自选股条目。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| watchlist_id | bigint / uuid | 分组 ID |
| stock_id | bigint / uuid | 股票 ID |
| note | text | 备注 |
| focus_reason | text | 关注原因 |
| is_pinned | bool | 是否重点关注 |
| tags_json | json | 自定义标签 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

唯一索引：

- `user_id + watchlist_id + stock_id`

### positions

已购入持仓。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| stock_id | bigint / uuid | 股票 ID |
| recommendation_id | bigint / uuid | 来源推荐，可为空 |
| position_type | string | short_term / long_term |
| status | string | holding / closed |
| currency | string | 交易币种，CNY / USD / HKD |
| buy_price | decimal | 买入价格 |
| buy_date | date | 买入日期 |
| quantity | decimal | 数量 |
| buy_fee | decimal | 买入手续费（佣金/过户费等） |
| buy_tax | decimal | 买入税费 |
| sell_fee | decimal | 卖出手续费 |
| sell_tax | decimal | 卖出税费（如印花税） |
| buy_reason | text | 买入理由 |
| user_note | text | 用户备注 |
| current_price_snapshot | decimal | 最近一次计算价格 |
| current_return_percent | decimal | 当前收益率 |
| sell_price | decimal | 卖出价格 |
| sell_date | date | 卖出日期 |
| sell_reason | text | 卖出原因 |
| review_note | text | 复盘备注 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### ai_analysis_reports

AI 分析报告。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| report_type | string | market / stock / watchlist / position / recommendation |
| target_type | string | market / sector / stock / watchlist / position |
| target_id | string | 目标 ID |
| request_params_json | json | 请求参数 |
| data_snapshot_json | json | 输入数据快照（含数据时间范围与实际注入内容） |
| result_json | json | 结构化结果 |
| summary | text | 摘要 |
| strategy_id | bigint / uuid | 使用的策略模板，可为空 |
| prompt_version | string | Prompt 模板版本 |
| strategy_version | string | 策略版本 |
| scoring_version | string | 评分方法版本 |
| llm_config_id | bigint / uuid | LLM 配置 |
| model | string | 模型 |
| prompt_tokens | int | 输入 token |
| completion_tokens | int | 输出 token |
| total_tokens | int | 总 token |
| latency_ms | int | 耗时 |
| validation_status | string | structured 校验：valid / repaired / partial |
| status | string | success / failed |
| error_message | text | 错误信息 |
| created_at | timestamp | 创建时间 |

### recommendation_records

AI 推荐记录。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| analysis_report_id | bigint / uuid | 来源分析报告 |
| stock_id | bigint / uuid | 股票 ID |
| recommendation_type | string | short_term / long_term |
| strategy_id | bigint / uuid | 策略模板 |
| strategy_version | string | 策略版本 |
| scoring_version | string | 评分方法版本 |
| currency | string | 计价币种 |
| base_price | decimal | 推荐基准价 |
| benchmark_symbol | string | 对比基准（指数）代码，用于 alpha |
| benchmark_base_price | decimal | 推荐时基准指数点位 |
| recommendation_time | timestamp | 推荐时间 |
| valid_until | timestamp | 有效期（按交易日计算） |
| confidence | decimal | 置信度 |
| reason | text | 推荐理由 |
| data_points_json | json | 数据依据 |
| risks_json | json | 风险点 |
| action_plan_json | json | 操作计划 |
| status | string | active / expired / closed |
| disclaimer | text | 免责声明 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### recommendation_status

推荐后追踪的**当前状态层**（与推荐 1:1，定时任务覆盖更新）。聚合值只存最新结果，价格序列另由 `daily_bars` 提供。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| recommendation_id | bigint / uuid | 推荐 ID，唯一 |
| stock_id | bigint / uuid | 股票 ID |
| last_updated_at | timestamp | 最近一次计算时间 |
| current_price | decimal | 当前价格（复权后） |
| return_percent | decimal | 当前收益率 |
| highest_price | decimal | 推荐后最高价 |
| lowest_price | decimal | 推荐后最低价 |
| max_gain_percent | decimal | 最大涨幅 |
| max_drawdown_percent | decimal | 最大回撤 |
| benchmark_return_percent | decimal | 同期基准收益率 |
| alpha_percent | decimal | 超额收益（相对基准） |
| take_profit_price | decimal | 止盈价，短线使用 |
| stop_loss_price | decimal | 止损价，短线使用 |
| tracking_status | string | 短线状态或通用状态，唯一来源 |
| triggered_reason | text | 触发原因 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

唯一索引：

- `recommendation_id`

> 说明：原 `recommendation_tracking` 把时序行与聚合值混在一张表里、语义冲突。现拆为：当前状态（本表，1:1）+ 价格序列（复用 `daily_bars`）。止盈/止损判断用当日 high/low，回撤/收益用复权序列。

### daily_bars

日线 OHLC，追踪、回撤、止盈止损判断的统一数据来源。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| stock_id | bigint / uuid | 股票 ID |
| trade_date | date | 交易日 |
| open | decimal | 开盘价 |
| high | decimal | 最高价 |
| low | decimal | 最低价 |
| close | decimal | 收盘价 |
| adj_close | decimal | 复权收盘价 |
| volume | decimal | 成交量 |
| turnover | decimal | 成交额 |
| source | string | 数据源 |
| created_at | timestamp | 创建时间 |

唯一索引：

- `stock_id + trade_date`

### corporate_actions

公司行为（拆股、分红、配股等），用于复权，避免价格序列断裂。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| stock_id | bigint / uuid | 股票 ID |
| action_type | string | split / dividend / rights / bonus |
| ex_date | date | 除权除息日 |
| ratio | decimal | 拆股/送股比例 |
| dividend_amount | decimal | 每股分红 |
| adjustment_factor | decimal | 复权因子 |
| source | string | 数据源 |
| created_at | timestamp | 创建时间 |

唯一索引：

- `stock_id + action_type + ex_date`

### trading_calendar

交易日历，用于按交易日计算有效期、持有周期、数据新鲜度。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| market | string | 市场 |
| trade_date | date | 日期 |
| is_open | bool | 是否交易日 |
| created_at | timestamp | 创建时间 |

唯一索引：

- `market + trade_date`

### strategy_templates

策略模板。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID，可为空表示系统模板 |
| name | string | 策略名称 |
| strategy_type | string | short_term / long_term |
| description | text | 描述 |
| filters_json | json | 候选池筛选条件 |
| scoring_weights_json | json | 评分权重 |
| prompt_template | text | Prompt 模板 |
| enabled | bool | 是否启用 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### stock_scores

股票评分。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| stock_id | bigint / uuid | 股票 ID |
| score_time | timestamp | 评分时间 |
| trend_score | decimal | 趋势分 |
| valuation_score | decimal | 估值分 |
| growth_score | decimal | 成长分 |
| financial_health_score | decimal | 财务健康分 |
| sentiment_score | decimal | 情绪分 |
| risk_score | decimal | 风险分 |
| overall_score | decimal | 综合分 |
| details_json | json | 评分细节 |
| created_at | timestamp | 创建时间 |

### alert_rules

用户自定义提醒条件（3.16）。命中后生成一条 `alerts` 记录。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| target_type | string | stock / position / recommendation |
| target_id | bigint / uuid | 目标 ID |
| rule_type | string | price / ma_cross / breakout / pullback / volume / limit / earnings / expiry / review |
| condition_json | json | 条件参数（阈值、均线周期、区间等） |
| enabled | bool | 是否启用 |
| last_triggered_at | timestamp | 最近触发时间，可空 |
| cooldown_seconds | int | 触发冷却，避免重复刷屏 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### alerts

提醒记录。MVP 可先只用于页面提示，后续再扩展主动通知。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| rule_id | bigint / uuid | 来源提醒规则，可空（系统触发如止盈止损可为空） |
| target_type | string | stock / position / recommendation |
| target_id | bigint / uuid | 目标 ID |
| alert_type | string | price / ma_cross / breakout / volume / take_profit / stop_loss / expired / review / earnings |
| title | string | 标题 |
| message | text | 内容 |
| status | string | unread / read / dismissed |
| triggered_at | timestamp | 触发时间 |
| created_at | timestamp | 创建时间 |

### ai_call_logs

AI 调用日志。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| llm_config_id | bigint / uuid | LLM 配置 |
| provider | string | Provider |
| model | string | 模型 |
| endpoint | string | 调用接口 |
| prompt_tokens | int | 输入 token |
| completion_tokens | int | 输出 token |
| total_tokens | int | 总 token |
| latency_ms | int | 耗时 |
| status | string | success / failed |
| error_message | text | 错误信息 |
| created_at | timestamp | 创建时间 |

### ai_conversations

个股 / 报告多轮问答会话（3.17）。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| context_type | string | stock / report |
| context_id | string | 绑定的股票 ID 或分析报告 ID |
| data_snapshot_ref | bigint / uuid | 复用的数据快照来源（如 ai_analysis_reports.id），可空 |
| title | string | 会话标题 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### ai_conversation_messages

会话消息。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| conversation_id | bigint / uuid | 会话 ID |
| role | string | user / assistant |
| content | text | 消息内容 |
| total_tokens | int | 本条消耗 token，可空 |
| created_at | timestamp | 创建时间 |

索引：

- `conversation_id + created_at`

### data_sync_logs

数据同步日志。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| source_config_id | bigint / uuid | 数据源配置 |
| sync_type | string | quote / fundamental / news / macro |
| status | string | success / failed |
| started_at | timestamp | 开始时间 |
| finished_at | timestamp | 结束时间 |
| rows_affected | int | 影响行数 |
| error_message | text | 错误 |
| created_at | timestamp | 创建时间 |

### refresh_tokens

刷新令牌，支持吊销与强制登出。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| token_hash | string | 令牌哈希，不存明文 |
| expires_at | timestamp | 过期时间 |
| revoked_at | timestamp | 吊销时间，可空 |
| user_agent | string | 客户端信息 |
| created_at | timestamp | 创建时间 |

### user_token_quotas

每用户 AI 配额与消耗。**2026-07-03 起标准化为次数制**：一次用户手动动作（发起分析/推荐/问答/对比点评）计 1 次，内部 repair/panel 多轮 LLM 请求不重复计；后台自动任务只记 token 审计、不计次。实际表名 `user_quota`。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| user_id | bigint | 主键（每用户一行） |
| action_limit | bigint | 次数上限，0=不限（熔断依据） |
| action_used | bigint | 已用次数（仅用户手动动作） |
| token_used | bigint | 累计 token（审计参考，不参与熔断） |
| request_count | bigint | LLM 调用轮次（审计参考） |
| updated_at | timestamp | 更新时间 |

> 历史列 `token_limit` 已废弃（模型不再映射，遗留列无害）；无 period/period_start 周期重置，管理员可手工清零。

### daily_reports（2026-07-03）

收盘日报：交易日 15:35 后为开启偏好 `enable_daily_report` 的用户自动生成（每 10min 后台检查，20:00 后不再补），也可手动重生成（计 1 次配额；自动生成不计次）。每用户每交易日一份。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint | 主键 |
| user_id + trade_date | — | 复合唯一（idx_report_user_date） |
| market | string | 当前恒 cn |
| status | string | success / partial（复盘或推荐一方失败）/ failed |
| review_json | text | LLM 结构化复盘 {summary, market_review, position_review, watch_review, risk_warnings[], tomorrow_plan}（列表查询排除） |
| snapshot_json | text | 复盘输入快照（市场概览+持仓当日涨跌+自选异动 top8+今日命中提醒），可复现（列表排除） |
| recommendation_batch_id | bigint | 明日推荐批次（复用 recommendation_batches 全链路；0=未生成） |
| error / total_tokens / latency_ms | — | 失败原因与用量审计 |

> 推荐生成同时为每条 pick 自动创建止盈(gte)/止损(lte)到价提醒（`alert_rules.note` 前缀「收盘日报 <date>」，21 自然日未命中自动 paused；手动重生成先删当日旧规则防重复）。`user_preferences` 新增 `enable_daily_report`（默认 false）。

### audit_logs

敏感操作审计（改 API Key、改数据源 URL 等）。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 操作者 |
| action | string | 操作类型 |
| target_type | string | 对象类型 |
| target_id | string | 对象 ID |
| detail_json | json | 变更摘要（脱敏，不含明文密钥） |
| ip | string | 来源 IP |
| created_at | timestamp | 创建时间 |

## 4. 关键关系

- `users` 1 对 1 `user_preferences`
- `users` 1 对多 `llm_configs`
- `users` 1 对多 `watchlists`
- `watchlists` 1 对多 `watchlist_items`
- `users` 1 对多 `positions`
- `stocks` 1 对多 `stock_quotes`
- `stocks` 1 对多 `stock_fundamentals`
- `ai_analysis_reports` 1 对多 `recommendation_records`
- `recommendation_records` 1 对 1 `recommendation_status`
- `recommendation_records` 1 对多 `positions`
- `strategy_templates` 1 对多 `recommendation_records`
- `stocks` 1 对多 `daily_bars`
- `stocks` 1 对多 `corporate_actions`
- `users` 1 对多 `refresh_tokens`
- `users` 1 对多 `alert_rules`
- `alert_rules` 1 对多 `alerts`
- `ai_conversations` 1 对多 `ai_conversation_messages`

## 5. MVP 必建表

第一阶段建议先建：

- `users`
- `user_preferences`
- `refresh_tokens`
- `llm_configs`
- `user_token_quotas`
- `stocks`
- `stock_quotes`
- `daily_bars`
- `trading_calendar`
- `market_snapshots`
- `watchlists`
- `watchlist_items`
- `positions`
- `ai_analysis_reports`
- `recommendation_records`
- `recommendation_status`
- `strategy_templates`
- `ai_call_logs`

第二阶段再补：

- `data_source_configs`
- `stock_fundamentals`
- `corporate_actions`
- `stock_scores`
- `alert_rules`
- `alerts`
- `ai_conversations`
- `ai_conversation_messages`
- `audit_logs`
- `data_sync_logs`

> 注：`corporate_actions`（复权）在追踪长期价格序列时即需要；若 MVP 阶段已做短线/长线推荐追踪，建议提前到第一阶段，否则除权日的收益与回撤会出错。
> 注：`alert_rules` / `alerts` 与 `ai_conversations` 服务于个股选股增强功能（条件提醒、个股 AI 问答），随对应功能（路线图阶段 7 的前半部分）一起建。
