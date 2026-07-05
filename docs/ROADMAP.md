# 实施路线图

## 2026-07-04 推荐流水线重构 + 全 AI 链路信任度增强 ✅

> 用户痛点：①候选池不透明、觉得「推的都是大票」；②没有股价范围筛选（资金少买不起高价股）；③单次 LLM 直选不可信；④历史标题会变/显示英文 key（如 "value"）；⑤所有 AI 功能都要更可信。方案对标调研结论（Qlib 三阶段流水线/问财条件回显/TradingAgents 角色分工/学术共识「LLM 不做 listwise 海选」），一次落地。

- **推荐四阶段流水线（rec prompt p2→p4、strategy s1→s2）**：
  1. **多源建池**：自选 ∪ 涨幅榜 ∪ 成交额榜 ∪ **换手率榜**（新解锁 sina turnoverratio 排序，对冲成交额榜大市值偏向），每源 20 条，来源可叠加（`sources[]` 落库）；
  2. **用户筛选硬过滤**：股价区间 / 流通市值区间 / 换手率区间 / 排除当日已涨停（买不进）/ 近 5 日涨幅追高保护（默认 25%，20cm 板放大 1.6 倍）+ 换手 >20%「死亡换手」硬拦；**被筛掉的标的保留在池快照并标注原因**（学问财条件回显）；条件快照 `filters_json` 落库可回显；偏好 `rec_filters_json` 存默认、请求可临时覆盖、日报自动推荐同享；
  3. **本地量化评分**（零 LLM 成本）：Top36 拉 90 根日线（新增 10min 缓存）算技术因子（MA5/10/20/60、近 5/20 日涨跌、20 日新高、多头排列、量能倍数、波动率、回撤、BIAS20、60 日位置）+ 五维评分（复用 computeScore 同口径）+ 策略加分项（逐条中文说明落库），池内排名；
  4. **LLM 只做精选+解读**（角色从「海选者」降级为「解释者/否决者」——学术共识 listwise 选择位置偏差 13~75%）：只看量化 Top12，prompt 强制引用字段名+数值、禁用先验记忆（防「知名大票」偏好）、允许少选甚至 0 只（防硬凑）。
- **信任层（推荐）**：**程序化证据核验**（LLM evidence 里的数字逐一与快照容差比对，`evidence_check` 落库，前端「数据核验 N/M」徽章 + 未吻合数字列出）；**程序合成置信度**（量化排名×核验×数据完备度三档，LLM 口头置信度系统性过度自信不可单独信）；**一手成本**展示（价格×100，资金约束直观化）；**AI 复核员**（`verify` 可选二次调用：独立风控人设逐条挑刺 pass/warn/reject，reject 自动降级为观察，结论 `review_json` 落库展示）。
- **标题修复（根因：前端用「当前所选类型的策略列表」查名，跨类型批次回退显示原始 key 且随类型切换变化）**：批次落库 `title`（筛选条件组合：「短线·动量突破 · ≤30元 · 3只」）；前端历史/结果头用 title、旧记录回退全量静态策略字典。后端 `strategyByKey` 未知 key 从静默回退改为报错。
- **前端**：推荐页筛选区（价格/市值快捷档+自定义、换手区间、追高/涨停开关、AI 复核开关、保存为默认）；**「候选池全景」折叠面板**（排名/来源/换手/量比/市值/量化分/加分明细 tooltip/AI 名单标记/被筛原因，含被筛掉标的）；推荐卡信任徽章行；设置页偏好加「推荐筛选默认」。
- **信任层（其余 AI 功能）**：个股分析/问答快照注入 `quant_score` 五维评分锚点（analysis p3→p4；与评分明显相悖时模型必须解释分歧）；全模块 prompt 加「关键判断必须引用具体数值」「禁用先验记忆」；对比点评把已算好的综合分/量比喂给模型（此前算了没喂）；日报复盘要求引用数字佐证；**日报明日推荐策略按涨跌家数动态选**（强势 momentum / 弱势 pullback / 中性 active，旧版恒为 momentum）；推荐 LLM 输入注入市场环境锚点（指数/涨跌家数/主力资金/上证 vs MA60/MA200）；免责话术统一补「AI 生成、可能存在数字与事实偏差」。
- **测试**：新增 30 项（筛选归一/逐条过滤/涨停幅度按代码前缀/追高板块放大/条件回显/标题组合/策略严格匹配/日报动态策略/复核应用/因子上升下降空数据/策略加分方向/证据核验吻合与噪声/置信度三档）；既有反编造/归一化/隔离测试全部保持通过。后端 build/vet/test 全绿，前端 vue-tsc + vite build 通过。
- **遗留**：浏览器目验（筛选表单/池全景面板/徽章 6 主题）；东财 clist 全市场扫描候选源（B 档限流，列入待办储备）；分析历史 rating 命中率统计（信任闭环二期）。
- **收尾修复（2026-07-05）**：换手筛选区间统一钳制 ≤20%（死亡换手硬顶内，UI 上限同步并注明，消除 turnover_min>20 的必然空池死局）；证据核验免误报（≤99 整数/rank 类引用跳过，模型计划价与用户筛选阈值并入合法值域）；池快照省略数（pool_omitted）前端提示；生成前偏好未加载时兜底补拉。

## 当前进度（更新于 2026-07-02）

- **阶段 0：已完成 ✅**（commit `2c428c5`）。全栈骨架就位，后端 + 容器内均端到端拉到真实行情（三源链：东财 → 腾讯 → 新浪，主备切换实测生效），单镜像 64MB 构建运行通过。
  - 唯一保留项：前端 Vue **运行时未在真实浏览器实测**（本机 node16 跑不了 vite dev，仅验证到构建+类型检查+容器静态托管）。低风险，建议人工开一次浏览器确认。
