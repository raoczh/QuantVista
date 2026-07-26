# 第五十五批：部署验收报告（ROADMAP §5 LLM 准确性 P0~P2 集中清单）

> 2026-07-26 执行。方式：本地构建（go build，SQL_DSN=local + 独立临时 SQLite `D:/TestWorkSpace/qv-accept-tmp/qv.db`，端口 3100，**未触碰仓库内 `server/quantvista.db` 与业务代码**）。LLM 依赖项用两类替身：①指向不存在端口的假配置（验证失败路径/审计如实性）；②本地假 OpenAI 兼容 SSE 服务（固定自由文本回答，验证 QA/分析链路与请求体形态）。**验收零改业务代码**；DB 直插数据仅发生在临时库（影子样本/PASS 工件/标签种子），等价单测 seed 手法。
>
> 结论：**程序化可测项 47/47 通过，未发现新 bug**。真实 LLM/浏览器/交易日 job 依赖项列 §3 人工清单。

## 1. 程序化实测结果（逐条）

### 1.1 环境与迁移

- [x] `go build` 通过；`SQL_DSN=local` + `SQLITE_PATH` 启动正常，「数据库自动迁移完成」，66 张表齐全（llm_experiments/llm_experiment_runs/llm_release_audits/llm_module_routes/recommendation_reflections/prompt_template_revisions 等新表全部在列）。
- [x] 关键新列实测存在：`llm_experiments.champion_content/pre_promote_*/rolled_back_at`、`llm_release_audits.champion_hash`、`llm_module_routes.auto_fallback_*`、`recommendation_batches.reflection_json/regime_json/llm_run_json`、`ai_conversation_messages.context_json`。
- [x] 首启引导 `/api/setup/status` → `/api/setup/admin` 创建管理员正常；无 ENCRYPTION_KEY 时创建 LLM 配置被 fail-fast 拒绝（「加密不可用」如实报错，非静默明文）——带 key 重启后正常。
- [x] 真实数据源冒烟：600519 quote 实拉成功（source=eastmoney，本日东财可达）。

### 1.2 P1-7/P1-8 批（校准报表/角色 registry）

- [x] `GET /api/admin/llm-calibration` 空库形态：推荐两组（short h=10/long h=20）`evaluated=false`、**Brier/ECE=null（非 0 分）**、coverage 全零、notes 含「未评估≠0 分」「已评估硬门槛 ≥100」「orphan_excl」声明；分析组 scanned=0 各分列齐全；elapsed_ms 有值。
- [x] 造 30 条成熟标签后：sample=30、evaluated 仍 false（<100 正确）、分档表出现、slices 四维（策略/regime/provider·model/prompt_version）出现且小样本层不产 Brier。
- [x] `GET /api/admin/llm-roles`：**18 角色**（17+release_audit，含 challenger 影子研究员/发布审计员）+全局纪律 5 条；每卡 version/schema_version/budget/repair/trigger/白名单/必答/禁止动作/反例坐标齐全；预算与 llm_budget 表值一致（analysis/rec/qa/news 2500、debate 1200/800、release_audit 1500 等）。
- [x] 纯测量确认：两页多次刷新（含 refresh=1）后 `llm_call_logs` 计数零新增。

### 1.3 P2-1/P2-2 批（实验飞轮/发布门）

- [x] 实验 CRUD：空列表正常；缺 hypothesis/expected_improvement 拒绝（文案点名 P2-2）；创建成功固化 challenger_hash + **champion_content（实测=内置任务段全文 102 字，非占位说明）**。
- [x] promote 五门+审计门逐门实测：draft 拒（状态机）→ running 拒 → completed 但样本 0 拒（门 3）→ conclusion=no_improvement 拒（门 2，且 complete 时不填 failure_reason 被拒——反馈闭环必填）→ 样本足但工件 verdict=error 拒（门 6「未 PASS 不晋级」）→ PASS 工件后晋级成功（promoted_revision 递增、模板切 challenger 内容、revision 快照落库）。
- [x] complete 时 actual_json 程序聚合落库（samples/valid_rate/token/overlap 对照结构齐全）。
- [x] flag `llm_challenger` 管理设置实测默认 **false**（缺省关，成本开关语义正确）。

### 1.4 P2-4/P2-6 批（模型路由/发布审计）

- [x] flag `llm_model_routing` 默认 **false**。路由 CRUD 校验四连：module=experiment 拒（单变量别名纪律，文案点名）；config 不存在拒；未知模块拒；同 module upsert 复用同一行（id 不变）。
- [x] 路由生效端到端：开总开关+route(release_audit→config2) 后触发审计，**`llm_call_logs` 记路由后真实目标（llm_config_id=2/fake-model-2）**；关总开关后同一动作回原配置（config_id=1）——审计双侧真实性实证。
- [x] 路由列表健康快照：routed/baseline 近 24h 调用与失败计数、cost_ratio、config 名与 missing 标记齐全。
- [x] 自动回退持久化不自动恢复：置 auto_fallback_at/reason 后接口透出黄 tag 数据；`POST /llm-routes/:id/reset` 显式恢复后清空。
- [x] 发布审计员不出假 PASS：无 LLM 配置时如实报错「未就绪」不落工件；LLM 不可达时落 **verdict=error 工件**（summary 带真实 dial 错误、trace_id 有值、工件挡晋级但非 FAIL 判定）；工件逐次落库全保留（验收结束共 5+ 件未覆盖删除）。
- [x] 一键切回：promoted→rollback 成功（PrePromoteEnabled=false 场景=停用自定义模板不删内容），状态 rolled_back **终态**——再 rollback 拒（「仅 promoted 可回滚」）、abandon 拒（「终态不可废弃」）；工件与 revision 快照全保留。

