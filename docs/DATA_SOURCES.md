# 数据源选型

## 1. 选型原则

- 后端是 Go，**优先选能直接 HTTP 调用的数据源**，避免为某个 Python 库（如 AKShare）单独起一个 Python 微服务。
- 个人自用，**先用免费、实时、能直连的源打底**，再按需补正规带 token 的源。
- **第一阶段（MVP）以东财 + 新浪为主源，Tushare 仅作可选辅助、不作为前置依赖**；Tushare 按积分分档，免费档与低 cost 档按需启用，高级档暂不实现（见第 3 节）。
- 所有数据落库都带 `source` 和数据时间，AI 分析时明确告知数据时间范围。
- 通过 `DataSourceAdapter` 适配层接入，新增源只加 adapter，不改上层（见 ARCHITECTURE 5.2）。

## 2. 当前选型（阶段 0 / MVP）

主市场 A 股（**仅沪深**：代码首位 6/5/9 为沪、0/2/3 为深；北交所 4/8 开头暂不支持），先用以下公开接口打底。

**口径约定**：成交量统一为**手**（东财/腾讯原生手；新浪返回股，解析时 /100 归一）；成交额单位为**元**。

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

### 2.3 三源关系

- 行情链路优先级：**东财（数据最全）→ 腾讯（稳定）→ 新浪（兜底）**。
- **日线主源是东财**（`push2his` kline，`fqt=1` 前复权），新浪 KLine 仅作东财失败时的兜底（该接口无复权参数、不提供成交额，落库时不覆盖已有成交额）；指数/榜单走新浪。
- 同一字段多源都能取时优先靠前的源；前源异常自动回退后源，并在数据上标注实际来源（`source` 字段）。
- 东财对单只 `stock/get` 限流较狠（常 EOF），实战中腾讯/新浪兜底命中率更高。

### 2.4 腾讯财经（qt.gtimg.cn）

- 用途：实时行情快照（独立于东财/新浪的第三源，稳定性好）。
- 接口：`https://qt.gtimg.cn/q=sh600000`（批量用逗号分隔），需带 `Referer`，返回 GBK 文本，`~` 分隔字段。
- 已接入为行情链路第二源；日线腾讯接口字段不稳，未用。

### 2.5 东财负载节点（重要技巧）

- 东财把接口分流到 `{1..99}.push2.eastmoney.com` / `{1..99}.push2his.eastmoney.com` 负载节点（来自 akshare 实现）。
- 本项目对东财请求**轮询数字子域名**（而非裸 `push2.eastmoney.com`），分散单节点限流、降低 EOF 概率。

## 3. 按需启用：Tushare Pro（分档）

**定位（重要）**：第一阶段（MVP）以东财 + 新浪为主源，Tushare 仅作可选辅助，**不作为前置依赖**。Tushare 采用积分制、按积分分档，本项目策略：免费档与低 cost 档按需保留，高级档暂不实现。

| 档位 | 积分 | 是否花钱 | 本项目策略 |
| --- | --- | --- | --- |
| 免费档 | 120 | 注册 + 完善资料即可，不花钱 | 可选，东财也能覆盖，Tushare 更规整 |
| 低 cost 档 | 2000 | 需少量捐赠 | 按需启用，长线财务深度依赖此档；非 MVP 前置 |
| 高级档 | 5000 | 捐赠较多 | **暂不实现** |

### 3.1 免费档（120 分，不花钱）

接口：`stock_basic`（股票清单）、`daily`（日线）、`weekly`/`monthly`、`trade_cal`（交易日历）、`holiday`。

用途：规整的股票清单、日线 OHLC、交易日历。东财也能给日线和清单、日历可从指数日线反推，所以**这档不是必须**，但 Tushare 字段更规整、有文档，想省事可启用。

### 3.2 低 cost 档（2000 分，需少量捐赠）

接口：`daily_basic`（PE/PB/股息率/换手率）、`income`/`balancesheet`/`cashflow`（财务三表）、`fina_indicator`（ROE/毛利率/负债率等财务指标）、`forecast`/`express`（业绩预告/快报）、`dividend`（分红送股）、`adj_factor`（复权因子）、`stk_limit`（涨跌停价）、`index_daily`（指数日线，算 alpha 用）。

用途：**这是长线推荐财务深度的来源**（财务三表 + 多期财务指标），也是精确复权因子、分红明细、涨跌停价、基准指数序列的规整来源。