- **阶段 0 前置（安全地基）：已完成 ✅**（commit `d364fde`）。生产密钥 fail-fast + API Key AES-256-GCM 加密（详见项目记忆 `phase0-skeleton-review`）。
- **阶段 1：用户与设置：已完成 ✅**。GitHub OAuth + 密码登录、JWT(access) + refresh token 落库可吊销、首启建管理员引导、DB 系统设置(注册开关 + GitHub 凭证落库)、LLM 配置增删改查 + 测试连接(OpenAI 兼容)、用户偏好、每用户配额表、管理员后台。后端 API 全链路实测通过，前端 vue-tsc 类型检查通过、生产镜像(64.9MB)托管真实 SPA 实测。
- **阶段 2：市场数据与首页：已完成 ✅**。市场看板（指数/涨幅/热门/板块 + 涨跌家数情绪 + 两市资金流），日线批量同步（已跟踪股票，节流 + data_sync_logs 审计）、完整交易日历回填（上证指数推导开市日 + 补休市日）、市场情绪快照表（去重落库形成历史）。涨跌家数(东财 getTopicZDFenBu)与资金流(东财 fflow/kline)均端到端实测 + fixture 单测；全部后台定时任务与管理员维护端点实测通过。
- **阶段 3：自选股与持仓：已完成 ✅**。自选股分组增删改 + 条目增删改备注/关注原因/重点关注（置顶）+ 实时行情富化；已购入持仓增删改查 + 从自选一键建仓 + 短线/长线分类 + 实时盈亏（成本含买入费税，已平仓算已实现收益）+ 标记卖出与复盘。全部按 user_id 隔离，后端 CRUD/校验/盈亏计算端到端实测、前端 vue-tsc 通过、SPA 托管与鉴权保护实测。
- **阶段 4：AI 分析中心：已完成 ✅**。五个分析模块（个股/全市场/板块/自选股/持仓），**分模块定制的精确提示词**（个股=技术面且明示无基本面数据、全市场=情绪+资金+风格、板块=轮动、自选=清单点评、持仓=组合风险）。数据上下文分级注入 + 软预算裁剪 + 可复现快照；调用用户 LLM 配置（JSON mode 优先，不支持则自动 fallback）；结构化输出校验 + 最多 2 次 repair + 校验始终不过时优雅降级为原文（不写脏结构化数据）；分析历史落库（含 data_snapshot 与 prompt/策略版本号，可复现）；token 统计 + 每用户配额熔断；发起分析限流 20/min。SSRF 防护复用 SafeHTTPClient（仅管理员可触达内网自建模型）。后端 build/vet/test 全绿（含 AI 客户端 mock 端到端、结构化解析、SSRF 拦截、JSON mode 降级、History 列名/用户隔离/配额落库集成测试），前端 vue-tsc + vite build 通过。
- **阶段 5：短线/长线推荐：已完成 ✅**。候选池（自选∪涨幅榜∪活跃榜，去重富化，上限 24）+ **反编造硬约束**（AI 只能从池中选，生成后逐一校验 symbol 必须∈候选池，越池标的一律丢弃、不落库）。短线输出买入观察区间/止盈/止损/有效期(交易日)/失效条件；长线输出基本面逻辑/估值区间/关键指标/复盘周期。每类 3 套策略模板（短线:动量突破/强势回踩/热点活跃；长线:价值低估/成长趋势/龙头优选）。每条含理由/风险/数据依据/免责。生成 3-5 个、结果落库(批次+条目事务)、一键建仓跳持仓预填、配额熔断、限流 15/min、复用阶段4 AI 客户端。后端 build/vet/test 全绿（含反编造过滤/全越池降级/去重/字段归一/DB 隔离与级联删除集成测试），前端 vue-tsc + vite build 通过。
- **阶段 6：推荐追踪与短线卖出提示：已完成 ✅**。新增 `recommendation_status`（与推荐 1:1），复用 `daily_bars` 作价格序列 + 追加当日实时行情，**按当日 high/low 判止盈/止损盘中触达**（取最早触发者，同日双触保守取止损），按 `trading_calendar` 交易日判有效期/过期，以**上证指数为基准**算区间超额收益（alpha）。计算当前收益率、最大涨幅、最大回撤。后台每 2h 刷新（按用户遍历近 90 天成功批次）+ 手动 `POST /recommendations/:id/track`。推荐详情每条附追踪状态与结局徽章（进行中/止盈/止损/过期/跟踪中）+ 复盘提示；`GET /recommendations/performance` 输出历史表现（样本量 n/胜率/均收益/均 alpha/各结局计数）。持仓页短线持仓展示已持有交易日 + 超阈值复盘提示。后端 build/vet/test 全绿（10 个追踪纯计算用例 + upsert 幂等 + performance 统计/用户隔离 DB 集成），前端 vue-tsc + vite build 通过。**已知非 bug**：无 `corporate_actions` 复权表，用原始日线（短周期影响有限，note 标注）；us/hk 无基准源时 alpha 记 0 并说明；配额/追踪均个人自用低频。
- **阶段 7：个人选股增强：已完成 ✅**。四项能力独立提交：①**条件提醒**（`alert_rules`：到价/异动/均线/突破 × ≥/≤，命中按当日 high/low 判盘中触达，once 命中自动暂停，后台每 15min 评估 + 手动立即检查）；②**个股 AI 问答**（`ai_conversations`/`ai_conversation_messages`：首轮固定一份个股数据快照，多轮追问复用不重复拉数，复用 ai_client + 配额 + SSRF）；③**个股横向对比**（多股并排比行情/技术指标，最优值高亮，可选 AI 一句话点评，纯读不落库）；④**今日待办/待复盘**（`TodoService` 聚合命中提醒 + 短线推荐复盘 + 短/长线持仓复盘，按优先级排序、一键跳转处理）。后端 build/vet/test 全绿（4 类提醒命中纯计算 + 交易日聚合排序/隔离 + 会话隔离级联 + 对比指标计算 DB/纯计算测试），前端 vue-tsc + vite build 通过，导航新增 待办/问答/对比/提醒。
- **阶段 8：完整度与可信度增强：进行中（5/9 已完成，其中「管理员后台」系阶段 1/2 既有能力）**。已完成四项新功能、各自提交：①**股票评分系统**（`stock_scores`：趋势/动量/位置/量能/风险 5 维加权 0-100 + 强弱标签，纯技术面量化，集成到横向对比每行）；②**模拟交易**（`paper_*`：虚拟账户默认 10 万，真实行情成交与估值，佣金+印花税，成本基含买入费、卖出算真实净已实现盈亏，可重置）；③**主动推送**（`notify_channels`：Server酱/自定义 webhook，target 加密、SafeHTTPClient 防 SSRF，提醒命中时聚合推送、同日去重、受偏好「开启提醒」总闸控制）；④**Prompt 模板管理**（`prompt_templates`：每用户每模块自定义分析系统提示，启用后覆盖默认 moduleGuidance，可恢复默认；启用时分析记录 prompt_version 标记 `-custom`）。后端 build/vet/test 全绿，前端 vue-tsc + vite（Node16 需 crypto 垫片）通过。**未做（需外部数据源，暂缺）**：新闻情绪（无新闻源）、财务数据详情（需 Tushare）、回测模块；多数据源系统级切换（`data_source_configs` 表已建，管理端未接）。
- **2026-07-02 全项目审查修复 ✅**：分模块审查（基础设施/数据源、用户域/组合域、AI 链路、前端/文档）后集中修复约 40 项——安全（SESSION_SECRET 随机回退+生产判定与 DEBUG 解耦、refresh 轮换防重放、登录计时侧信道、OAuth state cookie 绑定防登录 CSRF、TRUSTED_PROXIES 支持、公开行情限流、熵源失败 fail-fast、Recovery 记堆栈返 500）；数据正确性（成交量统一为手、新浪日线兜底不覆盖成交额、东财行情时间用 f86、指数解析按代码对位、overview 缓存不被取消请求毒化、批量同步游标轮转防饿死）；业务逻辑（止盈/止损判定限有效期窗口内、坏 Low 数据防护、长线 60 交易日复盘提示、短线价位与现价锚定+无效清零、候选池 ST/停牌/流动性前置筛选、推荐条数尊重用户选择、模拟交易行锁+碎股精度、提醒 breakout 剔当日、暂停规则不进待办、前端命中判定与后端对齐）；体验（AI 请求超时放宽至 5 分钟、提交按钮防连击、自选/持仓行内「分析/提醒/问答」快捷入口、分析历史模块筛选、设置页 AI 用量展示、登录跳转带回跳、ECharts 释放）。

