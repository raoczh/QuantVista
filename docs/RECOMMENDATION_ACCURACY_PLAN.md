# QuantVista 推荐准确性长期优化规划（合并版）

> 2026-07-16 定稿。由两轮独立调研合并而成：一轮聚焦"测量口径 / point-in-time / 模型化排序"（源码证据索引见 §12），
> 一轮聚焦"反思记忆 / 择时护栏 / 对抗复核 / 仓位输出"（参考代码索引见 §11）。
> 背景：用户跟随推荐买入两只股票整体亏损。本方案把"推荐准确性"从主观体验升级为**可测量、可回放、可校准、可持续迭代的工程闭环**，作为长期目标分七个阶段实施。
> 本方案不构成投资建议。

## 0. 总路线（一句话版）

```text
S0 先把结果测准（口径/标签/统一执行器/影子标签/确定性错误归因/PIT 基础与历史宇宙）
S1 同时止血（执行纪律 + 仓位输出 + 组合去相关；大盘闸门先影子运行）
S2 反馈闭环（确定性归因先行，LLM 反思/反方复核先影子结算、证明增量后才赋予强制效力）
S3 历史重放与验证（每日快照 + 候选池召回评估 + 因子 IC + walk-forward 基线）
S4 模型化横截面排序（净收益+Alpha 双标签，线性 → LightGBM）
S5 逐模型概率校准与"不推荐"门槛 → 锁定测试集验收 → 影子运行 + champion/challenger 发布
S6 自动因子研究与策略进化（防过拟合硬门槛）
```

原则：
- **凡是能用代码验证的绝不交给 LLM 自觉**（延续本项目信任层哲学）；
- S0 完成前不再手调评分权重（没有准的尺子，调了也不知道好坏）；
- **任何新门控/复核/记忆机制先影子运行**：同时保存门控前动作与分数、门控后动作、被改写标的的后续影子收益、推荐覆盖率、gated 与 ungated 配对结果——证明有真实增量后才赋予强制效力。否则"少发 buy 就能做高表面胜率"，显示的胜率升高不代表选股能力提高；
- S1/S2 的程序件与 S0 并行，S3 起有严格先后依赖；
- 每期独立可验收；个人自用成本敏感，单次生成的 LLM 调用次数受控（当前 1~2 次，上限 3 次）。

## 1. 问题定性：为什么"跟着买会亏"

对照源码逐层检视，准确率问题 = **三个测量缺陷 + 六个结构性缺口**。

### 1.1 测量缺陷（先修，否则一切优化无法评价）

| # | 缺陷 | 源码证据 | 影响 |
|---|------|---------|------|
| M1 | 胜率把 `watch`、未到期 `active` 混入分母分子 | `tracking.go:485-546`、`tracking_test.go:312-335` | "成功率"不代表跟买结果 |
| M2 | 追踪用生成时 `RefPrice`，不用实际买入价；线上追踪与批次回验入场口径不同 | `tracking.go:276-320`、`position.go:302-322`、`backtest.go:868-1005` | 用户真实盈亏无法反哺推荐器；两套指标不可比 |
| M3 | 前复权历史随最新价重锚，历史计划价/收益错位；只刷新近 90 天但 Performance 查询全部状态 | `tracking.go:480-483`、`tracking.go:185-193` | 历史标签被污染 |

另有两个顺手修的准入 bug：自选股 `Amount=0` 绕过最低成交额门槛（`recommendation.go:1082-1084,1134-1144`）；`success` 与 `degraded` 批次统计未分开。

### 1.2 结构性缺口

| # | 缺口 | 现状证据 | 后果 |
|---|------|---------|------|
| G1 | **无学习闭环** | tracking 已算出 return/alpha/胜率，但从不反哺生成端 prompt | 同类错误（高位追热点）每次重犯 |
| G2 | **评分权重从未被验证** | 五维权重 0.30/0.25/0.15/0.15/0.15（`score.go:29-36`）与 s8 几十个加分项全是人工常数 | 量化排序本身可能是弱信号，LLM 在弱排序上精选自然不准。五类具体失真（S3 权重校准时逐项检查）：①"价值低估"仍由趋势动量主导（占 55%），估值/ROE 奖金改变不了评分本质；②"强势回踩"仍奖励更高区间位置，真正靠近支撑的标的反而排后；③"热点活跃"在基础量能分与策略放量奖金中重复计分；④趋势/多头排列/创高/MACD/涨幅高度相关，人工相加重复计权同一风险暴露；⑤固定阈值不随市场波动/行业/市值横截面归一化，同一个 70 分在不同市场阶段不是同一含义 |
| G3 | **候选池各来源贡献未经历史验证** | 短线来源以涨幅/成交额/换手榜为主（`recommendation.go:179-234`）——已有"不热"方向（回调/低PB榜）与全市场 strategy_signal 来源对冲，但**没有任何来源级召回评估**（好股票有没有进池、哪路来源贡献了 alpha、被过滤者后续表现如何均未测量）；且"价值低估"策略排序仍由趋势动量主导、"强势回踩"仍奖励高区间位置 | 供给结构可能偏热偏晚（情绪高位入场）却无法证实或证伪——跟买亏损的候选根因之一 |
| G4 | **大盘择时只是 prompt 一句话** | `buildMarketContext` 只把"MA60 下方"注入 prompt，无程序性硬约束 | 弱市模型照样给满 5 个 buy，用户在系统性下跌里满仓跟买 |
| G5 | **只荐标的、不管仓位与组合** | recPick 无仓位字段；同批次无行业/相关性约束 | 两只同风格股一起亏，个股风险放大成账户风险 |
| G6 | **单视角单轮 LLM，置信度未校准** | verify 复核可选且同视角；confidence 是模型自报数字非概率 | 无结构性反方压力；"高置信"不等于高命中率 |

