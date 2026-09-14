# 股票项目业务算法与提示词审查

本轮以 2026-09-14 的本地源码为依据，对历史 Top50 样本和补充的三个量化项目进行静态对照，并落实可由现有数据支持的改进。原 [GitHub Top50 快照](GITHUB_STOCK_ANALYSIS_TOP50_SNAPSHOT.md) 是 2026-07-28 的关键词最佳匹配结果，保留其原始排序，不能称为当前星数榜。

本轮新增 **4 个内置策略（总计 30 个）**，修正盈利质量评分及执行确认，统一个股/推荐/问答/复核的盈利解释纪律，消除推荐提示词中的两套价格规则冲突，并补齐策略原始指标的证据核验。

这里的“覆盖 50 项”指每个项目都检查了与任务相关的源码或提示词入口，并记录采纳判断；不等于逐行审计这 50 个仓库的全部代码。源码对照和合成回归能证明规则与数据口径，不能证明实盘胜率已经提高。

## 1. 已落实的业务改进

### 1.1 正同比不再直接等于盈利成长

原 `qualityFinanceScore` 会在营收同比和净利润同比达到阈值时加分，但没有消费归母/扣非利润金额和上年同期金额。由亏损 100 收窄到亏损 50，或者由亏损转盈利，都不能直接当成正盈利基期上的持续成长。

新 `ff2` 财务事实复用原有 8 期本地财报查询，增加归母净利润、扣非净利润、扣非同比、每股经营现金流和上年**同一报告季**归母净利润。最新报告、年报和可比基期在补拉前一起冻结；本轮没有刷新时，不允许其他并发刷新改变这组证据。金额以元保留正负，避免亿元展示舍入把小额亏损变成零；原始可用性掩码继续区分真实零和缺失。

`fq1` 区分亏损、盈亏平衡、扣非未盈利、基期缺失、扭亏、零基期、正盈利基期增长等情况。`qr3` 的正向盈利质量信用要求当期归母和扣非均盈利；双增长信用还要求上年同期归母盈利、当前金额确实增加、披露同比为正。原有分组权重与上限没有因为参考项目的主观打分而重新调参。

- 价值/成长/质量侧重缺少必要金额时，执行状态保持依据不足。
- 当期归母或扣非不为正时，这些财务侧重等待业绩验证；成长侧重遇到亏损/零基期，或者同比与缓存金额方向矛盾时，先观察。
- 技术侧重不会因此被改写成财务选股。单期负经营现金流只提示进一步核对季节性、应收和存货，不直接增加扣分或认定财务造假。
- 不以每股经营现金流除以加权 EPS 拼造现金转换率；两者分母、期间或合并口径可能不同。当前三表缓存尚不适合承担统一的现金流硬门槛。
- 个股页决策摘要同步提示当期亏损或扣非未盈利，避免仅凭正同比显示为正向变化。

代码：[盈利分类](../server/service/finance_earnings.go)、[财报冻结](../server/service/finance_f10.go)、[评分](../server/service/recommendation_scoring.go)、[执行确认](../server/service/recommendation_signal.go)、[页面摘要](../web/src/components/stock-detail/decisionSummary.ts)。

### 1.2 增加有区别、可由当前日线计算的常用策略

| 新策略 | 条件与最低数据要求 | 与已有策略的区别 |
| --- | --- | --- |
| DMI 多头趋势确认 | +DI 近 3 日上穿 -DI 并保持，ADX14≥20 且回升，站上 MA60，距 MA20 为 0～2 ATR；至少 150 根完整日线 | 同时核对方向和趋势强度，不把下跌中的高 ADX 当买入信号 |
| NR7 窄幅整理突破 | 昨日高低振幅严格小于此前 6 日，今日放量收盘突破昨日高点，站上 MA20/60，收盘位置≥0.65，排除涨停；整套策略至少 60 根 | 观察短窗口波动收缩，区别于布林长窗口收口；成交缺失和一字线不算收敛 |
| 长期趋势内 RSI2 回调修复 | 上行 MA200 上方、MA5 下方，昨日 RSI2<10，今日回到 10～50，收盘上涨且低点不下移；至少 220 根 | 长期趋势中的短期修复，区别于没有长期方向约束的超卖反弹 |
| 50/150/200 日趋势模板 | 收盘>MA50>MA150>MA200，MA200 高于 20 日前；距 250 日低点≥30%、距高点≤25%，距 MA20≤2 ATR；至少 250 根 | 更长周期的趋势结构；仅实现价格条件，不冒充包含相对强度排名、盈利验证和完整仓位纪律的 SEPA 系统 |