## 2026-07-03 体验与闭环增强（7 项） ✅ 全部完成

> 用户提出的批量需求：日报闭环 / 个股详情 / AI 链路加固 / 账号绑定 / 配额口径 / 占位清理 / 默认主题。每项独立 commit，后端 build/vet/test 全绿、前端 vue-tsc + build 零错误。

- **默认主题樱桃红 ✅**：`DEFAULT_THEME_KEY` 改 `light-rose`（已保存主题的浏览器不受影响）。
- **Tushare 占位隐藏 ✅**：首页「财经新闻 / AI 今日观点」占位卡撤下；DATA_SOURCES.md §7 集中登记「待 Tushare/新闻源解锁」清单（财务详情/财报日历/复权因子/回测/新闻情绪），约定接入前 UI 不留占位。
- **配额次数制 ✅**：`user_quota` 改 action_limit/action_used 熔断（一次手动动作=1 次，repair/panel 多轮请求不重复计，后台任务不计次），token_used/request_count 转审计；四入口统一走 `service/quota.go`；admin 端点与设置页/后台弹窗同步。
- **GitHub 绑定 ✅**：`POST/DELETE /api/user/github/bind`，设置页账号安全区绑定/解绑（github_id 占用校验、纯 OAuth 账号禁解绑防锁死）；绑定与登录共用回调页（sessionStorage 标记 + 路由守卫放行）。**答疑**：绑定前密码登录+GitHub 登录确实是两个账号（注册开放时）；普通用户无密码注册入口，GitHub OAuth 是唯一注册途径。
- **个股详情页 ✅**：`/stocks/:market/:symbol`（行情头/日K/估值快照/技术评分五维条 + 分析/问答/对比/提醒/逻辑卡/自选/建仓快捷动作）；首页涨幅榜/热门榜行与速查按钮可点击进入；`useStockActions.goDetail` 全站复用；api/market.ts 补 `getScore`。
- **AI 请求加固（参考 new-api）✅**：ai_client 连接池复用（allowPrivate 两态包级 client）、瞬时错误单次重试（未达上游网络错误 + 429/500/502/503，504 不重试，退避 800ms，SSRF 拦截不重试）、状态码中文归类提示、usage 缺失字符粗估兜底；新增 3 个 httptest 用例。
- **收盘日报 ✅（最大项）**：新表 `daily_reports`（user+trade_date 唯一）+ 偏好 `enable_daily_report`（默认关）；`StartDailyReportJobs` 交易日 15:35~20:00 每 10min 检查补生成（交易日历判定、无日历回退周一~五、失败落 failed 防反复烧 token、atomic 防重入）；复盘=市场概览+持仓当日涨跌+自选异动 top8+今日命中提醒 快照 → LLM 结构化总结（1 次 repair，快照落库可复现）；明日推荐=复用推荐域 `GenerateAuto`（短线买点/止盈/止损/有效期，不计次）；**卖点提醒**=每条推荐自动建止盈/止损到价规则（note「收盘日报 <date>」，21 天未命中自动 paused，重生成先清当日旧规则）；生成后按推送总闸推送摘要。前端 `/daily-report` 页（历史下拉/手动重生成限流 5/min）+ 首页「AI 今日观点」卡接最新日报 + 设置页开关；导航入「AI 研究」组。

## 2026-07-02 无新数据源功能扩展（B 档落地 + 免费估值源），批次 A~J ✅ 全部完成（2026-07-03 收官）

> 本轮目标：不接付费数据源，把 REFERENCE_PROJECTS.md 借鉴点 + 待办储备 B/C 档中无外部依赖的功能全部落地，并利用**腾讯行情免费自带的估值字段**（qt.gtimg.cn 行情串 38~53 号字段：换手率/PE-TTM/振幅/流通与总市值/PB/涨停价/跌停价/量比/PE 动静）补上估值维度。共 10 个批次，每批次一个 commit，后端 go build/vet/test 全绿、前端 npm run build 零错误。

**批次清单（全部完成）：**

- **批次 A ✅（`4ad4943`）估值数据地基**：`datasource.Valuation` + `ValuationProvider` 能力接口（腾讯实现，fixture 单测）；`MarketService.GetValuation`（60s 缓存）+ `ValuationsFor` 并发批量；公开端点 `GET /markets/:market/stocks/:symbol/valuation`；接入横向对比（PE/PB/市值/换手/量比行 + ST 标记 + AI 点评喂估值）、个股分析与问答快照（`valuation` 块 + `freshness_note` 数据新鲜度标注，个股 moduleGuidance 加估值水位维度，**prompt_version p1→p2**）、推荐候选池富化（pe_ttm/pb/total_cap/turnover_rate，长线 spec 同步，**rec prompt p1→p2**）；前端对比表 3 行 + Home 个股速查估值 6 格。
- **批次 B ✅（`306f6b0`）投资逻辑卡片**：`thesis_cards`（user+symbol+market 唯一；thesis/key_evidence/risks/kill_switches/track_metrics/next_review_date；状态机 active|invalidated|archived 失效带原因）；到期进今日待办（`thesis_due`，pri 3）；一键体检 `GET /thesis-cards/checkup`（行情富化 + 近20日回撤≤-15% 警示 + 到期标记）；前端 ThesisCards.vue（`/thesis`）+ 自选/持仓行内「逻辑卡」入口 + 导航「更多」组。
- **批次 C ✅（`a7a9f0c`）投资笔记**：`research_notes`（可选绑定标的，kind=decision/review/idea/event）；CRUD + 标的过滤 + 关键字搜索；前端 Notes.vue（`/notes`）时间线，深链 `?symbol=`/`?add=1`。
- **批次 D ✅（`0712a8d`）机会池漏斗 + 错过机会**：WatchlistItem 加 `research_stage`（discovered→screening→watching→waiting_price→planned→bought/passed/reviewed）+ passed_reason/passed_price/stage_at；`PUT /watchlist-items/:id/stage`（转 passed 记当时现价）；`GET /watchlist-items/missed` 错过机会复盘（放弃价 vs 现价 ±5% 判 avoided_loss/missed_gain/neutral，纯函数 missedVerdict）；前端自选行阶段标签 popselect 流转 + 「错过机会」弹窗。
- **批次 E ✅（`4b0880e`）风险计划 + 检查清单 + 结构化复盘**：Position 加 plan_stop_loss/plan_take_profit/checklist_json + sell_planned/ai_verdict/lesson_learned；校验止损<买价<止盈；前端建仓弹窗实时风险计算（投入/触发止损亏损额与占比/盈亏比）+ 5 项买入前检查清单（勾选随持仓落库），平仓弹窗三维结构化复盘。
- **批次 F 后端 ✅（`abdd7c4`）组合总览/风控/分析时效**：PositionView 富化 near/below_stop_loss（3% 阈值）+ last_analyzed_at/analysis_stale（>7 天，一次分组查询）；`PositionService.Overview`（总市值/成本/盈亏/已实现/盈亏仓数/短长线分布/最大单一持仓占比/信号：集中度>40%、破/近止损、未分析计数）；`GET /positions/overview`；待办加 `stop_loss` 类型（pri 1）。

**待做（新会话从这里继续，按批次顺序）：** —— 已无待做项，以下为各批次落地记录。