启用时机：**按需、非 MVP 前置**。当真正要做长线基本面分析、且愿意少量捐赠时再启用。未启用时，长线推荐降级为"轻量版"——用东财实时估值（PE/PB/市值）+ 部分实时财务快照 + 行情 + 新闻做粗判，不依赖财务三表，深度有限（无多期财务趋势、无三表细节）。

### 3.3 高级档（5000 分，本项目暂不实现）

接口：`stk_mins`（分钟线）、`margin_detail`（融资融券明细）、`ccass_hold`（中央结算持股）等。

用途：分钟级行情、融资融券明细、沪深港通持股明细。

**标记暂不实现**：对个人自用研究工具，分钟线与这类明细数据性价比不足（捐赠门槛高、对长短线研究与追踪非必需），本项目不做。实时盘中仍走东财/新浪；如未来确需分钟级数据再评估。

### 3.4 启用步骤

1. 注册 <https://tushare.pro>，在个人主页拿到 **API token**。
2. 把 token 填进 `deploy/.env` 的 `TUSHARE_TOKEN`（已预留该变量）。
3. 免费档：注册 + 完善资料 + 关注公众号即可达到 120 分。低 cost 档：需少量捐赠到 2000 分（具体以官网当时规则为准）。高级档需更高捐赠，本项目不启用。
4. 调用方式：HTTP POST `https://api.tushare.pro`，body 里带 `api_name`、`token`、`params`、`fields`，Go 直接发请求即可，无需 SDK。
5. 实现一个 `TushareAdapter`，归一化到内部标准结构后入库。

### 3.5 注意

- Tushare 免费版**没有真正的逐笔实时行情**，实时仍走东财/新浪；Tushare 负责财务/历史/复权/日历这类"慢数据"。
- 接口有频率限制（按积分），同步类任务要做节流与重试，失败写 `data_sync_logs`。
- token 后端加密保存，不进前端、不进日志。

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

## 6. akshare 作为"接口字典"（调研结论）