所有策略均接入选股、推荐策略目录、条件命中、适用周期、风险等级和自然语言因子字典。样本不足保持未知，`is_false` 不能捞出缺数据股票。DMI 采用完整窗口种子的 Wilder 平滑，ADX 额外预热；NR7 的共同研究价使用昨日窄幅高点，不被错误替换为 20 日新高。

这些是可选的筛选规则，并未把新指标逐项叠加进默认推荐权重。没有成熟样本前，不宣称其中某一种更高胜率。高股息、现金流质量、行业相对强度、完整缠论/VCP 等方向暂不包装成已实现策略：前两者还需要一致的全市场财务事实，后两者需要更明确的形态定义、历史行业/成分身份或独立验证。

代码：[指标与时序规则](../server/service/screener_trend_setups.go)、[策略目录](../server/service/screener_builtin.go)、[侧重映射](../server/service/recommendation_profile.go)、[共同研究价](../server/service/research_price_plan.go)。

### 1.3 提示词与实际数据、执行规则一致

新增固定研究纪律，放在默认和自定义任务都不能移除的系统段中，标准分析、panel 和推荐共享；没有增加模型调用次数。

- 区分策略成立、研究价值和现在能否入场；长持有周期不能把技术策略强行变成财务成长论证。
- 同源的均线、MACD、涨幅不是多份独立证据；量化分与模型 confidence 都不是统计胜率。
- 反证必须指向可能推翻核心判断的事实；缺失数据写待核查，不编造已发生事件，不把缩量解释为“主力洗盘”。
- 失效条件明确观察对象和时点，区分盘中触及、完整收盘、新财报披露；已触发的持仓保护不能等待 AI 改口或下一次复盘。
- 个股、推荐、问答与独立复核采用同一盈利分类和同比可比性纪律。
- 有 `price_plan` 的候选只复述程序价位。旧的固定 `-5%～-7%` 止损提示已移除；中值毛盈亏比 1.5 规则只给确实没有程序计划的兼容输入。当前计划继续使用买入上沿、扣费后第一目标至少 1.2R，不抬目标凑比例。
- 修正“一律 100 股”的预算提示，普通 A 股新买入最低 100 股、科创板最低 200 股；低价本身不作为低估或低风险证据。
- 新策略的 `strategy_hit.values` 纳入数值核验，保持已知零值并固定遍历顺序，避免 AI 正确引用 ADX 等指标却被当成无来源数字。

代码：[共享研究纪律](../server/service/research_prompt.go)、[推荐消息](../server/service/recommendation.go)、[个股消息](../server/service/analysis.go)、[数值核验](../server/service/recfactor.go)。

### 1.4 卖出点与提醒：保留已经更完整的实现

本轮对照了事件/止损/时间退出优先级、ATR 保护、波动/趋势风险、多角色风险决策和通知入口。本项目已有的 `xp1`/`pea5` 覆盖结构与 ATR 保护、扣费后保本、利润锁定、保护只收紧、分阶段减仓、T+1、公司行动和通知台账。

没有将其他项目的固定百分比目标、按多数意见决定卖出、收盘后才确认所有止损、或不区分买卖现金流与实际盈亏的示例迁入。当前价格保护独立于 AI 意见，研究性时间复核也不自动等于卖出成交。此次增量是提示词及证据链对齐，而非未经验证地重调持仓退出阈值。

## 2. 版本与验证边界

当前新增/更新版本：`qr3` 评分、`ff2` 财务、`fq1` 盈利分类、`eq3` 入场、`ep6` 执行计划、`sp3` 策略侧重、`fv8` 因子快照、`of3` 优化事实、`rp2` 研究价、`rr3` 排序研究；个股提示 `p23`、推荐提示 `p19`、推荐策略 `s14`、问答 `q17`、自然语言选股字典 `sp5`。