- **批次 F 前端 ✅（`1504bfe`）持仓页收尾**：api/position.ts 加 PortfolioOverview/getPortfolioOverview + 富化字段；Positions.vue 汇总区接 /positions/overview（已实现盈亏/短长线分布/最大持仓占比 + signals 警示条）；持仓行内破止损（error）/近止损（warning）/N 天未分析 tag；Today.vue kindMeta 加 stop_loss。
- **批次 G ✅（`934aef7`）推荐↔持仓血缘 + 追踪节点 + 回避规则 + 落选理由**：Position 加 `recommendation_id`（一键建仓带 `&rec_id=` 落血缘，推荐详情回显「已建仓」+ 推荐价 vs 实际买价对比）；RecommendationStatus 加 `return_7d/14d/30d` 节点收益（evaluateTracking 按第 N 交易日收盘记录，performance 统计各节点均值）；UserPreference 加 `blacklist_json` 黑名单与 `min_candidate_amount` 流动性门槛（候选池 candidateEligible 应用，Settings.vue 偏好区管理）；推荐 prompt 输出 `rejected:[{symbol,reason}]` 落 batch.rejected_json，前端「为什么没选它」折叠块。
- **批次 H ✅（`1286a59`）提醒命中明细/状态机 + 放量振幅类型 + 待办完成**：新表 `alert_events`（unread/read/dismissed 状态机，命中同日去重落明细，删规则时未读事件转 dismissed）；`GET /alerts/events?status=` + `PUT /alerts/events/:id/status` + `PUT /alerts/events/read-all`；AlertRule kind 加 `volume_surge`（当日量≥N 倍 20 日均量，均量窗口剔当日）与 `amplitude`（优先腾讯估值源振幅、缺则 (high-low)/prev_close 回退）；TriggeredForUser 改基于 unread 事件，待办 alert 条目 RefID 换事件 id、可就地已读/忽略；Alerts.vue「命中历史」区（状态筛选/全部已读）。
- **批次 I ✅（`fddae75`）AI 增强**：分析 schema 加 `anti_thesis/kill_switches/unknowns`（moduleGuidance 五模块各加反方视角要求，**prompt_version p2→p3**，前端反方观点 warning 色卡三块）；`GET /analysis/:id/diff` 变化检测（同 user+module+market+symbol、sector 加 target 的上一份 success 对比 rating/confidence/summary/highlights/risks 差异，前端「与上次对比」弹窗）；`mode=panel` 多角色观点（仅 stock：technical/momentum/risk/contrarian 四角色独立评级+共识+分歧，rating 多数投票，落 result_json.panel，AnalysisRecord 加 mode 列，前端四角色卡）；QaAskRequest 加 `analysis_record_id` 复用分析快照（校验归属，分析结果头「继续问答」深链）。
- **批次 J ✅（本提交）导出/导入/配额/同步日志/备份文档**：`GET /api/export/:kind`（positions/watchlist/recommendations/analyses，CSV 带 UTF-8 BOM，限流 10/min，Settings.vue「数据导出」四按钮）；`POST /api/positions/import`（multipart，模板列 symbol,market,type,buy_price,buy_date,quantity,buy_fee,buy_tax,reason，逐行校验+错误行报告+上限 500 行+超限整体拒绝，Positions.vue 导入弹窗+模板下载）；`GET/PUT /api/admin/users/:id/quota`（调整 token_limit/清零已用量，AdminSettings.vue 用户行「配额」弹窗）；AdminSettings.vue「数据源同步日志」区（接现有 sync-logs）；DEPLOYMENT.md 补「数据备份与恢复」节（用户数据表 vs 可重建行情缓存表清单 + mysqldump/恢复 + 密钥一致性）；ARCHITECTURE/DATABASE_DESIGN 同步新端点与新表列。
- **收尾核对 ✅**：`go build/vet/test` 全绿；`npm run build` 零错误；ROADMAP 本区块勾稿 + 记忆更新。**浏览器目验 6 主题（至少 1 亮 1 暗，含 /thesis /notes）仍欠人工确认**。

- **下一步：阶段 8 剩余项按数据源可得性推进**，或进入待办储备 B 档（逻辑卡片/买入前检查清单/仓位风险计算器等，多数复用现有结构、无外部依赖）。
- **当前实现边界（代码审查后补充）**：
  - 数据市场：当前数据适配器只支持 A 股 `cn`（仅沪深，不含北交所 4/8 开头代码）；前端业务页已暂时只暴露 A 股选项。美股/港股仍是产品可扩展方向，需接入对应数据源后再开放。
  - AI 分析结构：当前后端结构化字段为 `rating/confidence/summary/highlights/risks/opportunities/suggestions/disclaimer`，与 PRD 中更完整的 `market_context/data_points/recommendations/next_watch_points` 属于目标态差异；扩字段前需同步后端 schema、前端展示和历史兼容。
  - 追踪复权：`daily_bars` 主源为东财**前复权**（fqt=1，以最新价重锚），除权除息后历史序列会整体重刷、与生成时点的 RefPrice/止盈止损快照价错位（追踪 note 已如实标注）；新浪日线兜底无复权参数，两源口径不保证一致。彻底解决需复权因子表（`corporate_actions`）。
  - 新闻/财务/宏观：当前分析、问答、对比、推荐均已要求模型不得虚构未提供数据；新闻情绪、财务详情、财报提醒仍依赖后续数据源。
  - 短线计划：已在提示词和后处理里加入 A 股 T+1、涨跌停、100 股一手、交易日有效期约束；仍未做完整订单可执行性模拟。
  - 推荐↔持仓血缘：~~已解决（批次 G）~~ `positions.recommendation_id` 已落地，一键建仓带 `rec_id` 落血缘，推荐详情回显「已建仓」与推荐价 vs 实际买价对比。
  - 提醒状态机：~~已解决（批次 H）~~ `alert_events` 命中明细表与 unread/read/dismissed 状态机已落地，待办可标记完成；规则行仍保留最近命中快照（同日去重依据）。
  - 审计与调用日志：`audit_logs`、`ai_call_logs` 未建（个人自用降级）；~~配额为 token 终身累计~~ **2026-07-03 已标准化为次数制**（action_limit/action_used：一次手动动作计 1 次、内部多轮请求不重复计、后台任务不计次；token 仅审计），仍无自动周期重置，管理员可经 `PUT /api/admin/users/:id/quota` 调整上限并手工清零。
  - LLM：单一默认配置（无「分析/推荐」双默认）；`stream` 字段存而未用（当前恒非流式）；仅 OpenAI 兼容 provider。

> 进度标记约定：每完成一个阶段，更新本区块 + 给对应阶段标题打 ✅，便于新会话快速恢复。

## 阶段 0：项目初始化 + 数据源验证 ✅ 已完成

目标：建立可运行的全栈骨架，并**先证明能稳定拿到真实、足够实时的行情数据**——这是全项目最大的不确定性，必须最先验证。

任务：

