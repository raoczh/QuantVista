# 开发计划（参考项目借鉴落地路线）

> 依据 `REFERENCE_ANALYSIS.md`（StockNova 等 6 参考项目源码分析）整理的**面向执行的施工图**，后续开发按本文档推进。
> 上游接口的 URL/参数/坑一律查 `REFERENCE_ANALYSIS.md §6` 速查表，本文不重复；防回归红线见 `ROADMAP.md §3`。
> 每批做完：勾选状态、补 commit 号、把新增防回归认知写进 ROADMAP §3。

## 0. 总原则（每批开工前重读）

1. **诚实原则**：龙虎榜/人气/情绪等扩展数据从接入日起积累，**不做历史回填统计**，UI 与 prompt 都注明数据起点（学 StockNova no_backtest：宁可功能少，不出撒谎的曲线）。
2. **信任层风格延续**：所有新增 LLM 输出必须走既有核验路径——引用数字进 `verifyEvidenceValues` 值域、板块/标题类文本引用对照白名单校验、结果落快照可复现。新数据源字段一旦喂给 LLM，就必须同时进核验值域（否则忠实引用会被误报幻觉，信任层自伤）。
3. **免费源敬畏**：新上游接口一律挂 `datasource` 适配层、复用 `http.go` 的 `doGet/httpClient`（强制 HTTP/1.1 + 浏览器 UA + 瞬时错误重试的实战组合；POST/JSONP 场景对齐它加 doPost）；`SafeHTTPClient` 仅用于**用户可配 URL** 的场景（LLM/webhook，SSRF 防护）。有限流风险的加令牌桶（QPS 1~2）；单源失败降级不阻断；盘后任务错峰（参照 StockNova 15:35→15:40→15:45→15:50 依赖顺序编排）+ 失败落库防反复烧。
4. **字段生产必须有消费**：每个新数据字段落地时同批接好至少一个消费方（AI 上下文/评分因子/提醒/页面），不留"辛苦算了没人用"的脱节（StockAgent 反面教材）。
5. **参考项目代码仅作方案参考**：所有实现用 Go 重写、对齐本项目分层（datasource → service → controller → web），不引 Python 依赖；参考代码里的已知 bug（RSI SMA 口径、MACD_Signal 列名、正则与 prompt 脱节）不带过来。

## 1. 批次总览与依赖

```text
N1 新闻采集地基 ──→ N2 新闻→AI信号
F1 datacenter网关+财报日历+公告 ──→ F2 财务数据与估值增强
T1 指标库+筹码分布 ──→ M1（因子宽表含 T1 指标；内置策略含 MACD/RSI 类）
S1 稳定性与流式体验（无依赖，可穿插）
M1 全市场宽表+条件树选股 ──→ M2 回测+AI校验闭环
M3a/M3b/M3c 扩展数据与工作流增强（仅 M3a 的龙虎榜依赖 F1 网关，其余独立）
```

推荐推进序：**N1 → N2 → F1 → T1 → S1 → F2 → M1 → M2 → M3a → M3b → M3c**（价值密度×依赖排序；N 线是最大数据盲区优先补）。

| 批次 | 主题 | 状态 |
|---|---|---|
| N1 | 新闻舆情地基：采集/去重/存储/页面 | ✅ 完成（后端 `dc26e7f`：模型/采集/去重/TTL/API+单测+live 冒烟；前端 `69e7b3a`：/news 消息页+详情关联新闻+导航）；「连续采集 1h 无重复」验收欠部署后观察 |
| N2 | 新闻→AI 信号：情绪增强/分析注入/日报事件段/推荐因子 | ✅ 完成（commit `11657d1`：`service/newsai.go` 分级情绪增强+聚合情绪分、个股分析/问答舆情段（p6/q4）、日报事件段 4 步硬规则（`newsevent.go`）、推荐消息面因子（p6/s4）+前端情绪标签）；「LLM 调用比例 <50%」验收欠部署后观察日志 |
| F1 | 东财 datacenter 网关 + 财报日历/业绩预告 + 公告 + 提醒联动 | ✅ 完成（commit `7074ec6`：`datasource/emdatacenter.go` 通用客户端（翻页/QPS≤2 全局令牌桶/重试退避+单测+LIVE_FIN 冒烟）、三类财报表增量刷新（`model/finance.go`+`service/finance.go` 每日 19:05 job）、公告采集/详情公告块/AI 证据池（p7/q5）、提醒 earn_date/earn_fcst 每日一评（盘中评估显式排除）、日报明日披露名单）；财报提醒实际命中与公告块数据欠部署观察 |
| F2 | F10 财务指标/三大报表 + 详情财务 Tab + 长线因子 | ☐ 未开始 |
| T1 | RSI/MACD/BOLL/ATR 指标库 + 筹码分布本地复算 | ☐ 未开始 |
| S1 | 域名断路器/健康滑窗/风险闸门/LLM 流式输出 | ☐ 未开始 |
| M1 | 全市场日线 + 因子宽表 + 条件树选股 | ☐ 未开始 |
| M2 | 回测时光机 + 历史推荐回测 + 回溯诊断校验 | ☐ 未开始 |
| M3a | 扩展数据接入：龙虎榜/人气榜/涨停池/个股资金流（排行+历史） | ☐ 未开始 |
| M3b | 5 分钟线 + 盘中因子 | ☐ 未开始 |
| M3c | 交易员阶段/提示词扩展/热力图与板块详情页 | ☐ 未开始 |
| P3 | 储备池（价值确认后再排期） | — |

