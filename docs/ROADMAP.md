# 项目现状与实现边界

> 本文档原为实施路线图。阶段 0~8 与后续增强批次已**全部完成并入库**（历史流水账见 git log 与各 commit message），
> 现只保留对后续开发有约束力的内容：当前状态、实现边界、防回归要点、未完成项与欠人工验证清单。
> 表结构以 `server/model/*.go`（GORM AutoMigrate）为准；数据源认知见 `DATA_SOURCES.md`；架构与 UI 约定见 `ARCHITECTURE.md`；部署见 `DEPLOYMENT.md`。

## 1. 当前状态（截至 2026-07-05）

个人自用 AI 股票研究平台，Go(Gin+GORM)+Vue3(Naive UI)，单容器 go:embed 托管前端，生产 MySQL。已具备：

- **账户与设置**：密码 + GitHub OAuth 双登录、账号绑定/解绑、JWT(access+refresh 可吊销/即时失效)、管理员后台（注册开关/GitHub 凭证/用户与配额管理）、用户级 LLM 配置（OpenAI 兼容、密钥加密、测试连接）、偏好（默认市场/风险/推荐筛选默认/黑名单/流动性门槛/提醒与日报开关）。
- **行情与数据**：东财→腾讯→新浪三源主备切换；实时行情/日线（东财前复权 fqt=1）/指数/榜单（涨幅/成交额/换手率/回调[跌幅升序]/低PB[升序]，方向参数支持「不热」来源）/板块/涨跌家数/两市资金流/腾讯免费估值（PE/PB/市值/换手/量比/振幅/涨跌停价）；交易日历回填、日线批量同步、市场情绪快照。
- **研究工作台**：市场首页看板、个股详情页（日K/估值/五维评分/快捷动作）、自选股（分组/研究阶段漏斗/错过机会复盘）、持仓（盈亏费税精算/风险计划/买入前清单/结构化复盘/组合总览与风控信号/CSV 导入导出）、投资逻辑卡、投资笔记、条件提醒（6 类规则+命中明细状态机+主动推送）、今日待办聚合、模拟交易（虚拟账户/真实行情成交/佣金印花税）、**指数 ETF 交易**（精选指数 ETF 行情清单、复用模拟盘买卖、ETF 免印花税费率）。
- **AI 链路（全链路信任层）**：五模块分析（个股/市场/板块/自选/持仓，反方视角三字段、panel 多角色、变化检测 diff）、个股问答（快照复用多轮）、横向对比（可选 AI 点评）、短线/长线推荐（**四阶段流水线**：多源建池→用户筛选硬过滤→本地量化评分→LLM 仅对 Top12 精选/否决）、推荐追踪（止盈止损盘中触达判定/基准 alpha/7/14/30 日节点收益/历史表现统计）、收盘日报（复盘+明日推荐+卖点提醒自动建规则）。
- **信任机制（推荐域已全量，其余 AI 模块同标准推广）**：程序化证据核验（LLM 引用数字与数据快照容差比对）、程序合成置信度（量化排名×核验吻合率×数据完备度，不信 LLM 口头置信度）、AI 复核员（可选二次调用挑刺 pass/warn/reject，reject 强制降级）、候选池全景透明面板、prompt 强制引用数值+禁先验记忆+允许拒选、数据快照落库可复现。

## 2. 当前实现边界（承认的简化，非隐藏问题）