- 初始化 Go 后端工程。
- 初始化 Vue 3 + Vite + TypeScript 前端工程。
- 建立 `docs/`、`server/`、`web/`、`deploy/` 等目录。
- 配置 Docker Compose。
- 配置数据库连接。
- 配置 Redis，允许本地可选。
- 建立基础日志和错误返回格式。
- **设计数据适配/标准化层接口（DataSourceAdapter）**，定义内部标准结构（Quote / Bar / Fundamental / News）。
- **接入一个公开数据源、打通一个市场的行情**：获取 → 标准化 → 入库（`stocks` / `stock_quotes` / `daily_bars`）→ 缓存 → 前端展示，端到端跑通。

验收：

- 后端健康检查接口可访问。
- 前端开发服务器可启动，能调用后端健康检查接口。
- **能从真实数据源拉到一个市场的实时/准实时行情并在前端展示，数据带来源与更新时间。**
- 数据源失败时有明确日志与前端提示。

> 这一步如果走不通（数据拿不到、不实时、不稳定），后面所有 AI 分析/推荐/追踪都建立在空中，应先解决数据问题再继续。

## 阶段 1：用户与设置 ✅ 已完成

目标：完成登录、用户持久化和 LLM 配置。

> **实际实现要点（落地后补充）**：
> - **双登录方式**：用户名+密码（bcrypt，主要给管理员）+ GitHub OAuth。
> - **首启引导**：系统无用户时走 `/setup` 创建首个管理员（密码方式，解 GitHub 凭证"鸡生蛋"问题——凭证要登录后台才能配）。第一个账号强制 admin。
> - **GitHub 凭证落库**：client_id/secret 存 DB 系统设置（`options` 表，secret 经 AES 加密），管理员后台可配；env 的 `GITHUB_CLIENT_ID/SECRET` 仅作首启种子。**与原计划"凭证只走 env"不同**。
> - **JWT**：access token（HS256，2h，无状态）+ refresh token（落 `refresh_tokens` 表，存 sha256、可吊销、换发时轮换）。`/api/auth/refresh` 换发、`/api/auth/logout` 吊销、禁用用户即吊销其全部令牌（强制登出）。
> - **OAuth 流程**：前端 `/login/callback` 回调页用 code 换 token；state 用 HMAC 无状态签名防 CSRF。GitHub OAuth App 回调地址填 `<站点>/login/callback`。
> - **测试连接**：仅 OpenAI 兼容（`{base_url}/chat/completions` 最小请求），provider 留分支口子。
> - **注册策略**：后台可开关 `registration_open`；关闭时仅已存在账号可登录。

任务：

- GitHub OAuth 登录。
- JWT 鉴权。
- refresh token 落库，支持吊销/强制登出。
- 用户表和用户偏好表。
- LLM 配置增删改查。
- LLM API Key 加密保存（主密钥独立管理）。
- LLM 测试连接。
- 用户偏好设置：风险等级、默认市场、默认周期、默认推荐数量。
- 每用户 token 配额表（成本控制基础）。

验收：

- 用户可以使用 GitHub 登录。
- 登录后可以保存 LLM 配置。
- 前端不会显示明文 API Key。
- 测试连接结果可以展示成功或失败原因。

## 阶段 2：市场数据与首页 ✅ 已完成

> 进度：市场首页已从验证页升级为**市场看板**（指数概览/涨幅榜/热门榜/板块榜/涨跌家数情绪/两市资金流/个股速查 + 新闻/AI 占位）。指数/榜单/个股走新浪（稳），板块走东财 clist（间歇限流，best-effort），**涨跌家数走东财 getTopicZDFenBu、资金流走东财 fflow/kline（均单调用、实测可用，限流时优雅降级）**。**已补全**：①个股日线查询触发写入 `daily_bars`；②「已跟踪股票」日线批量同步（后台每 6h + 管理员手动触发，节流 300ms、并发互斥、写 `data_sync_logs` 审计）；③完整交易日历（用上证指数日线推导开市日，再回填休市日 is_open=false，非仅开市日）；④市场情绪快照表 `market_snapshots`（后台每 10min 落库、相邻相同则去重）。管理员维护端点 `/api/admin/market/{sync-bars,backfill-calendar,snapshot,sync-logs}`。**未做（属后续阶段）**：全 A 股 5000 只普查（个人自用只覆盖用户关心标的，避免长时间打免费源）。

目标：在阶段 0 已打通单源行情的基础上，扩展数据维度并完成首页看板。

任务：

- 扩展股票基础信息与行情（指数、板块、热门股票）。✅
- 市场快照表与市场首页接口。✅（`market_snapshots` 涨跌家数情绪快照 + `GET /markets/:market/overview`）
- 日线 OHLC（`daily_bars`）入库，供后续追踪复用。✅（个股查询触发入库 + 已跟踪股票批量同步，后台 6h/管理员手动，`data_sync_logs` 审计）
- 交易日历（`trading_calendar`）入库。✅（上证指数日线推导开市日 + 回填休市日 is_open=false，完整日历）
- 首页图表和榜单。✅
- 数据更新时间展示。✅
- 数据源异常提示。✅（单块失败降级 + 前端「数据源繁忙」标签）

验收：

- 首页可以展示指数、板块、热门股票和市场摘要。
- 行情数据有缓存。
- 数据更新时间清晰可见。

## 阶段 3：自选股与已购入持仓 ✅ 已完成

目标：完成用户个人股票管理。

> **实现要点（落地后补充）**：标的用 `symbol+market` 自然键建模（不依赖 stocks 表主键，与数据源/行情一致，冗余 name 便于无行情时展示）；盈亏在读取时用实时行情计算、不落库快照（展示始终最新，成本含买入费税，已平仓算已实现收益扣买卖全部费税）；列表用并发 `QuotesFor`（缓存复用、并发上限 8）富化现价。所有查询/变更按 user_id 隔离。

任务：

- 自选股分组。✅（`watchlists`，增删改 + 默认分组）
- 自选股添加、删除、编辑备注。✅（`watchlist_items`，note/focus_reason；唯一约束防同组重复；添加时行情校验代码+取名）
- 标记重点关注。✅（is_pinned，置顶排序，一键切换）
- 已购入持仓增删改查。✅（`positions`）
- 从自选股创建持仓。✅（自选「建仓」跳持仓页预填）
- 记录买入价、买入日期、数量、短线/长线类型。✅（含买入/卖出手续费与税费）
- 持仓列表显示当前盈亏。✅（成本/市值/盈亏额/收益率，实时现价）
- 支持标记已卖出和填写复盘。✅（平仓填卖出价/日期/费税/原因/复盘，算已实现收益）

验收：

- 自选股和持仓数据按用户隔离。✅
- 用户可以将股票标记为已购入。✅
- 持仓页面可以展示短线和长线分类。✅（类型筛选 + 类型标签）

## 阶段 4：AI 分析中心 ✅ 已完成

