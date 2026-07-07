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

## 6. 技术风险（长期有效）

- **数据源**：免费源限流/字段变更是最大不确定性；必须显示数据更新时间，异常时降级而非编造。Tushare 分档按需启用（免费档/低 cost 档），高级档不实现。
- **LLM 幻觉**：结构化输出 Schema 校验 + 有限次 repair + 优雅降级；数据快照与 prompt 版本落库保证可复现；信任层核验结果透明展示而非静默修正。
- **合规**：大陆向不特定对象荐股属持牌业务；本项目定位**个人自用研究工具**，全产品统一「研究参考/交易计划/候选标的」表述，公开部署前须合规评估。
- **成本**：LLM 调用有缓存/配额熔断/限流/token 审计；高频页面不直调 LLM；后台任务失败落库防反复烧 token。

## 附：工程约束

- **go:embed 路径约束**：`go:embed` 不能引用包外/上层路径，前端产物构建时拷入 `server/web/dist`（Dockerfile `COPY --from=webbuilder`）；本地构建会改写 `server/web/dist/index.html`，**提交前在仓库根目录执行 `git checkout -- server/web/dist/index.html`**。
- 反代部署需配 `TRUSTED_PROXIES`，否则限流/日志拿不到真实 IP（详见 DEPLOYMENT.md）。
