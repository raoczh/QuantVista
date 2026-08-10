# §8 验收指标达标测量 + P3 立项门槛核对（第五十四批）

> **文档状态：2026-07-26 历史测量快照。** 本文不是当前功能规划；只有生产真实样本重新测量并达到门槛后，P3 才可重新立项。最新未完成项与线上验收入口看 `ROADMAP.md`。
>
> 测量日期：2026-07-26；测量口径：`docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md` v2.7 §7.4/§8/§8.1。<br>
> 性质：**纯测量零门控**——本文只报告实测值与「未评估」，不修改任何行为、阈值或 flag；每项如实声明样本量（§8：样本不足时报告"未评估"，不得把缺失当 0 分）。<br>
> 结论先行：**离线 golden 全绿（91 个测试文件 / 587 个测试函数，`go test ./...` 全量通过）；线上影子与生产闸门三类指标全部「未评估」（真实样本量=0）；P3 六项 0/6 达立项门槛**——全部被同一先决条件卡住：部署验收未执行（ROADMAP §5 积压）+ 真实样本零积累。下一步唯一有意义的动作是部署验收并开始积累样本，非继续写代码。

## 1. 数据源与方法（如实声明）

- **可测数据源**：工作区仅有 `server/quantvista.db`（本地 SQLite）。实测确认它是早期骨架阶段的开发试跑产物：33 张表（缺 `recommendation_labels`/`llm_call_logs`/`llm_experiments`/`llm_release_audits`/`llm_module_routes`/`recommendation_reflections`/`prompt_template_revisions`/`recommendation_events`/`daily_reports` 等后续批次新表——新表由服务启动 AutoMigrate 建，说明该库自新功能入库后未再被服务跑过）；`recommendation_batches`/`recommendations`/`analysis_records`/`ai_conversation_messages`/`llm_configs`/`prompt_templates`/`options` 全部 **0 行**；仅 `daily_bars` 243 行、`stocks` 2 行、`users` 1 行。
- **含义**：**生产/真实使用数据在本工作区不可得**（部署环境的库不在此仓库）。凡依赖真实调用与成熟标签的指标（线上影子、生产闸门、校准、收益、覆盖率）本轮一律「未评估」，附达标所需的样本口径供后续复测。
- **离线证据**：`go test -count=1 ./...` 全量绿（service 135.1s，2026-07-26 本机实跑，含本批修复的 4 项审查回归）；测试规模 91 文件/587 函数；预算表 19 个模块登记、registry 18 角色六组一致性测试全过。

## 2. §8 分模块测量结果

三类指标分层报告（§8：离线 golden / 线上影子 / 生产闸门）。

### 2.1 离线 golden（可测——本轮实测全绿）

| 模块/维度 | 证据（测试坐标） | 实测结果 |
|---|---|---|
| 中央 LLM 客户端（契约/温度钳制/完整性门禁/repair 上限/finish state 归一） | llm_contract_test.go（22）+ llm_run/llm_call_log 测试 | 全绿；attempt=1+repair 不变式、eof/incomplete fail-closed 反例在锁 |
| 角色/提示词资产 registry | llm_roles_test.go（6 组）：预算表↔registry 键集双向一致、必填齐全、schema 锚对拍源码、反例坐标存在性、反投票三重锚 | 全绿；18 角色 registry 完整率 100%（**注意 §8 的「每角色 2 known-answer+1 edge 满额」未达——现状为反例坐标存在性校验，v2.4 起已声明为已知限制**） |
| 结构化工件/发布门 | llm_experiment_test.go+llm_release_gate_test.go（promote 门 1~6b 逐门反例、审计收口、回滚一致性） | 全绿（含本批新增门 5b/6b/回滚 stale 反例） |
| 推荐 coverage/越池 | rec_coverage_test.go（9）：五类计数不变式、全越池、prompt_trimmed | 全绿 |
| claim/evidence/核验 | llm_claims_test.go（8）+llm_golden_test.go（10）：三态 known-answer、恶意模板全模块枚举、窗口外 0 注入 | 全绿 |
| 辩论/反思/校准/联合评估/QA 分层 | analysis_debate（12）/recreflect（9）/reccalib（8）/recjointeval（9）/qa_context（6） | 全绿；Brier=0.04/ECE=0.2 等 known-answer 验算在锁 |
| 模型路由 | llm_router_test.go（8）：换目标端到端、三信号回退、Brier 消费、批次归因（本批新增） | 全绿 |

