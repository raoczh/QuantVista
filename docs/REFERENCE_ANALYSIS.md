# 参考项目分析与借鉴开发计划

> 2026-07-06 对 StockNova 及其 5 个上游/同类项目做的全量源码分析（20 号分析代理并行深读，共 137 万 token），
> 目标：把可借鉴的功能梳理成对照清单与分期开发计划——**QuantVista 已有的在本项目上优化，缺失的按价值分期引入**。
> 分析铁律：一切以源码为准，README 吹的但代码没有/被禁用的已逐一标出。
> 上游接口速查表见 §6（本次分析最值钱的资产），不借鉴清单与理由见 §7。

## 1. 六项目一览

| 项目 | 定位 | 技术栈 | 对 QuantVista 的核心价值 |
|---|---|---|---|
| **StockNova**（主要借鉴对象） | 本地自持的 A 股个人投研工作台：16 节点多角色 AI 诊股 + 72 因子全市场选股 + 真实约束回测 | Python FastAPI + DuckDB/SQLite；React 19 | 全市场扫描选股、回测引擎、回溯诊断校验、新闻三件套、盘中因子、东财限流实战经验 |
| stock-mcp | 金融数据中台：12 数据源 provider 插件，HTTP+MCP 双协议 | Python FastAPI + Redis/PG/MinIO | 巨潮公告接口、健康滑窗踢源路由、分级 TTL 缓存（架构参考为主） |
| stock-scanner-mcp | stock-scanner 剥离前端后的 MCP 工具箱 | Python FastAPI + akshare | 经典指标库清单（RSI/MACD/BOLL/ATR——QV 全缺）、"程序先算 LLM 后叙"佐证 |
| stock-scanner | 轻量"输入代码→技术面秒出→AI 流式解读"工具 | Python FastAPI + Vue3 Naive UI | **LLM 流式输出全链路**（QV 所有 AI 模块目前整段等待）、NDJSON 行协议 |
| StockAgent | 社区型 AI+量化全家桶（微服务多节点） | Python FastAPI + Mongo/Redis/Milvus | **新闻聚合链路完整设计**（源优先级/四层去重/分级 LLM 增强/报告硬规则筛选）与全部新闻源 URL |
| AKShare | 1500+ 个免费数据接口的逆向封装库 | Python | 当"**已验证的上游接口逆向档案**"用：东财 datacenter 统一网关 + 12+ 高价值数据源的真实 URL |

**三个项目共同的反面教材（对照自省）**：README 与代码脱节是常态——StockAgent 的新闻主流水线整片被注释禁用（默认只跑热榜）、stock-mcp 约 20 个方法实现了但无暴露口、stock-scanner 的建议提取正则与 prompt 脱节恒失效。QuantVista 的「结构化输出+程序化核验+文档以代码为准」路线被反向验证正确；借鉴任何功能前先确认它真的在跑。

## 2. StockNova 功能对照（主要借鉴对象）