新任务对旧质量规则选择升级到当前规则，已保存的政策行和历史输出不改写。旧因子快照不能回填成新版本事实；学习工件读取时严格核验 `FeatureVersion`，当前为 `of3 / fv8 / sq2`，旧版本权重不能直接用于新口径，需重新评估后启用。仍保留同机会集的原加法评分对照，不把它称为旧版完整流水线重放。

验证记录见文末。未读取应用 `server/.env`、未使用线上临时数据库凭据、未修改线上数据、未调用真实 AI。测试使用合成行情/财报与隔离测试数据库；参考源码只静态读取，不运行其采集、训练或交易脚本。没有复制参考项目源码或新增其依赖。

## 3. 原 50 项源码覆盖

参考目录为 `D:\TestWorkSpace\_refs\stock-analysis-top50`。表内提交为本地固定提交。许可证列仅记录已读根许可证，不代替完整法律审查；“根未见”不表示嵌套目录没有许可证。少量旧文件不是 UTF-8，按 UTF-8 容错读取可辨识代码，不能可靠辨认的注释不计为验证证据。

深度分为：**深入**（检查算法公式、条件分支或风险/提示词约束）、**定向**（检查相关流程或数据边界）、**入口**（确认教程、封装、展示等入口，增量有限）。原 50 项共计深入 22 项、定向 18 项、入口 10 项。