---

## 2. 批次详单

### N1 新闻舆情地基（工作量：约 1 个会话）

**目标**：新闻从无到有——三个免费源稳定入库、去重可靠、页面可看。

**交付与方案锚点**：
1. `model/news.go`：`News{ID, Title, Content(截断3000字), Summary, URL, Source, Category, PublishTime, CollectTime, RelatedSymbols(JSON数组,经 normalizeSymbolMarket 归一为本项目 symbol 口径), SourcePriority(1-5), ContentHash(唯一索引), Sentiment, SentimentScore, RelatedSectors(JSON), ImportantMark}`；`content_hash = MD5(标题+正文前500字)`，唯一索引兜底去重。注册迁移。
2. `datasource/cls.go` 财联社电报（**第一源**：自带 subjects 股票关联+important 标记，免认证；只有 telegraph 接口活着）：解析 `data.roll_data`、过滤 `is_ad`、`ctime` 秒级时间戳、subjects[].code 归一为本项目 symbol。`datasource/eastmoney_news.go` 东财 7×24 快讯（np-weblist，**req_trace=uuid 必带**，sortEnd 游标）+ 个股新闻（search-api-web JSONP 剥壳+去 `<em>`；**先标准 client+完整 Chrome 头实测**，被拒则本批降级只做前两源、utls 评估丢给 P3，不阻塞）。
3. `service/news.go`：采集编排（快讯类 5min 一轮挂 `StartMarketJobs` 同款定时基建；上次采集游标落库防重启重采）；**轻量去重三层**——DB content_hash 唯一索引（INSERT IGNORE 语义）、进程内 `source:source_id` 与 title_hash 缓存（1 万上限砍半）、标题相似度（归一化后 bigram Dice ≥0.85，比对最近 72h 内存标题池；不抄 StockAgent 拉全量逐条比对）。
4. 保留 TTL 清理任务：每日凌晨按 source_priority×category 档位删过期（政策 90 天/快讯 7 天/公告 60 天，档位表抄 collector.yaml lifecycle 段为 Go 常量）。
5. API `GET /api/news?symbol=&source=&limit=` + 前端轻量消息页（快讯流+个股筛选；样式服从 [[quantvista-ui-system]] 6 主题约束、移动端单根）。

**验收**：连续采集 1 小时无重复入库；同一事件多源标题相似度去重生效；个股详情能看到关联新闻；`go test` 覆盖去重三层与 RelatedSymbols 归一。

### N2 新闻→AI 信号（工作量：约 1 个会话；依赖 N1）

**目标**：新闻变成 AI 链路的第四类证据（行情/估值/因子之外），并进日报与推荐。