| StockNova 功能 | QV 现状 | 借鉴价值 | 处置 |
|---|---|---|---|
| 16 节点多角色诊股（7 分析师并行→辩论→总监→交易员→风控三方→组合经理） | partial（有 panel 多角色/反方视角/AI 复核） | 中 | 不搬全流程（1~3 分钟+大量 token）；只取 3 个增量件：**交易员阶段**（分析后追加价位计划）、**量化仓位公式**（纯 Go：100%×clip(2.5/20日波动率,0.3,1.0)×择时系数）、**操作清单 checklist 输出字段** |
| 回溯诊断 as_of + 走势自动校验（+5/10/20/60 日收益、目标价/止损首触日） | partial（推荐追踪只能前向等待） | **高** | P2 引入：分析/推荐加 as_of 历史模式（日线截断复算、无历史快照的证据声明局限），校验端点复用追踪的基准 alpha 代码——快速积累样本量化 AI 准确率，反哺 prompt |
| 全市场因子宽表（72 因子向量化）+ 条件树选股 DSL + 命中原因人话化 | missing（全市场扫描是明确缺口） | **高** | P2 核心：Go 列式结构（map[string][]float64）实现宽表，日线库已有；条件树 JSON 语法（all/any/factor/op/value/ref）直接抄；`✓ 量比(5日) > 1.5（当前 2.13）` 的命中解释格式照搬；形态因子精确阈值全在 factors.py 里 |
| 43 个白话策略（带讲解/适用周期/风险等级/失效场景） | partial（推荐有 6 策略模板） | 高 | 随全市场扫描落地挑 20 个高价值的；白话讲解文案直接参考 |
| 回测引擎（时光机+定期调仓，A 股真实约束五件套） | missing | **高** | P2：先做时光机（历史日选股持 5/10/20 日统计 vs 沪深300）；五约束必抄：次日开盘成交/一字板跳过（开盘涨幅≥涨停阈值-0.5）/跌停 defer_sell 顺延/整百股/买 0.025% 卖 0.075%。与推荐追踪联动：把历史推荐当策略回测 alpha |
| 7×24 快讯 + 个股新闻 + AI 情绪打分三件套 | missing（QV 最大数据盲区） | **高** | P0 新闻线主料之一（接口：np-weblist 快讯带 req_trace、search-api-web 个股新闻 JSONP；情绪 prompt 纪律照抄：消息权重公告>政策>报道>传闻、旧闻不加分、无消息给 45~55 中性） |
| 龙虎榜/人气榜/业绩预告/个股资金流排行扩展数据层 | missing | 高 | P1 随东财 datacenter 通用客户端一并解锁（15:45 盘后错峰任务+分项容错模式照抄；**从接入日积累，不做历史统计**——诚实原则） |
| 5 分钟线 + 盘中因子（尾盘抢筹/量能占比/VWAP 重心） | missing | 中 | P2：腾讯 ifzq mkline 接口（QV 腾讯 client 加方法），盘中因子是短线评分的强增量 |
| 风险闸门（ST/一字板/流动性/小市值硬规则前置） | partial（推荐有硬筛，分析/问答无闸门） | 中 | P1：几十行 Go——涨跌幅≥9.5% 且振幅<1% 判一字板、成交额<3000 万流动性 warn、市值<30 亿提示，注入分析 prompt 并展示 |
| 提示词注册表 + 可视化编辑 | missing（prompt 写死在 Go 里，调优要重编译） | 中 | P2：PromptDef 注册表 + settings 表覆盖 + 缺占位符不炸的宽容渲染 |
| 东财域名断路器（push2→push2delay 降级 + 死域熔断） | partial（有跨源主备，无域内降级） | 中 | P1：几十行——限流错误时换 host 到 push2delay（官方 15 分钟延迟池，盘后无差别）重试并记住；连续 5 次熔断快速失败 |
| 行业热力图（ECharts treemap）+ 板块详情页 | partial（有板块榜单数据，缺可视化） | 中 | P2 前端：面积=成交额、颜色=涨跌幅；板块指数日线 secid=90.板块代码 |
| 复权因子防御（负/零因子沿用前日） | partial（QV 前复权单序列） | 中 | 记入认知：东财加法复权对深度缩水 ST 股会给负价；QV 追踪如遇除权异常价可加同类防御 |
| 持仓 position_context 注入（成本/浮亏/占总资金比→强制割守补三选一） | partial（持仓分析未注入账户层资金视角） | 中 | P1 小改动：设置加 total_capital，持仓 AI prompt 拼接资金上下文 |
| 数据同步断点续传/启动补偿 catchup | partial（QV 有日线批量同步） | 低 | 认知参考；QV 个人自用规模暂不需要 |
| DuckDB 列存/WebSocket 推送/桌面端 | — | 低 | 不借鉴（QV MySQL+轮询已够用） |

## 3. 其余项目的关键借鉴点

**stock-scanner → LLM 流式输出全链路（高价值）**：QV 已核实 `ai_client.go` 硬编码 `stream:false`，所有 AI 模块整段等待。移植路线：ai_client 加 StreamChatCompletions（bufio.Scanner 逐行剥 `data: `、取 delta.content）→ Gin 设 `application/x-ndjson` + Flusher 逐行推 `{module,code,chunk,status}` → Vue fetch+getReader+行缓冲状态机（StockAnalysisApp.vue 652-695 是现成参考）。**与信任层兼容的设计**：初稿流式吐出→流结束后跑证据核验→徽章与置信度后置更新，流式只改善首字节体验不动信任层。