### 1.5 P2-5/P2-3 批（联合评估/校准分层/QA 上下文）

- [x] `GET /api/admin/llm-joint-eval` 空库可序列化（两 section 全零、elapsed_ms=1、notes 齐全含换手/串联净值/无滑点口径声明）。
- [x] 造 15 信号日×2 buy 成熟标签后：**切分 70/30 手工验算吻合**（dev=06-01→06-10 十日 20 样本；locked_preview=06-11→06-15 五日 10 样本只出范围与样本数、指标缺席）。
- [x] 锁定段审计：include_locked=1 → locked 指标出现 + options 表 `jointeval_locked_reads` **count=1（含时间戳 log）**；再读 +1→count=2；回默认视图 locked 指标消失、审计数不变（缓存不烧审计、审计不回退）。
- [x] 校准分层批次归因关联实测：造两组批次（openai/routed-model-x 20 样本、anthropic/claude-x 10 样本）后 provider_model/prompt_version 分层各出两行且样本数正确（p13 vs p13-custom.abcd1234 晋级对照落点形态齐活）。
- [x] 联合评估版本对照只吃 dev 段：slices 只出现 dev 段 20 样本行（locked 段 10 样本不进对照）。
- [x] QA 分层上下文端到端（假 LLM 10 轮真实会话）：第 9 轮 context_json=`{version:qc1, tier1_msgs:12, tier3_rounds:2, tier3_matched:[{round:1,score:12},{round:2,score:10}], approx_tokens:4771}`——Tier3 按 rune bigram 检索命中第 1/2 轮（问题里点名 QVACC1 关键词，检索正确）；**RequestBody 实证 system 含【历史会话分层上下文】段+「摘录非当前快照事实」声明+第 1 轮原文摘录**。
- [x] flag `llm_layered_context` 默认开；关后新提问：system 无分层段（RequestBody 实证）、**context_json 仍落库**且如实记 `tier2/tier3=0, invisible_rounds=3`（观测不受 flag 控）；重开恢复。

### 1.6 P1-2/P1-4/P1-6 与 P1-3/P1-5 批（可程序化部分）

- [x] 四开关默认值全对：`llm_conditional_debate=true`/`llm_reflection_shadow=true`（缺省开）+上述两 flag 缺省关；管理设置接口全部透出。
- [x] P1-4 新闻窗口 nw2 实测：真实分析记录 data_snapshot 的 news.window_meta=`{window_start/end=[now-7d,now], total_in_window:0, injected_count:0, source_query_status:"ok", source_alignment:"unavailable", version:"nw2"}`——空窗口如实 unavailable 且窗口边界仍声明（「确无」≠「没查」）。
- [x] 分析降级链路如实：假 LLM 自由文本 → status=degraded「结构化输出校验失败，已降级为原文展示」（不伪造结构化结果）；manifest run 与 llm_call_logs 同 trace 串联（attempt=1 主调+attempt=2 repair=1，p20/analysis.v1/json_object）。
- [x] QA 核验 check_json 10/10 落库（assistant 消息全带）；llm_call_logs 按模块汇总版本号全对：qa=q13/free_text、analysis=p20、news=n3、release_audit=ra1/json_object。
- [x] `recommendation_reflections` 表已建（列齐：outcome/lesson/factor_digest/available_from/reflection_version）；`/admin/llm-calls?module=` 筛选接口正常（release_audit 4 行/news 5 行）；前端 AdminLlmCalls.vue moduleLabel 含辩论四角色/reflection/experiment（**注意：缺 release_audit 选项，见 §2**）。
- [x] 离线回归单测实跑全绿：TestDeriveClaims*/TestNewsWindow*/TestDebate*/TestReflection*/TestQaBuildMessagesLayered*/TestPromptLayered*/TestCalibSliceRows/TestRecCalibReportSlices/TestJointEval*/TestChallengerShadow*（11.4s ok）。

### 1.7 第五十四批 4 回归点（2026-07-26 审查修复批）