**交付与方案锚点**：
1. `service/newsai.go` LLM 分级情绪增强（**成本控制是设计核心**）：P1/P2 源全量提取 `{sentiment, sentiment_score(-1~1), impact_scope(market/sector/stock), related_sectors(≤5), policy_level}`；P3 仅缺板块时调简化版；P4/P5 纯关键词规则表（25 组 keywords→sectors+sentiment 映射抄成 Go 常量）。**两个幂等键分清**：逐条新闻增强按 `news.id`（或 content_hash）幂等、与 symbol 无关（市场/政策类新闻常无 symbol）；个股当日聚合情绪分（12 条新闻→senti_score，供 N2-4 推荐因子消费）才按 `(symbol,date)` 幂等一天一次 + 并发合并（**用 mutex+map 写等价实现，项目 go.mod 无 x/sync 依赖**）。**related_sectors 对照本地板块列表白名单校验防幻觉板块**（信任层风格）。prompt 纪律照抄 StockNova：消息权重 公告>政策>报道>传闻、旧闻不加分、无消息给 45~55 中性。
2. 个股分析/问答 prompt 注入舆情段：最近 5 条标题+情绪标签；**无新闻 fallback 原样抄**——注入程序算好的涨跌五档/量能三档/换手率并明示"暂无直接相关新闻，请按市场信号判断"；新闻标题进 `analysisSystemConfidence` 数据完备度与证据核验值域（文本型合法来源，同日报 Alerts 前例）。
3. 收盘日报「今日重要事件」段：**4 步硬规则先行 LLM 只写摘要**——①约 40 词降噪黑名单；②三维可解释打分（来源级别 中央5/部委4/交易所3 + 影响范围 全市场5/板块3/个股1 + 资金敏感度 直接5/间接3），≥6 保留、≥10 标重磅；③同主线合并；④截断 Top8~12。打分明细落快照（透明池风格）。
4. 推荐流水线消息面因子：当日 senti_score 进 `strategyAdjust` 加分项（如"当日利好情绪 0.72（+3）"），数值进证据核验值域。

**验收**：分析结果引用新闻标题且核验通过；日报出现事件段且打分明细可查；推荐 Bonus 出现情绪加分项；LLM 调用比例监控（增强调用数/新闻数 <50%）。

### F1 东财 datacenter 网关 + 财报日历 + 公告（工作量：1~2 个会话）

**目标**：一个通用客户端解锁十几类报表数据；财报披露/业绩预告/公告接入提醒、日报与 AI 证据池。

**交付与方案锚点**：
1. `datasource/emdatacenter.go` 通用客户端（**本批核心资产，~100 行**）：`func (e *EastMoneyAdapter) DataCenterQuery(ctx, reportName, filter, columns string, sort string, pageSize int) 迭代器`——统一处理 pageNumber 翻页/result.pages、全局令牌桶（QPS ≤2）、瞬时错误重试退避、响应快照留存。所有 RPT_* 走它。
2. 业绩预告（RPT_PUBLIC_OP_NEWPREDICT）/业绩快报（RPT_FCI_PERFORMANCEE）/预约披露（RPT_PUBLIC_BS_APPOIN）三接口 → `model/finance.go` 落库：**三类数据分开建表**（字段差异大，一表一用途），唯一键统一 `(symbol, market, report_date)`（对齐项目自然键惯例）。**每日盘后按 NOTICE_DATE/变更日期增量刷新**当前报告期（业绩预告全年零散发布、预约披露有三次变更，季度性刷新会让提醒滞后数月），报告期切换时全量重拉。
3. **公告接入**（对抗审查补入，原计划遗漏）：东财 `np-anotice-stock.eastmoney.com/api/security/ann` 主源（无鉴权最稳，按自选∪持仓股每日增量拉取落库，字段：标题/类型/日期/art_code 拼原文链接）；巨潮三接口兜底后置 P3。消费方：个股详情"公告"Tab + 最新公告标题进 AI 个股分析证据池（进核验值域的文本型合法来源）。
4. 提醒体系新增两类 kind——**注意 `AlertRule.Kind`/`AlertEvent.Kind` 列宽 size:16，且项目约定 AutoMigrate 不做已有列扩宽**：kind 取 `earn_date`（N 日内披露财报）与 `earn_fcst`（新业绩预告发布，带预增/预亏类型），均 ≤16 字符。**评估口径**：财报日历类不走盘中 `evaluateRules` 的 high/low 判定（15 分钟盘中评估的规则查询要显式排除这两类 kind，否则会被拉去每 symbol 拉行情空转），单独每日一评；命中走 `alert_events` 状态机与推送总闸同口径。
5. 收盘日报加"明日披露名单"（自选∪持仓中次日披露的标的）。

**验收**：自选股设财报提醒能命中且盘中评估不空转；日报出现披露名单；详情页公告 Tab 可见且分析引用公告标题核验通过；datacenter 客户端有翻页与限流单测。