**stock-scanner-mcp → 经典指标库补缺（高价值）**：QV 五维评分只用 MA/涨幅/区间位置/量比/回撤，**无 RSI/MACD/BOLL/ATR**。新增 `indicator.go` 纯函数（90 日线已够算），注意 RSI/ATR 用 **Wilder 平滑（α=1/n）对齐通达信口径**，别照抄参考项目的 SMA 口径（它与国内行情软件有偏差，是已识别的反面教材）；MACD 信号列名 bug（读不存在的列退化成 DIF>0）也是其真实事故，移植时写与已知序列对拍的表驱动单测。产出并入 candFactors（推荐评分/证据核验）与个股详情。

**StockAgent → 新闻聚合链路设计（P0 主参考）**：
- **源优先级体系 P1~P5 是单一支点**：去重 TTL、跨源保留谁、LLM 增强级别、保留天数全由它驱动——原样引入。
- **轻量去重单机版**：content_hash（MD5 标题+正文前500字）DB 唯一索引兜底 + 进程内 title_hash 缓存（1 万上限砍半）+ 标题相似度（bigram Dice ≥0.85 替代 SequenceMatcher）。不抄它拉全量逐条比对的实现。
- **LLM 分级增强控成本**：P1/P2 全量提取 {sentiment, sentiment_score, impact_scope, related_sectors≤5, policy_level}；P3 缺板块才调简化版；P4/P5 纯关键词规则（collector.yaml 约 25 组映射直接搬）。LLM 返回板块名对照本地板块列表白名单校验防幻觉——延续 QV 信任层风格。
- **AI 注入与 fallback**：个股分析 prompt 加舆情段（最近 5 条标题+情绪标签）；**无新闻时注入程序算好的涨跌五档/量能三档/换手率并明示"暂无相关新闻按市场信号判断"**——fallback 原样抄；别学它"字段生产与消费脱节"（辛苦算的 sentiment 没喂给分析）。
- **日报"今日重要事件"三维打分**：来源级别(中央5/部委4/交易所3)+影响范围(全市场5/板块3/个股1)+资金敏感度(直接5/间接3)，≥6 保留 ≥10 重磅，截断 Top8~12 后 LLM 只写摘要；约 40 词降噪黑名单先行。打分可解释可落快照。

**stock-mcp → 基建两件（中价值）**：①巨潮公告三接口（topSearch 查 orgId→hisAnnouncement 分页→static.cninfo PDF 直链，纯 HTTP 无 token）作为东财公告兜底；②Provider 健康滑窗（每源 50 次环形窗口，empty>50% 或 error>30% 冷却 300s 踢出轮询）——QV 三源主备目前源抽风时每次都要撞超时，值得移植并顺手做成 `/api/debug/sources` 最小管理端。

**AKShare → 东财 datacenter 统一网关（最大杠杆）**：几十类数据（业绩预告/快报/披露日历/龙虎榜/股东户数/解禁/北向/估值历史/机构调研）全走同一个 GET 网关，`reportName + filter(类SQL) + columns + pageSize/pageNumber` 参数 DSL。**Go 写一个 ~100 行通用客户端（全局令牌桶 QPS 1~2 + 翻页迭代 + 响应快照落库）即可批量解锁 §6 表里 12+ 个数据源**，边际成本趋近零。另一个白捡项：**筹码分布没有上游 API**——akshare 是拿日 K+换手率在本地跑东财前端 JS 算法（150 价格档三角分布、按 (1-换手率) 衰减），纯数值逻辑可直译 Go，QV 已有前复权日线+换手，零上游成本白得获利比例/成本分位/集中度。

## 4. 分期开发计划

> **本节已细化为独立施工图 `DEVELOPMENT_PLAN.md`（批次 N1~M3 + P3 储备，含每批方案锚点/依赖/验收标准），后续开发以那边为准**；本节保留粗粒度分期供快速定位价值排序。

> 工作量：小=半天内，中=1~2 天，大=3 天+。依赖关系已排进分期。P0 内部按序，P1/P2 内部可并行。

### P0 新闻舆情最小闭环（QV 最大数据盲区，4 个项目共同指向）