- **市场范围**：数据适配器只支持 A 股 `cn`（仅沪深股票 + 沪深 ETF/LOF 基金，不含北交所 4/8 开头）；美股/港股需接数据源后再开放。
- **AI 分析结构**：结构化字段为 `rating/confidence/summary/highlights/risks/opportunities/suggestions/anti_thesis/kill_switches/unknowns/disclaimer`；更完整的 `market_context/data_points` 等属目标态，扩字段需同步 schema/前端/历史兼容。
- **追踪复权**：`daily_bars` 主源为东财**前复权**（fqt=1，最新价重锚），除权后历史整段重刷、与生成时点 RefPrice 快照价错位（note 已标注）；新浪兜底无复权参数、无成交额（落库不覆盖 amount），两源口径不保证一致。彻底解决需 `corporate_actions` 复权因子表（未建）。
- **未建的表**：`corporate_actions`、`audit_logs`、`ai_call_logs`（个人自用降级）；`data_source_configs` 已建但**无读写方**（死表，管理端未接）。
- **配额**：次数制终身累计（`action_limit/action_used` 熔断：一次手动动作=1 次、repair/panel 等内部多轮不重复计、后台任务不计次、token 仅审计），无自动周期重置；管理员可 `PUT /api/admin/users/:id/quota` 调上限/手工清零。
- **LLM**：单一默认配置（无分析/推荐双默认）；`stream` 字段存而未用（恒非流式）；仅 OpenAI 兼容 provider。
- **stock_quotes** 为 symbol+market 唯一的最新快照单行覆盖（非历史序列）；`daily_bars` 用 symbol+market 自然键、无 adj_close。
- **短线追踪状态**枚举 `active/take_profit/stop_loss/expired/tracking/no_data`，无用户买卖联动（watching/closed 未实现）。
- **股票评分**为五维技术面（趋势/动量/位置/量能/风险）；估值/成长/财务/情绪维度待财务数据源。
- **新闻/财务/宏观**：分析/问答/对比/推荐均已要求模型不得虚构未提供数据；新闻情绪、财务详情、财报日历依赖后续数据源（Tushare 低 cost 档等）。
- **短线计划**：prompt 与后处理已含 A 股 T+1、涨跌停、100 股一手、交易日有效期约束；未做完整订单可执行性模拟。
- **ETF**：仅接入沪深场内基金行情/日线与模拟盘交易；ETF 不参与个股推荐候选池（涨停幅度按股票板块前缀推断对 ETF 不适用，已透明排除）；ETF 无 PE/PB 个股估值（分析快照已标注不适用）。

## 3. 推荐流水线与信任层防回归要点（改代码前先读）