### 1.3 关于那两只亏损股

本地 `server/quantvista.db` 推荐相关表均为空（部署库才有数据），无法直接复盘。**不应因两个样本就手调阈值**——正确做法是 S0 落地后把这两个案例纳入统一标签与错误归因，与足够多的成熟样本一起判断是候选池偏差、排序错误、同风格暴露、执行价差还是系统性下跌。

## 2. 调研来源与核心启示

两轮调研共覆盖 6 个原有项目 + 13 个新拉项目。参考代码位置：
`D:\TestWorkSpace\_refs\`（TradingAgents、TradingAgents-astock、ai-hedge-fund、qlib、daily_stock_analysis、alphaevo、alphasift）与
`D:\TestWorkSpace\_refs\_related_rounds\`（round1: akquant、DR-lin-eng/stock-scanner、TradingAgents-AShare；round2: qlib、TradingAgents、FinMem；round3: RD-Agent、FinGPT；round4: FinRL）。

### 2.1 每个来源一句话结论

| 项目 | 对准确性的核心启示 |
|---|---|
| **TradingAgents**（62.6k★） | 反思记忆闭环：决策落日志→到期回填真实收益/alpha→LLM 反思恰好 2-4 句教训→未来 prompt 注入 5 条同票+3 条跨票教训（`agents/utils/memory.py`、`graph/reflection.py`）；结构化对抗：多空辩论→5 档裁决→交易员→三方风控 |
| **TradingAgents-astock** | A 股专属信息维度（政策/游资/解禁）**显式接线**进辩论；quality_gate.py 的"必采清单+ABCDF 分级"检查思路（注意：它只把质量摘要注入后续 prompt，**并未程序化封顶置信度或禁止交易**——硬门是本项目自己的设计）；A 股 bear 论据框架（T+1 锁仓/解禁悬顶/游资撤退） |
| **ai-hedge-fund** | 风控是纯程序：年化波动率分档给单票仓位上限（<15%→25%仓位…>50%→10%）× 相关性乘数（≥0.8→×0.70）；LLM 只能在程序预校验的动作空间里选（`src/agents/risk_manager.py:222-330`） |
| **qlib** | 研究纪律而非模型：train/valid/test 严格按时间分离、IC/RankIC/ICIR 评价排序能力、横截面 Top-K 组合、含涨跌停与成本回测、定期滚动重训；Alpha158 因子系统性生成再筛选 |
| **daily_stock_analysis** | 大盘护栏是程序不是提示词：保守环境下代码扫描并强制改写"立即买入"→"观望"（`src/daily_market_context_guardrail.py`；注意它依据的是**文本风险标签**而非 MA/涨跌家数/资金流计算——本项目的三档行情 regime 是新设计，需先影子验证）；15 个 A 股策略 YAML |
| **AlphaEvo** | 策略参数用回测进化：LLM 失败归因→定向变异→再测留优；防过拟合门槛机制（train/val gap、walk-forward 通过率——其 README 中的具体数值是命令示例非通用标准，且 gap 指**胜率差**） |
| **AlphaSift** | 三层漏斗与本项目同构；LLM 的正确任务是"候选池内横向比较"而非逐票研判；数据质量低的行进最终风险扣分 |
| **AKQuant** | 模型生效时点硬约束：训练窗口收盘完成的模型最早用于**下一交易日**信号；禁止在测试段反复调参后仍称样本外 |
| **FinMem** | 经验记忆按相似度/时效/重要性/**实际盈亏反馈**调权，错误经验降权、长期无效淘汰（`puppy/memorydb.py:71-287`） |
| **RD-Agent** | 自动因子研究循环（假设→实现→评价→对比 SOTA）只能在离线隔离环境跑，必须加 Purge/Embargo/锁定测试集/多重检验，否则加速过拟合 |
| **FinRL** | 借鉴交易成本/风险状态/滚动验证；监督排序未稳定前**不上 RL** |
| **FinGPT** | 金融情绪可作特征；自然语言方向预测不可直接当买入结论 |
| **StockNova/StockAgent 等原有 6 项**（存量结论） | StockNova as_of 截断与次日开盘/涨跌停约束可迁移，但当前财务回填历史有泄漏（`factors.py:551-565`）；StockAgent 横截面 MAD/Z-score 可迁移，但其回测有未来函数（`backtester.py:208-260`）；stock-scanner 固定打分是反例；量化仓位公式 `clip(2.5/vol20,0.3,1.0)×择时系数` 未落地 |
| **反例警示** | TradingAgents-AShare 回测 SELL 方向符号重复处理、推荐日收盘入场不可成交、交易日历只排周末（`backtest_service.py:60-211`）——回测代码本身也要被审计 |

## 3. 重新定义"推荐准确性"（三层指标）

单一"胜率"不够用，目标拆三层：

**信号层**（每个固定持有期 1/5/10/20/60 日分别统计）：Precision@1/3/5（Top-K 中未来净收益/Alpha 为正比例）、RankIC/RankICIR、平均与**中位**收益、平均与中位 Alpha（基准：沪深300/上证）、MFE/MAE（最大有利/不利波动）、三重障碍标签（止盈先触/止损先触/到期未触）。

**概率层**：输出可校准概率——未来 5 日 P(alpha>0)、10 日 P(net_return>0)、有效期内 P(先止盈)；指标：Brier Score、ECE、可靠性曲线（预测 60% 的样本是否约 60% 命中）、每桶样本数与置信区间。

**组合与执行层**：扣费后组合收益/Alpha/波动/最大回撤/信息比率；行业集中度、风格暴露、组合相关性；可成交率、涨停跳过率、滑点；**用户实际成交收益 vs 模型模拟收益差异**。

> 长期主目标（**双重硬约束**，防"Alpha 为正但用户仍亏钱"：个股亏 2%、指数亏 5% 时 Alpha 为正但真实亏损）：
> ① 硬约束层：扣成本**净收益为正的概率**、亏损概率与尾部亏损（如 P5 分位亏损）必须受控；
> ② 优化层：在①满足的前提下，提高滚动样本外 Top-K 的扣成本 Alpha、收益中位数、RankIC 与概率校准度，控制最大回撤。
> 胜率仅作辅助指标。**短线与长线分开建模、分开校准、分开验收**（持有期/标签/基准/门槛互不共用）。

## 4. 目标架构

```text
Point-in-time 数据层（available_at/data_date/source/version）
  ↓