| # | 事项 | 来源 | 工作量 |
|---|---|---|---|
| 0-1 | `news` 表 + 源优先级常量 + 轻量去重（content_hash 唯一索引 + 内存标题池 + Dice 相似度） | StockAgent | 中 |
| 0-2 | 财联社电报源（唯一自带股票关联+重要标记且免认证；**只有 telegraph 接口活着**，roll_news/heads 已死别抄）5 分钟一轮挂现有定时任务 | StockAgent | 小 |
| 0-3 | 东财个股新闻（search-api-web JSONP；akshare 用 curl_cffi 说明有 TLS 指纹风险，先标准 client+完整浏览器头实测，失败再评估 utls）+ 东财 7×24 快讯（np-weblist，req_trace=uuid 必带） | StockNova/stock-mcp | 中 |
| 0-4 | LLM 情绪/板块分级增强（P1~P3 全量、P4/P5 关键词规则）+ 板块白名单核验；(symbol,date) 唯一键一天一次 + singleflight | StockAgent/StockNova | 中 |
| 0-5 | AI 个股分析/问答注入舆情段（最近 5 条标题+情绪标签，无新闻 fallback 到量价信号），新闻标题进证据核验值域 | StockAgent | 小 |
| 0-6 | 收盘日报加「今日重要事件」段（三维打分 ≥6 保留、Top8~12、LLM 只写摘要）+ 推荐流水线消息面因子（senti_score 进策略加分） | StockAgent/StockNova | 中 |
| 0-7 | 新闻保留 TTL 清理任务（政策 90 天/快讯 7 天/公告 60 天档位表） | StockAgent | 小 |

### P1 基本面数据层 + 稳定性基建

| # | 事项 | 来源 | 工作量 |
|---|---|---|---|
| 1-1 | **东财 datacenter 通用客户端**（reportName/filter/columns DSL + 翻页 + 令牌桶限速 + 快照落库） | AKShare | 中 |
| 1-2 | 业绩预告/快报/财报预约披露日历三接口 → 新增「财报披露」「业绩预告」两类条件提醒 + 日报明日披露名单 | AKShare | 中 |
| 1-3 | F10 财务主要指标（单请求 200 期）+ 三大报表（emweb，companyType 1~4 试探）落库，个股详情财务 Tab + 长线推荐/value 策略注入 ROE/净利增速/毛利率因子 | AKShare | 大 |
| 1-4 | 经典指标库 RSI/MACD/BOLL/ATR（**Wilder 口径**）并入 candFactors 与五维评分；K 线加 MACD/BOLL 副图 | stock-scanner-mcp | 中 |
| 1-5 | 筹码分布本地复算（日K+换手三角分布衰减算法直译 Go）：个股详情筹码峰图 + 获利盘比例进推荐量化评分 | AKShare | 中 |
| 1-6 | 公告接入：东财 np-anotice（无鉴权最稳）主源 + 巨潮三接口兜底；公告标题进 AI 证据池 | AKShare/stock-mcp | 中 |
| 1-7 | 东财域名断路器（push2delay 降级+死域熔断）+ Provider 健康滑窗踢源 + `/api/debug/sources` 最小数据源管理端 | StockNova/stock-mcp | 中 |
| 1-8 | 风险闸门前置到分析/问答（一字板/流动性/小市值 warn 注入 prompt 并展示）；持仓 AI 注入 total_capital 资金上下文强制割守补 | StockNova | 小 |
| 1-9 | LLM 流式输出（个股分析+问答先行；NDJSON 行协议；核验徽章后置更新） | stock-scanner | 大 |

### P2 全市场扫描 + 回测 + AI 校验闭环

| # | 事项 | 来源 | 工作量 |
|---|---|---|---|
| 2-1 | **全市场因子宽表**（盘后任务全市场日线增量→列式因子表缓存）+ 条件树 DSL + 命中原因人话化 + 内置 20 个白话策略；作为推荐候选池的终极「不热」来源（策略信号直接进池） | StockNova | 大 |
| 2-2 | 回测时光机（历史日选股持 5/10/20 日统计 vs 沪深300；A 股约束五件套）；历史推荐批次当策略回测 alpha | StockNova | 大 |
| 2-3 | 回溯诊断 as_of + 走势自动校验（AI 准确率量化，反哺 prompt 迭代） | StockNova | 中 |
| 2-4 | 龙虎榜/人气榜/涨停池/个股资金流排行（datacenter/push2ex/fflow 接口，盘后错峰落库）：情绪温度计进市场分析与日报、机构买入/人气跃升进候选加分 | AKShare/StockNova | 中 |
| 2-5 | 5 分钟线同步 + 盘中因子（尾盘抢筹/量能占比/VWAP 重心）进短线评分 | StockNova | 中 |
| 2-6 | 交易员阶段 + 量化仓位公式 + 操作清单字段（分析模块增强件） | StockNova | 中 |
| 2-7 | 提示词注册表 + 设置页可视化编辑 | StockNova | 中 |
| 2-8 | 行业热力图 treemap + 板块详情页（板块指数日线 secid=90.xxx） | StockNova | 中 |
| 2-9 | AI 白话建策略（自然语言→条件树，unmatched 兜底防硬凑）——依赖 2-1 | StockNova | 中 |

