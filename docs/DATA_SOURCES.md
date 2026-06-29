# 数据源选型

## 1. 选型原则

- 后端是 Go，**优先选能直接 HTTP 调用的数据源**，避免为某个 Python 库（如 AKShare）单独起一个 Python 微服务。
- 个人自用，**先用免费、实时、能直连的源打底**，再按需补正规带 token 的源。
- 所有数据落库都带 `source` 和数据时间，AI 分析时明确告知数据时间范围。
- 通过 `DataSourceAdapter` 适配层接入，新增源只加 adapter，不改上层（见 ARCHITECTURE 5.2）。

## 2. 当前选型（阶段 0 / MVP）

主市场 A 股，先用以下两个公开接口打底：

### 2.1 东方财富（push2 / push2his）

- 用途：实时行情快照、指数、板块涨跌、热门股票、资金流向、日线历史。
- 特点：免费、无需 key、覆盖最全、Go 直接 HTTP GET。
- 接口形态（社区整理，非官方文档，字段可能变动）：
  - 实时快照：`https://push2.eastmoney.com/api/qt/stock/get`
  - 列表/板块：`https://push2.eastmoney.com/api/qt/clist/get`
  - 日线历史：`https://push2his.eastmoney.com/api/qt/stock/kline/get`
- 注意：请求需带常见浏览器 `User-Agent`；股票代码要按东财规则加市场前缀（沪市 `1.`、深市 `0.`）。

### 2.2 新浪财经（hq.sinajs.cn）

- 用途：实时行情快照（作为东财的备份/交叉校验源）。
- 特点：免费、格式极简、Go 直接 HTTP GET。
- 接口形态：`https://hq.sinajs.cn/list=sh600000,sz000001`
- 注意：**必须带 `Referer: https://finance.sina.com.cn`**，否则可能被拒；返回是 GBK 文本，需转码。

### 2.3 两源关系

- 东财为主，新浪为辅。
- 同一字段两源都能取时，优先东财；东财异常时回退新浪，并在数据上标注实际来源。

## 3. 备选 / 按需启用：Tushare Pro

东财/新浪适合实时行情，但**财务数据、复权因子、交易日历、规整的日线**用 Tushare Pro 更省心。当需要做基本面分析、长线研究、准确的收益/回撤计算时再启用。

### 3.1 何时需要 Tushare

- 要填充 `stock_fundamentals`（PE/PB/ROE/营收/净利润等财报数据）。
- 要做长线推荐的基本面与估值分析。
- 要准确的 `daily_bars`（前复权/后复权）和 `corporate_actions`（除权除息）。
- 要 `trading_calendar`（交易日历）。

### 3.2 启用步骤

1. 注册 <https://tushare.pro>，在个人主页拿到 **API token**。
2. 把 token 填进 `deploy/.env` 的 `TUSHARE_TOKEN`（已预留该变量）。
3. Tushare 是**积分制**：注册有基础积分，部分高频接口需要更高积分（可通过完善资料/社区贡献/付费获得）。日线、财务、交易日历等常用接口的门槛不高。
4. 调用方式：HTTP POST `https://api.tushare.pro`，body 里带 `api_name`、`token`、`params`、`fields`，Go 直接发请求即可，无需 SDK。
5. 实现一个 `TushareAdapter`，归一化到内部标准结构后入库。

### 3.3 注意

- Tushare 免费版**没有真正的逐笔实时行情**，实时仍走东财/新浪；Tushare 负责财务/历史/复权/日历这类"慢数据"。
- 接口有频率限制（按积分），同步类任务要做节流与重试，失败写 `data_sync_logs`。

## 4. 后续扩展（暂不实现）

| 市场 | 候选源 | 说明 |
| --- | --- | --- |
| 美股 | Finnhub（免费 60 次/分，REST，实时报价+基本面）/ Alpha Vantage（免费额度小，仅够日线） | Go 直连，核心闭环跑通后再接 |
| 美股 | yfinance | Python 库，Go 不直连且 15 分钟延迟，不作主源 |
| 港股 | 东财/新浪（多为延迟 15 分钟）/ 富途 OpenD（实时但需开户、需常驻客户端） | 免费实时最难，建议最后做 |
| A股全量聚合 | AKShare | 数据最全但为 Python 库，需单独起 Python 服务，按需再评估 |

## 5. 风险与合规提醒

- 东财/新浪均为**非官方公开接口**，无 SLA、字段可能变动、可能限流封 IP。**仅适合个人自用，禁止公开高频拉取。**
- 需要稳定保障时，用 Tushare Pro 这类带 token 的正规 API，或考虑付费数据源。
- 适配层要把"换源"成本降到最低：上层只依赖内部标准结构，单个源挂掉可整体切换。