> **实现要点（落地后补充）**：五模块（market/sector/stock/watchlist/position），**系统提示词严格分模块**（`service/analysis.go` 的 `moduleGuidance` 映射），每个模块的分析维度与实际注入的数据字段一一对应，个股明确声明「无财务/新闻数据、以技术面为主、不得虚构基本面」。数据上下文组装在 `service/analysis_context.go`（个股含 MA5/10/20、区间高低、近 5/20 日涨跌、近 30 根日线明细；软预算 8000 字符，超限先丢逐日明细再截断列表）。AI 调用 `service/ai_client.go` 走 OpenAI 兼容 `/chat/completions`，`response_format=json_object` 优先、服务端不支持（4xx 命中关键字）则去掉重试；复用 `common.SafeHTTPClient` 防 SSRF（仅 admin 放行内网）。结构化校验 `parseAnalysisResult`（容忍代码块包裹、中文枚举归一、confidence 钳制、数组兜底、disclaimer 回退），失败最多 repair 2 次，仍不过则降级存原文（status=degraded，不伪造结构化字段）。落库 `analysis_records`（重字段 result_json/data_snapshot 列表查询用 `Select` 排除）。配额在 `user_quota` 累计 token/request，`TokenLimit>0 且已用尽`则熔断（0=不限）。

目标：完成可配置的 AI 分析。

任务：

- 分析请求接口。✅（`POST /api/analysis`，限流 20/min）
- 市场、板块、个股、自选股、持仓等分析模块。✅（五模块，分模块提示词）
- 组装数据上下文（含上下文 token 预算与分级注入）。✅（`analysis_context.go`，软预算 + 分级裁剪）
- 调用用户 LLM 配置（function calling / JSON mode 优先，不支持则 fallback）。✅（JSON mode + 自动 fallback；function calling 暂未用，JSON mode 已足够）
- 结构化输出校验 + 有限次 repair 重试 + 优雅降级。✅（校验 + 2 次 repair + 降级存原文）
- 保存分析历史（含 data_snapshot 与 prompt/策略/评分版本号）。✅（`data_snapshot` + `prompt_version`/`strategy_version`）
- AI 调用日志和 token 统计、每用户配额熔断。✅（token/request 落 `user_quota`，额度用尽熔断）
- 前端分析中心页面。✅（`Analysis.vue`：模块/标的/LLM 选择 + 结构化结果展示 + 历史 + 详情复现）

验收：

- 用户可以选择模块发起 AI 分析。✅
- 分析结果持久化，且可凭版本号复现。✅（快照 + 版本号落库，详情可回看）
- 分析历史可查询。✅
- 失败时能看到明确错误；结构化校验失败时优雅降级而非写脏数据。✅

## 阶段 5：短线/长线推荐 ✅ 已完成

> **实现要点（落地后补充）**：**反编造是本阶段验收核心**——候选池由真实数据构建（`RecommendationService.buildCandidatePool`：自选∪东财/新浪涨幅榜∪活跃榜，按 symbol 去重、保序自选优先、上限 24），落库 `candidate_pool`；LLM 只被允许从池中选，生成后 `parseAndFilterPicks` 逐一校验 `symbol ∈ 候选池`，越池/杜撰标的直接丢弃（不落库不展示），全越池则触发 repair、仍无效则降级。短线/长线**分模块精确提示词**（`shortTermSpec`/`longTermSpec`），字段与前端展示严格对应。策略模板 `shortStrategies`/`longStrategies`（各 3 套，含内部 guide 注入 prompt、对外清单剥离 guide）。复用阶段4 AI 客户端（`chatCompletion` JSON mode + fallback + SafeHTTPClient 防 SSRF）、配额（共用 `user_quota`）。批次+条目事务落库，`Recommendation.RefPrice` 存生成时现价（供阶段6 追踪基准）。前端 `Recommendations.vue`：类型/策略/市场/数量/LLM 选择 → 卡片展示（短线关键价位 or 长线估值/指标）+ 理由/风险/依据/免责 + 一键建仓跳 `/positions?add=1&...`。

目标：实现核心推荐能力。

任务：

- 策略模板。✅（短线 3 套 + 长线 3 套，`StrategiesFor` 暴露、`GET /api/recommendations/strategies`）
- 候选池筛选。✅（自选∪涨幅榜∪活跃榜，真实数据，上限 24）
- 推荐生成接口。✅（`POST /api/recommendations`，限流 15/min）
- 短线推荐输出买入观察区间、止盈、止损、有效期、失效条件。✅
- 长线推荐输出基本面逻辑、估值区间、关键指标、复盘周期。✅（数据缺财报时如实说明局限）
- 推荐结果保存。✅（`recommendation_batches` + `recommendations`，事务）
- 从推荐一键加入已购入持仓。✅（跳持仓页预填）

验收：

- 用户可以选择短线或长线推荐。✅
- 系统推荐 3 到 5 个标的。✅（count 钳制 3-5）
- 推荐结果必须包含理由、风险、数据依据和免责声明。✅（reason/risks/evidence/disclaimer 强制字段）
- 推荐股票来自候选池，不允许 AI 无依据编造。✅（生成后逐一校验∈候选池，越池丢弃，单测覆盖）

> 注：长线推荐的财务深度依赖 Tushare 低 cost 档（2000 分，少量捐赠），按需启用、非本阶段前置。未启用时降级为东财实时估值轻量版（PE/PB/市值 + 部分实时指标 + 行情 + 新闻）。第一阶段始终以东财 + 新浪为主源。Tushare 高级档（5000 分，分钟线/融资融券明细等）暂不实现。
> **本次实现的数据现实**：当前候选池仅带实时行情（price/change_pct/amount），未接 PE/PB/财务；长线 prompt 已要求「缺财务时如实说明、不臆测」。接 Tushare 后可增强长线候选池的基本面维度。

## 阶段 6：推荐追踪与短线卖出提示 ✅ 已完成

> **实现要点（落地后补充）**：新增 `RecommendationStatus`（与 `Recommendation` 1:1，`recommendation_id` 唯一）。价格序列复用 `daily_bars`（`TrackingService.symbolBarsAfter` 拉取并落库，仅取推荐日之后的日线）+ 追加当日实时行情为一根 bar（用于盘中触达与最新价刷新）。**纯函数 `evaluateTracking`**（可测）：止盈/止损按当日 high/low 判断、取最早触发者（同日双触保守取止损）；未触发且超有效期（按 `trading_calendar` 交易日）则过期；长线仅 `tracking` 不做价格触发。计算 当前收益率/最大涨幅/最大回撤（相对追踪期峰值）/基准区间收益/超额收益 alpha（基准 = 上证指数 sh000001，新增 `datasource.BenchmarkBarsProvider` + sina 实现 + manager 路由）。后台 `StartTrackingJobs`（启动延迟 90s 一次 + 每 2h，遍历近 `trackWindowDays=90` 天成功批次的用户）；手动 `POST /api/recommendations/:id/track` 刷新单批。`Performance` 聚合表现（样本量 n、胜率、均收益、均 alpha（仅基准有效样本）、各结局计数）。删除批次级联删 status。持仓页短线状态提示：`PositionView.HeldTradeDays`/`ShortTermReview`（短线持有超 `shortHoldReviewDays=10` 交易日提示复盘），交易日经 `countOpenTradeDaysAfter`（trading_calendar）计算。

目标：补齐推荐后的闭环。

任务：