### P3 储备（价值确认后再做）

多平台热榜聚合页（财联社/格隆汇/金十/雪球热股）、国务院/工信部政策源、股东户数/解禁/十大流通股东（datacenter 顺手）、研报评级/机构调研/千股千评（机构评级分布与目标价区间可作分析核验锚点）、板块资金流历史+板块估值分位（成分股中位 PE 聚合 250 日分位，喂板块 AI 分析）、融资融券（沪深交易所官方接口带 Referer）、简化版事件聚类（规则指纹合并计数）、多通道推送（企微/飞书/TG）、自选分组批量体检入口、市场级北向历史。

## 5. 与既有候选池优化的衔接

本次已落地的「策略-来源映射 + 不热方向榜单」（见 ROADMAP §3）解决的是**热度榜供给结构**问题；P2-1 全市场因子宽表落地后，候选池可再加一路 `strategy_signal` 来源（当日命中策略信号的股票直接进池），彻底摆脱对榜单的依赖——这是「不热的股票」的终极答案。P0-6 的 senti_score 则给评分加上消息面维度。三者叠加后四阶段流水线的漏斗形态不变，供给质量逐级增强。

## 6. 上游接口速查表（源码实测提取，接入时直接查这里）

**东财 datacenter 统一网关**（`datacenter-web.eastmoney.com/api/data/v1/get`，免 token，500 条/页，QPS 建议 <2）：

| reportName | 数据 |
|---|---|
| RPT_PUBLIC_OP_NEWPREDICT | 业绩预告（预测指标/变动幅度/类型/原因） |
| RPT_FCI_PERFORMANCEE | 业绩快报（EPS/营收净利同比/ROE） |
| RPT_PUBLIC_BS_APPOIN | 财报预约披露时间（首约+三次变更+实际） |
| RPT_DAILYBILLBOARD_DETAILSNEW | 龙虎榜每日详情（pageSize=5000 一页全天） |
| RPT_LHB_BOARDDATE / RPT_BILLBOARD_DAILYDETAILSBUY/SELL | 个股上榜日 / 席位明细 |
| RPT_HOLDERNUMLATEST / RPT_HOLDERNUM_DET | 股东户数（最新/单股历史） |
| RPT_LIFT_STAGE / RPT_LIFT_GD / RPT_LIFTDAY_STA | 限售解禁（队列/股东/汇总） |
| RPT_MUTUAL_HOLDSTOCKNDATE_STA / RPT_MUTUAL_DEAL_HISTORY | 北向个股持股 / 市场净买历史（注意 2024-08 起个股口径降级，先实测存量） |
| RPT_VALUEANALYSIS_DET | 个股估值历史（逐日 PE/PB/PEG/市值） |
| RPT_ORG_SURVEYNEW | 机构调研 |
| RPT_F10_FINANCE_MAINFINADATA（datacenter.eastmoney.com …/data/get + source=HSF10） | F10 主要财务指标（一次 200 期） |

**其它高价值接口**：