每日不可变特征快照（因子宽表 + 行业/市值/Beta/波动 + 市场状态 + 质量标记）
  ↓
全市场候选与横截面预处理（MAD 去极值 / Z-score / 行业市值中性化 / 高相关因子去重）
  ↓
排序层：手工评分 baseline → 线性/Logistic → LightGBM（+ 概率校准）
  ↓
门控层：市场状态硬护栏 + 数据质量门控 + "不推荐"阈值（0 推荐是正常输出）
  ↓
LLM 层：解释 Top-K、冲突检查、反方论证、受限 veto（不负责主排序）
  ↓
组合层：行业/相关性去重 + 波动率仓位预算 + 流动性容量
  ↓
统一执行模拟器（次日开盘/涨停买不到/T+1/跌停顺延/整百股/费率/滑点）+ 实际持仓血缘
  ↓
成熟标签（1/5/10/20/60 日 return/alpha/MFE/MAE/三重障碍）
  ↓
反馈层：反思记忆 + 错误归因 + IC 校准 + walk-forward 实验对比 + 策略进化
```

## 5. 分阶段实施

### S0 把结果测准（1~3 周；一切优化的前提）

**S0-1 推荐结果事实表 `recommendation_labels`**：
`(recommendation_id, horizon_days∈{1,5,10,20,60}, action, signal_date/signal_asof, entry_mode∈{next_open,buy_zone,actual_position}, entry/exit date+price, gross/net_return, bench_return/alpha, mfe/mae, hit_take_profit/hit_stop_loss, maturity_status∈{pending,matured,no_data,skipped}, position_id/actual_buy_price, label_version)`。
统计铁律：最终买入胜率只统计 `action=buy AND matured`；watch 单独统计"观察判断质量"；active/pending 不进分母；实际跟买结果与统一模拟结果**并列展示不混算**。

**S0-2 统一执行模拟器 `execution_sim.go`**：从 backtest.go 抽出信号日/下一交易日定位、涨停买不到、停牌、T+1、跌停顺延、整百股、佣金印花税滑点、固定持有期与止盈止损障碍——tracking、BatchBacktest、未来模型验证共用同一套执行语义。

**S0-3 复权与 point-in-time 基础（含历史股票宇宙）**：保存不复权 OHLCV+每日复权因子（短期至少保存生成时点的价格版本防重锚）；财务/业绩预告/解禁/新闻保存公告或首次可知时间，历史特征只取 `available_at <= signal_asof`。
**历史宇宙审计**（防幸存者偏差——`available_at` 不足以防"用今天存活的股票清单回放历史"）：逐步记录历史上市/退市状态、历史 ST 状态、停牌状态、行业与板块归属变更、指数成分变更、财务修订版本、公告盘中/盘后可用时点、分红除权与代码变更；历史回放的候选宇宙必须按 as_of 日重建。

**S0-4 口径 bug 修复**：自选股补成交额（实时或近 20 日中位数），Amount=0 不再绕过流动性门槛；Performance 加时间窗与版本过滤；success/degraded 分开报告；批次增加 `feature_schema_version/score_version/label_version/regime/data_quality_score`。

**S0-5 影子标签与反事实账本**：不止给入选的 pick 打标签——**被过滤、被门控改写、落选（rejected）标的同样记后续影子收益**。这是评估一切门控/复核/候选来源的地基：推荐覆盖率、错失机会率、gated vs ungated 配对结果、risk-coverage 曲线都从这里出。
反事实必须有独立数据结构支撑，新增 `recommendation_candidate_events`：
`(batch_id, symbol, candidate_stage, raw_score, raw_action, would_be_action, post_gate_action, gate_type, gate_version, rejection_reason, source, sent_to_llm, opportunity_set_id)`——否则日后无法稳定重建"没有门控会怎样"。
**两类事实分表**：①模型结果事实（统一模拟成交 + 后续标签，进 recommendation_labels，供训练与评估）；②用户执行事实（是否买、真实价格、提前卖出、执行偏差，挂 position 血缘）。**真实持仓含用户选择偏差，只用于执行差异分析，不得作为模型训练标签**。

**S0-6 确定性错误归因报表**（纯 SQL/Go，不依赖 LLM）：成熟样本按"入场特征桶（如 chg_5d 分位）× 市场状态 × 策略 × 来源 × 行业"分组统计胜率/中位收益/尾部亏损，定位系统性亏损集中在哪类推荐。LLM 反思（S2）是它的补充，不是替代。

验收：watch/active 不再混入买入胜率；实际持仓可按 recommendation_id 结算真实收益；线上追踪与回测同输入同结果；历史标签不受复权重锚影响。

### S1 止血护栏（1~2 周，与 S0 并行；直接针对"跟着买亏钱"）

**S1-1 大盘闸门（先影子、后强制）**：上证 MA20/MA60 位置 + 涨跌家数/涨跌停比 + 两市主力净流向 + 成交额分位，合成 `offense/neutral/defense` 三档落库进 batch。注意：三档判定是**本项目的新设计**（daily_stock_analysis 的护栏依据的是文本风险标签，不是行情计算——只借"程序改写而非 prompt 恳求"的思路），因此必须先影子运行：
- 影子期：regime 照算照落库、前端展示标签与提示，但**不改写 action**；同时按 S0-5 记录"若强制降级会改写哪些标的"及其影子收益；
- 转正条件：影子样本表明 defense 档被降级标的的配对收益显著差于保留标的（闸门确实在避免亏损而非错过盈利，看 risk-coverage 曲线），才启用强制 buy→watch；
- 强制期仍持续记录门控前动作/分数与门控后动作，防"少发 buy 做高表面胜率"。

**S1-2 仓位建议字段**（服务端程序计算，非 LLM 输出；单位口径必须显式）：目标波动模型——
`position_pct = min(单票上限, target_vol_annual / vol20_annual) × regime系数(1.0/0.6/0.3) × 相关性系数`
其中 `vol20_annual` = 近 20 日日收益标准差 × √252（**小数口径**，如 0.35 表示 35%）；`target_vol_annual` 建议 0.15~0.20；单票上限按波动分档（借 ai-hedge-fund 实测口径：年化波动 <15% → ≤25%，15~30% → ≤20%，30~50% → ≤15%，>50% → ≤10%）；相关性系数用与同批次/现有持仓的**真实收益相关性**分档（≥0.8→×0.70，0.6~0.8→×0.85）。最后**整批归一化**：Σposition_pct 超过**总仓位预算**（如 defense 30%/neutral 60%/offense 100%；这是仓位暴露上限，真正的组合风险预算——组合预测波动与边际风险贡献——留待 S5 后增强）时按比例缩。全部参数配置化，公式与参数写进批次快照可回溯。

**S1-3 批次组合去相关**：同行业最多 1~2 只；近 60 日收益相关性超阈值只保留分高者；小盘高波动风格不得占满整批。

**S1-4 执行纪律显式化**（无验证风险，可立即上）：推荐详情固定三条——buy_zone 外不追、止损价一键挂 alert（复用现有 alert）、T+1 首日不满仓。把"推荐胜率"与"用户执行"的偏差截住。

验收：regime 与影子改写记录落库可查；仓位字段单位正确、整批归一化生效、一键挂 alert 可用；同批次不再出现两只强相关票。闸门转正另按影子数据单独评审。

### S2 反馈闭环与对抗复核（依赖 S0 的成熟标签与影子账本；LLM 件全部先影子）

**排序原则**：确定性统计（S0-6 错误归因报表）先行——当前部署库推荐样本尚少，20/60 日成熟标签在前几周内不可能积累充分，**LLM 教训生成延后到成熟样本 ≥30 条再启用**，第一批只做确定性归因。

**S2-1 反思记忆闭环**（TradingAgents memory.py + FinMem 调权；启用门槛见上）：
- 新表 `recommendation_reflections(rec_id, symbol, strategy, rec_type, horizon_days, outcome, return_pct, alpha_pct, lesson TEXT, factor_digest, label_matured_at, available_from, reflection_version)`——`available_from` 保证记忆层历史回放不泄漏（只能注入 available_from ≤ 生成时点的教训）；标签成熟（matured）时触发一次轻量 LLM 反思，prompt 固定三问（方向对不对·引用 alpha 数字 / 论点哪部分成立或失败 / 一条可迁移教训），**限定 2-4 句**；
- 生成端 buildMessages 注入【历史战绩与教训】：事实层纯 SQL（来自 S0-6 报表：本策略成熟样本 n、胜率、平均 alpha、入场特征分桶胜率）+ 教训层（同票 ≤3 条 + 同策略 ≤5 条）；
- FinMem 式管理：教训按时效衰减、按后续验证的可靠度调权、长期无效淘汰；**只有成熟标签才进经验库**；记忆只影响解释/风险核查，不直接改量化分；
- 影子验证：注入教训前后的批次分开标记版本，配对比较成熟结果，无增量则撤。

**S2-2 反方研究员**（+1 次 LLM 调用，verify 升级）：对每只 buy 用独立调用构建最强 bear case（注入 astock 的 A 股 bear 论据框架：解禁/T+1 锁仓/高位放量/拥挤/估值），输出 `{symbol, bear_case, severity}`。**影子期**：bear_case 与 severity 只展示不改写 action/置信度，同时记录"若按 high→watch 执行会改写谁"及影子收益；证明否决的标的确实更差后，才启用程序裁决（high→降 watch+置信度≤40；med→置信度 -15）。

**S2-3 数据质量门控**（纯程序零成本，但**同样先影子**——改变信号的门控无一例外）：关键因子缺失面映射置信度上限。影子期只记录 `would_be_confidence_cap, missing_critical_fields, data_age, quality_gate_version` 不实际封顶，验证"被封顶的标的确实更不可靠"后再启用强制。缺失判定**按策略分场景**而非一刀切"缺三项"：长线缺财务应直接拒绝；短线缺最新财务不严重；行情/日线/成交额过期属通用严重问题；**情绪数据缺失 ≠ 情绪中性**（两者必须区分标记）。注：这是**本项目的原创硬门**——astock 的 quality_gate 实际只做报告长度/表格/缺失文字检查并注入后续 prompt，**并未程序化封顶置信度或禁止交易**，只借其"必采清单+分级"的检查思路。

**S2-4 LLM 职责收缩方向**：逐步不允许自报 confidence 覆盖程序合成置信度、不允许凭叙事把低排名标的抬成高置信 buy（S4 校准概率上线后彻底切换）。

验收：S0-6 归因报表上线；影子记录齐全；数据残缺票不再出现 high 置信。反思注入与反方裁决的**转正**各自凭影子配对数据评审。

### S3 历史重放与验证基线（3~6 周，依赖 S0-3）

**S3-1 每日全市场不可变快照**：收盘后固化因子宽表全部因子 + 行业/板块/市值/流动性/Beta/波动率 + 市场状态 + 各扩展数据的 data_date/age/质量标记 + **当日股票宇宙状态**（S0-3 历史宇宙字段）。禁止历史回测临时调"当前最新接口"填旧日期。

**S3-2 候选池召回评估**（排序优化之前先回答"好股票有没有进池"）：
- Recall@K：未来 N 日全市场 Top-K 收益股中，有多少进过当日候选池 / 进过 LLM 名单。**机会集（opportunity_set）必须限定为当日可交易、满足基本流动性门槛、扣成本后仍有意义的股票**——连续一字涨停等买不进的标的要剔除，否则污染召回率；
- 来源消融：逐路来源（watchlist/gainer/dipper/strategy_signal…）的增量贡献——去掉该来源后 Recall 与成熟收益掉多少；
- 被过滤/被"池满"/落选标的的影子标签（S0-5 账本）→ 错失机会率；
- 全市场机会集收益分布 vs 候选池收益分布对比。产出直接指导来源组合与名额分配的调整。

**S3-3 标签生成**（净收益与 Alpha 并列，落实双重硬约束）：
- 短线：未来 5/10 日**扣成本净收益**（回归 + `net_return>0` 分类 + 严重亏损 `net_return<-5%` 分类）、扣成本 Alpha（回归 + `alpha>0` 分类）、净收益分位（P10/P50）、三重障碍；
- 长线：20/60 日净收益与 Alpha、最大回撤；
- 评估层 Precision_net@K 与 Precision_alpha@K **分开报告**。短线/长线分开建模与验收。

**S3-3b 历史数据可用性分层**（walk-forward 需要 24~36 个月数据，而 PIT 快照从现在才开始积累——必须分层，否则"第 6~9 周完成模型验证"不可执行）：
- **A 类**：可从历史日线可靠重建的技术/量价因子（MA/动量/波动/位置/量能/ATR/筹码等）——**第一版 walk-forward 只用 A 类**；
- **B 类**：有公告日期、可按 available_at 可靠回填的财务/事件数据（业绩预告/财报/解禁）——第二版加入；
- **C 类**：资金流/人气/盘中/新闻情绪等无法可靠回填的数据——**只从接入日前向积累，不参与历史模型**，待积累够 walk-forward 窗口再入模；
- **D 类**：历史来源不明或可能泄漏的数据——禁止使用。

**S3-4 因子 IC 验证**（Go 实现 Spearman 秩相关即可）：对快照因子 × 未来 5/10/20 日收益算 RankIC 时间序列，输出各因子 IC 均值/ICIR/胜率，管理后台只读页展示排行。权重与加分项调整**只以样本外结果为准**：IC 不显著且 walk-forward 中剔除后不劣化的加分项才删（不设"必须删几个"的指标——为验收而强迫改动本身就是过拟合）；横截面预处理（MAD 去极值、Z-score、行业市值中性化、高相关因子去重、缺失值加 is_missing 特征）。

**S3-5 Walk-forward 框架**：训练 24~36 月 / 验证 3~6 月 / 测试 3~6 月，每 20 交易日滚动；Purge ≥ 最大持有期，Embargo 5~10 日；测试段只用于最终验收；模型/阈值/提示词版本只能用发布时点之前的数据选择（AKQuant 生效时点约束）。手工评分作为 baseline 跑通整套评估。

验收：召回/消融报表与 IC 页上线；权重调整凭样本外证据（有多少调多少）；"评分 Top10 组合"月度走查常态化。

### S4 模型化横截面排序（依赖 S3 数据/标签/baseline/walk-forward）

按"简单可审计"顺序：手工分 baseline → 横截面线性/岭回归（净收益与 Alpha 双标签）→ Logistic 预测 `P(net_return>0)` 与 `P(alpha>0)` → LightGBM 回归/LTR → 简单集成。训练用 Python 离线任务，**在线 Go 只加载冻结产物**（线性导出 JSON 权重 Go 推理；LightGBM 导出模型文本）；产物带训练截止日/特征 schema/标签版本/校准器/OOS 报告；模型失败回退已验证旧模型。第一版特征只用 A 类（见 S3-3b）。

### S5 逐模型概率校准、门槛与发布（依赖 S4——校准必须针对已训练完成的具体模型，不同模型分别校准）

顺序固定：`训练完成的模型 → 用验证折/OOF 预测做校准（小样本优先 Platt，样本充足再考虑 Isotonic）→ 选"不推荐"门槛与 Top-K → 锁定测试集最终验收 → 影子运行`。
- 门槛体系（双重硬约束落地）：**净收益硬约束优先**——最低 `P(net_return>0)`、最高严重亏损概率 `P(net_return<-5%)`、净收益 P10 下限，**不满足时程序拒绝推荐**；满足后再看最低 `P(alpha>0)`/预测收益门槛；
- 前端展示校准概率对应的历史真实命中率、样本数、置信区间；市场状态或数据质量不达标时**0 个推荐是正常输出**（文案"今日无合格标的+原因"）；
- 市场状态路由：弱势/高波动提门槛减 Top-K；趋势上行允许动量但加拥挤/高位反转过滤；震荡加权回踩策略。

发布流程：`离线实验 → 锁定测试集验收 → 影子运行（不影响用户）→ 与旧模型并行结算 → 达样本量与风险门槛 → 启用 → 可一键回滚`。
**运行期治理**：champion/challenger 并行结算常态化（新模型永远先当 challenger）；监控模型漂移（滚动 RankIC 衰减）与校准漂移（滚动 Brier/ECE）；预设重训频率与触发条件；预设**退役标准**（连续 N 个滚动窗口劣于 baseline 即退回旧模型）。
**门控/模型转正的评价协议必须预注册**：评价窗口、统计方法、最低有效样本量、覆盖率下降上限、多重检验方法在影子期开始前锁定——不允许看完结果再换阈值。

### S6 自动因子研究与策略进化（长期）

- 策略加分规则参数化落库（沿用 prompt 注册表 settings 覆盖模式）；
- AlphaEvo 循环：月度走查失败归因 → LLM 提议 ≤3 处定向变异 → BatchBacktest 对比 → **人工审核采纳**；
- RD-Agent 式因子假设实验仅限离线隔离环境；
- 防过拟合硬门槛：多个 walk-forward 折稳定优于现有模型、提升需同时体现在净收益/Alpha/RankIC/回撤/校准（不接受只提升单一胜率）、影子运行 1~3 个月、最小成熟样本量。具体阈值（AlphaEvo 命令示例给的 train/val **胜率差** ≤0.12、walk-forward 通过率 ≥0.5、≥30 信号）是**初始候选参数而非通用标准**——必须配置化并在本项目数据上重新验证后再定。

## 6. 数据与特征优先级

**先用足已有**：技术指标全家桶、筹码、主力资金流、龙虎榜/机构席位/人气、盘中 VWAP/尾盘量价、PE/PB/财务摘要、新闻情绪。

**优先新增（必须可 point-in-time）**：历史 PE/PB 分位（而非单日绝对值）、业绩预告（含公告时间/方向/幅度）、股东户数变化、解禁占流通市值比与距解禁日、涨停池（封板资金/炸板/连板高度）、行业相对强弱与行业内横截面排名、市值/Beta/残差波动分位。上游接口速查见 REFERENCE_ANALYSIS §6。

**暂缓/不做**：北向资金因子（2024-08 起上游行业性断供，astock Week5 实证）；无历史积累的 T-1 信号做伪历史回测；FinGPT 自然语言方向预测直接当结论；RL 选股；16 节点全流程辩论（30~50 次调用撞 60s 超时，S2 反方研究员是最小对抗形态）；深度学习起步（简单模型未稳定胜过 baseline 前不加复杂度）。

## 7. 代码级落地清单（第一批）

- `server/model/recommendation_label.go`：标签事实表 + `recommendation_candidate_events` 反事实事件表
- `server/service/execution_sim.go`：统一执行模拟器
- `server/service/tracking.go`：改写成熟标签；用户执行事实单独记录（不作训练标签）
- `server/service/backtest.go`：复用统一执行器
- `server/service/recommendation.go`：regime 判定与**影子记录**（不改写 action，强制模式由 feature flag 控制默认关闭）、position_pct、组合去相关、自选 Amount 修复
- `server/service/reflection.go`（新）：反思表结构可先建；**反思生成与注入延后**（启用门槛见 S2）
- `server/service/recfilter.go`/`riskgate.go`：数据质量门控**影子输出 would_be_cap**
- 测试：watch/active 不计入买入胜率、实际成交价、成熟周期、**影子模式只产生 would_be_action 不改真实 action、feature flag 默认关闭、质量门只记 would_be_cap 不实际封顶**、仓位公式单位与整批归一化等表驱动用例

**前端指标（随各阶段逐步交付，推荐页新增）**：
- 成熟样本数、按固定周期分列的买入胜率、平均/中位 Alpha（S0 后）
- 市场状态（regime）标签与"不推荐/降级"原因、建议仓位与执行纪律三条（S1 后）
- 历史战绩段与教训、bear_case、数据质量与时点标记（S2 后）
- 校准概率及其对应的历史命中率区间与样本数（S4 后）
- 实际成交 vs 模拟成交差异、同批次行业/相关性暴露（S0/S1 后）

## 8. 排期建议

| 周 | 内容 |
|---|---|
| 1~2 | S0 口径修复 + 标签事实表 + 统一执行器 + 持仓成交反馈 + **影子标签账本**；S1 执行纪律/仓位/组合去相关（并行），regime 闸门**影子上线** |
| 3~5 | S0-6 确定性错误归因报表；S2 数据质量门控 + 反方研究员（**影子模式**）；PIT 快照与历史宇宙字段开始每日积累 |
| 6~9 | S3 候选池召回评估 + IC 验证 + walk-forward 基线（**A 类因子重建历史**，见 S3-3b）+ 凭样本外证据的权重校准 |
| 10~12 | S4 模型化排序（净收益+Alpha 双标签）；闸门/反方/质量门按影子数据评审转正；成熟样本 ≥30 后启用 LLM 反思教训 |
| 之后 | S5 逐模型校准 + 门槛 + 锁定测试集验收 + 影子运行（champion/challenger）→ S6 进化循环 |

注：S3 依赖快照历史积累，**越早开始每日固化快照越好**（S0-3 与 S3-1 的落库动作应提前到第 1 批一起做，即使消费端后置）。

## 9. 验收标准汇总

- **S0**：四条（见 §5 S0 验收）全绿；每条推荐可追溯完整时间/数据/版本。
- **S1/S2**：regime 与影子改写记录落库；仓位字段单位正确且整批归一化；alert 一键创建；数据残缺票无 high 置信；任何强制改写（闸门/反方否决）的转正凭 gated vs ungated 影子配对数据评审——覆盖率下降但收益不改善即回退。
- **S3~S5 模型验收**：多个 walk-forward 测试折中 RankIC/ICIR、扣成本**净收益**与 Alpha 稳定优于手工 baseline（Precision_net@K 与 Precision_alpha@K 分开达标）；Top-K **净收益中位数**为正且严重亏损概率受控（不许靠少量大涨样本拉均值）；回撤不恶化；校准后 Brier/ECE 显著改善；弱市/强市/震荡与不同行业市值桶无明显失效；每个阈值有最小成熟样本量，不足显示"不确定"。

## 10. 最终建议（先做什么）

第一批只聚焦六件事：①成功率口径修正；②标签事实表（含影子标签账本）与实际成交反馈；③统一执行模拟器；④PIT 快照与历史宇宙开始积累；⑤执行纪律+仓位输出+组合去相关（闸门影子上线）；⑥确定性错误归因报表。
完成后 QuantVista 才真正具备"长期提高推荐准确性"的地基——否则继续加因子、Agent 或提示词，只会让推荐理由更丰富，不会证明推荐更准。

## 11. 参考代码索引 A（D:\TestWorkSpace\_refs\）

> 注：两个索引中的行号均基于 2026-07-16 浅克隆时点的本地副本，上游更新后可能漂移；实施时以本地 `_refs` 副本为准。Commit 锚点：
> TradingAgents `01477f9`、TradingAgents-astock `e6b32a4`、ai-hedge-fund `09dd331`、qlib `d5379c5`、daily_stock_analysis `5594653`、alphaevo `712e60b`、alphasift `9f52274`；
> _related_rounds：akquant `1c565bc`、stock-scanner-upstream `de6a79e`、TradingAgents-AShare `06a0e28`、FinMem `be814aa`、RD-Agent `4f9ecb0`、FinGPT `3799a0f`、FinRL `2334a5f`（round2 的 qlib/TradingAgents 与上同）。

| 项目 | 看什么 |
|---|---|
| TradingAgents | `agents/utils/memory.py`（反思日志全实现）、`graph/reflection.py`（三问反思 prompt）、`agents/managers/research_manager.py`（5 档裁决） |
| TradingAgents-astock | `CHANGES_FROM_UPSTREAM.md`（施工日志）、`agents/quality_gate.py`、`agents/risk_mgmt/*`（A 股风控辩论框架） |
| ai-hedge-fund | `src/agents/risk_manager.py:222-330`（波动率仓位+相关性乘数）、`src/agents/portfolio_manager.py`（预校验动作空间） |
| qlib | `qlib/contrib/data/loader.py`（Alpha158）、`contrib/report/analysis_position/score_ic.py` |
| daily_stock_analysis | `src/daily_market_context_guardrail.py`、`strategies/*.yaml` |
| alphaevo | README「How Self-Evolution Works」、防过拟合参数 |
| alphasift | `README.zh-CN.md`（三层漏斗、LLM 横向比较任务设计） |

## 12. 参考代码索引 B（源码证据，含 _refs\_related_rounds\）

### QuantVista 自身
推荐主流水线 `recommendation.go:477-660`；五维权重 `score.go:29-36`；位置越高分越高 `score.go:163-178`；策略加减分 `recfactor.go:231-480`；候选榜单来源 `recommendation.go:179-234`；自选缺成交额 `recommendation.go:1082-1084,1134-1144`；胜率混入全部状态 `tracking.go:485-546`（回归测试 `tracking_test.go:312-335`）；RefPrice 口径 `tracking.go:276-320`；持仓血缘 `position.go:302-322,400-446`；前复权重锚 `tracking.go:480-483`；批次回验 `backtest.go:868-1005`。

### 关联项目
Qlib 时间切分与含成本回测 `round2/qlib/examples/benchmarks/LightGBM/workflow_config_lightgbm_Alpha158.yaml:4-71`、IC/RankIC `qlib/workflow/record_temp.py:295-349`、横截面 Top-K `qlib/contrib/strategy/signal_strategy.py:138-200`；AKQuant walk-forward 与下一 Bar 生效 `round1/akquant/python/akquant/strategy_ml.py:72-184,244-269`；TradingAgents 结算与反思 `round2/TradingAgents/tradingagents/graph/trading_graph.py:251-334`、`graph/reflection.py:14-57`；FinMem 记忆调权 `round2/FinMem/puppy/memorydb.py:71-287`；RD-Agent 实验循环 `round3/RD-Agent/rdagent/components/workflow/rd_loop.py:185-241`、实验评价 `rdagent/scenarios/qlib/developer/feedback.py:17-118`；FinRL 成本与强制清仓 `round4/FinRL/finrl/meta/env_stock_trading/env_stocktrading.py:120-220,318-362`。

### 原有项目（可迁移与陷阱）
StockNova 次日开盘/涨跌停/费率/整百股 `backend/app/backtest/engine.py:95-140,265-346`、as_of 截断 `services/diagnosis_service.py:382-390,913-933`、财务回填泄漏 `strategy/factors.py:551-565`；StockAgent 横截面 MAD/Z-score `nodes/backtest_engine/factor_selection/factor_engine.py:276-367`、回测未来函数 `nodes/backtest_engine/backtester.py:208-260`；stock-scanner 固定评分反例 `services/stock_scorer.py:29-102`；stock-mcp Provider 健康窗口 `src/server/domain/routing/health.py:19-61`。

### 反例警示
TradingAgents-AShare 回测三处缺陷（SELL 符号重复处理 `api/services/backtest_service.py:172,211`、收盘价入场 `:205`、交易日历只排周末 `:60-65`）——**回测代码本身也要被审计**；接口预算好的"上榜后收益"类字段只能当标签绝不能当特征。