### F2 财务数据与估值增强（工作量：1~2 个会话；依赖 F1 网关）

**目标**：补上"长线推荐无基本面"的根本短板。

**交付与方案锚点**：
1. F10 主要财务指标：RPT_F10_FINANCE_MAINFINADATA（单请求 200 期，`SECUCODE="301389.SZ"` 口径转换）落 `finance_indicators` 表；按需拉取（个股详情/推荐候选首次访问触发）+ 缓存，不做全市场普查。
2. 三大报表（emweb zcfzbAjaxNew/lrbAjaxNew/xjllbAjaxNew，先 DateAjaxNew 取报告期、companyType 1~4 试探）——**二阶段**：先只取最近 8 期关键科目供 AI 上下文，全表明细页面后置。
3. 个股详情页财务 Tab：营收/净利/ROE/毛利率趋势（近 8 期柱线图）。
4. 消费方接线：长线推荐 value/growth 策略加分项引入 ROE/净利增速/营收增速（有数据才加分，缺失不惩罚）；`longTermSpec` prompt 撤掉"缺财务明细"的声明改为注入真实财务摘要；分析模块个股上下文加财务段。财务数字全部进证据核验值域。
5. 可选（时间富余）：个股估值历史 RPT_VALUEANALYSIS_DET（PE/PB 历史分位供"当前估值处于历史 X% 分位"证据）。

**验收**：长线推荐 evidence 引用 ROE/增速数字且核验通过；详情页财务 Tab 6 主题目验；推荐 p5→p6 版本号递增（prompt 变更）。

### T1 技术指标库 + 筹码分布（工作量：约 1 个会话；无依赖可并行）

**目标**：补齐经典指标（QV 现无 RSI/MACD/BOLL/ATR），白得筹码分布高级功能。

**交付与方案锚点**：
1. `service/indicator.go` 纯函数库：EMA 递推（α=2/(n+1)，等价 pandas ewm adjust=False）、MACD(12,26,9)、BOLL(20,2σ)、**RSI(14)/ATR(14) 用 Wilder 平滑（α=1/n）对齐通达信口径**——别抄参考项目的 SMA 口径（已识别偏差）。表驱动单测：固定输入序列对拍预算值。
2. 并入 `candFactors`（推荐评分与 LLM 证据：如 RSI/MACD 金叉状态/布林位置）+ 五维评分（动量维 RSI 分档：55~70 满分、≥70 显著降分的凹形逻辑；风险维 ATR/价百分比）+ 个股详情 K 线 MACD/BOLL 副图（ECharts 主题感知）。
3. `service/chip.go` 筹码分布本地复算（**零上游成本**：日 K+换手率三角分布衰减算法，150 价格档、每日以 (开+收+高+低)/4 为峰、存量按 (1-换手率) 衰减）：**输入需 ≥210 根日线**（对齐 akshare lmt=210 口径——筹码是累积模型，输入窗口不足会让获利比例/成本区间系统性失真；注意这与指标库"90 根够用"不同，个股详情按需拉 210 根或等 M1 的 250 日历史初始化），输出取近 90 日的获利比例/平均成本/90%与70%成本区间及集中度。**标注与东财展示可能因复权口径略有差异**。消费方：个股详情筹码峰图 + 获利盘比例进推荐量化评分（如获利盘 <10% 超跌信号加分项）。

**验收**：指标对拍单测过；详情页副图/筹码峰 6 主题目验;candFactors 新字段进核验值域（verifyEvidence 的 candidateValueSet 同步扩）。

### S1 稳定性与流式体验（工作量：1~2 个会话；无依赖可穿插）

**目标**：数据源抗抖动 + AI 首字节体验。