| # | 项目与固定提交 | 深度 | 源码入口 | 业务结论 | 根许可证 |
| ---: | --- | --- | --- | --- | --- |
| 1 | [wkingnet/stock-analysis](https://github.com/wkingnet/stock-analysis/tree/814f1d4db3a7ecac0b21f61e6ea6214a9ea73742) `814f1d4db3a7` | 深入 | `CeLue模板.py` | 均线/基准过滤与首个退出事件；时间退出示例含价格与收益率混用，不迁入。保留本项目结构止损和交易日口径。 | AGPL |
| 2 | [ArvinLovegood/go-stock](https://github.com/ArvinLovegood/go-stock/tree/4f82ac96ee465bb7a76efd642005504604a5db88) `4f82ac96ee46` | 定向 | `backend/agent/tools/choice_stock_by_indicators_tool.go` | 自然语言选股主要封装外部查询，返回条数带随机性；不替代本项目确定性候选边界。 | GPL |
| 3 | [qilihei/StockAgent](https://github.com/qilihei/StockAgent/tree/82fbd6619e92e79172756d7c689bb1ec5dc0f8b6) `82fbd6619e92` | 定向 | `AgentServer/src/analysis/stock_linkage.py`；`AgentServer/src/tools/analysis/technical.py` | 板块角色按市值、连板、题材和时间分档；不能把手设置信度或大市值当作已验证龙头。 | MIT |
| 4 | [waditu/czsc](https://github.com/waditu/czsc/tree/9ab62854f6bfab8515115b942baf4deb0f06185c) `9ab62854f6bf` | 深入 | `crates/czsc-core/src/objects/position.rs` | 事件、止损、超时退出及T+0开关；核对退出优先级，保留本项目更细的T+1/公司行动/阶段减仓。 | Apache-2.0 |
| 5 | [x-pai/AlphaBot](https://github.com/x-pai/AlphaBot/tree/4329acbc802e7583ae9253553c55cae4b9056d32) `4329acbc802e` | 深入 | `backend/app/services/risk_control_service.py` | 仓位集中度思路可参考；所读单日盈亏示例把买卖金额都减去，不可当成真实损益算法。 | Apache-2.0 |
| 6 | [ganshane/shakey](https://github.com/ganshane/shakey/tree/f6938bdef210cc44d661fb0f200e37f279e7ba23) `f6938bdef210` | 入口 | `shakey-client/src/main/java/shakey/config/VolumeStrategy.java` | 量能周期和交易接入入口；本入口不提供可用于本次评分优化的收益验证。 | Apache-2.0 |
| 7 | [youerning/pstock](https://github.com/youerning/pstock/tree/39f48353f2b59bf3952a6b2be65d49f607f22fb7) `39f48353f2b5` | 入口 | `www/tpls/strategy.html` | 策略页为展示占位，没有可借鉴的筛选公式。 | 根未见 |
| 8 | [chjm-ai/stock-daily-analysis-skill](https://github.com/chjm-ai/stock-daily-analysis-skill/tree/af5e467b90ce9d682084801280cd01fe90ef5b85) `af5e467b90ce` | 深入 | `scripts/trend_analyzer.py` | 按趋势/乖离/量能/MACD/RSI固定加分；固定MA5乖离阈值及缩量等于洗盘的解读不采用。 | MIT |
| 9 | [terancejiang/Stock_Analyze_Prompts](https://github.com/terancejiang/Stock_Analyze_Prompts/tree/f1fe2eaacc088c857c48fc7baabd10a633860e04) `f1fe2eaacc08` | 深入 | `turtle_framework/龟龟投资策略_v0.15/phase3_分析与报告.md` | 核查数据缺口、否决门和利润/现金分配的区分；采纳证据边界，未提供的审计/完整三表不能交给AI猜测。 | 根未见 |
| 10 | [Python3Spiders/StockSpider](https://github.com/Python3Spiders/StockSpider/tree/46edf2f7d85142107aa0f35a34ae964affb51e6e) `46edf2f7d851` | 入口 | `data_viewer/main.py` | 采集与图表展示入口，未据此增加预测或推荐能力。 | MIT |
| 11 | [pengxungui/LSTM_stock](https://github.com/pengxungui/LSTM_stock/tree/d12d1c96619736479f656a249e38bda5eb8b8dbf) `d12d1c966197` | 深入 | `LSTM.py` | 全样本归一化后随机拆分的预测示例，不能作为时间外收益证据；不引入模型或其效果数字。 | 根未见 |
| 12 | [lc2panda/StockAnal_Sys](https://github.com/lc2panda/StockAnal_Sys/tree/bf20433460b3cb353d4fd2f672030da50fa13852) `bf20433460b3` | 深入 | `app/analysis/risk_monitor.py` | 波动、趋势、反转、量能分档；反向和正向线索简单计数易混淆方向，不替代现有风险门控。 | MIT |
| 13 | [axiaoxin-com/investool](https://github.com/axiaoxin-com/investool/tree/2323bfdd2ba785cee851f74bb8a3b4a255ddc7d0) `2323bfdd2ba7` | 深入 | `core/selector.go`；`core/checker.go` | 多期ROE、正利润/EPS及现金流核对；采纳先验证盈利金额与可比基期，完整多年质量与现金流门槛待数据齐备。 | Apache-2.0 |
| 14 | [liangdabiao/crewai_stock_analysis_system](https://github.com/liangdabiao/crewai_stock_analysis_system/tree/aab66c6a5782bb74542bdce6996baba30742e337) `aab66c6a5782` | 定向 | `src/tools/technical_tools.py` | 按MACD/RSI文字信号计数生成看法；不把多数技术信号等同于独立证据。 | 根未见 |
| 15 | [AkatsukiYamisora/stock-prediction-with-DL](https://github.com/AkatsukiYamisora/stock-prediction-with-DL/tree/2a37e5085d1f8cb00ebc338a0eea9e7700757acd) `2a37e5085d1f` | 定向 | `strategy.py` | 深度预测选股和持仓收益入口；未见适配本项目数据口径的独立收益证据，不直接迁入。 | GPL |
| 16 | [Erenest/-](https://github.com/Erenest/-/tree/15f9a7374b16a610f8407c49ebabf9270172417d) `15f9a7374b16` | 深入 | `TradingAgents-CN-main/tradingagents/agents/managers/risk_manager.py` | 提示强调果断决策且限制持有作为后备，基本面变量读取新闻字段；不采用行动偏置及交叉污染。 | 根未见 |
| 17 | [164149043/AlphaCouncil](https://github.com/164149043/AlphaCouncil/tree/39961e25ce926f94a4a8d7881d2592198624e3db) `39961e25ce92` | 定向 | `App.tsx` | 分析师、总监、风控、总经理分阶段编排；本项目已有反方/裁判，不通过增加同类角色宣称更准。 | 根未见 |
| 18 | [2429298470/stock4j](https://github.com/2429298470/stock4j/tree/b1e847288437968f18f381c7563ffb1a0556d365) `b1e847288437` | 入口 | `stock4j/src/com/stock4j/Stock.java` | 证券身份与数据模型，旧编码注释不计证据；未迁入简化市场识别。 | 根未见 |
| 19 | [oficcejo/aiagents-stock](https://github.com/oficcejo/aiagents-stock/tree/020f512e965cef77a7842f81f2f85b67265206dc) `020f512e965c` | 定向 | `sector_strategy_agents.py` | 板块涨幅与资金情况喂给角色分析；这些是市场上下文，不等于行业超额收益或持续强度。 | 根未见 |
| 20 | [ZhuLinsen/daily_stock_analysis](https://github.com/ZhuLinsen/daily_stock_analysis/tree/f4d9956c527562ba08a1dbc2ca6d20e6b25d4756) `f4d9956c5275` | 深入 | `src/agent/risk_override.py`；`src/agent/skills/aggregator.py`；`strategies/growth_quality.yaml` | 风险否决状态转换、证据不足转hold、成长质量提示；采纳跨阶段一致性，继续由程序控制执行门槛。 | MIT |
| 21 | [CatsJuice/quant-on-volume](https://github.com/CatsJuice/quant-on-volume/tree/2a17ec1114c4789cd44148916bb630be6998e625) `2a17ec1114c4` | 深入 | `analyze_data/hly_vol.py`；`analyze_data/verify_hly_buy.py` | 量价同向按正负1/2累积分，偏好连续上涨放量；不增加到已有高度相关的动量评分。 | 根未见 |
| 22 | [Stardustsky/Saistock](https://github.com/Stardustsky/Saistock/tree/2e35f7538967e18f06cd7ff8876c91e208ba76de) `2e35f7538967` | 定向 | `module/stock_self.py` | 市值/PE乘数式评分，缺少对应校准证据；不沿用分档作为盈利质量。 | 根未见 |
| 23 | [oficcejo/tradingagents](https://github.com/oficcejo/tradingagents/tree/a8f95cfeaf5b6fee2093b8d5fcabc9d67b3dff8b) `a8f95cfeaf5b` | 深入 | `tradingagents/graph/signal_processing.py` | 文本抽价失败后按固定涨跌百分比估目标；不采用，程序共同价位和unavailable更可核验。 | Apache-2.0 |
| 24 | [ccsdu2004/snailstock](https://github.com/ccsdu2004/snailstock/tree/ba45cea006f4910e25871e95c3960e86fc579f56) `ba45cea006f4` | 定向 | `SnailAnalyst/indicator/StockMACDScanner.cpp` | 核对MACD交叉扫描，现有策略覆盖；旧源码非UTF-8，无法可靠辨识的注释未计证据。 | GPL |
| 25 | [victorgau/PyConTW2018Tutorial](https://github.com/victorgau/PyConTW2018Tutorial/tree/445e660f4f1ae693826c7a53f72d67d665e0b132) `445e660f4f1a` | 定向 | `07. system/all_strategies.py` | RSI/布林进出场和信号移位教程；作为时点检查线索，不视为A股执行回测证明。 | 根未见 |
| 26 | [wbsu2003/stock-scanner-mcp](https://github.com/wbsu2003/stock-scanner-mcp/tree/8ffcdfb1f50fc2b74e6cc185fa1ed9bf7143aa85) `8ffcdfb1f50f` | 深入 | `services/stock_scorer.py` | MA/RSI/MACD/量比固定加总再映射强烈推荐；本项目继续分组限幅，不复制推荐阈值。 | MIT |
| 27 | [xingetouzi/jaqs-fxdayu](https://github.com/xingetouzi/jaqs-fxdayu/tree/01ae42855ec4da22211eb3fd475288126ebbf341) `01ae42855ec4` | 深入 | `jaqs_fxdayu/research/signaldigger/multi_factor.py` | IC、收缩协方差和按持有期移位的权重；时间因果纪律已有，动态权重须真实时间外验证后启用。 | 根未见 |
| 28 | [real-time-machine-learning/1-pandas-intro](https://github.com/real-time-machine-learning/1-pandas-intro/tree/990ab2519f061e54e6bb2caf903164fc0554d378) `990ab2519f06` | 入口 | `example.py` | Pandas时间戳与股票数据清理教程，不提供可直接迁入的推荐算法。 | Apache-2.0 |
| 29 | [aahl/mcp-aktools](https://github.com/aahl/mcp-aktools/tree/6c46a0613bdadbb50a27020274a908e33cb7c793) `6c46a0613bda` | 定向 | `mcp_aktools/__init__.py` | 行情/历史数据和缓存工具封装；数据可访问性不等于推荐质量，未增加外部数据依赖。 | MIT |
| 30 | [Hansonnnnn/Quantour](https://github.com/Hansonnnnn/Quantour/tree/020a797cc0b4633b4b51c235101d28e3647f94bf) `020a797cc0b4` | 定向 | `Quantour/src/main/java/data/StockStrategyServiceImpl.java` | 区间收益与股票选择数据服务；缺样本置零的示例不采用，保持缺失和真实零分开。 | 根未见 |
| 31 | [niaicmy/gpfx](https://github.com/niaicmy/gpfx/tree/d67e4b990314feb600a400ea5c99669b960e8ce5) `d67e4b990314` | 定向 | `stock_data_process.py` | 多周期MACD/TRIX扫描；不引入首下标读取尾元素等时序风险，现有完整窗口规则优先。 | 根未见 |
| 32 | [freedream520/stock-technical-analysis](https://github.com/freedream520/stock-technical-analysis/tree/1eb2638cd6c7b93ec4c6b75633b3fe9049d1b86b) `1eb2638cd6c7` | 定向 | `technical-analysis.py` | 收益率和Williams指标教学；仅作指标定义对照，不将单序列教学视为收益检验。 | MIT |
| 33 | [wjt0321/china-stock-analyst](https://github.com/wjt0321/china-stock-analyst/tree/42c277cda5eb44e7d44b2d3cf7fc34ad13f9712c) `42c277cda5eb` | 深入 | `desktop/analysis_engine.py` | ATR止损、流动性风险与专家投票；ATR/结构保护已有，不沿用把数据不足映射50分的合成。 | MIT |
| 34 | [yuanfengyun/stock](https://github.com/yuanfengyun/stock/tree/8e5737dd5dfc75e82d4ba03787611b7195ff0904) `8e5737dd5dfc` | 入口 | `database/cash_statement.py` | 财务采集入口，不执行其脚本、连接配置或会话信息；不从采集字段推断已有质量模型。 | 根未见 |
| 35 | [nzai/Tast](https://github.com/nzai/Tast/tree/7ea7f5deec62721aa119521ad09b247d84aea951) `7ea7f5deec62` | 深入 | `turtle/index.go`；`trading/turtle.go` | 海龟/TR示例；所读递推没有更新prevn且交易测试为占位，不替换已验证的ATR与退出执行器。 | MIT |
| 36 | [tt07406/StockiiPanel](https://github.com/tt07406/StockiiPanel/tree/b3332867fe88c7c207323165762fc7abc5ca2875) `b3332867fe88` | 入口 | `StockiiPanel/Form1.cs` | 桌面面板/事件入口，未提供本次所需的排序与退出规则。 | 根未见 |
| 37 | [jerrylee529/twelvewin](https://github.com/jerrylee529/twelvewin/tree/f26a943e810f1999036e7d8d8801c14acc39c679) `f26a943e810f` | 深入 | `analysis/technical_screens.py` | 均线筛选将不足窗口均值置零，会使短史股票站上MA250；本项目保持NaN及完整窗口。 | Apache-2.0 |
| 38 | [csxiaoyaojianxian/StockTech](https://github.com/csxiaoyaojianxian/StockTech/tree/e00edc4a6c871f70db95cdad045d45b8ec28ddd6) `e00edc4a6c87` | 入口 | `StockTech/Py/StockEngine.cs` | 脚本引擎和指标数据桥接；不直接提供新的候选质量判定。 | 根未见 |
| 39 | [AdvancingStone/stock-data-analysis-and-prediction](https://github.com/AdvancingStone/stock-data-analysis-and-prediction/tree/a596e43c5bb5528b9e27fb5b17dd4a4d11b6a511) `a596e43c5bb5` | 入口 | `src/main/python/com/bluehonour/spider/single_stock_fund_flow.py` | 资金流采集与归档入口；资金流数据不自动等于方向预测，当前量价核验已覆盖。 | Apache-2.0 |
| 40 | [wellwind/MyStockAnalyzer](https://github.com/wellwind/MyStockAnalyzer/tree/c8b9f24c0ffbced142b7365a5e974a35303d1eec) `c8b9f24c0ffb` | 深入 | `MyStockAnalyzer.StockSelectionAlgorithms/BollingerFistAlgorithm.cs` | 压缩区间、放量突破与涨停排除；布林收口已覆盖，本轮补更短窗口NR7并保留收盘确认。 | 根未见 |
| 41 | [chenwr727/Stock-Insight-AI](https://github.com/chenwr727/Stock-Insight-AI/tree/6db07cc72776175fb9460492f6eaa647baddea70) `6db07cc72776` | 定向 | `core/llm/stock.py` | 资讯与趋势分段提示、时点和观察条件；不引入视频输出，保留本项目字段/数字核验契约。 | 根未见 |
| 42 | [mpquant/Ashare](https://github.com/mpquant/Ashare/tree/7ef1ce07579416d5c9e58f713867132bbaa9390d) `7ef1ce075794` | 深入 | `MyTT.py` | 通达信式指标公式对照；其DMI为SUM/MA口径，本轮显式采用Wilder版并独立手算验证，避免同名混用。 | 根未见 |
| 43 | [lakaxile/daily_stock_analysis](https://github.com/lakaxile/daily_stock_analysis/tree/1e53c39d54cb8640ecf07ee560979442aad98610) `1e53c39d54cb` | 定向 | `scripts/strategy_scanner_v2.py` | 大盘MA20/MA5与MA10环境过滤；数据失败直接放行的路径不采用，缺失不当成环境良好。 | MIT |
| 44 | [huang1125677925/stock_analyse](https://github.com/huang1125677925/stock_analyse/tree/379f5ffdb8c27cd7ae27e74ae049792156767d0b) `379f5ffdb8c2` | 定向 | `vue-frontend/src/services/strategyApi.ts` | 多周期RPS接口与展示，所读项目未含对应后端公式；不从接口名称反推算法或行业历史。 | 根未见 |
| 45 | [1254011956/-PyQt5-](https://github.com/1254011956/-PyQt5-/tree/b1cb4331363fe23817d8d105006f4a28a6d4985d) `b1cb4331363f` | 定向 | `StockData_System/gui/stockindex/Stock_index.py` | 均线/RSI/KDJ计算示例；不采用不足窗口填充来形成可执行信号。 | 根未见 |
| 46 | [xieyan0811/xystock](https://github.com/xieyan0811/xystock/tree/745af023448f308d39171e38c1f04ad0548f6211) `745af023448f` | 深入 | `utils/risk_metrics.py` | 波动、回撤、Sharpe、VaR/CVaR；保留已有成本/成熟样本/日期分组，不以孤立风险分档替代退出计划。 | MIT |
| 47 | [hengruiyun/AI-Stock-Master](https://github.com/hengruiyun/AI-Stock-Master/tree/82f6050c8cafe3f168000ad97a16c573c6206ea5) `82f6050c8caf` | 深入 | `algorithms/ai_enhanced_signal_analyzer.py` | 所读增强模块将trained/accuracy设常量，并按洞察关键词调整分数；这些数字不是验证结果，不采纳。 | AGPL |
| 48 | [Austin-Patrician/eastmoney](https://github.com/Austin-Patrician/eastmoney/tree/e293bf7effad2898e996c724ad3c8faa8b3a4f9a) `e293bf7effad` | 深入 | `src/analysis/recommendation/stock_engine/strategies/long_term.py`；`src/analysis/portfolio/signals.py` | 质量/成长/估值分组与现金转换；采纳先区分利润性质，缺证据不补中性50，未复制非商业许可代码。 | 非商业自定义 |
| 49 | [louis-xie-programmer/go-stock-analyzer](https://github.com/louis-xie-programmer/go-stock-analyzer/tree/f0ef2f0cf447374803d6b324c6c28f66883edf6c) `f0ef2f0cf447` | 定向 | `backend/strategy/ma_strategy.go`；`backend/strategy/macd_strategy.go` | 持续站均线与MACD交叉的纯规则接口；已有覆盖，不重复添加同名策略。 | 根未见 |
| 50 | [zyy5411/SmartStocker](https://github.com/zyy5411/SmartStocker/tree/06dff152948dd9f9844ed5c84d7b36aee0eb34c4) `06dff152948d` | 入口 | `src/zyy/stocker/spider/consumermode/AnalysisThread.java` | 消费数据源并存储的工作线程，不是推荐/卖出算法。 | 根未见 |

## 4. 补充 GitHub 搜索与对照

2026-09-14 通过 GitHub 公共 API 检索技术指标、量化研究项目，并核对下面三个成熟项目的元数据与源码。星数仅用于说明关注规模，不作为算法效果证据。新参考目录为 `D:\TestWorkSpace\_refs\stock-algorithm-supplement`；未覆盖原 50 项快照。

| 项目 | API查询时星数 | 固定提交 | 已读根许可证 |
| --- | ---: | --- | --- |
| [mementum/backtrader](https://github.com/mementum/backtrader/tree/b853d7c90b6721476eb5a5ea3135224e33db1f14) | 23,246 | `b853d7c90b67` | GPL |
| [microsoft/qlib](https://github.com/microsoft/qlib/tree/79633dd9506ea689e5400dea0197717b5b3d74b7) | 48,548 | `79633dd9506e` | MIT |
| [quantopian/alphalens](https://github.com/quantopian/alphalens/tree/77084f1e4c2c0be407e032d444fb19e4be4b0f37) | 4,445 | `77084f1e4c2c` | Apache-2.0 |

- **Backtrader**：`backtrader/indicators/directionalmove.py`、`rsi.py` 对照方向变动、Wilder 平滑、ADX 只有强度没有方向，以及 RSI 平盘的零分母处理。新增 DMI 根据公开数学定义独立实现，手算小周期样本验证种子与递推，不引入其 GPL 源码或运行库。
- **Qlib**：`qlib/contrib/strategy/signal_strategy.py` 检查上一交易步信号、可成交过滤和 TopK 换仓；`qlib/data/dataset/processor.py` 检查训练区间拟合。保留本项目已有的时间外拟合和“先按当时信号选中，再模拟未来成交”，不使用未来可成交性补选下一只。
- **Alphalens**：`alphalens/performance.py` 检查按日期计算 Spearman IC、行业调整；`utils.py` 检查前瞻收益移位及收益异常值过滤的前视偏差提示。行业调整可作为后续研究方向，但在缺少一致的历史行业身份时不把当日候选样本分位冒充行业相对强度。保留现有日期分组、成熟标签、purge/embargo 和时间外比较。

## 5. 验证记录

2026-09-14 完成以下验证，均通过：

| 验证 | 执行位置与结果 |
| --- | --- |
| `go test -json -p 1 ./... -count=1` | `server`，全量测试通过 |
| `go vet -p 1 ./...` | `server`，静态检查通过 |
| `npm test` | `web`，前端回归通过 |
| `npm run type-check` | `web`，类型检查通过 |
| `npx vite build --outDir .audit-top50-dist` | `web`，生产构建通过；仍有既有的大 chunk 提示 |
| `git diff --check` | 仓库根目录，差异格式检查通过 |

新增及扩展的回归覆盖盈利金额正负、真实零与缺失、同季基期和财务冻结、评分及执行门控、新策略完整窗口与 Wilder 平滑、程序价位与兼容提示词分支、策略指标证据编号，以及旧排序政策和模型特征版本隔离。前端同步验证盈利摘要。另修正预热测试夹具生成周末日线的问题，未为迁就测试修改生产资金流规则；定向复验和最后一次全量测试均通过。

Go 测试子进程清空了 `LIVE_*`、`QV_REVIEW_MYSQL`、`SQL_DSN`、`MYSQL_DSN`，AI 请求使用本地 mock。本轮临时前端构建产物 `web/.audit-top50-dist` 不纳入提交；工具自动审批拒绝删除该目录，因此本次未完成其清理。三个补充参考仓库作为研究资料保留。

后续收益评估应使用积累的新版本不可变事实，按策略、市场阶段和持有周期比较扣费收益、超额收益、严重亏损、覆盖率和实际可执行比例。此次没有以合成样本、模型自报置信度或参考项目 README 的宣传数字替代真实市场验证。