| 数据 | URL 要点 | 鉴权/坑 |
|---|---|---|
| 财联社电报 | `www.cls.cn/nodeapi/updateTelegraphList?app=CailianpressWeb&os=web&sv=8.4.6` | 免 token；自带 subjects 股票关联+important；只有此接口活着 |
| 东财 7×24 快讯 | `np-weblist.eastmoney.com/comm/web/getFastNewsList?...&fastColumn=102&sortEnd=&req_trace={uuid}` | **req_trace 缺失返回空** |
| 东财个股新闻 | `search-api-web.eastmoney.com/search/jsonp?param={json}` type=cmsArticleWebOld | 有 TLS 指纹风险（akshare 用 curl_cffi），JSONP 剥壳 |
| 东财公告 | `np-anotice-stock.eastmoney.com/api/security/ann?ann_type=A&stock_list=...` | 无鉴权，最稳公告源 |
| 巨潮公告 | `www.cninfo.com.cn/new/{information/topSearch/query → hisAnnouncement/query}`；PDF=`static.cninfo.com.cn/{adjunctUrl}` | 免 token，需 UA+Referer+orgId 映射 |
| 三大报表 | `emweb.securities.eastmoney.com/PC_HSF10/NewFinanceAnalysis/{zcfzb,lrb,xjllb}AjaxNew?companyType=1~4&code=SH600519` | 先 DateAjaxNew 取报告期；companyType 试探 |
| 个股资金流历史 | `push2his.eastmoney.com/api/qt/stock/fflow/daykline/get?secid=1.600094&lmt=0&ut=b2884a393a59ad64002292a3e90d46a5` | 公共 ut 硬编码；QV 已接同域 |
| 资金流排行 | `push2.eastmoney.com/api/qt/clist/get?fid=f62|f164|f174&fs=m:0+t:6,...` | QV 已有 clist 经验，换 fid 即得 |
| 涨停池五件套 | `push2ex.eastmoney.com/getTopicZTPool|getYesterdayZTPool|getTopicQSPool|getTopicCXPooll|getTopicZBPool?ut=7eea3edcaed734bea9cbfc24409ed989&date=YYYYMMDD` | 公共 ut；封板资金/连板数/炸板次数 |
| 融资融券 | 沪 `query.sse.com.cn/marketdata/tradedata/queryMargin.do`；深 `www.szse.cn/api/report/ShowReport/data?CATALOGID=1837_xxpl` | **必须带各自官网 Referer**；深市数值带千分位 |
| 5 分钟线 | `ifzq.gtimg.cn/appstock/app/kline/mkline?param={code},m5,,60` | 腾讯免鉴权；无成交额需量×均价估算 |
| 板块指数日线 | 东财 kline 接口 `secid=90.板块代码` | QV 日线客户端加前缀即可 |
| 股吧人气榜 | POST `emappdata.eastmoney.com/stockrank/getAllCurrentList` body={appId:"appId01",globalId:uuid} | 只有前 100 名 |
| 东财域内降级 | push2 被灰名单（302/502）→ 换 host `push2delay.eastmoney.com`（官方 15 分钟延迟池，接口同构） | StockNova 实战结论：push2dhis 不可用别加 |

**反爬分层结论（选源即选命）**：东财 datacenter/push2 系基本裸奔（最多公共 ut）→ 交易所官方只要 Referer → 东财新闻搜索要 TLS 指纹 → 同花顺要跑 989 行混淆 JS 生成 hexin-v。**锁定前两层，回避同花顺**。

## 7. 明确不借鉴清单

| 项 | 理由 |
|---|---|
| 微服务多节点/gRPC/MongoDB/Milvus（StockAgent） | 个人自用过重；QV 单容器 MySQL 形态已验证 |
| 16 节点全流程诊股工作流（StockNova） | 单次 1~3 分钟+巨量 token；QV 已有 panel/反方/复核，只取三个增量件（P2-6） |
| Tushare 依赖（stock-mcp/StockAgent） | 需 token+积分；东财免费网关可覆盖所需的绝大部分 |
| 同花顺系接口（AKShare 教训） | hexin-v 反爬需嵌 JS 引擎，成本高易失效 |
| 多市场港美股（stock-scanner） | QV 明确 A 股定位；架构上列名归一层的思路留档即可 |
| MCP 服务化（stock-mcp/scanner-mcp） | 自用 Web 平台非刚需；未来想让 Claude 查自家数据再用 mark3labs/mcp-go 薄包 |
| BYOK 前端传 API Key（stock-scanner） | Key 过前端扩大泄露面；QV 服务端加密配置已是更优解 |
| LLM 事件聚类完整版（StockAgent） | 每 30 分钟批量 LLM 提指纹，个人成本高；P3 简化版（规则指纹合并计数）够用 |
| DuckDB 双库/WebSocket/桌面端（StockNova） | QV 规模用不上列存；轮询已够 |
| Tavily 新闻检索（stock-mcp） | 需 key 且英文源为主，对 A 股价值低于免费中文源 |