- 推荐当前状态表（`recommendation_status`，1:1）。✅（`RecommendationStatus`，`recommendation_id` 唯一，upsert 幂等）
- 复用日线 OHLC（`daily_bars`）作为价格序列，并结合 `corporate_actions` 复权。✅ 价格序列复用；⚠️ 复权待办（暂无 `corporate_actions` 表，用原始日线，短周期影响有限，note 标注）
- 定时更新当前价、最高价、最低价（复权后）。✅（后台每 2h + 手动触发；未复权）
- 计算当前收益率、最大涨幅、最大回撤、相对基准的超额收益（alpha）。✅
- 止盈/止损按当日 high/low 判断，避免漏判盘中触达。✅（`evaluateTracking` 遍历 bar high/low）
- 判断止盈、止损、过期（按交易日）、需要重新分析。✅（结局 active/take_profit/stop_loss/expired/tracking/no_data + review_needed）
- 持仓页展示短线状态提示。✅（已持有交易日 + 短线超阈值复盘提示）
- 推荐历史页展示表现统计（带样本量 n）。✅（`GET /recommendations/performance`：n/胜率/均收益/均 alpha/结局计数）

验收：

- 用户查看短线推荐或短线持仓时，能看到是否该复盘、是否触发止盈/止损、是否过期。✅
- 系统能展示 AI 推荐历史表现，包含相对基准超额收益与样本量。✅
- 不做主动推送也不影响查询提示。✅（仅刷新状态供查询，无推送）

## 阶段 7：个人选股增强（A 档）✅ 已完成

> **实现要点（落地后补充）**：四项能力独立成 commit。①**条件提醒**：`model/alert.go` AlertRule（kind=price/pct_change/ma/breakout × op=gte/lte，once 命中自动暂停）；纯函数 `evaluateAlert` 按当日 high/low 判盘中触达（`service/alert.go`），后台 `StartAlertJobs` 每 15min 评估有 active 规则的用户 + 手动 `POST /alerts/evaluate`；命中落 status=triggered，不推送。②**个股 AI 问答**：`model/conversation.go` AiConversation/AiConversationMessage；首轮 `buildStockSnapshot`（从个股分析重构出的共用快照）落库，后续追问复用快照 + 最近 12 条历史，不重复拉数；复用 ai_client(JSONMode=false)/配额/SSRF/隔离。③**横向对比**：`service/compare.go` 并发采集 2~6 只的行情+技术指标（复用 `movingAverage`），最优值高亮，可选 AI 一句话点评；纯读不落库。④**今日待办**：`service/todo.go` TodoService 聚合命中提醒 + 阶段6 `review_needed` 短线推荐 + 短线持仓超阈值(`shortHoldReviewDays=10`) + 长线持仓超 `longHoldReviewDays=60`，按优先级排序（止损/命中最急），RefType/RefID 供一键跳转。前端新增 Alerts/Qa/Compare/Today 四页 + 导航 待办/问答/对比/提醒。

目标：补齐贴合"个人选股"高频动作的功能，多数复用已有数据结构。

任务：

- 条件提醒（3.16）：`alert_rules`，到价/均线/突破/异动，进页面高亮，命中按交易日与 OHLC 判断。✅（4 类 × ≥/≤，当日 high/low 盘中触达判定，命中落库供待办聚合）
- 个股 AI 问答（3.17）：`ai_conversations` / `ai_conversation_messages`，复用已存数据快照做多轮追问。✅（首轮快照固定、多轮复用不重复拉数）
- 个股横向对比（3.18）：多股并排对比 + AI 一句话点评。✅（行情+技术指标对比 + 可选 AI 点评；财务维度待 Tushare，暂缺）
- 今日待复盘 / 待办清单（3.19）：汇总止盈止损过期、长线复盘触发、命中提醒。✅（聚合提醒+推荐+持仓复盘信号，一键跳转；临近财报待财报数据源，暂缺）

验收：

- 用户可设置提醒条件，命中时看到提示。✅（提醒页命中高亮 + 待办页聚合）
- 用户可对某只股票多轮追问，无需重复拉数据。✅
- 用户可一次对比多只候选股。✅（2~6 只，最优值高亮 + AI 点评）
- 待办页能列出"今天该看的"并一键跳转处理。✅

> 注：横向对比的财务/估值维度与待办的"临近财报"依赖 Tushare 财务数据，未接入前分别降级为行情+技术指标对比、以及不含财报提醒；prompt/展示已如实标注数据边界。

## 阶段 8：完整度与可信度增强（进行中）

> **实现要点（落地后补充）**：本阶段按「无外部数据源依赖优先」推进，已完成四项，各自成 commit。①**股票评分**：`model/score.go` + `service/score.go` 纯函数 `computeScore`（趋势/动量/位置/量能/风险五维加权 0-100，复用 `movingAverage`/`changeOverN`），快照 `stock_scores`（symbol+market+交易日唯一），`GET /markets/:market/stocks/:symbol/score`，并内联进横向对比每行。②**模拟交易**：`model/paper.go` + `service/paper.go`，PaperAccount/PaperHolding/PaperTrade，佣金万2.5(最低5)+A股卖出印花税万5，AvgCost decimal4 含买入费、卖出算净已实现盈亏、清仓删持仓，事务保证一致，可重置。③**主动推送**：`model/notify.go` + `service/notify.go`，Server酱/webhook（target 加密、`SafeHTTPClient` 禁内网防 SSRF），`AlertService.evaluateRules` 命中时聚合本轮新命中、同日去重、异步 best-effort 推送。④**Prompt 模板**：`model/prompt.go` + `service/prompt.go`，每用户每模块唯一模板，`analysisSystemPrompt(userID, module)` 启用时用 `userPromptOverride` 覆盖默认 `moduleGuidance`，`Modules()` 暴露默认指引供参照/恢复。

目标：提升产品完整度和可信度。

任务：

- 股票评分系统。✅（5 维加权技术面评分 + 快照 + 集成对比）
- 新闻情绪分析。⏳ 待新闻数据源（当前无稳定新闻源；**首页「财经新闻 / AI 今日观点」占位卡已于 2026-07-03 隐藏**，待接清单见 DATA_SOURCES.md §7）
- 财务数据详情。⏳ 待 Tushare（PE/PB/三表）
- 模拟交易。✅（虚拟账户 + 真实行情成交估值 + 费用/印花税 + 已实现盈亏 + 重置）
- 主动提醒（Server酱等推送通道，在阶段 7 条件提醒之上扩展）。✅（Server酱/webhook，命中聚合推送、同日去重、加密防 SSRF）
- 回测模块。⏳ 后续（可复用 daily_bars，工程量较大，暂缓）
- 管理员后台。✅（阶段1 已具备用户/设置管理；阶段2 已加市场数据维护端点）
- 多数据源配置。◻ 部分（DataSourceConfig 表已建，管理端切换待补）
- Prompt 模板管理。✅（按模块自定义系统提示，覆盖默认、可恢复）

验收：

- 推荐表现可以量化。✅（阶段6 performance + 本阶段评分 + 模拟交易盈亏）
- 用户可以复盘 AI 推荐质量。✅（阶段6 历史表现 + 追踪结局）
- 管理员可以维护系统级数据源和模型配置。◻ 用户级 LLM 配置已具备；系统级多数据源切换待补。

> 说明：新闻情绪 / 财务详情 / 回测 依赖尚未接入的数据源或较大工程量，本阶段先完成无外部依赖的四项；其余按数据源可得性与优先级后续推进。

