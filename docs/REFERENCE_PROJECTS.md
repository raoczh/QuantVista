# 开源参考项目调研

## 1. 调研目标

本文档用于沉淀 QuantVista 可借鉴的开源项目，重点回答：

- 哪些项目值得持续关注。
- 每个项目的核心能力是什么。
- 可以应用到 QuantVista 的哪些模块和阶段。
- 当前规划中哪些功能应提前、补强或重新拆分。

调研对象覆盖五类：

- 金融数据与投研平台。
- AI / 多 Agent 投研框架。
- 个人组合、财富和持仓管理。
- 交易日志、复盘和结果追踪。
- 回测、量化和交易系统框架。

## 2. 参考项目总览

| 项目 | 类型 | 技术栈 | 参考价值 | 建议关注级别 |
| --- | --- | --- | --- | --- |
| [OpenBB](https://github.com/OpenBB-finance/OpenBB) | 金融数据平台 | Python | 数据接入、分析工具、AI agent 数据接口 | P0 |
| [TradingAgents](https://github.com/TauricResearch/TradingAgents) | 多 Agent 投研 | Python | 分析师团队、牛熊辩论、风险经理、组合经理 | P0 |
| [TradingAgents-astock](https://github.com/simonlin1212/TradingAgents-astock) | A 股多 Agent 投研 | Python | A 股规则、本土数据源、政策/游资/解禁角色 | P0 |
| [ai-berkshire](https://github.com/xbtlin/ai-berkshire) | 价值投资研究框架 | Python / Codex / Claude Code | 反偏见机制、投资逻辑卡、四大师视角、可复现研究 | P0 |
| [AgentFloor / trading-command-center](https://github.com/saketnayak/trading-command-center) | 自托管 AI 投资研究工作台 | Python | 持仓 AI 体检、每日 briefing、watchlist schedule、结果追踪 | P0 |
| [Ghostfolio](https://github.com/ghostfolio/ghostfolio) | 财富/组合管理 | TypeScript / Angular / NestJS | 账户、交易流水、资产配置、收益统计 | P0 |
| [Maybe](https://github.com/maybe-finance/maybe) | 个人财务管理 | Ruby | 净资产视图、账户聚合、个人财富总览 | P1 |
| [Rotki](https://github.com/rotki/rotki) | 隐私优先资产管理 | Python | 本地优先、隐私、组合分析、会计/税务思路 | P1 |
| [Quovibe](https://github.com/quovibe-web/quovibe) | 自托管组合追踪 | TypeScript | TTWROR、IRR、Sharpe、FIFO/移动平均成本、多币种 | P0 |
| [php-invest](https://github.com/matthiasstraka/php-invest) | 自托管股票组合追踪 | PHP / Symfony | 多账户、交易记录、衍生品、持仓风险告警 | P1 |
| [Invester](https://github.com/onur-celik/invester) | 投资 Dashboard | TypeScript | 可定制小组件、经济日历、技术分析卡片 | P1 |
| [Pfolie](https://github.com/HarryDulaney/pfolie) | 投资组合 Dashboard | TypeScript | 搜索、watchlist、行情图、新闻聚合 | P2 |
| [TradeNote SelfHost](https://github.com/gitricko/tradenote-selfhost) | 交易日志部署方案 | Makefile / Docker | 交易日志、备份恢复、自托管数据安全 | P1 |
| [WyckoffTradingAgent](https://github.com/YoungCan-Wang/WyckoffTradingAgent) | A 股量价筛选 Agent | Python | 量价结构、AI screener、MCP/CLI 工作流 | P1 |
| [FinRobot](https://github.com/AI4Finance-Foundation/FinRobot) | 金融 AI Agent 平台 | Python | 财务分析 Agent、报告自动化、LLM 工作流 | P1 |
| [FinGPT](https://github.com/AI4Finance-Foundation/FinGPT) | 金融大模型 | Python / Jupyter | 金融 NLP、情绪分析、领域模型 | P2 |
| [Qlib](https://github.com/microsoft/qlib) | AI 量化研究平台 | Python | 数据集、因子、模型训练、研究流水线 | P1 |
| [Qbot](https://github.com/UFund-Me/Qbot) | 中文 AI 量化平台 | Python / Jupyter | 中文投研、量化研究、本地部署经验 | P1 |
| [freqtrade](https://github.com/freqtrade/freqtrade) | 交易机器人 | Python | 策略配置、回测指标、风控、运行监控 | P2 |
| [vn.py](https://github.com/vnpy/vnpy) | 国内量化交易框架 | Python | 行情/交易/策略/风控模块边界 | P2 |
| [backtrader](https://github.com/mementum/backtrader) | 回测库 | Python | 回测抽象、策略指标、时间序列处理 | P2 |
| [StockSharp](https://github.com/StockSharp/StockSharp) | 量化交易平台 | C# | 跨市场交易、策略、风控、订单模块 | P2 |

## 3. 逐项目可借鉴点

### 3.1 OpenBB

定位：面向分析师、量化研究和 AI agent 的金融数据平台。

可借鉴点：

- 把金融能力组织成可组合的工具和数据接口，而不是散落在页面逻辑中。
- 数据源扩展走插件/后端连接思路，适合 QuantVista 后续接 Tushare、公告、新闻、财务和宏观数据。
- 面向 AI agent 暴露稳定的数据工具，避免 prompt 直接拼接非结构化数据。
- 把数据接口文档化，便于 AI 分析中心调用时可复现、可测试。

映射到 QuantVista：

- 阶段 2：Market Data Service、数据适配层、数据源健康状态。
- 阶段 4：AI 分析中心的数据工具层。
- 阶段 8：多数据源配置、Prompt 模板管理。

建议：

- 在 `datasource.Adapter` 上方再加一层 `research tools`，例如 `GetCompanySnapshot`、`GetMarketBreadth`、`GetRecentNews`、`GetValuationSnapshot`。
- AI 分析不要直接依赖页面接口，应该依赖稳定的研究工具接口。

### 3.2 TradingAgents

定位：多 Agent 金融交易分析框架。

README 关键信息：

- Analyst Team：基本面、情绪、新闻、技术分析。
- Researcher Team：牛方和熊方研究员辩论。
- Trader Agent：整合报告形成交易决策。
- Risk Management 和 Portfolio Manager：评估风险并决定是否批准交易。
- 有 decision log、checkpoint resume、结构化输出、provider registry。

可借鉴点：

- AI 分析不要做成单次“一个模型给一篇报告”，而应拆为多角色观点。
- 强制 bull / bear debate，减少单边论证。
- Risk Manager 与 Portfolio Manager 不应只在交易系统中存在，也可作为研究工具里的风控审查。
- 每次 AI 结论保存 decision log，支持复盘和重新运行。

映射到 QuantVista：

- 阶段 4：AI 分析中心。
- 阶段 5：短线/长线推荐。
- 阶段 6：推荐追踪。
- 阶段 8：多模型交叉观点。

建议：

- 先不做完整多 Agent 调度，MVP 可实现“单模型多角色模板”：技术面、基本面、新闻/情绪、反方、风险审查。
- 推荐结果落库时保存各角色摘要，而不是只保存最终结论。

### 3.3 TradingAgents-astock

定位：基于 TradingAgents 深度改造的 A 股多 Agent 投研框架。

README 关键信息：

- A 股数据源：mootdx、东财、新浪、同花顺、财联社、百度股市通等。
- A 股规则：T+1、涨跌停、最小手数、ST。
- 角色扩展：政策分析师、游资追踪师、解禁监控师。
- Alpha 基准从 SPY 改为沪深 300。
- 东财请求统一节流，避免批量多 Agent 分析触发风控。

可借鉴点：

- A 股不是美股翻译版，必须把交易制度、政策、资金风格、限售解禁纳入分析。
- 短线计划必须考虑 T+1、涨跌停、手数、ST、停牌。
- A 股风险中，解禁、减持、龙虎榜、政策窗口往往比普通新闻更关键。
- 东财等公开数据源需要统一节流、抖动、串行化和错误降级。

映射到 QuantVista：

- 阶段 2：市场数据补全、资金流、情绪、日线入库。
- 阶段 5：短线/长线推荐。
- 阶段 6：推荐追踪。
- 阶段 7：今日待复盘、条件提醒。

建议：

- 在 `recommendation_records.action_plan_json` 中显式记录市场规则约束。
- 增加 A 股特有风险字段：`is_st`、`limit_up_price`、`limit_down_price`、`unlock_event`、`major_holder_reduce_event`。
- 对东财接口增加全局节流器，而不是每个 adapter 自己请求。

### 3.4 ai-berkshire

定位：AI 价值投资研究框架。

README 关键信息：

- 四大师视角：巴菲特、芒格、段永平、李录。
- 结构化反偏见机制：信息丰富度评级、逆向检验、快速否决清单、反共识检查、留白原则。
- 可复现研究流程：同样输入得到结构一致的报告，支持横向比较和半年后变化对比。
- Skills：深度研究、财报分析、行业筛选、持仓管理、管理层研究等。

可借鉴点：

- 长线研究必须关注“投资逻辑是否成立”，不是只输出估值和风险。
- 对不确定数据要标注置信度或灰色地带，禁止伪装确定性。
- 反方观点不是附加项，应成为每份分析的必填字段。
- 研究报告要可横向比较，评分维度和版本必须稳定。

映射到 QuantVista：

- 阶段 4：AI 分析中心。
- 阶段 5：长线推荐。
- 阶段 7：个股问答、横向对比。
- B 档储备：投资逻辑卡片、反方观点、变化检测、研究证据包。

建议：

- 将“投资逻辑卡片”提前到阶段 3/4，作为自选股和持仓的核心补充字段。
- AI 输出 schema 增加 `anti_thesis`、`kill_switches`、`unknowns`、`confidence_reason`。

### 3.5 AgentFloor / trading-command-center

定位：自托管 AI 投资研究工作台。

README 关键信息：

- 支持上传组合、每日 AI briefing、持仓健康分、行动项、风险提醒、行业暴露。
- Watchlist 可按日/周定时监控。
- AI 分析包含基本面、情绪、新闻、技术分析、牛熊辩论、风险经理。
- Outcome tracking：跟踪 +7d / +14d / +30d / +90d 表现。
- 支持持仓 AI 问答、投资 thesis cross-reference、发现组合缺口。
- 支持 run comparison、报告导出、深度控制。

可借鉴点：

- “今日待看清单”比“每天生成新推荐”更贴合个人使用。
- AI 输出要变成 action items：TRIM / EXIT / WATCH / REANALYZE 等，而不是长篇文字。
- 推荐或分析应有 outcome tracking 时间点。
- 持仓分析要结合行业暴露、集中度、行为提醒。
- 同一标的多次分析需要 diff。

映射到 QuantVista：

- 阶段 3：持仓管理。
- 阶段 4：AI 分析历史。
- 阶段 6：推荐追踪。
- 阶段 7：今日待复盘、个股 AI 问答、横向对比。
- B/C 档：组合暴露分析、变化检测、个人风控提示。

建议：

- 把“今日待复盘 / 待办清单”提前做轻量版。
- 对每条 AI 推荐预设追踪节点：7、14、30、90 个交易日或自然日需明确。
- 为持仓页加入“上次分析时间”和“一键分析所有过期/未分析持仓”。

### 3.6 Ghostfolio

定位：开源财富管理软件。

可借鉴点：

- 账户、持仓、交易流水、资产类别和基准收益分层清晰。
- 组合管理关注长期净资产、资产配置、收益统计，而不仅是单股盈亏。
- 隐私和自托管定位清晰，适合个人部署工具。
- 适合参考用户侧“组合总览”而不是只做股票研究页面。

映射到 QuantVista：

- 阶段 3：持仓管理。
- 阶段 6：表现统计。
- C 档：组合总览、组合暴露分析。

建议：

- 持仓不要只建 `positions`，后续最好补 `transactions` 或 `position_lots`，支持分批买卖和真实成本。
- 组合层至少预留：总市值、现金、行业分布、市场分布、币种分布、最大持仓占比。

### 3.7 Maybe

定位：个人财务管理应用。

可借鉴点：

- 用户真正关心的是净资产、现金流、资产负债全貌。
- 股票研究工具未来可以作为“投资资产子模块”，而不是孤立股票看板。
- UI 上可参考全局资产视角：账户、资产类别、趋势、目标。

映射到 QuantVista：

- C 档：组合总览、资产负债、长期财务目标。
- 阶段 8：完整度增强。

建议：

- 当前不需要做完整个人财务，但数据库里持仓成本和币种语义要严谨，避免未来无法汇总净资产。

### 3.8 Rotki

定位：隐私优先的组合追踪、分析、会计和管理应用。

可借鉴点：

- 本地优先和隐私优先是个人投资工具的重要卖点。
- 对交易和资产变动要能审计、导入、修正，而不是只保存当前状态。
- 税务/会计对本项目不是 MVP，但“交易流水不可丢”非常重要。

映射到 QuantVista：

- 阶段 3：持仓管理。
- 阶段 8：数据导入/导出、审计日志。

建议：

- 增加 CSV 导入/导出规划优先级。
- 交易流水和 AI 推荐记录都要可导出，便于用户离开系统也能保留资料。

### 3.9 Quovibe

定位：自托管组合追踪工具。

仓库描述关键信息：

- 支持股票、ETF、债券。
- 指标包括 TTWROR、IRR、Sharpe、波动率。
- 支持 FIFO / 移动平均成本、资产配置、股息、多币种。
- Docker-ready。

可借鉴点：

- 收益指标不要只算当前盈亏，应区分资金加权收益、时间加权收益、已实现/未实现收益。
- 成本法要明确，A 股和多市场会涉及分批买卖、费用、税费。
- 股息、分红、拆股、复权会影响真实表现。

映射到 QuantVista：

- 阶段 3：持仓管理。
- 阶段 6：推荐追踪和表现统计。
- C 档：组合总览、股息、资产配置。

建议：

- `positions` MVP 可以先做，但应尽早补交易流水表，避免分批买卖无法处理。
- 推荐表现统计要同时展示绝对收益、相对基准收益、样本量、最大回撤。

### 3.10 php-invest

定位：自托管股票组合追踪。

README 关键信息：

- 支持 instrument、account、broker account。
- 支持现金管理、开平仓、分红。
- 支持资产价格导入、笔记、交易分析。
- 规划中有相对持仓大小/亏损警告、保证金账户等。

可借鉴点：

- 账户和资产应分离，持仓只是账户中某资产的结果。
- 股票笔记和事件笔记应绑定到资产，不一定绑定到持仓。
- 风险警告可以从简单规则开始：持仓占比过高、亏损接近止损、未分析时间过久。

映射到 QuantVista：

- 阶段 3：持仓管理。
- B 档：投资笔记、仓位风险计算器。

建议：

- 自选股/股票详情页增加“事件笔记”能力，区别于 AI 报告。

### 3.11 Invester

定位：可定制投资跟踪 Dashboard。

README 关键信息：

- 支持实时追踪、可定制 dashboard、多资产类别、图表。
- 小组件包括 ticker、mini chart、TradingView chart、经济日历、技术分析、新闻、恐惧贪婪、笔记等。

可借鉴点：

- 首页可以从固定布局逐步演进为“可配置研究工作台”。
- 用户可以选择自己关心的 widget：持仓、待复盘、市场指数、新闻、日历、资金流、AI 摘要。
- “笔记 widget”对个人研究很有用。

映射到 QuantVista：

- 阶段 2：首页市场看板。
- 阶段 7：今日待复盘。
- B 档：投资笔记。

建议：

- 首页优先呈现“今天该看什么”，其次才是市场大盘。
- 后续可支持 dashboard 布局配置，但 MVP 不必做拖拽。

### 3.12 Pfolie

定位：投资组合和市场数据 Dashboard。

README 关键信息：

- 支持 quick search、多 watchlist、实时价格和成交量图、新闻聚合。

可借鉴点：

- 快速搜索和多 watchlist 是高频入口。
- 自选股不应只是列表，还应能快速切换图表、新闻和笔记。

映射到 QuantVista：

- 阶段 3：自选股。
- 阶段 4：个股分析入口。

建议：

- 自选股页面提供“速查抽屉”：行情、K 线、新闻、备注、AI 分析入口。

### 3.13 TradeNote SelfHost

定位：自托管交易日志部署方案。

README 关键信息：

- 强调备份、恢复、私有仓库、数据安全。
- 提供本地或 Codespaces 部署。

可借鉴点：

- 复盘数据和交易日志比行情缓存更重要，需要备份恢复说明。
- 用户笔记、持仓、推荐历史、分析历史应支持导出。
- 自托管工具要明确“哪些目录是数据、如何备份、如何恢复”。

映射到 QuantVista：

- 阶段 3：持仓复盘。
- B 档：决策日志 / 投资笔记、卖出后复盘模板。
- 部署文档：备份恢复。

建议：

- `docs/DEPLOYMENT.md` 补一节“数据备份与恢复”。
- 增加导出 JSON/CSV 的规划。

### 3.14 WyckoffTradingAgent

定位：A 股量价分析和 AI stock screener。

可借鉴点：

- 候选池筛选应先由规则/数据完成，再交给 AI 解释。
- 量价结构、趋势阶段、成交量异常是短线候选池的重要输入。
- CLI / MCP 工具适合给 AI 编码助手和自动任务调用。

映射到 QuantVista：

- 阶段 5：候选池筛选、短线推荐。
- 阶段 8：股票评分系统。

建议：

- 短线策略模板中加入量价规则：放量、突破、回撤、趋势阶段。

### 3.15 FinRobot

定位：金融分析 AI Agent 平台。

可借鉴点：

- 财务分析、报告生成、Agent 工作流可以模板化。
- 长线研究应有财报分析专用流程，而不是和短线共用 prompt。

映射到 QuantVista：

- 阶段 4：AI 分析中心。
- 阶段 5：长线推荐。
- 阶段 8：财务数据详情、Prompt 模板管理。

建议：

- 长线分析单独定义 schema：商业模式、财务质量、管理层、估值、安全边际、关键跟踪指标。

### 3.16 FinGPT

定位：金融大模型和金融 NLP。

可借鉴点：

- 新闻情绪、公告摘要、财报文本理解是金融 LLM 的重要子任务。
- 自建模型不是当前重点，但数据标注和任务定义可以借鉴。

映射到 QuantVista：

- 阶段 8：新闻情绪、财务数据详情。

建议：

- 先不要训练模型，先把新闻/公告摘要和情绪结果作为可复用数据资产落库。

### 3.17 Qlib

定位：AI 量化研究平台。

可借鉴点：

- 数据集、特征、模型、回测、评估要有明确研究流水线。
- 因子和评分体系应版本化。
- 研究结果需要和数据快照绑定。

映射到 QuantVista：

- 阶段 6：推荐追踪。
- 阶段 8：股票评分、回测模块。

建议：

- 当前不做复杂 ML，但 `stock_scores` 应保存 scoring_version 和 details_json。
- 推荐统计要区分样本内解释和样本外追踪。

### 3.18 Qbot

定位：中文 AI 量化投研平台。

可借鉴点：

- 中文用户的部署、本地化数据源、量化研究路径。
- 本地部署场景下，对 LLM、数据源、任务调度的配置体验很重要。

映射到 QuantVista：

- 阶段 1：设置。
- 阶段 4/5：AI 分析与推荐。
- 阶段 8：回测、模拟交易。

建议：

- 管理后台后续补“任务状态”和“数据源同步日志”页面。

### 3.19 freqtrade / vn.py / backtrader / StockSharp

定位：交易机器人、量化交易框架和回测库。

可借鉴点：

- 策略配置应可版本化、参数化。
- 回测指标包括胜率、最大回撤、收益回撤比、Sharpe、交易次数、平均持有期。
- 风控应独立于推荐逻辑，例如最大仓位、止损、冷却期。
- 交易执行系统复杂度高，不适合 QuantVista 当前阶段直接实现。

映射到 QuantVista：

- 阶段 5：策略模板。
- 阶段 6：推荐追踪与统计。
- 阶段 8：回测、模拟交易。

建议：

- 本项目定位为研究工具，不做实盘交易接入。
- 但可以借鉴回测指标和策略参数体系，用于评估 AI 推荐质量。

## 4. 对 QuantVista 规划的映射建议

### 阶段 2：市场数据与首页

参考项目：

- OpenBB
- TradingAgents-astock
- Invester
- AgentFloor

建议补强：

- 数据新鲜度闸门：行情过旧、数据源异常时禁止或降级 AI 分析/推荐。
- 日线入库优先级提高：`daily_bars` 是推荐追踪、提醒、回撤的基础。
- 交易日历入库优先级提高：有效期、持有周期、提醒都要用交易日。
- 首页从“市场展示”升级为“今日研究工作台”：市场、待复盘、持仓风险、数据异常。

### 阶段 3：自选股与持仓

参考项目：

- Ghostfolio
- Quovibe
- php-invest
- Rotki
- Pfolie
- AgentFloor

建议补强：

- 自选股增加投资逻辑字段：关注原因、核心假设、失效条件、下次复盘日期。
- 持仓应预留交易流水或 lot 模型，避免只保存单个买入价。
- 持仓页增加仓位占比、行业暴露、未分析天数、接近止损提示。
- 增加 CSV 导入/导出规划。

### 阶段 4：AI 分析中心

参考项目：

- TradingAgents
- ai-berkshire
- FinRobot
- OpenBB
- AgentFloor

建议补强：

- 输出 schema 增加反方观点、未知项、数据置信度、失效条件。
- 分析报告保存多角色摘要。
- 每次分析保存数据快照、prompt 版本、工具调用清单。
- 支持同一股票“与上次分析对比”。

### 阶段 5：短线/长线推荐

参考项目：

- TradingAgents-astock
- WyckoffTradingAgent
- ai-berkshire
- Qlib

建议补强：

- 短线推荐先做规则候选池，再由 AI 解释排序。
- 候选池排除 ST、停牌、退市风险、低流动性、涨跌停不可交易状态。
- A 股短线计划必须写入 T+1、涨跌停、手数、交易日有效期。
- 长线推荐和短线推荐分 schema，长线强调基本面逻辑、估值、安全边际、复盘条件。

### 阶段 6：推荐追踪与卖出提示

参考项目：

- AgentFloor
- Quovibe
- Qlib
- backtrader

建议补强：

- 固定追踪节点：+7 / +14 / +30 / +90，可按交易日或自然日统一定义。
- 表现统计同时展示绝对收益、相对基准收益、样本量、最大回撤。
- 止盈止损判断使用 daily high/low。
- 记录 AI 推荐与用户实际买入表现差异。

### 阶段 7：个人选股增强

参考项目：

- AgentFloor
- ai-berkshire
- Invester
- Pfolie

建议补强：

- 今日待复盘清单提前做轻量版。
- 条件提醒先做页面内提醒，不做主动推送。
- 横向对比加入“为什么没选它”和“反方观点”。
- 个股 AI 问答复用已保存数据快照，避免每次重新拉数据。

### 阶段 8：完整度与可信度增强

参考项目：

- Qlib
- FinGPT
- FinRobot
- freqtrade
- vn.py
- StockSharp

建议补强：

- 股票评分体系版本化。
- Prompt 模板和策略模板可版本化、可回滚。
- 模拟交易和回测只用于复盘，不承诺预测收益。
- 管理后台增加数据源同步日志、AI 调用日志、任务日志。

## 5. 建议提前调整的规划优先级

### 建议提前到阶段 2/3

- 数据新鲜度闸门。
- 日线入库和交易日历。
- 投资逻辑卡片。
- 交易流水 / lot 模型预留。
- CSV 导入/导出。

理由：这些是后续 AI、追踪、复盘的地基，越晚补成本越高。

### 建议提前到阶段 4/5

- 反方观点 / 否决理由。
- 买入前检查清单。
- 仓位风险计算器。
- 分析结果与上次对比。
- 研究证据包。

理由：这些直接提高 AI 分析可信度和实际使用价值。

### 可保持后置

- 完整回测系统。
- 主动推送。
- 多模型复杂编排。
- 实盘交易接入。
- 复杂管理员权限系统。

理由：这些工程量大，且当前个人研究工具阶段未必马上产生收益。

## 6. 建议新增或细化的数据模型

### 6.1 交易流水

用于替代或补充单条 `positions` 的买入价模型。

建议字段：

- `user_id`
- `stock_id`
- `account_id`
- `type`：buy / sell / dividend / fee / split / transfer
- `trade_date`
- `price`
- `quantity`
- `fee`
- `tax`
- `currency`
- `note`
- `source`

### 6.2 投资逻辑卡片

绑定自选股、持仓或推荐。

建议字段：

- `target_type`
- `target_id`
- `thesis`
- `key_evidence_json`
- `risks_json`
- `kill_switches_json`
- `next_review_date`
- `last_ai_report_id`
- `status`：active / invalidated / archived

### 6.3 研究证据包

绑定 AI 分析报告。

建议字段：

- `report_id`
- `quote_snapshot_id`
- `news_refs_json`
- `fundamental_refs_json`
- `macro_refs_json`
- `prompt_version`
- `tool_calls_json`
- `data_freshness_status`

### 6.4 买入前检查单

从候选标的转持仓时使用。

建议字段：

- `recommendation_id`
- `max_loss_amount`
- `max_loss_percent`
- `planned_position_size`
- `stop_loss_price`
- `invalid_conditions_json`
- `event_risks_json`
- `confirmed_at`

## 7. 可直接转化为开发任务的清单

P0：

- 修复 LLM Base URL SSRF 防护。
- 修复禁用用户/改密码后 access token 仍可用。
- 修复首启管理员并发创建。
- 日线 OHLC 入库。
- 交易日历入库。
- 用户偏好推荐数量改为 3 到 5，补 `mid_term` 映射。

P1：

- 自选股和持仓 MVP。
- 投资逻辑卡片轻量版。
- 分析历史与数据快照。
- 推荐记录与候选池。
- 买入前检查清单。
- 仓位风险计算器。
- 今日待复盘轻量版。

P2：

- 多角色 AI 分析模板。
- 推荐追踪节点与表现统计。
- 横向对比。
- 个股 AI 问答。
- CSV 导入/导出。
- 数据源同步日志和任务日志。

P3：

- 完整回测。
- 多模型交叉观点。
- 新闻情绪模型。
- 主动提醒。
- 模拟交易。