- **parseAndFilterPicks 空数组=合法拒选**（`*[]recPick` 指针语义区分「缺 picks 字段」与「显式空数组」），别改回「空=报错」——p4 prompt 明示宁缺毋滥可 0 只，空数组触发 repair 会强迫模型硬凑标的。
- **scorePool 日线拉取失败=透明排除**，不能让无日线标的按中性 50 分参与排名（会挤占 Top12 且绕过追高保护）。
- **池满补位只补一轮**（poolFullPrefix 前缀识别），拉取总量有界；`maxScanCandidates=48 / maxLLMCandidates=16 / maxPoolIntake=240 / poolSnapshotMax=150` 四个常量各司其职。
- **换手率两级阈值**（2026-07-06 分位化，替代旧的 20% 一刀切）：>30%（deadTurnoverHardPct）阶段②无条件硬拦；20~30% 放行到阶段③按 pos_60 分档——≥65% 高位判「死亡换手」排除，低位保留但评分扣 5 分并标注风险。用户筛选区间必须被钳在 30 之内（UI :max=30），「钳制上限=绝对硬顶」不变式防空池死局；别改回一刀切，否则换手率榜来源几乎被清空（该榜前排常年 20%+）。
- **榜单来源随策略组合**（strategySources）：升序榜只有低PB可用并必须带行级 keep（滤负 PB+亏损/高PE 价值陷阱）；**「回调票」严禁用跌幅榜升序取**——全市场跌超 2% 家数常年远大于 100，升序前 100 永远是深跌段（2026-07-06 实测 [-21%,-7.6%]），「温和回调」必须从成交额/换手深捞结果里 keep（ChangePct∈[-7,-0.5]）；换手路深捞 100 后取温和带（3~15%），榜单前排极端换手大多活不过硬拦。北交所（43/83/87/920 前缀）在 candidateEligible 排除（数据源不支持，日线必失败、白占评分名额）。新浪榜单自带 pe/pb/nmc（万元）仅作行级过滤与腾讯估值缺失时的兜底（PB/FloatCap 腾讯 >0 才覆盖），腾讯口径优先。
- **评分名额按来源轮转**（assignScanQuota）：自选整组优先、其余来源逐轮各出一只——别改回按池序先到先得（第一路榜单 50+ 只会垄断整个量化窗口，其余来源全部沦为「池满」）。换手下限钳 25（非 30）：给 [min,30] 留可行带。长线「PE 为负（亏损）」扣分条件是严格 `PETTM < 0`——PETTM==0 是估值缺失而非亏损，新浪 PB 兜底存在后 `<=0 && PB>0` 会把缺失误标成假证据（信任层自伤）。Pos60 取尾部 60 根（bars 全长 90，全窗遍历口径会漂成 90 日，而它是高位换手排除的判定依据）。
- **LLM 只见量化 Top16 子集 map**（poolBySymbol），parseAndFilterPicks 以它做反编造校验——池内但非 Top16 的标的同样会被丢弃，这是有意设计。
- **verifyEvidence 的 extra 变参契约**：调用方必须把「模型自己输出的计划价」与「用户设定阈值」并入合法值域，否则模型合法复述自己的结论会被误标为幻觉（信任层自伤）；无小数点整数跳过规则（≤99 的 rank 类/年份/六位代码/噪声整数）同理不可删。
- **复核 reject 必须级联**：强制 Action=watch 且置信度压 ≤25，否则「复核否决」徽章与高置信度并排自相矛盾；复核 best-effort 失败不阻断主结果，但 token 必须累加进同一批次并 consumeQuota。
- **LLM 输出的整数字段一律用 FlexInt**（模型会输出 72.5/"80"，裸 int 让整个 JSON 反序列化失败被静默丢弃）。
- 前端 `STRATEGY_NAME` 静态字典是历史批次标题（"value"）bug 的兜底，别删；新记录走后端固化的 `batch.title`。
- 日报明日推荐与手动推荐完全同链路（GenerateAuto→generate），筛选偏好 `rec_filters_json` 对两者同时生效；自动链路不触发 AI 复核、不计次数配额。
- prompt/策略改动须递增版本号（分析 `analysisPromptVersion`、推荐 prompt/strategy 版本），保证历史记录可复现可归因。
- **新闻采集（N1）上游口径**（2026-07-06 实测）：财联社只有 `/v1/roll/get_roll_list` 活着，`sign = md5hex(sha1hex(按参数名字典序的 querystring))`，老 nodeapi 已死；东财快讯 `req_trace`（uuid hex）缺失返回空，`stockList` 是**字符串数组**（"1.688035"/"90.BK1175"，别按对象数组解析）；东财个股新闻 search-api 标准 Go client 实测**可用**（无需 utls，但保留「单只失败整轮降级」逻辑防日后被指纹拦截）。去重 Dice 阈值 0.85 是「尾部增删判重、改动较大放行」的实测锚点，调低会误杀无关新闻。
- **新闻→AI 信号（N2）防回归**（2026-07-07）：①`News.Sentiment` 空串=未增强、增强后恒为 positive/negative/neutral——增强失败也要走规则兜底落枚举值，别留空串（会被每轮重挑反复烧 token）；②两个幂等键分清：逐条增强按 news.id、个股聚合情绪分按 (symbol,date) 唯一索引+包级 mutex+map 合并（stock_sentiments 一天一行，当日后来的新闻不改已落库分——有意设计，保证推荐批次内证据可复现）；③LLM 增强挂**首个管理员默认配置**、consumeQuota(adminID, tokens, false) 只记审计不扣次；成本分级（P1/P2 全量、P3 规则先行缺板块才简化 LLM 且每轮≤10 条）别拉平；④related_sectors 必须过 `newsSectorWhitelist` 白名单（防幻觉板块），LLM 输出方向与分数矛盾时以方向为准分数归 0；⑤新闻标题是「文本型合法来源」：分析 fillAnalysisTrust / 问答 Ask / 日报 dailyReviewEvidence 三处都已把标题 decimalNumbersIn 并入核验值域，新增消费方照此办理（否则忠实引用标题里的数字会被误报幻觉）；⑥日报事件段 4 步硬规则在 `newsevent.go`（LLM 只写 events_review 摘要），黑名单/打分/合并/TopN 都是纯函数有单测，调阈值先跑 `TestSelectReportEvents*`；⑦推荐 senti 因子只在 SentiNews>0 时动分（无新闻≠中性 0 分证据），senti_score 已进 candidateValueSet 核验值域；本批版本号：分析 p6、问答 q4、推荐 p6/s4。
- **财报日历与公告（F1）防回归**（2026-07-07）：①所有 RPT_* 报表必须走 `datasource/emdatacenter.go` 的 DataCenterQuery 迭代器（全局令牌桶 QPS≤2 是**包级共享**，绕开直连会破坏限流）；上游「无数据」返回 success=false+code=9201，客户端已归一为 ErrNoData——增量刷新空轮是正常态，别当错误重试；②三类财报表唯一键 (symbol,market,report_date)、upsert 幂等（业绩预告会修正、预约披露有三次变更，重复拉取靠 DoUpdates 覆盖）；增量游标存 options（"period|date"），报告期切换自动全量、NOTICE_DATE 过滤带 1 天重叠防同日晚间发布漏采；预约披露无 NOTICE_DATE 走每日全量重拉（一期约 12 页，别改成增量）；`reportPeriodsAsOf` 返回**最近两期**是为覆盖 1~4 月年报+一季报并行披露季，别省成一期；③提醒 kind `earn_date`/`earn_fcst` ≤16 字符（列宽 size:16）；盘中 15min 行情评估（StartAlertJobs→evaluateUserMarket）的规则查询**显式 kind NOT IN 排除财报类**（否则每 symbol 拉行情空转），财报类由 finance job 每日 19:05 数据刷新后 EvaluateEarningsAll 一评；手动「立即检查」EvaluateUser 两类都评（财报类查本地表零上游成本）；防重靠 TriggeredAt 与披露窗口/预告 NoticeDate 比较（纯函数 evaluateEarnDate/evaluateEarnFcst 有单测），别改成只靠同日去重（Once=false 会连续多日重复提醒）；④公告 (symbol,art_code) 唯一冲突忽略即增量；详情页按需补拉有 1h 进程内冷却（annFetchAllowed），别删（防详情页被刷打上游）；⑤公告标题进 AI 证据池：个股快照 announcements 块 + `announcementTitleTexts` 已并入分析/问答两处核验值域（同新闻标题前例）；本批版本号：分析 p7、问答 q5。
- **技术指标库与筹码分布（T1）防回归**（2026-07-07）：①指标口径（`service/indicator.go` 头注释是权威）：RSI/ATR 用 **Wilder 平滑 α=1/n、seed=首值**（通达信 SMA(X,N,1) 递推），别改回参考项目的滚动 SMA 口径；MACD 柱=**2×(DIF−DEA)** A 股口径别改 1 倍；EMA seed=首值（pandas ewm adjust=False）；BOLL 样本标准差（n-1）。递推指标数值受输入窗口起点影响：详情页 API 加 100 根 warmup 后截尾、推荐因子吃 210 根全长，两处允许 <1% 微差，属固有性质非 bug；②`scorePool` 拉日线已从 90 改为 **chipBarLimit=210**（同一上游请求仅行数差异）：五维评分**必须截尾部 factorBarLimit=90 再喂 computeScore**——positionScore 是全窗口径，直接喂 210 根会把「90 日区间位置」漂成 210 日（Pos60 同类前科）；computeCandFactors 可吃全长（内部全部尾窗/递推，不漂移）；③五维升级：动量维=涨幅合成 60%+RSI **凹形** 40%（55~70 满分、≥70 陡降是刻意的反追高逻辑，别改成「RSI 越大分越高」的线性）；风险维=回撤反向 60%+ATR% 反向 40%；样本不足 rsiMinBars/atrMinBars 时自动退回纯旧口径，不臆测；④筹码（`service/chip.go`）：<120 根拒算、<210 根标 data_limited；**推荐加分门槛 ChipBars≥210**（次新股累积失真不给分）；换手率优先日线自带（东财 f61），缺失根按股本推断（有换手根的中位数反推，全缺失再用腾讯 FloatCap/现价），推断也不可得=拒算而非拍默认值；停牌/零成交 t=0 不衰减不新增；⑤喂给 LLM 的新因子字段（rsi_14/macd_*/boll_*/atr_*/chip_profit/chip_avg_cost）已同步进 `candidateValueSet` 核验值域——日后再加因子字段必须同步，否则忠实引用被误报幻觉（信任层自伤）；⑥`datasource.Bar.TurnoverRate` 只有东财日线有（f61），新浪兜底恒 0；`persistDailyBars` 在本批换手全 0 时不覆盖已写真值（amount 同款防回退模式）；daily_bars 新增 turnover_rate 列；⑦详情页副图：indicators 与 bars 是两次独立请求，前端 `alignByDate` 按 trade_date 对齐，别改成按下标 zip（末根可能差一天）；本批版本号：推荐 p7/s5。
- **稳定性与流式体验（S1）防回归**（2026-07-07）：①东财断路器（`datasource/breaker.go`）：push2 族限流（EOF/断连/302/403/429/502）自动降级 `push2delay.eastmoney.com` 并**实例级记住不回切**；push2his/push2ex **无备用域**（push2dhis 不可用，别加），连续 5 次限流熔断 2 分钟快速失败；ctx 取消/超时不计入熔断计数；非 push2 族（datacenter/公告）不经断路器（自有限流治理）。适配器 `fetch` 字段可注入假实现，改断路器逻辑先跑 `TestBreaker*`。②健康滑窗（`datasource/health.go`+manager routeCap）：每 (源,能力) 50 次环形窗，empty>50% 或 error>30%（样本≥20）冷却 300s 踢出轮询、**触发即清窗**（恢复后从零观察）；**全部源冷却时有补跑轮兜底**，别改成直接报错；ErrNotSupported/ErrSymbolInvalid 不记滑窗（非健康问题）；两层超时=总预算 15s（仅调用方无 deadline 时兜底）+单源 6s（须小于 doGet 2×8s 最坏耗时）；错误归一 {EMPTY/UPSTREAM_TIMEOUT/PARSE_ERROR/UPSTREAM_ERROR} 记 DEBUG；健康端点 `GET /api/admin/datasources`；**DataSourceConfig 死表已删**（模型+迁移注册，旧库物理表可手工 DROP）。③风险闸门（`service/riskgate.go`）：block（ST/退市）/warn（一字板=|涨跌幅|≥9.5 且振幅<1、成交额<3000 万）/info（市值<30 亿）随个股快照 risk_gate 块注入分析/问答 prompt 并透传前端（AnalysisView/QaConversationView 的 risk_flags）；**flags 为空也注入 note**（「质押/解禁未接入请自行核查」措辞纪律恒在）；**riskGateTexts 已并入分析/问答核验值域**（提示文本里的 9.5/3000 等阈值数字是合法来源，删了会误报幻觉）；持仓模块 capital_context 只在偏好 total_capital>0 时注入，割/守/补三选一是 prompt 硬要求（禁骑墙）；版本 p8/q6。④问答流式：`chatCompletionStream`（SSE 剥 data:/[DONE]/finish_reason 终止/坏 chunk 容错跳过/**流中断即失败不落半截回答**）；Ask 与 AskStream 共用 prepareAsk/finalizeAsk（快照/核验/落库口径必须一致，改一处两处同步）；`LLMConfig.Stream=false` 时 AskStream 退回非流式整段吐出；NDJSON 协议 {module,code,chunk,status}（**code 字段为批量场景预留、单标的恒空**）+`X-Accel-Buffering: no`；前端 fetch+getReader 行缓冲、100ms 节流渲染、**LLM 输出 v-html 前必须过 `lib/markdown.ts` 的 renderMarkdown（marked+DOMPurify 白名单，链接/图片已剥）**；核验徽章靠 done 行整体替换会话后置出现。
- **财务数据与估值增强（F2）防回归**（2026-07-07）：①F10 主要财务指标走 `datasource/emfinance.go` 的 GetF10MainFinance——base URL 是 `datacenter.eastmoney.com/securities/api/data/get`（type/sty/p/ps 参数族，与 datacenter-web v1 的 reportName 参数族不同），但**共享包级令牌桶 dcThrottle（QPS≤2）与重试纪律**，别绕开直连；三大报表 emweb 三表 AjaxNew 先 lrbDateAjaxNew 试探 companyType 4→3→2→1（不匹配返回无 data 空对象非错误），dates 5 期一批防 URL 过长；②按需拉取+缓存：7 天新鲜期（按表内 MAX(updated_at) 判）+1h 尝试冷却（成功失败都记，**包级共享 finSyncTry**——FinanceService 有多实例，别改成实例字段），触发点只有 详情页财务块/个股 AI 快照（仅 F10）/长线推荐（预算 finRecFetchBudget=12 只/次），**严禁全市场普查**；③GORM 列名坑：字段名含 YoY 会被转成 yo_y（revenue_yo_y）——新表 FinanceIndicator 用显式 `gorm:"column:revenue_yoy"`，**F1 旧表 EarningsExpress 保持物理列 revenue_yo_y**（AssignmentColumns 已按物理列名修正，改回 revenue_yoy 会让快报修正永远覆盖不进去），AutoMigrate 不改已有列名；④值域同步铁律（T1 前例）：长线名单 fin 字段（ROE/增速/毛利率/净利率/负债率）已进 candidateValueSet；分析/问答的 finance 段是 JSON 数值叶子由 snapshotValueSet 自动收集，无需手工；日后给 fin/finance 加字段必须同步；⑤财务加分只在 c.Fin 非 nil 时动分（缺失不惩罚是明文设计），业绩恶化扣分（净利同比≤-30）是长线通用项；⑥上游 null 落 0：消费方按「0 可能=缺失」处理，prompt 已声明不得据 0 下「归零」结论；本批版本号：推荐 p8/s6、分析 p9、问答 q7。
- **全市场日线地基（M1 第一部分）防回归**（2026-07-08）：①clist 全市场快照（`datasource/eastmoney_clist.go`）：上游把 **pz 硬钳制在 100**（实测 pz=6000 只回 100 行），必须 56 页翻页；**fid=f12 代码升序翻页**（涨跌幅序盘中排名漂移会重复/漏行，别改回 fid=f3）；fs 四段=沪深 A 股不含北交所（cnSecid 不识别，纳入前先扩 secid 映射）；**半截快照必须整轮拒绝**（<total 90% 报错——部分落库会留"当日只有一半股票有 bar"的静默缺口）；停牌行价格字段是 "-"（emNum 容错为 0）但 f18 昨收有值、行保留进宇宙字典；走 `e.get`（push2 断路器）。②宇宙字典 `market_sync_states` **有意不并入 stocks 表**——stocks 是"用户关心的标的"语义，SyncTrackedDailyBars 按 800 上限轮转同步它，灌入全市场 5500 只会稀释轮转；两条链路独立。③全市场链路（增量/初始化/重锚）**强制东财直连（wideSource），不走 mgr 路由**——新浪日线不复权，兜底写库会把前复权基准锚坏；直连 bars 须手动补 Source="eastmoney"（manager 层才自动填）。④除权双层检测：persistDailyBars 的 detectAndRebase **均匀采样 60 点多点比对**（只取尾部会漏"窗口外旧基准"断层——这就是它要防的"部分窗口重写"漏检）；**比对锚必须排除"今天"的根**（当日 close 盘中持续变化，会把盘中波动误判成除权，每天海量误重锚）；仅 Source=="eastmoney" 才检测（新浪不复权必偏差）；盘后 SyncMarketWide 的 f18 昨收初筛只兜底无人访问的标的；容差 0.5%（rebaseTolerance，更小的分红除权会漏检但量级可忽略，是承认的简化）；单轮重锚上限 200 只（超过=上游口径整体漂移，拒绝批量重写）。⑤重锚=**事务内整股删+插 250 根**（窗口外更老数据无法对齐新基准，留着是毒数据；因子/筹码/回测窗口均 ≤250 日）+记 adjust_epoch；重锚失败退回写本窗并**把 states 踢回 pending** 交初始化任务自愈（那条路拉全量 250 根多点采样必再抓住断层）；rebaseInflight 防并发重复重拉。⑥历史初始化**源故障熔断**：连续 10 只网络类失败（非 ErrNoData）判源限流，本轮中止且**不把失败记到标的头上**（push2his 限流期逐只硬扫会把全宇宙误标 failed 还加速打上游）；ErrNoData/ErrSymbolInvalid 是标的问题直接记，fail_count 达 3 置 failed 不再骚扰（退市整理/长停股）；断点=表状态，游标只向前（防失败行同轮反复取）。⑦f124 定 trade_date（max 时间戳的日期），全缺失整轮报错不猜日期；周末手动跑落的是上一交易日快照，幂等无害。
- **因子宽表与条件树选股（M1 第二部分）防回归**（2026-07-08）：①宽表因子口径纪律（`service/factortable.go`）：computeWideRow **直接复用 indicator.go 底层序列函数（rsiSeries/macdSeries/bollSeries/atrSeries）与最小样本常量、窗口因子照 recfactor.go 尾窗口径**，`TestComputeWideRowParity` 与 computeCandFactors 对拍锁死——改任一侧口径对拍必炸，两处同步改；atr_14 存 **round2**（与 computeIndicatorSnapshot 对齐），别顺手改 round3。②缺失=**NaN 语义**：样本不足/筹码拒算的因子列是 NaN，条件求值 NaN 恒 false，**布尔因子 NaN 连 is_false 也不命中**（「未知≠否」——macd 样本不足的股不能命中「非金叉」）；candFactors 的 0=缺席语义在宽表升级为显式 NaN，新增因子别拿 0 当缺失。③涨停判定：阈值=limitUpPctFor−0.2（主板 9.8/创业科创 19.8/ST 4.8），**收盘涨幅口径**（相邻 close 比），ST 判定依赖 states.name——宇宙字典 name 空时按非 ST 处理。④新鲜度：表 TradeDate=**states MAX(last_bar_date)**（5500 行小表；别改成扫 daily_bars MAX——130 万行对该查询无友好索引），60s 缓存；个股末根≠表日期=stale，**扫描默认排除**（停牌股旧价因子会误导），include_stale 显式放开；构建的 DB 读 `ORDER BY symbol, trade_date` 恰与唯一索引序一致（免 filesort），别改列序。⑤构建互斥：ensureFactorTable（同步构建+双检）与 RebuildFactorTableAsync（TryLock 防抖）**共用 factorBuildMu**——重建进行中扫描请求阻塞等新表而非双建；重建挂点=16:10 增量成功后+初始化轮 Succeeded>0 后，删挂点会让宽表永远陈旧只剩懒加载兜底。⑥内置策略（`screener_builtin.go`）只能引用 factorDefs 内因子（TestValidateCondTree 全量校验 21 个内置树）；**宽表没有估值因子**（PE/PB 是实时单只接口无法全市场普查）——破净类策略别写进内置；DSL 的 Value 用 *float64 区分「显式 0」与「未填」，between 上下界宽容交换，all/any 同节点互斥。⑦推荐池 strategy_signal 来源 **best-effort**：宽表未就绪/无日线时 strategySignalHits 返回 nil 静默跳过（与单榜失败降级同款，别改成报错阻断建池）；命中进池后**批量拉实时行情覆盖收盘口径 Price**（盘中生成推荐防滞后一天）；来源字典 sourceLabelCN（后端）与 Recommendations.vue SOURCE_LABEL（前端）两处必须同步。

## 4. 未完成项与储备（按数据源可得性推进）

> **2026-07-06 起后续开发按 `DEVELOPMENT_PLAN.md` 的批次（N1→N2→F1→T1→S1→F2→M1→M2→M3）推进**——它是面向执行的施工图（每批含方案锚点/依赖/验收）；分析依据与上游接口速查表在 `REFERENCE_ANALYSIS.md`。以下原有储备多数已并入该计划：

- 新闻情绪（→ 批次 N1/N2，财联社/东财源已调研齐）、财务详情/财报日历（→ 批次 F1/F2，东财 datacenter 网关免 Tushare）、回测模块（→ 批次 M2 时光机）。
- 多数据源系统级切换管理端（`data_source_configs` 接读写）。
- 东财 clist 全市场扫描候选源（B 档限流，储备；「不热」来源已由新浪 Market_Center 方向参数（跌幅/低PB 升序榜）覆盖大半，引入 clist 的边际收益降低）。
- 分析历史 rating 命中率统计（信任闭环二期）。
- 多模型交叉观点、研究证据包、财报季工作流、组合风格暴露分析（低优储备）。

## 5. 欠人工验证清单

- 推荐页/设置页新 UI 浏览器目验（筛选表单交互、真实生成一次、候选池全景、信任徽章 tooltip、AI 复核开关，亮/暗主题各一）。
- 指数 ETF 页与各 AI 页新增信任徽章/透明面板的浏览器目验。
- 6 主题目验欠账（至少 1 亮 1 暗，含 /thesis /notes /daily-report /stocks 详情 /etf）。
- 手机浏览器真机目验（≤768px 适配）。
- S1 欠目验：问答页流式渲染（首 chunk <300ms/markdown 排版/光标动画/核验徽章后置出现）、分析页与问答页风险闸门标签（找一只 ST 或一字板标的目验 block/warn 展示与 prompt 生效）、设置页总投资资金保存回填、`GET /api/admin/datasources` 健康端点真实数据；断路器 push2delay 降级须部署环境观察日志（本机 push2his 长期 EOF 正好是真实场景）。
- F2 详情页「财务摘要」块：真实数据展示（首次访问触发拉取，600519 等大票秒级可见）、柱线图 6 主题+移动端、长线推荐真实生成一次看 fin 字段与 ROE/增速 Bonus/evidence 引用财务数字且核验通过。
- T1 详情页 MACD/BOLL 副图与筹码峰卡：6 主题（副图 legend/双 grid 十字光标）+ 移动端单列布局欠人工目验；东财日线 f61 换手率与 lmt=210 真实拉取欠部署环境冒烟（本机 push2his 被限流，`LIVE_MARKET=1 go test ./datasource/ -run LiveMarket`）；推荐生成一次看 factors 新字段（rsi_14/macd_*/chip_profit）与 Bonus 新加分项实际出现。
- M1 第一部分欠部署验收：历史初始化断点续传真实跑通（本机 push2his 被限流拉不了 kline，部署环境 `LIVE_WIDE=1 go test ./service/ -run TestLiveWide -timeout 15m`，或直接管理端 `POST /api/admin/market/wide-init` 后 `GET /wide-status` 观察 done 推进/暂停恢复）；每日 16:10 job 首轮全量建库（5535 只×250 根约 40 分钟）；除权股真实命中观察（除权日看日志「检测到除权/送转」与 states.adjust_epoch，历史窗口衔接无断层）。
- M1 第二部分欠部署验收：/screener 选股页 6 主题+移动端目验（策略卡/命中表/条件编辑器弹窗）；全市场日线就绪后真实扫描一次——宽表**含 DB 读**的总构建耗时核对 <5s 量级（因子计算部分 5150 只已实测 1.43s，日志有「因子宽表构建完成」耗时明细）、内置策略命中数量合理性与命中原因人话展示；推荐真实生成一次看候选池全景出现「策略信号」来源。

## 6. 技术风险（长期有效）

- **数据源**：免费源限流/字段变更是最大不确定性；必须显示数据更新时间，异常时降级而非编造。Tushare 分档按需启用（免费档/低 cost 档），高级档不实现。
- **LLM 幻觉**：结构化输出 Schema 校验 + 有限次 repair + 优雅降级；数据快照与 prompt 版本落库保证可复现；信任层核验结果透明展示而非静默修正。
- **合规**：大陆向不特定对象荐股属持牌业务；本项目定位**个人自用研究工具**，全产品统一「研究参考/交易计划/候选标的」表述，公开部署前须合规评估。
- **成本**：LLM 调用有缓存/配额熔断/限流/token 审计；高频页面不直调 LLM；后台任务失败落库防反复烧 token。

## 附：工程约束

- **go:embed 路径约束**：`go:embed` 不能引用包外/上层路径，前端产物构建时拷入 `server/web/dist`（Dockerfile `COPY --from=webbuilder`）；本地构建会改写 `server/web/dist/index.html`，**提交前在仓库根目录执行 `git checkout -- server/web/dist/index.html`**。
- 反代部署需配 `TRUSTED_PROXIES`，否则限流/日志拿不到真实 IP（详见 DEPLOYMENT.md）。