### 2.2 线上影子与生产闸门（真实样本依赖——全部未评估）

| §8 行 | 必测指标 | 实测样本量 | 判定 |
|---|---|---|---|
| 中央客户端 | 真实调用审计覆盖率 100% | llm_call_logs 表不存在于可测库=0 调用 | **未评估** |
| 个股/持仓分析 | 首轮合法率≥98%/最终≥99.5%、证据覆盖≥90% | analysis_records=0 | **未评估** |
| 推荐精选 | coverage=100%、越池=0、量化 fallback 比例 | recommendation_batches=0 | **未评估** |
| 推荐追踪/回测 | 标签口径、Alpha、成本后收益、ECE/Brier | recommendation_labels 表不存在=0 成熟标签 | **未评估** |
| 新闻/情绪、问答/对比、日报、策略解析、反思 | 各自线上指标 | 相应业务行=0 | **未评估** |
| §8.1 统一门槛 | 每模块每状态 ≥100 样本（低频模块分级门槛=固定回归集全绿+线上≥30） | 全模块线上样本=0——**分级门槛的「线上≥30」半边同样未满足**，只有「固定回归集全绿」半边成立 | **未评估** |

## 3. §7.4 P3 六项立项门槛核对（0/6 达标）

§7.4 前置纪律：「P3 不进入本轮代码交付；只有 P0-P2 在真实样本上达标后才立项」——该前置本身即未满足（上表全部未评估）。逐项核对：

| 项 | 门槛（§7.4 原文口径） | 实测 | 判定 |
|---|---|---|---|
| P3-1 跨模型校准和路由学习 | **至少两个 provider 各有 ≥500 个成熟标签** | 成熟标签=0；已配置 provider=0（llm_configs 0 行）；差距=2 个 provider × 500 全额 | **不够，等积累** |
| P3-2 主动学习与困难样本队列 | 只收证据冲突/低 confidence/validator 拒绝样本 | 三类样本源（分析/推荐真实运行）=0 行，无样本可收 | **不够，等积累** |
| P3-3 数据漂移/模型漂移监控 | 监控来源覆盖、因子分布、repair/fallback/ECE | 无任何时间序列基线（ECE 未评估、llm_call_logs 零积累）——没有「正常水位」就没有「漂移」可监控 | **不够，等积累** |
| P3-4 可移植 Skill 包 | 打包数据契约、评测 fixture、边界 | 材料侧最接近就绪（golden fixture/契约/边界文档齐备），但受 §7.4 前置「P0-P2 真实样本达标后才立项」约束 | **不够（前置未满足）** |
| P3-5 多代理扩大试验 | 条件式 2+1 panel **已证明成本后增量** | 辩论已实施但真实触发样本=0，成本后增量无证据 | **不够，等积累** |
| P3-6 自动研究建议 | 只生成 challenger 与实验假设（依托实验飞轮） | llm_experiments 真实实验=0 个，飞轮没有历史可学习 | **不够，等积累** |

## 4. 复测口径（样本积累后按此重测）

1. **先决动作**：执行 ROADMAP §5 部署验收（P0~P2 各批积压验收点），使服务真实跑起来开始积累 llm_call_logs / recommendation_labels。
2. **P3-1 复测**：`GET /api/admin/llm-calibration` 与 `/api/admin/llm-joint-eval` 的 provider·model 分层各层 Sample——两个不同 provider 各 ≥500 且状态 matured（l2/next_open/非 forced/orphan/degraded）即达门槛。
3. **§8.1 复测**：校准页「已评估」绿 tag（≥100 样本）出现 + llm-calls 审计覆盖抽查 + 结构化最终成功率从 llm_call_logs 聚合。
4. **P3-5 复测**：辩论真实触发批次 ≥30 且触发批次的后验命中率/校准对未触发对照有可测差异。
5. **P3-6 复测**：实验飞轮走完 ≥3 个完整周期（create→run→complete→promote/abandon，含失败原因资产）。

> 本文是测量快照非承诺：数字随部署使用自然变化，复测时以当时报表为准；不得引用本文的「未评估」当作「0 分」或「达标」。
