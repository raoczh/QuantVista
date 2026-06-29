# 数据库设计

## 1. 设计原则

- 所有用户数据必须通过 `user_id` 隔离。
- 自选股和已购入持仓分开建模。
- AI 分析报告和推荐记录必须持久化。
- 短线推荐必须保存止盈、止损、有效期和状态。
- 长线推荐必须保存复盘周期和关键跟踪指标。
- LLM API Key 加密保存，不明文返回前端。
- 金额、价格、收益率建议使用 decimal 类型，避免浮点误差。

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

- `short_term`
- `mid_term`
- `long_term`

### recommendation_type

- `short_term`
- `long_term`

### position_status

- `holding`
- `closed`
- `watching`

### short_tracking_status

- `watching`
- `active`
- `take_profit_triggered`
- `stop_loss_triggered`
- `expired`
- `needs_review`
- `closed`

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
| status | string | holding / closed / watching |
| buy_price | decimal | 买入价格 |
| buy_date | date | 买入日期 |
| quantity | decimal | 数量 |
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
| data_snapshot_json | json | 输入数据快照 |
| result_json | json | 结构化结果 |
| summary | text | 摘要 |
| llm_config_id | bigint / uuid | LLM 配置 |
| model | string | 模型 |
| prompt_tokens | int | 输入 token |
| completion_tokens | int | 输出 token |
| total_tokens | int | 总 token |
| latency_ms | int | 耗时 |
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
| base_price | decimal | 推荐基准价 |
| recommendation_time | timestamp | 推荐时间 |
| valid_until | timestamp | 有效期 |
| confidence | decimal | 置信度 |
| reason | text | 推荐理由 |
| data_points_json | json | 数据依据 |
| risks_json | json | 风险点 |
| action_plan_json | json | 操作计划 |
| status | string | active / expired / closed |
| disclaimer | text | 免责声明 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

### recommendation_tracking

推荐后追踪。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| recommendation_id | bigint / uuid | 推荐 ID |
| stock_id | bigint / uuid | 股票 ID |
| tracking_time | timestamp | 追踪时间 |
| current_price | decimal | 当前价格 |
| return_percent | decimal | 当前收益率 |
| highest_price | decimal | 推荐后最高价 |
| lowest_price | decimal | 推荐后最低价 |
| max_gain_percent | decimal | 最大涨幅 |
| max_drawdown_percent | decimal | 最大回撤 |
| take_profit_price | decimal | 止盈价，短线使用 |
| stop_loss_price | decimal | 止损价，短线使用 |
| tracking_status | string | 短线状态或通用状态 |
| triggered_reason | text | 触发原因 |
| created_at | timestamp | 创建时间 |
| updated_at | timestamp | 更新时间 |

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

### alerts

提醒记录。MVP 可先只用于页面提示，后续再扩展主动通知。

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| id | bigint / uuid | 主键 |
| user_id | bigint / uuid | 用户 ID |
| target_type | string | stock / position / recommendation |
| target_id | bigint / uuid | 目标 ID |
| alert_type | string | price / take_profit / stop_loss / expired / review |
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

## 4. 关键关系

- `users` 1 对 1 `user_preferences`
- `users` 1 对多 `llm_configs`
- `users` 1 对多 `watchlists`
- `watchlists` 1 对多 `watchlist_items`
- `users` 1 对多 `positions`
- `stocks` 1 对多 `stock_quotes`
- `stocks` 1 对多 `stock_fundamentals`
- `ai_analysis_reports` 1 对多 `recommendation_records`
- `recommendation_records` 1 对多 `recommendation_tracking`
- `recommendation_records` 1 对多 `positions`
- `strategy_templates` 1 对多 `recommendation_records`

## 5. MVP 必建表

第一阶段建议先建：

- `users`
- `user_preferences`
- `llm_configs`
- `stocks`
- `stock_quotes`
- `market_snapshots`
- `watchlists`
- `watchlist_items`
- `positions`
- `ai_analysis_reports`
- `recommendation_records`
- `recommendation_tracking`
- `strategy_templates`
- `ai_call_logs`

第二阶段再补：

- `data_source_configs`
- `stock_fundamentals`
- `stock_scores`
- `alerts`
- `data_sync_logs`
