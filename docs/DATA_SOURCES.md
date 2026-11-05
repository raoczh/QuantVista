# 数据源选型

> 当前实现与 2026-11-02 调研、验证范围见 [免费数据源稳定性说明](DATA_SOURCE_RELIABILITY.md)。免费、免鉴权不代表无限请求或永久稳定。
> 2026-11-03 部署故障专项修复、主备板块分类和补采验收见 [取数链路复核](DATA_PIPELINE_REVIEW.md)。
> 2026-11-04 新增新浪比例前复权、搜狐历史量额及全市场 250 日补采；当前接口与验证边界见 [稳定性说明第七节](DATA_SOURCE_RELIABILITY.md#7-2026-11-04-新增免费历史能力) 和取数链路复核第八节。

## 1. 选型原则

- 后端是 Go，**优先选能直接 HTTP 调用的数据源**，避免为某个 Python 库（如 AKShare）单独起一个 Python 微服务。
- 个人自用，**本轮只接免费公开接口**，不引入付费积分、限时试用或需要人工续期 Cookie 的默认依赖。
- 按能力组合东财、腾讯、新浪、搜狐、巨潮和财联社；Tushare 尚未接入，不作为任何功能的前置依赖。
- 所有数据落库都带 `source` 和数据时间，AI 分析时明确告知数据时间范围。
- 通过 `DataSourceAdapter` 适配层接入，新增源只加 adapter，不改上层（见 ARCHITECTURE 5.2）。

## 2. 当前选型

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

- 行情链路优先级：**东财 → 腾讯 → 新浪**，同时参考按能力记录的健康冷却状态。
- **前复权日线：东财 `fqt=1` → 腾讯 `qfqday` → 新浪原始 KLine 加 `qfq.js` 因子**。新浪价格来源为 `sina_qfq`，切换比例复权算法须完整重建；普通 raw KLine 仍不得直接进入股票分析及共享历史表。缺失的成交额、换手率可由搜狐同股同日事实补充，保留独立来源；落库不清除已有真实量额。
- **行业/概念热力图及行业榜：东财 → 新浪独立分类**。新浪行业为 `newSinaHy.php`，概念为 `newFLJK.php?param=class`，分类码保留 `sina:new_*` / `sina:gn_*`，不与 BK 同名合并。成员行情补查带日期的报价；板块指数/资金流和估值只在本来源口径中使用。板块主备同样进入能力矩阵。
- 指数、上证基准及历史交易日：腾讯 → 新浪；榜单及估值：东财与原有来源互补。全市场快照：东财 → 新浪，必须先通过完整性校验。
- 5 分钟线：腾讯 → 新浪；1 分钟线仍只有腾讯。公告：东财 → 巨潮公开披露网站；快讯分别采集财联社、东财和新浪后去重。
- 同一字段多源都能取时优先靠前的源；前源异常自动回退后源，并在数据上标注实际来源（`source` 字段）。
- 东财对单只 `stock/get` 限流较狠（常 EOF），实战中腾讯/新浪兜底命中率更高。

### 2.4 腾讯财经（qt.gtimg.cn）

- 用途：实时行情、估值、前复权日线、指数及 mkline m1/m5 分钟线，独立于东财/新浪。
- 接口：`https://qt.gtimg.cn/q=sh600000`（批量用逗号分隔），需带 `Referer`，返回 GBK 文本，`~` 分隔字段。
- 分钟线接口：`https://ifzq.gtimg.cn/appstock/app/kline/mkline?param=sh600000,m1,,260`；m1 用于个股分时图按需拉取，
  m5 用于盘中因子盘后同步。两者都无成交额列；m1 均价线按 `Σ(close×volume)/Σvolume` 估算并显式标注。
- 日线入口：`https://web.ifzq.gtimg.cn/appstock/app/fqkline/get`，股票只接受 `qfqday`；默认 120 根，上限 640 根，返回超出请求的当日附加根时取尾窗。指数显式使用 `sh000001` 的 `day`，不能把同号深市股票当指数。

### 2.4a 新浪前复权与搜狐历史量额

- 新浪：原始日线 `CN_MarketData.getKLineData?scale=240` 与证券对应 `finance.sina.com.cn/realstock/company/<sh/sz代码>/qfq.js` 两次有界请求，选取不晚于交易日的最近因子，将 OHLC 除以因子；成交量只由股转手。上限 1023 根，限沪深 A 股正股，不把股票两位小数算法套给 ETF。
- 新浪比例前复权和腾讯现金分红样本的算法存在差异；不能仅因最近收盘相同就拼接。换源完整替换、失败保留原序列；新源缺少因子时直接失败，不冒充不复权回退。
- 搜狐：`https://q.stock.sohu.com/hisHq`，参数 `code=cn_<股票代码>`、`start/end=YYYYMMDD`、`order=A&period=d&rt=json`。只接历史成交额与换手率事实，不把原始 OHLC 暴露成调整后价格。
- 搜狐成交额从万元乘 10000 转元；换手率保留百分数。仅填缺失同日事实，记录 `amount_source/turnover_source`，不改 `source` 所指的价格来源或成交量。
- 自动补充最多一个 400 自然日尾窗、3 秒且不超过日线路由剩余预算；不逐股扩散扫描。来源健康登记 `sina/daily_bars`、`sohu/daily_facts`，均可有界探测。

### 2.4b 东财 datacenter RPT_* 报表（免 token 统一网关）

- 入口：`https://datacenter-web.eastmoney.com/api/data/v1/get`，免 token，一个客户端解锁全部 RPT_* 报表。
- 常规 RPT 查询经 `DataCenterQuery`；F10 和三表保留专用入口，但和数据中心共用 `dcThrottle`。每次物理重试都重新节流，默认相邻请求至少 500ms；不叠加外层重试。
- 已接入报表：业绩预告/快报、预约披露、龙虎榜主表与机构统计、股东户数、研报评级/机构调研，
  以及 B8~B9 的四张公司行动报表（2026-07-28 逐字段实测）：

| 报表 | 用途 | 关键字段与**实测口径** |
| --- | --- | --- |
| `RPT_SHAREBONUS_DET` | 分红送转 / 除权除息 / 股息率 | `BONUS_RATIO`(每10股送) `IT_RATIO`(每10股转) `PRETAX_BONUS_RMB`(每10股派息税前,元) `EQUITY_RECORD_DATE` `EX_DIVIDEND_DATE` `REPORT_DATE` `ASSIGN_PROGRESS` `IMPL_PLAN_PROFILE` `DIVIDENT_RATIO`(**小数**,落库 ×100 转百分比)。**不返回 `SECURITY_TYPE_CODE`**，须用 A 股代码段白名单过滤（B 股 900xxx 会分红且被 `cnSecid` 放行） |
| `RPT_LIFT_STAGE` | 限售解禁 | `FREE_DATE` `CURRENT_FREE_SHARES`(**本次解禁量,万股**) `LIFT_MARKET_CAP`(**万元**) `FREE_SHARES_TYPE` `FREE_RATIO`/`TOTAL_RATIO`(**小数**) `SECURITY_TYPE_CODE`(058=股票,须过滤 060 可转债) |
| `RPTA_APP_IPOAPPLY` | 新股申购 | `APPLY_CODE`(与股票代码可不同) `APPLY_DATE` `ONLINE_APPLY_UPPER`(股) `ISSUE_PRICE`(**未定价为 null**) `BALLOT_PAY_DATE` `BALLOT_NUM_DATE` `LISTING_DATE` `MARKET`(板块) |
| `RPT_BOND_CB_LIST` | 可转债申购 | `SECURITY_CODE`(转债) `CORRECODE`(**申购代码**) `PUBLIC_START_DATE`(**网上申购日**) `CONVERT_STOCK_CODE`/`SECURITY_SHORT_NAME`(正股) `RATING` `ISSUE_PRICE` `ACTUAL_ISSUE_SCALE`(亿元) |

> **解禁字段的坑（必读）**：`FREE_SHARES` **不是**本次解禁量，而是「解禁后的已流通股数」；
> 本次真正解禁的是 `CURRENT_FREE_SHARES`（≈`ABLE_FREE_SHARES`）。实测 600675：
> CURRENT=21276.6 万股 vs FREE_SHARES=132074.5 万股，差 6 倍。
> 验算锚点：`CURRENT_FREE_SHARES × NEW(现价) == LIFT_MARKET_CAP`；
> `FREE_RATIO == CURRENT/(FREE_SHARES−CURRENT)`（占解禁前流通股）；
> `TOTAL_RATIO == CURRENT/(FREE_SHARES+NON_FREE_SHARES)`（占总股本）。
> 单位换算（万股→股、万元→元、小数→百分数）在 `datasource/emcorpaction.go` 收口，落库后全链路统一。

> **股息率字段的坑（C10，2026-07-29 第六十二批）**：`DIVIDENT_RATIO` 只在方案给出确定分配额时才有值，
> **预案与纯送转方案返回 null → 落库为 0**。因此「取最近一期方案的 `dividend_yield`」会在
> 刚发完中报预案的窗口里把股息率报成 **0%**，而 0% 会被用户和模型读成「这家公司不分红」。
> 正确口径是 `service/dividendyield.go` 的 `pickLatestDividendYield`：**只认 `> 0` 的行**，
> 最近一期没有就回退上一期（报告期须在 800 天窗口内），全都没有则**整项缺席**。
> 该值是「某期方案的年度口径」，**不是按当前股价实时折算的值**，展示与引用一律带报告期 as-of。

### 2.5 东财域名与请求保护

- 不再把 `{1..99}` 数字子域当成已验证的可用节点池；发送前统一到 `push2.eastmoney.com` / `push2his.eastmoney.com` 主域。
- `push2delay.eastmoney.com` 只是东财同源的延迟备用入口，不能算独立供应商；降级到期会单请求试探恢复，避免永久粘住备用域。
- 全进程同一域族共享限速，换域名不会增加请求额度。HTTP 429 的 `Retry-After` 不会被截短或通过立即换域绕过。
- 主域同样可能 EOF 或限流。恢复主要依靠真正独立的腾讯/新浪备用能力、缓存和有界退避，而不是不停更换域名。

### 2.6 P0-3A/P0-3B 能力注册与运维口径（代码已实现，待部署验收）

`server/datasource/health.go` 以代码内 `provider × capability × market` 注册表作为支持面的唯一事实来源，覆盖东财、腾讯、新浪、搜狐和巨潮经 `Manager` 可路由的能力。每行明确频率、时效、预算与缓存语义；未调用的组合显示 `unknown`。全市场快照注册为 `probe_disabled`，只在实际同步任务形成完整性观测，不允许普通探测按钮触发全市场抓取。新闻、F10、三表等仍有独立采集路径，不能把此矩阵当成全站数据已完整的证明。

健康滑窗按“源、能力、市场”隔离，并区分三种调用结局：

- `success`：响应通过该能力的非空校验；
- `empty`：调用正常结束但没有可消费数据，包含 `nil,nil` 和空切片归一结果；
- `error`：超时、网络或解析失败。`ErrNotSupported` 不进入健康滑窗。

现有 `GET /api/admin/datasources` 只读合并注册表与滑窗，返回样本数、三类计数、平均延迟、最后观测、最后成功、冷却剩余和恢复建议。它不发上游请求。冷却结束只恢复参与路由资格，不把历史空响应/错误伪装成成功。

P0-3A 同时增强现有 `GET /api/admin/data-health` 与三个补采 POST：健康报告只查本地库，以最近 30～60 个交易日为窗口，按日期索引和硬上限生成休市、停牌、缺失、部分覆盖与未知日历；`sync-bars`、`backfill-calendar`、`wide-sync` 的新 JSON body 支持有界范围与 `dry_run`，且只接受单个、最大 4KiB 的 JSON 对象。执行必须携计划 hash 并在上游调用前根据本地事实重算；完全无 body 的旧调用继续走原兼容路径。日历 dry-run 的 `target_count` 和 `sample_targets` 是执行时会逐日核对订正的完整日期范围，`missing_count` 才是当前缺行数量，不能把“已有但可能需订正”的日期从预演目标中隐藏。全市场 `wide-sync` 仍只具备当前应有交易日快照能力，不能冒充历史范围回补。

P0-3B 在上述只读矩阵之外补齐两个显式副作用 POST：

1. 管理员 `POST /api/admin/datasources/probe` 只接受注册表中的精确 provider/capability/market，直达指定适配器而不走备用源；quote/valuation 使用固定 `600000`，所有列表能力固定 `limit=1`。进程窗口最多 12 次/分钟、同三元组间隔 15 秒、全局并发最多 2，并按注册能力超时（最大 8 秒）收敛。
2. probe 的 `success/empty/error` 都写入现有健康滑窗。HTTP 响应和 `DataSyncLog` 只包含三元组、样本数、归一化代码与耗时；不保存原始请求/响应、token、cookie、API Key 或 Authorization。
3. 管理员 `POST /api/admin/datasources/uncool` 要求填写 1～200 字且不含凭据字段的原因，只清指定三元组的 cooldown；环形样本、最后观测/成功和冷却次数全部保留。审计记录管理员、原因、解冷前剩余秒数和是否实际清除。
4. 管理后台能力矩阵接入确认、loading、防连点、错误恢复和成功后的能力/日志局部刷新；375px/1440px 与亮暗主题均使用现有主题变量。禁止用有副作用的 GET 或页面刷新替代。

部署验收至少核对：注册未调用能力为 `unknown`；空响应与错误分列；冷却倒计时自动恢复；probe 非白名单、三元组/全局限流、并发满、超时、空响应和错误均按安全代码收敛且不含原始响应；解冷只影响指定三元组并保留历史观测。30/45/60 日查询均不触发上游；休市不进入业务覆盖分母、停牌从日线分母扣除；补采的 `null`、数组、布尔值、多对象和超过 4KiB 正文均被拒绝，旧无 body 请求仍可用；日历预演目标数等于完整订正范围且缺行数单列；dry-run 后改变本地覆盖事实会使旧 hash 拒绝执行；同步日志只含白名单摘要，不出现 token、cookie、密钥或原始正文；至少一亮一暗主题及 375px/1440px 下能力矩阵、按钮和缺口图例无硬编码白底、失真颜色或溢出。

## 3. Tushare：仅保留变量，未接入

当前没有 `TushareAdapter`，`TUSHARE_TOKEN` 只是历史预留变量。填写 Token 不会启用任何备用行情、财务或日历能力。本轮不接入付费积分接口，也不把可能变化的免费额度作为运行前提。财务三表与财务指标已由东财提供，不依赖购买 Tushare。

如未来另行评估，需以 [Tushare 官方文档](https://tushare.pro/document/1) 当时的权限与授权条件为准，不能沿用旧文档中固定积分档位的承诺。

## 4. 后续扩展（暂不实现）

| 市场 | 候选源 | 说明 |
| --- | --- | --- |
| 美股 | Finnhub（免费 60 次/分，REST，实时报价+基本面）/ Alpha Vantage（免费额度小，仅够日线） | Go 直连，核心闭环跑通后再接 |
| 美股 | yfinance | Python 库，Go 不直连且 15 分钟延迟，不作主源 |
| 港股 | 东财/新浪（多为延迟 15 分钟）/ 富途 OpenD（实时但需开户、需常驻客户端） | 免费实时最难，建议最后做 |
| A股全量聚合 | AKShare | 数据最全但为 Python 库，需单独起 Python 服务，按需再评估 |

## 5. 风险与合规提醒

- 东财/新浪均为**非官方公开接口**，无 SLA、字段可能变动、可能限流封 IP。**仅适合个人自用，禁止公开高频拉取。**
- 本轮不要求付费。免费接口只能通过多源互补和工程保护降低故障影响，不能承诺 SLA。
- 适配层要把"换源"成本降到最低：上层只依赖内部标准结构，单个源挂掉可整体切换。

## 6. akshare 作为"接口字典"（调研结论）

[akshare](https://github.com/akfamily/akshare) 是 Python 库，本项目**不引入**（后端 Go，不为它起 Python 服务），但把它当**公开接口字典**用——从其源码挖出真实 HTTP 接口，用 Go 直连。已采纳/候选：

| 能力 | 来源 | 接口 | 状态 |
| --- | --- | --- | --- |
| 实时行情 | 腾讯 | `qt.gtimg.cn/q=` | ✅ 已接入（2.4） |
| 东财主域 | 东财 | `push2.eastmoney.com` | 已取消任意数字节点轮询，见 2.5 |
| 板块/榜单 | 东财 | `push2.eastmoney.com/api/qt/clist/get` | 已接入，依赖缓存与共享限速 |
| 1/5 分钟线（分时图/盘中因子） | 腾讯 | `ifzq.gtimg.cn/appstock/app/kline/mkline` | ✅ 已接入（A1/M3b，m1 按需短缓存、m5 盘后同步；`tencent_mkline.go`） |
| 财务三表/F10/财报日历/公告 | 东财 | `datacenter.eastmoney.com`（RPT_* 报表族） | ✅ 已接入（F1/F2，`emdatacenter.go` 网关 QPS≤2） |
| 龙虎榜/机构席位 | 东财 | datacenter `RPT_DAILYBILLBOARD_DETAILSNEW` 等 | ✅ 已接入（M3a，`emlhb.go`） |
| 涨停池/连板/炸板情绪 | 东财 | `push2ex.eastmoney.com/getTopicZTPool` 族 | ✅ 已接入（M3a，`eastmoney_ztpool.go`，不可回溯靠每日快照积累） |
| 股吧人气榜 | 东财 | `emappdata.eastmoney.com` 人气榜 | ✅ 已接入（M3a） |
| 个股/两市资金流 | 东财 | clist f62 族排行 + `push2his` fflow/daykline 单股历史 | ✅ 已接入（M3a，`eastmoney_fflow.go`，规避了同花顺反爬） |
| 板块热度榜/成分股/板块指数 | 东财 | clist fs=m:90 + secid=90.BKxxxx kline | ✅ 已接入（M3c，`eastmoney_board.go`） |
| 板块资金流历史/行业估值聚合 | 东财 | fflow/daykline secid=90.BKxxxx + clist 扩 f9/f115/f23/f100 估值字段 | ✅ 已接入（P3b，板块资金流纯透传；行业估值按 f100 聚合中位数+分位落库） |
| 研报评级/机构调研 | 东财 | `reportapi.eastmoney.com/report/list` + datacenter `RPT_ORG_SURVEYNEW` | ✅ 已接入（P3a，`emorgview.go`，按需拉取缓存禁普查） |
| 快讯/个股新闻 | 财联社 + 东财 | `/v1/roll/get_roll_list`（sign 双哈希）+ 东财快讯/search-api | ✅ 已接入（N1，`cls.go`/`eastmoney_news.go`） |
| 个股资金流 | 同花顺 | `data.10jqka.com.cn/funds/ggzjl/...` | ❌ 放弃：hexin-v 反爬成本高，已改走东财 f62 族（上行） |
| 千股千评 | 东财 | `stock_comment_em` 系列 | ❌ 砍掉（P3a 评估：东财机器生成参考价值薄，市场情绪已由涨停池/人气榜覆盖） |
| 热搜榜 | 百度 | `stock_hot_search_baidu` | ⏳ 候选 |
| 个股榜单 | 腾讯 | `proxy.finance.qq.com/cgi/.../getBoardRankList` | ⏳ 候选（参数待调） |

> 资金流的反爬（同花顺 hexin-v）已绕开：M3a 用东财资金流（clist `f62` 族 + push2his fflow 单股历史）实现，同花顺路线放弃。

### 6.1 免费与可用性分开判断

- 东财、腾讯、新浪的本次接入不需要个人凭证，但全部可能限流或改版。一次样本成功不等于“稳定免维护”。
- 巨潮本次使用 `www.cninfo.com.cn` 的公开证券搜索及公告查询，和需要 Token 的 `webapi.cninfo.com.cn` 是不同产品，不能混为一谈。
- AKShare、efinance 是封装库，不是天然独立的供应商；例如二者调用东财时，不能抵御同一次东财故障。
- BaoStock、mootdx 的公共路线涉及 TCP/SDK 与额外协议维护；本轮做了源码核实但未加入默认部署。雪球 Cookie、同花顺复杂反爬、付费积分和未经验证的营销 API 不作为稳定性兜底。
- 完整研究链接、哪些能力仍是单源、实测成功和失败均记录在 [稳定性说明](DATA_SOURCE_RELIABILITY.md)。

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