**交付与方案锚点**：
1. 东财域名断路器：限流类错误（302/502/断连）把 host 换 `push2delay.eastmoney.com` 重试并记住降级（实例级 map）；无备用域接口连续 5 次限流熔断快速失败。**push2dhis 不可用别加**（StockNova 实战结论）。
2. Provider 健康滑窗：每 (源,数据类型) 50 次环形窗口记 success/empty/error+延迟，empty>50% 或 error>30% 冷却 300s 踢出轮询；挂 `datasource.Manager` 各能力路由入口。配套**两层超时预算**：handler 层总预算 + 单源短预算（context.WithTimeout 两层包裹，超预算即换源），错误统一归一为 `{code: UPSTREAM_TIMEOUT|EMPTY|PARSE_ERROR, source, latency}` 记日志并作为滑窗输入。顺手 `GET /api/admin/datasources` 健康状态端点（补"多数据源管理端"最小版，`data_source_configs` 死表可一并接活或删除）。
3. 风险闸门前置到分析/问答：ST/退市 block、涨跌幅≥9.5% 且振幅<1% 判一字板 warn、成交额<3000 万流动性 warn、市值<30 亿提示——注入 prompt 且 UI 展示；"未接入数据（质押/解禁）请自行核查"措辞纪律照抄。持仓 AI 注入 `total_capital` 资金上下文（设置项+prompt 拼接，强制割/守/补三选一）。
4. LLM 流式输出（**大项；与结构化输出的冲突要先想清**）：分析模块是 `JSONMode:true` 结构化输出（前端按 rating/highlights 字段渲染），按 token 流式吐出的是半截 JSON 无法渲染——**问答模块（`qa.go` JSONMode:false 自由文本）先行流式**；分析模块二选一：(a) 先流式吐自由文本"初稿解读"、流结束后再一次结构化提取调用（多一次 LLM 成本），或 (b) 本批不做分析流式只做问答。**前置基建**：web 现无 markdown 渲染依赖（package.json 无 marked）——引入 marked+DOMPurify（XSS 防护）作为流式渲染地基。技术路线：`ai_client.go` 加 `chatCompletionStream`（bufio.Scanner 逐行剥 `data: `、[DONE]/finish_reason 终止、delta.content 经 channel 吐出）；Gin `application/x-ndjson` + Flusher 逐行推 `{module, code, chunk, status}`（**code 字段现在就进协议**，单标的留空，为横向对比/批量场景预留）+ **响应头 `X-Accel-Buffering: no`**（反代部署下防整段缓冲）；Vue fetch+getReader+行缓冲状态机，渲染 100ms 节流。**与信任层的兼容设计**：初稿流式吐出→流结束跑核验/复核→徽章置信度后置更新。推荐链路不动（结构化流水线不适合流式）。

**验收**：手动断东财域名系统优雅降级且健康端点可见；问答**收到响应后首个 chunk 渲染 <300ms、总耗时不高于非流式模式**（不承诺外部 LLM 的 TTFB）；流式模块的核验徽章正常后置出现。

### M1 全市场因子宽表 + 条件树选股（工作量：2~3 个会话；依赖 T1 指标库；本项目最大新工程）

**目标**：全市场扫描选股——"不热的股票"的终极供给（摆脱榜单依赖）。

**交付与方案锚点**：
1. **全市场日线数据是前置**（现状 `SyncTrackedDailyBars` 只同步已跟踪标的、上限 800）。**覆盖范围=沪深 A 股约 5150 只**——北交所（43/83/87/920）现有 `cnSecid` 不识别、日线拉不了、推荐链路也排除，不纳入（涨停判定阈值只需 9.8/19.8/ST4.8；要含北交所须先扩 secid 映射，非本批）。**每日增量走东财 clist 全市场快照**（59 页翻页拿当日全部 OHLCV+换手，1~2 分钟）落 daily_bars；**历史初始化**（新覆盖标的按需拉 250 日 kline，进度表断点续传、暂停恢复，参照 init_progress；5150 只×250 日 ≈130 万行/年 MySQL 可承受）。
2. **除权检测必须防"部分窗口重写"漏检**（对抗审查抓出的坑）：现有 `persistDailyBars` 每次只 upsert 本次拉取的窗口（90~120 根），除权日若任何在线路径先触发 GetDailyBars，最近窗口已被重锚到新基准，盘后任务再比对"clist 昨收 vs DB 末根昨收"就吻合了→漏检，窗口之外残存旧复权基准与新数据断层。方案：检测锚不用当日昨收单点，改**取 DB 内 60 日窗多点与本次拉取序列比对**（任一日 close 偏差超阈值即判除权）→ 确认除权后**强制全量重拉该股 250 日覆盖**；每股记录 `adjust_epoch`（最近全量重锚日）。M2 回测开跑前对样本做一次"复权自洽校验"（历史窗口首尾衔接检查）。
3. `service/factortable.go` 列式因子宽表：`map[string][]float64` 按因子名存列、`[]string` symbol 索引；盘后全量计算一次缓存（复用 indicator.go/recfactor.go 的因子实现），Go 循环 5150 只×250 日无压力（<5s 目标）。因子集：现有 candFactors 全量 + **T1 指标（依赖 T1 先落地；若 T1 未完成则砍掉 MACD/RSI 类内置策略**）+ 涨停判定（按板块阈值 9.8/19.8/ST4.8）。
4. 条件树 DSL（JSON 语法直接抄）：`{all/any:[...]}`、`{factor, op(>/>=/</<=/between/is_true/is_false), value 或 ref}` 递归求值返回掩码+叶子明细；**命中原因人话化**：`✓ 量比(5日) > 1.5（当前 2.13）` 格式照搬（透明池风格）。
5. 内置约 20 个白话策略（从 StockNova 43 个挑：均线多头/放量新高/缩量回踩MA20/MACD 水上金叉/RSI 超卖回升/温和放量/破净站上MA20 等，**只挑现有因子可支撑的**，白话讲解+适用周期+风险等级文案参考其 builtin.py）+ 自定义策略表（user_id 隔离）。
6. 选股页（策略广场式：内置策略卡+一键扫描+命中列表带人话原因+加自选/深链分析）+ **推荐池接线**：`strategySources` 加 `strategy_signal` 来源（当日命中当前策略对应信号的股票进池，与榜单来源并列）。
7. 前端页面遵守 UI 系统约束；扫描接口做并发防抖（`atomic.Bool` 互斥同款）。