[akshare](https://github.com/akfamily/akshare) 是 Python 库，本项目**不引入**（后端 Go，不为它起 Python 服务），但把它当**公开接口字典**用——从其源码挖出真实 HTTP 接口，用 Go 直连。已采纳/候选：

| 能力 | 来源 | 接口 | 状态 |
| --- | --- | --- | --- |
| 实时行情 | 腾讯 | `qt.gtimg.cn/q=` | ✅ 已接入（2.4） |
| 东财负载节点 | 东财 | `{1..99}.push2.eastmoney.com` | ✅ 已采纳（2.5） |
| 板块/榜单 | 东财 | `{n}.push2.eastmoney.com/api/qt/clist/get` | ✅ 已用（best-effort，限流时降级） |
| 5 分钟线（盘中因子） | 腾讯 | `ifzq.gtimg.cn/appstock/app/kline/mkline` | ✅ 已接入（M3b，`tencent_mkline.go`，count≤800≈18 交易日） |
| 财务三表/F10/财报日历/公告 | 东财 | `datacenter.eastmoney.com`（RPT_* 报表族） | ✅ 已接入（F1/F2，`emdatacenter.go` 网关 QPS≤2） |
| 龙虎榜/机构席位 | 东财 | datacenter `RPT_DAILYBILLBOARD_DETAILSNEW` 等 | ✅ 已接入（M3a，`emlhb.go`） |
| 涨停池/连板/炸板情绪 | 东财 | `push2ex.eastmoney.com/getTopicZTPool` 族 | ✅ 已接入（M3a，`eastmoney_ztpool.go`，不可回溯靠每日快照积累） |
| 股吧人气榜 | 东财 | `emappdata.eastmoney.com` 人气榜 | ✅ 已接入（M3a） |
| 个股/两市资金流 | 东财 | clist f62 族排行 + `push2his` fflow/daykline 单股历史 | ✅ 已接入（M3a，`eastmoney_fflow.go`，规避了同花顺反爬） |
| 板块热度榜/成分股/板块指数 | 东财 | clist fs=m:90 + secid=90.BKxxxx kline | ✅ 已接入（M3b/M3c，`eastmoney_board.go`） |
| 快讯/个股新闻 | 财联社 + 东财 | `/v1/roll/get_roll_list`（sign 双哈希）+ 东财快讯/search-api | ✅ 已接入（N1，`cls.go`/`eastmoney_news.go`） |
| 个股资金流 | 同花顺 | `data.10jqka.com.cn/funds/ggzjl/...` | ❌ 放弃：hexin-v 反爬成本高，已改走东财 f62 族（上行） |
| 千股千评 | 东财 | `stock_comment_em` 系列 | ⏳ 候选（市场情绪已由涨停池/人气榜覆盖大半，边际收益低） |
| 热搜榜 | 百度 | `stock_hot_search_baidu` | ⏳ 候选 |
| 个股榜单 | 腾讯 | `proxy.finance.qq.com/cgi/.../getBoardRankList` | ⏳ 候选（参数待调） |

> 资金流的反爬（同花顺 hexin-v）已绕开：M3a 用东财资金流（clist `f62` 族 + push2his fflow 单股历史）实现，同花顺路线放弃。

### 6.1 稳定性分级（实测结论）

按"是否无脑 Go 直连（免 token/cookie/反爬）"分三档：

- **A 档·稳定免鉴权（已全部接入为行情链路）**：东财（`*.push2.eastmoney.com`）、腾讯（`qt.gtimg.cn`）、新浪（`hq.sinajs.cn` + `money.finance` 日线 + `Market_Center` 榜单——同一接口支持 sort=changepercent/amount/turnoverratio/pb 等字段与 asc 方向参数（升序=跌幅/低PB「不热」榜），返回自带 per/pb/mktcap/nmc[万元]；实测 per 升序不可用：负 PE 亏损股整段排最前无法翻越）。三源互备，行情够用。
- **B 档·同源免鉴权但限流（按需扩数据类型）**：**东财数据中心 `data.eastmoney.com` / `push2.eastmoney.com clist`** —— 北向资金、龙虎榜、资金流（`f62` 等字段）、财报、研报、板块全在这，**免 token、数据最全**，代价是扛东财限流（已用数字子域名轮询缓解）。扩"资金流/北向/龙虎榜/财报"优先走这。
- **C 档·需鉴权/反爬（按需、非无脑）**：巨潮 `webapi.cninfo.com.cn`（财务/基本面，需 token，实测 451）、雪球 `stock.xueqiu.com`（需 `xq_a_token` cookie，会过期）、同花顺 `data.10jqka.com.cn`（hexin-v 反爬）。巨潮另有**公告披露** `www.cninfo.com.cn/new/disclosure`（免 token，做"公告/财报观察点"时可用）。

> 一句话：**稳定免维护的只有东财/新浪/腾讯三个行情源（已接入）**；要扩更多数据类型，优先用同属东财、免 token 的 `data.eastmoney.com`，扛住限流即可；巨潮/雪球/同花顺都带鉴权门槛，按需再接。

## 7. 待 Tushare / 新闻源解锁的功能清单（2026-07-09 更新：多数已由东财/财联社免 token 路线实现）

> 原约定：**凡必须 Tushare（或稳定新闻源）才能做的功能，接入前不在前端展示任何占位/敬请期待**。2026-07 起 N/F/M 批次用东财 datacenter + 财联社免 token 路线实现了原清单大部分项，Tushare 至今未成为任何功能的前置依赖。

| 功能 | 原依赖 | 状态 |
| --- | --- | --- |
| 财经新闻 / 新闻情绪分析 | 稳定新闻源 | ✅ 已实现（N1/N2：财联社+东财源，`/news` 页 + LLM 情绪增强，免 Tushare） |
| 财务数据详情（三表/F10） | Tushare 低 cost 档 | ✅ 已实现（F1/F2：东财 datacenter + emweb 三表，个股详情财务块 + 长线推荐 fin 富化） |
| 财报日历 / 临近财报待办 | Tushare 财报披露日期 | ✅ 已实现（F1：东财预约披露/业绩预告/快报，earn_date/earn_fcst 提醒类型） |
| 回测模块 | 复权后 daily_bars | ✅ 已实现（M2 时光机：东财前复权 + 复权自洽校验 adjustSuspect 剔除断层股） |
| 复权因子（corporate_actions） | Tushare 复权因子接口 | ⏳ 仍未建：现行方案为东财前复权 + 除权检测整股重锚（M1），彻底解法仍需复权因子表（见 ROADMAP 边界区「追踪复权」） |

> 注意：AI 提示词层面的数据边界声明（如问答页"仅依据行情与技术指标（无财务/新闻）回答"）**不是占位、不隐藏**——那是反编造的如实标注，接入财务数据后同步更新措辞。