## 待办储备（B / C 档，现有功能完成后再评估）

以下功能价值已确认，但优先级低于上述阶段，待 P0/P1 + 阶段 7 完成后再排期：

**B 档（提升决策质量）**

- 决策日志 / 投资笔记：独立于持仓的自由笔记，带股票标签与时间线，用于复盘"当时为什么这么想"。
- 投资逻辑卡片：为自选股、候选标的、持仓保存结构化研究假设，包括关注原因、核心逻辑、失效条件、跟踪指标、下次复盘日期、AI 观点变化。
- 反方观点 / 否决理由：每次 AI 分析强制输出"为什么不该买/不该继续关注"、最大错误来源、放弃条件、同业替代标的，避免只顺着用户观点强化结论。
- 变化检测：同一股票再次分析时，自动对比上次报告，突出价格、估值、财务、新闻、公告、AI 结论、原投资逻辑的变化。
- 机会池漏斗：在自选股和持仓之外增加研究进度状态，如发现、初筛、重点观察、等待价格、已生成计划、已买入、已放弃、已复盘。
- 买入前检查清单：从候选标的转为持仓前，检查买入理由、失效条件、最大可能亏损、行业集中度、财报/重大事件、市场环境等。
- 仓位风险计算器：根据买入价、止损价、计划投入金额估算触发止损时的亏损金额、占总资金比例、组合集中度影响。
- 错过机会记录：记录看过但未买入的标的，复盘当时未买原因、后续走势、是正确回避风险还是错过机会。
- 卖出后复盘模板：结构化记录是否按计划卖出、盈亏结果、卖出原因、AI 原判断对错、下次策略调整点。
- 自选/持仓"逻辑是否还成立"批量体检：一键轻量检查原推荐逻辑是否仍在、是否触发风险、是否需重新分析。
- 行业/板块自上而下入口：行业 → 龙头/候选 → 对比 → 分析的选股路径。
- 研究证据包：每份 AI 报告关联行情数据、财务指标、新闻链接、公告摘要、用户笔记和 AI 引用数据点，形成可回看的研究档案。
- 财报季工作流：财报前生成关注问题，财报后总结营收、利润、毛利率、指引变化，并判断长期逻辑是否变化。
- 多模型交叉观点：多个 LLM 分别承担基本面、技术面、风险审查、反方分析角色，最后输出共识与分歧。
- 数据导出 / 持仓 CSV 导入。

**C 档（锦上添花）**

- 组合总览：总市值、总盈亏、行业/市场分布、最大持仓占比（复用多币种与成本字段）。
- 组合暴露分析：分析行业、市场、短线/长线、单股集中度、高相关持仓，以及组合偏成长、价值、防御或周期的风格暴露。
- 财报日历联动：自选/持仓临近财报标记（复用交易日历）。
- AI 推荐"为什么没选它"：对候选池落选标的给一句话理由。
- 候选标的黑名单 / 回避规则：用户可配置 ST、亏损股、小市值、高负债、指定行业、历史亏损严重标的等回避条件，候选池筛选时自动排除并说明原因。
- 每日研究摘要：首页聚合当天该关注的自选异动、持仓风险、逻辑卡复盘、财报临近、候选池进入观察区间等事项，而不是每天生成新推荐。
- 个人风控提示：基于持仓的集中度/相关性提醒。

## 优先级建议

P0：

- 单源单市场行情端到端打通（数据验证）
- GitHub 登录
- 用户持久化
- LLM 配置
- 自选股
- 已购入持仓
- AI 分析
- 短线/长线推荐
- 推荐历史

P1：

- 市场首页完整看板
- 推荐追踪
- 短线卖出提示
- 分析历史
- 策略模板

P2：

- 个人选股增强（条件提醒、个股 AI 问答、横向对比、待复盘清单）
- 股票评分
- 新闻情绪
- 模拟交易
- 回测
- 主动提醒
- 管理员后台

## 技术风险

### 数据源风险（最高优先级）

股票行情和财务数据质量直接决定 AI 分析质量，且每个市场每类数据的供应商、格式、限频、稳定性、合规都不同，是全项目最大不确定性。**应在阶段 0 先用一个公开源打通一个市场端到端验证**，证明能稳定拿到真实、足够实时的数据后再扩展。必须显示数据更新时间，数据异常时阻止或弱化推荐。注意部分“想当然有”的数据可能不存在或不再实时披露（如盘中北向资金），需以实际数据源能力为准。**Tushare 分档处理**：第一阶段以东财 + 新浪为主、Tushare 非前置；免费档（120，股票清单/日线/交易日历）与低 cost 档（2000，少量捐赠，财务三表/复权因子/指数日线，长线财务深度来源）按需启用；高级档（5000，分钟线/融资融券明细等）暂不实现。

### LLM 幻觉与结构化输出风险

AI 必须基于候选池和输入数据分析。优先用 function calling / JSON mode，结构化输出做 Schema 校验 + 有限次 repair + 优雅降级，并保存输入数据快照与方法版本号，保证可复现。本地小模型强制 JSON 易失败，必须有 fallback。

### 合规与法律风险

在中国大陆，向不特定对象推荐具体证券及买卖时点属持牌业务，无证“荐股”即使加免责声明也可能违规。**本项目定位个人自用研究工具**，公开部署前须做合规评估；全产品统一“研究参考 / 交易计划 / 候选标的”表述，避免包装成确定收益或强投资建议。

### 成本风险

AI 调用需要缓存、每用户配额/熔断、频率限制和 token 日志。首页等高频页面不能每次刷新都调用 LLM。

## 建议目录结构

```text
QuantVista/
  README.md
  VERSION             # 版本号，构建时经 ldflags 注入（参照 new-api）
  Dockerfile          # 单镜像多阶段：bun 构建 web → go:embed 前端产物 → 单二进制（参照 new-api）
  docs/
    PRODUCT_REQUIREMENTS.md
    ARCHITECTURE.md
    DATABASE_DESIGN.md
    DATA_SOURCES.md
    DEPLOYMENT.md
    ROADMAP.md
  server/
    main.go
    router/
    middleware/
    controller/
    service/
    model/
    setting/
    oauth/
    datasource/      # 数据适配/标准化层，新增数据源只加 adapter
    common/
  web/
    package.json
    src/
      app/
      router/
      stores/
      api/
      pages/
      components/
      charts/
    dist/            # 构建产物，由后端 go:embed 托管（单容器部署，不单独起 web 容器）
  deploy/
    docker-compose.yml      # 单 quantvista 服务 + quantvista-redis，MySQL 用宿主机宝塔
    docker-compose.example.yml
    .env.example
```

> **embed 路径约束（实现时注意）**：`go:embed` 不能引用包目录之外或上层（`..`）路径。上面 `server/main.go` 与 `web/` 是兄弟目录，无法直接 embed `../web/dist`。两种解法二选一：(a) 构建时把 `web/dist` 拷进 server 包内的 embed 目标路径（new-api 的 Dockerfile 即这么做：`COPY --from=webbuilder .../dist ./web/dist`）；(b) 干脆照 new-api 把 `main.go` 放在仓库根、`web/` 作为根的子目录。阶段 0 搭骨架时定一种即可。
