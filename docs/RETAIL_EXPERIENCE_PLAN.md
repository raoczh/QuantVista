# 散户日常体验补齐计划

> **定位**：本项目前 57 批开发集中在 AI 链路准确性（`LLM_ACCURACY_OPTIMIZATION_PLAN.md`）与推荐测准
> （`RECOMMENDATION_ACCURACY_PLAN.md`）两条工程主线上，两者均已收官。本计划补的是另一类东西——
> **普通散户每天真正会用到的朴素功能**：看分时、看盘面情绪、记账本、算自己的收益、被提醒。
>
> 判断标准只有一条：**「一个拿点小钱买股票的普通人，会不会每天用到它」**。
> 不做面向机构/量化研究员的功能，不做为了对标而对标的功能。
>
> 权威性：本文档是散户体验域的施工图与验收依据。防回归要点入 `ROADMAP.md` §3。
>
> **实施状态（2026-07-28）**：第一批 A1~A4、第二批 B5~B7 已完成代码实现与程序化测试；B8~C13 按第三、四批继续实施。

---

## 0. 缺口来源与对比基准

2026-07-27 全项目功能审查结论。对比基准为 GitHub 同类开源项目
[go-stock](https://github.com/ArvinLovegood/go-stock)（3.5k★，涨跌报警/异动监控/涨停梯队/定时 AI 监控）、
[myhhub/stock](https://github.com/myhhub/stock) 与 [InStock](https://github.com/ethqunzhong/InStock)
（K 线形态识别 30+、综合选股五维度、龙虎榜/大宗交易），以及券商 APP 通用能力
（投资日历、打新提醒、除权除息推送、资产曲线）。

**审查结论**：本项目在 AI 可信度工程上显著领先同类；缺口集中在「日常盯盘 + 个人账本」两类朴素能力。
其中近半数缺口的**后端数据早已落库或上游已验证可拉**，只差消费出口——这是本计划优先级排序的主要依据。

---

## 1. 全量缺口清单（13 项）

| 编号 | 缺口 | 现状证据 | 散户价值 | 批次 |
| --- | --- | --- | --- | --- |
| **A1** | 分时图（当日走势） | `datasource/tencent_mkline.go` 已有 m5 适配；`web/src` 搜 `分时/intraday/min5` **零命中** | 打开个股第一眼要看的东西 | 一 |
| **A2** | 盘面情绪无前端出口 | `mood.go` 已落库连板分布/炸板率/昨涨停溢价，**只喂 AI**（`analysis_context.go:383`/`dailyreport.go:527`），前端零命中 | 复盘核心四问 | 一 |
| **A3** | 龙虎榜/人气榜只有个股维度 | `router/api.go:124` 仅 `/stocks/:symbol/lhb`；`SyncLhb` 落的是全市场 | 看游资动向 | 一 |
| **A4** | 提醒 15 分钟一轮 | `alert.go:655` 固定 `NewTicker(15*time.Minute)`，不分交易时段 | 到价提醒延迟最多 15 分钟，短线失效 | 一 |
| **B5** | 持仓不支持分批加减仓 | `position.go` 仅 `Create/Update/Close`，`Close` 一次性全平 | 散户必然分批操作 | 二 |
| **B6** | 无个人交易复盘统计 | `Close` 已收集 `SellPlanned/AiVerdict/LessonLearned`，只存不聚合 | 自我改进的核心 | 二 |
| **B7** | 无账户资产曲线 | 持仓/模拟盘 `Overview` 均读时计算不落快照 | 回答「这几个月赚没赚」 | 二 |
| **B8** | 除权除息不调整持仓 | `position.go` 全文无「除权」；10 转 10 后盈亏显示 -50% | **会直接显示错误数字** | 三 |
| **B9** | 事件日历缺解禁/除权/打新 | `emdatacenter.go:14` 注释列了解禁报表但未接；`analysis_context.go:166` 让 AI 声明「解禁未接入请自行核查」 | 打新白捡收益；解禁是 A 股头号利空 | 三 |
| **C10** | 无股息率 | `datasource/types.go:93` `Valuation` 有 PE/PB/换手/量比/振幅，无股息率 | 红利股必看 | 四 |
| **C11** | 提醒不关联持仓成本 | 8 类规则全基于市场价，无一类关联自己的成本 | 最贴身的提醒反而没有 | 四 |
| **C12** | 无 K 线形态识别 | 52 因子宽表有均线/位置/新高，无经典形态 | 对标 InStock；进选股条件树 | 四 |
| **C13** | 无行业/风格暴露 | `Overview` 仅集中度信号 | 看自己是不是压了单一赛道 | 四 |

---

## 2. 数据源可行性（2026-07-27 实测）

新增上游全部走已有网关，**不引入新鉴权、不依赖 Tushare**。

### 2.1 分时线：腾讯 mkline `m1`（实测通过）

```
https://ifzq.gtimg.cn/appstock/app/kline/mkline?param=sh600000,m1,,240
→ data.{code}.{ qt, m1, prec }
  m1 行 8 列 = [时间YYYYMMDDHHmm, 开, 收, 高, 低, 量(手), {}, 分钟换手]
```

**与既有 m5 完全同构**——`parseMin5Response` 的解析逻辑可直接复用，只需把 period 参数化。
`prec` 键提供前收盘（分时图基准线）。无成交额列，均价线按 `Σ(close×vol)/Σvol` 服务端估算
（与 `intraday.go` 既有 VWAP 口径声明一致，前端如实标注「估算均价」）。

### 2.2 公司行动：东财 datacenter RPT_*（实测通过，走 `DataCenterQuery` 网关）

| 报表 | 用途 | 关键字段（实测） |
| --- | --- | --- |
| `RPT_SHAREBONUS_DET` | 分红送转 / 除权除息 / 股息率 | `BONUS_RATIO`(每10股送) `IT_RATIO`(每10股转) `PRETAX_BONUS_RMB`(每10股派息税前) `EQUITY_RECORD_DATE` `EX_DIVIDEND_DATE` `ASSIGN_PROGRESS` `IMPL_PLAN_PROFILE` `DIVIDENT_RATIO`(股息率小数) |
| `RPT_LIFT_STAGE` | 限售解禁 | `FREE_DATE` `FREE_SHARES` `LIFT_MARKET_CAP` `FREE_SHARES_TYPE` `FREE_RATIO` `SECURITY_TYPE_CODE`(058=股票，**须过滤**) |
| `RPTA_APP_IPOAPPLY` | 新股申购 | `APPLY_CODE` `APPLY_DATE` `ONLINE_APPLY_UPPER` `ISSUE_PRICE` `BALLOT_PAY_DATE` `BALLOT_NUM_DATE` `LISTING_DATE` |
| `RPT_BOND_CB_LIST` | 可转债申购 | `SECURITY_NAME_ABBR` `CONVERT_STOCK_CODE` `RATING` `ISSUE_PRICE` 申购日 |

**纪律**：全部经 `DataCenterQuery`（包级令牌桶 QPS≤2 + 退避重试），不得另起 HTTP 路径；
`SECURITY_TYPE_CODE` 058 前缀过滤沿用龙虎榜先例（060=可转债，6 位数字代码挡不住）。

---

## 3. 第一批：盘面出口（A1~A4）

> 主题：**已落库/已可拉的数据，补上消费出口**。零新表、零新上游鉴权。

### A1 分时图

- **datasource**：`GetMin5Bars` 泛化为 `getMinuteBars(ctx, market, symbol, period, count)`，
  `GetMin5Bars` 保留为薄封装（既有调用方零改动），新增 `GetMin1Bars`。解析器共用。
- **service**（`minuteline.go`）：`MinuteLine(ctx, market, symbol)` → 分时点序列 + 前收盘 + 估算均价 +
  当日累计量。进程内短缓存（60s，同标的并发 miss 合并、过期清理且容量有界，盘中高频刷新防打上游）。
  **非交易时段返回最近交易日**（如实带 `trade_date`）。
- **API**：`GET /api/markets/:market/stocks/:symbol/minute`
- **前端**：`StockDetail.vue` K 线区加「分时 / 日K」切换。分时图 = 价格线 + 均价线 + 昨收虚线基准 +
  下方成交量柱；涨跌色随主题（**禁硬编码色**，走 CSS 变量）。

**边界声明**：仅 A 股；上游仅提供当日+近日 m1，不做历史分时归档。

### A2 盘面情绪页

- **service**（`mood.go`）：新增 `MoodOverview(market, days)` →
  ①最近一日 `MarketMoodDaily` 全字段；②**连板梯队**（当日 `LimitUpStock` 按 `Streak` 分组，
  高度降序，每档列个股）；③近 N 日涨停家数/炸板率序列（情绪趋势）；④封板资金 Top。
- **API**：`GET /api/markets/:market/mood?days=`
- **前端**：新页 `/mood`「盘面情绪」+ 菜单项 + `Home.vue` 情绪卡加跳转入口。

**边界声明**：涨停池上游不可回溯（既有纪律），历史靠每日盘后快照积累，**缺勤日如实缺失不补造**。

### A3 全市场龙虎榜 / 人气榜

- **service**（`mood.go`）：`LhbDaily(market, date, limit)`（按净买额降序，含机构席位统计）、
  `PopularityDaily(market, date)`（含新上榜标记）。date 空取最近有数据日。
- **API**：`GET /api/markets/:market/lhb?date=&limit=`、`GET /api/markets/:market/popularity?date=`
- **前端**：并入 `/mood` 页做 tab（情绪总览 / 连板梯队 / 龙虎榜 / 人气榜），避免页面爆炸。

### A4 提醒动态提频

- `alert.go` 的固定 15 分钟 ticker 改为**动态间隔循环**：
  - 交易日 09:25~11:35 / 12:55~15:05 → `alertIntervalTrading = 2min`
  - 其余时段 → `alertIntervalIdle = 30min`
- 交易日判定复用既有交易日历（`isTradingDayToday` 同源，**不另起判定**）。
- 调度按固定节拍并发派发用户，用户级 `TryLock` 防止慢轮次排队重叠；盘中单用户超时 110 秒，
  上一轮仍未结束则跳过该轮，避免恢复后集中重复评估。
- **净效果与容量口径**：盘中目标检查间隔从 15min 降到约 2min；一个完整交易日约 169 个调度轮次
  （交易窗口约 130 轮 + 其余时段约 39 轮），高于旧实现的 96 轮。非交易时段负载下降，但全天请求量约增加
  76%，这是到价提醒时效的明确成本，部署后须观察免费行情源限流与单轮耗时，不能再宣称总轮数下降。

---

## 4. 第二批：个人账本（B5~B7）

> 主题：**把「持仓」从一条静态记录升级为一本真实账本**。
>
> **实施状态（2026-07-28，第五十九批）**：B5~B7 全部落地。与本节原设计的三处偏差如实记录在
> 各小节的「实施补记」中——都是实施中发现原设计不足而做的加强，不是缩水。

### B5 分批加仓 / 减仓

- **新表** `PositionTrade`：`user_id / position_id / side(buy|sell) / price / quantity / fee / tax /
  trade_date / note`，索引 `(user_id, position_id)`。
- **口径铁律（下游兼容的关键）**：
  `Position.BuyPrice` 恒为**当前持仓的加权平均成本**、`Position.Quantity` 恒为**当前持仓数量**——
  所有既有消费方（tracking 的 `actual_position` 标签、`todo` 止损、`guard` 事件、组合总览）读法零改动。
  流水表是明细来源，汇总值回写 Position。
- **新增字段** `Position.RealizedPnl`（累计已实现盈亏，部分卖出时累加）。
- **行为**：
  - `Create` 时自动建首笔 buy 流水（保证账本自洽）；
  - `AddTrade(buy)` = 加仓 → 加权重算成本、数量累加；
  - `AddTrade(sell)` = 减仓 → 按当前均价结转已实现盈亏、数量递减；**减到 0 自动置 `closed`**
    并要求补复盘（沿用既有 `Close` 的复盘字段）；
  - 卖出数量超过持仓 → 拒绝。
- **旧数据兼容**：无流水的存量持仓，读取时**惰性补建**一笔等价 buy 流水（幂等，不改变任何汇总值）。
- **前端**：`Positions.vue` 每行加「加仓/减仓」入口 + 展开流水明细。

**实施补记（2026-07-28）**：

1. **`RealizedPnl` 一个字段不够**——全部卖出后 `Quantity` 归 0、`BuyFee/BuyTax` 结清，
   已平仓收益率的分母（累计买入成本）与「一共买过多少股」都无从还原，旧的
   `computeView` 算式 `SellPrice×Quantity − 卖出费税 − 成本` 在分批卖出下必然算错。
   故实际落了**四个**汇总列：`RealizedPnl / TotalBuyCost / TotalSellNet / TotalBuyQty`。
   `computeView` 对已平仓分两路：有流水走账本口径，旧记录（尚未补建）走原算式**逐字节等价**。
2. **`Close` 改为走同一流水逻辑**（= 卖出全部剩余数量的减仓笔 + 复盘字段），
   不再直接改状态——否则「一键平仓」绕过账本，已实现盈亏与流水当场对不上。
3. **`Update` 补了防绕过约束**：持仓一旦有过真实加/减仓（流水多于建仓那一笔），
   买入价格/数量/费税一律冻结；只有「仅建仓一笔」的持仓允许修正录入错误，
   且同一事务内把那笔流水一并改掉。CSV 导入同样落首笔流水。
4. **惰性补建不改动 `Quantity/BuyPrice/BuyFee/BuyTax/SellPrice`**：旧 `closed` 记录的
   `Quantity` 保持为「平仓时的数量」，导出与展示逐字不变；只回填四个新汇总列 + 流水行。

### B6 个人交易复盘统计

- **新 service** `tradestat.go`：吃已平仓持仓 + 流水，聚合
  ①总已实现盈亏 / 胜率 / 盈亏比 / 平均持有交易日；
  ②按**行业 / 持有时长档 / 买入理由 / `AiVerdict` / `SellPlanned`** 分布；
  ③最赚 & 最亏 Top5；④`LessonLearned` 汇总清单。
- **纪律**：纯读时聚合零落库；**不与推荐归因报表混算**（那是模型口径，这是用户执行口径）。
- **API**：`GET /api/positions/stats?range=`
- **前端**：`Positions.vue` 新增「复盘统计」tab。

**实施补记（2026-07-28）**：三处「诚实表达」按纪律落地——①窗口内零样本不给 0% 胜率，
如实声明「没有样本」；②无亏损交易时盈亏比为 `null`（不是 0 也不是 ∞，样本不足以给出该比值）；
③行业未积累的标的单列「行业未知」组且**恒排最后**（不摊派到其它行业，也不混在真实取值中间
被读成「未知行业赚得最多」）；缺买卖日期/无交易日历的样本不进平均持有交易日并计数声明。

### B7 资产曲线

- **新表** `PortfolioSnapshot`：`user_id / kind(real|paper) / trade_date / market_value / cost /
  unrealized_pnl / realized_cum / cash(仅 paper) / position_count`，唯一键 `(user_id, kind, trade_date)`。
- **job**：交易日 16:20（避开 16:10 全市场日线、16:35 涨停池）为全部用户落当日快照，幂等 upsert。
- **fail-closed**：某标的当日无有效收盘价 → 该用户当日快照**标记 `partial` 并记录缺口数**，
  不用旧价冒充（沿用 `Overview` 既有 `ValuationNote` 纪律）。
- **API**：`GET /api/positions/curve?days=`、`GET /api/paper/curve?days=`
- **前端**：`Positions.vue` / `Paper.vue` 顶部资产曲线图。

**实施补记（2026-07-28）**：①非 fresh 的标的**既不进市值也不进成本**——只剔市值会让浮亏
等于负的成本，比缺这个点更糟；②模拟盘快照与 `PaperService.Overview` 有意不同：Overview 用
成本兜底估值保证总资产可读，**快照不兜底**，否则一段停牌期会在曲线上画出一条平直的假净值；
③曲线点上 partial 日以空心大点标出并在 tooltip 说明，前端不得把 partial 点当完整净值。

---

## 5. 第三批：公司行动与事件日历（B8~B9）

> 主题：**接入 A 股散户绕不开的四类事件**，并修掉「除权后盈亏显示错误」这个真实缺陷。

### 数据层

- **新 adapter** `datasource/emcorpaction.go`：四张 RPT_* 报表的拉取与解析（走 `DataCenterQuery`）。
- **新表**：
  - `CorporateAction`（分红送转）：`symbol/market/ex_date/record_date/bonus_ratio/transfer_ratio/
    dividend_pretax/plan_profile/progress/report_date/dividend_yield`，唯一键 `(symbol,market,ex_date,report_date)`；
  - `RestrictedRelease`（解禁）：`symbol/market/free_date/free_shares/lift_market_cap/free_type/free_ratio`；
  - `IpoSubscription`（新股/可转债申购）：`kind(stock|cb)/code/apply_code/name/apply_date/issue_price/
    apply_upper/pay_date/listing_date/convert_stock_code/rating`。
- **job**：每日 19:25 增量同步（避开 18:45 龙虎榜、19:05 财报）。解禁与申购拉未来 60 天窗口，
  分红拉近 90 天公告窗口。

### B8 除权除息调整持仓

- **检测**：持仓股命中 `CorporateAction.ex_date == 今日` → 生成**待确认调整提示**。
- **折算**：`新数量 = 原数量 × (1 + (送+转)/10)`；`新成本 = (原成本×原数量 − 派息×原数量/10) / 新数量`。
- **纪律（不自动改用户账本）**：折算结果以**建议**形式呈现，用户一键确认才写入；确认后写一笔
  `PositionTrade{side: adjust}` 审计流水记录调整前后值，**可撤销**。
  理由：持仓是用户的真实账本，程序静默改写用户成本价不可接受。
- **模拟盘**：`PaperHolding` 同样折算，但**模拟盘可自动执行**（虚拟账户无真实后果），落交易流水。

### B9 事件日历与提醒

- **持仓盘后事件**（`guard.go` 扩展，沿用既有窗口去重机制）：
  新增「解禁临近」（提前 `guardLiftAheadDays=10` 天）、「除权除息日临近」（提前 3 天）。
- **打新提醒**（新增，不依赖持仓）：当日有新股/可转债可申购 → 进「今日待办」+ 可选推送。
- **前端**：
  - 新增「事件日历」卡（`Today.vue`）：未来 30 天与我相关的事件（持仓/自选股的解禁、除权、财报）+ 全市场打新；
  - `StockDetail.vue` 加「解禁 / 分红」块。
- **AI 链路联动**：解禁数据进个股分析快照 → **清掉 `analysis_context.go:166` 的「解禁未接入请自行核查」声明**，
  并进证据核验值域（`recbear.go` 的 bear 框架首条论据就是解禁，此前无数据可依）。

---

## 6. 第四批：增强项（C10~C13）

### C10 股息率

- 来源：`RPT_SHAREBONUS_DET.DIVIDENT_RATIO`（已随 B8 落库）。
- 落点：个股详情估值区、AI 个股快照证据链（进核验值域）、选股因子宽表新增 `div_yield`。

### C11 持仓成本关联提醒

- 新增两类 `AlertKind`：
  - `cost_drawdown`：现价跌破**持仓成本** N%（threshold=N）；
  - `peak_drawdown`：现价自**持仓期最高价**回撤 N%。
- 实现：规则不再必须绑 symbol，可绑「我的全部持仓」；评估时关联 `Position` 取成本/峰值。
- **fail-closed**：无 fresh 行情不触发（沿用既有纪律）。

### C12 K 线形态因子

- `factortable.go` 形态组新增布尔因子：`macd_golden_cross` / `ma_bull_align` / `morning_star` /
  `evening_star` / `bullish_engulfing` / `dark_cloud` / `hammer` / `doji` / `three_white_soldiers` /
  `consecutive_up`。纯本地日线计算，**自动进策略选股条件树与 AI 白话建策略的因子字典**。
- **纪律**：形态是描述性因子不是信号，**不进推荐评分权重**；仅供用户自行组条件与 AI 解读。
  形态定义写入因子字典 desc，避免 AI 望文生义。

### C13 行业 / 风格暴露

- `PositionService.Overview` 扩展：按**行业**（宇宙快照 `industriesFor`）、**市值风格**（大/中/小盘）、
  **估值风格**（高/低 PE）三维分布 + 集中度提示。
- **缺数据如实缺席**（行业未积累时该维不出，沿用既有纪律，不拍默认值）。

---

## 7. 全局纪律（四批共同遵守）

1. **不破坏既有 AI 准确性基线**：P0 九项、影子三不、fail-closed 门禁、证据核验值域同步铁律全部不动。
   新增进 AI 快照的数据（解禁/分红/股息率/分时）**必须同步进核验值域**，否则模型忠实引用会被误判幻觉。
2. **行情时效 fail-closed 一以贯之**：新功能凡消费实时价，一律走 `FreshQuotesFor`，stale 不参与计算。
3. **单位口径**：成交量=手、金额=元、比例=百分比数值。新表字段注释必须写明单位。
4. **用户数据不静默改写**：B8 除权折算必须用户确认（模拟盘除外）。
5. **UI 硬约束**：页面单根、6 主题零硬编码色、移动端 ≤768px 适配（`ARCHITECTURE.md` §4.2/§4.3）。
6. **诚实缺失**：上游不可回溯的数据（涨停池/情绪）缺勤即如实缺失，不补造历史。

---

## 8. 验收要点

| 批次 | 程序化可测 | 需人工目验 |
| --- | --- | --- |
| 一 | m1 解析 fixture、`IntradayLine` 非交易时段回退、`MoodOverview` 梯队聚合、龙虎榜排序与日期回退、提醒动态间隔判定 | 分时图 6 主题 + 移动端、盘面情绪页真实数据 |
| 二 | 加权成本重算、部分卖出已实现盈亏、卖超拒绝、旧持仓惰性补流水幂等、统计聚合手工验算、快照 upsert 幂等与 partial 标记 | 加减仓交互、资产曲线渲染 |
| 三 | 四报表解析 fixture、058 过滤、除权折算算式、guard 窗口去重、打新待办 | 真实除权日持仓提示、事件日历 |
| 四 | 形态因子逐个手工验算、成本提醒触发与 fail-closed、股息率进核验值域、暴露分布 | 选股页新因子、提醒实测 |

---

*创建于 2026-07-27（第五十八批起）。四批完成后本文档转为「现状与边界」参考，防回归要点归并至 `ROADMAP.md` §3。*