**验收**：全市场扫描 <5s；命中原因可解释可复核；推荐池全景出现 strategy_signal 来源；同步任务断点续传实测；除权股全量重锚后历史窗口衔接无断层；扫描结果与手工验算 3 只样本一致。

### M2 回测 + AI 校验闭环（工作量：2 个会话；依赖 M1 全市场日线）

**目标**：策略与 AI 推荐从"前向等待"变成"历史可验证"。

**交付与方案锚点**：
1. 回测时光机 `service/backtest.go`（纯计算不碰 LLM）：历史某日按条件树选股（命中+成交额排序取前 200）→ 次日开盘买入 → 持有 5/10/20 日统计收益/胜率/均值/中位/最好最差 **vs 基准指数同期（基准=上证指数，与现有推荐追踪 alpha 同口径——`GetBenchmarkBars` cn 现只有 sh000001；如需沪深300 对照再扩 sina 基准清单，两处共用）**。**A 股真实约束五件套一条不能少**：信号次日开盘成交（杜绝未来函数）/开盘涨幅≥涨停阈值-0.5 判一字板跳过/跌停卖不出 defer_sell 顺延重试/整百股取整钱不够放弃/买 0.025% 卖 0.075%（费率与 `tradeFee` 对齐）。
2. 历史推荐批次回测：把过往推荐批次的 picks 当"策略"跑同引擎，输出 alpha 分布——补齐推荐追踪只能前向统计的短板。
3. 回溯诊断：AI 个股分析加 `as_of` 参数——日线/指标截断到该日组装 prompt（**无泄露**：复用同一套因子函数对截断序列复算）；新闻/横截面等无历史快照的证据**在 prompt 中如实声明缺失**（不硬造）。校验端点：+5/10/20/60 日真实收益、最大涨跌幅、目标价（high 上穿）/止损价（low 下破）首触日，复用推荐追踪基准 alpha 代码。
4. 前端：回测页（策略选择+区间+结果统计卡）+ 分析历史"回溯校验"按钮 + 定期调仓模式后置（时光机先行）。

**验收**：任一内置策略可出 5/10/20 日胜率报告；一字板/跌停顺延单测；回溯诊断的因子与当日实时计算对拍一致（无未来泄露测试）；历史推荐批次能出 alpha 分布。

### M3a 扩展数据接入（工作量：1~2 个会话；仅龙虎榜依赖 F1 网关）

1. 龙虎榜（RPT_DAILYBILLBOARD_DETAILSNEW 每日一页全量，走 F1 网关）/股吧人气榜（emappdata POST 前 100，hisRc=-1 记新上榜）/涨停池五件套（push2ex：连板高度/封板资金/炸板率）/个股资金流**排行**（clist fid=f62/f164/f174）——盘后错峰落库。
2. **个股资金流历史**（对抗审查补入，原计划遗漏）：`push2his fflow/daykline/get`（公共 ut 硬编码，与现有日线同域同栈成本极低），按需拉单股全历史+缓存；消费方：个股详情资金流图、五维评分量能维、推荐流水线"主力连续净流入天数"因子（数值进核验值域）。
3. 消费方接线：市场情绪温度计（连板高度分布+炸板率进市场分析与日报）、推荐候选加分项（龙虎榜机构买入/人气跃升）、个股详情上榜记录。