- [x] **①回滚 stale**：晋级后编辑模板（champion 前移）→ 列表 `rollback_stale` 出现原因文案、`POST rollback` 拒绝（指引提示词页 revision 快照）；把模板内容改回 challenger 原文（hash 复原）→ rollback 放行、对称恢复（默认态=停用自定义）、audits/revisions 零删除。
- [x] **②换手 locked 隔离**：默认视图 turnover pairs=**9**（仅 dev 段 10 日）、include_locked pairs=**14**（全量 15 日）——手工验算吻合（n 日相邻对=n-1）；默认视图不烧审计、include_locked 每读 +1。
- [x] **③路由归因**：`applyBatchRouteAttribution` 挂点确认（recommendation.go 主调后、影子采样前）+ TestApplyBatchRouteAttribution 实跑过（未路由零改写/路由后 Provider/Model 改写/LLMConfigID 恒原值）。真实批次级验证需真 LLM，列人工清单。
- [x] **④审计绑基线**：工件落 champion_hash 实测（error 工件也带）；**门 6b** 直插 champion_hash=deadbeef 的 PASS 工件被拒（「未绑定基线须重审」）、正确 hash 工件放行；**门 5b** 创建实验后改基线模板 → promote 拒（「对照失效须重建实验」）。

## 2. 观察项（非 bug，记录评估）

1. **AdminLlmCalls.vue 的 moduleLabel 缺 `release_audit` 条目**（后端筛选接口支持、审计行正常落库；前端下拉少一项，该模块调用行的模块列会显示原字符串 `release_audit` 而非中文标签）。影响仅管理端展示便利性。**建议下一批顺手补一行**（`release_audit: '发布审计'`），属第五十三批⑥前端接线的小遗漏；本批守「验收不改业务代码」边界，未当场修。
2. `POST /api/analysis` 传 `"mode":"standard"` 会被拒（标准模式的常量是空串 `""`，前端传空串没问题）——纯 API 直调时的取值约定，行为与代码注释一致，非缺陷。
3. QA 单轮耗时约 40~60s（本地假 LLM 秒回，时间花在快照组装/核验链路），与既有性能画像一致，不属本批范围。

## 3. 人工执行清单（需真人浏览器/真实 LLM 配置/交易日 job）

以下无法在本环境程序化替代，按批次归组（原文见 ROADMAP §5 各条目）：

**A. 需真实 LLM 配置（配好管理员默认 LLM 后逐条）**
1. 真实生成一批推荐：结果头 coverage/「AI 输出剥除」零噪声、llm_run_json coverage 对象、rec_review/rec_bear 无 coverage 键（P1-1①②③）；claims 结论追踪+长线 invalidation p13/recommendation.v2（P1-2②）；反思影子 tag（积累后，P1-5⑥）；**路由开启时批次归因列=路由后模型**（第五十四批回归点③的线上半边：造一条 recommendation 路由→生成→查 recommendation_batches.provider/model 应为路由目标、llm_run_json 有 routed 字段）。
2. 真实个股分析：claims 结论追踪/交易计划失效条件 trade_plan.v2（P1-2①）；低置信/ST 标的触发辩论 3~4 调+高置信不触发零成本+降级说明（P1-3①②③④）；契约卡 RequestBody/温度 ≤0.2/截断拒收（P0 集中验收①②）。
3. 真实日报/问答：日报 claims 两条（P1-2③）；问答 >6 轮看前端「上下文」徽章（P2-5⑤的 UI 半边——接口/落库本批已验）。
4. 实验飞轮全真跑：开 llm_challenger→本人生成推荐→采样计数 +1、同 trace 出现 module=experiment、推荐结果与关实验时一致（P2-1①）；真实发布审计 PASS/FAIL 工件（P2-6④——本批只验了 error 路径与门逻辑）。
5. 模型路由真实场景：news→小模型跑通、结构化 unsupported 目标跳过（需真探测）、错误 Key 跑几次后自动回退黄 tag 出现（P2-4②③——本批已验 DB 态与 reset，触发链路需真调用积累）。
6. `endpoint_type=responses` 配置真实上游测试连接+问答冒烟（2026-07 杂项批遗留）。

**B. 需浏览器目验（6 主题+移动端）**
7. 新页四张：LLM 校准报表/LLM 角色资产/LLM 实验/联合评估+「模型路由」卡+契约卡第八/九开关（P1-7③/P2 各批 UI；NDataTable scroll-x 已带）。
8. 历史欠账目验池：推荐页/设置页/信任徽章/热力图/screener/backtest/走查页等（§5 前半 M1~P3c 各条——与本批无关但同池，建议一次过）。

**C. 需交易日部署环境观察（服务常驻+16:10/2h job）**
9. 宇宙快照/因子快照/标签结算/反思生成（≥30 成熟标签后「推荐反思生成完成」日志、表逐轮增长 ≤5 条）——**这是 P3 立项样本积累的起点**（P3_GATE_ASSESSMENT 先决动作）。
10. 断路器/东财限流降级/涨停池等 job 日志观察（M1/M3 各条）。

## 4. 验收基础设施（可复用）

- 临时环境：`D:/TestWorkSpace/qv-accept-tmp/`（qv.db 验收库+server2.log+sqltool 只读查库工具+fake_llm.py 假 OpenAI SSE 服务）。复跑：`SQL_DSN=local SQLITE_PATH=... PORT=3100 SESSION_SECRET=... ENCRYPTION_KEY=...`（创建 LLM 配置必须带 ENCRYPTION_KEY）。
- 该目录不在仓库内，可整体删除。