**验收**：日报出现连板/炸板情绪段；推荐 Bonus 出现龙虎榜/人气/主力净流入加分；详情页资金流图 6 主题目验。

### M3b 5 分钟线 + 盘中因子（工作量：约 1 个会话；独立）

腾讯 ifzq mkline 盘后全市场同步（8 并发+80ms 节流 2~4 分钟，先删当日再插保幂等）+ 盘中因子五件（尾盘30分涨幅/尾盘量能占比>20%异常/早盘1小时涨幅/收盘vs全天VWAP/下午VWAP>上午）进短线评分。**声明积累满 60 交易日才开放盘中策略回测**（诚实原则）。

**验收**：盘后同步实测耗时达标；短线推荐 Bonus 出现盘中因子加分项。

### M3c 工作流增强与可视化（工作量：1~2 个会话；独立）

1. 交易员阶段（分析产出后追加一次 LLM：买入区间/目标价/止损/周期，"止损必须低于现价、盈亏比<2:1 降仓"自洽纪律）+ 量化仓位公式纯 Go（100%×clip(2.5/20日波动率,0.3,1.0)×择时系数，择时系数=0.6+(涨家占比-0.5)×1.2 夹 [0.3,1.2]）+ 操作清单 checklist 输出字段。
2. 提示词管理——**扩展现有系统而非另起注册表**（项目已有 `PromptTemplate`/`PromptService`/`Prompts.vue`，user_id+module 唯一、删除即恢复默认）：module 枚举从 5 个分析模块扩到 推荐/日报/问答/复核（**module 列 size:16，新枚举值取短名**），占位符宽容渲染并入现有 `userPromptOverride` 读取链，Prompts.vue 列表加新模块。**调 prompt 不再重编译**。
3. 行业热力图（ECharts treemap 面积=成交额/颜色=涨跌幅，industry/concept 切换）+ 板块详情页（板块指数日线 secid=90.xxx + 成分股排行标龙头）。

**验收**：分析结果出现交易计划与仓位建议；推荐/日报提示词改动即时生效且历史记录 prompt_version 可归因；热力图 6 主题目验。

### P3 储备池（价值确认后排期）

多平台热榜聚合页（格隆汇/金十/雪球热股，雪球需 cookie 预热）、国务院政策源（魔法参数易碎的工信部二期再说）、股东户数/解禁/十大流通股东（datacenter 顺手）、**研报评级/机构调研/千股千评**（reportapi + RPT_ORG_SURVEYNEW，datacenter 顺手；机构评级分布与目标价区间可作分析模块核验锚点）、**板块资金流历史+板块估值分位**（东财板块资金流同域扩展；成分股中位 PE 聚合 250 日分位，喂板块 AI 分析）、融资融券（沪深交易所官方接口带 Referer）、**AI 白话建策略**（自然语言→条件树，strategy_parse prompt + unmatched 兜底防硬凑；依赖 M1 落地后价值才确认）、简化版事件聚类（规则指纹合并计数）、企微/飞书/TG 多通道推送、自选分组批量体检入口、市场级北向历史（个股口径 2024-08 起降级需先实测）、东财个股新闻 utls 方案（若 N1 实测被 TLS 指纹拦截）、巨潮公告兜底源（orgId 映射三接口）、MCP 服务面（mark3labs/mcp-go 薄包只读工具）。

## 3. 横切约定

- **版本号纪律**：prompt/策略每变一次递增（推荐现 p5/s3、分析现 p5），历史记录可归因。
- **新表一律 GORM AutoMigrate + user_id 隔离审查**（涉及用户数据的）；纯行情缓存表标注"可重建"进 DEPLOYMENT 备份清单。
- **每批测试底线**：纯函数表驱动单测 + DB 集成测试（隔离/幂等）+ `go build/vet/test` 全绿 + `vue-tsc`+build 过；提交前 `git checkout -- server/web/dist/index.html`。
- **每批收尾**：更新本文档状态列与 commit 号 → ROADMAP §3 增补防回归 → 记忆文件同步。
