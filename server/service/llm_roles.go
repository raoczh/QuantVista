package service

import "sort"

// P1-8 角色/提示词资产 registry 与适用性路由声明
// （docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md §7.2 P1-8、§4.5.3 角色卡模板、§4.5.4 编排表）。
//
// 形态与边界（改本文件前先读）：
//   - 这是**系统内建角色的机读描述登记**（llm_budget.go 同款「代码内声明表+测试锁定
//     一致性」模式），不是第二套 prompt 模板注册表——用户自定义模板仍走既有
//     PromptTemplate/PromptService（P0-6 v1.4 落地约束），本表描述的是代码里真实存在的
//     角色编排（prompt 常量+schema+预算+校验+触发条件），管理端只读透出。
//   - 纯资产登记零行为变化：本表不参与任何运行时路由决策——「适用性路由」字段
//     （Trigger）是对代码既有条件式编排的**声明**（辩论三条件/反方只对 buy/复核可选），
//     锁定测试保证声明与代码一致，而非由本表驱动代码。
//   - 版本锚用**常量引用**（编译期绑定）：代码版本常量递增时 registry 自动跟随，
//     不存在手抄漂移；SchemaVersion 是字符串，由 TestLLMRoleSchemaAnchors 读源码
//     对拍 newLLMRun 调用处防漂移。
//   - 反例坐标（CounterExamples）引用真实测试函数名，TestLLMRoleCounterExamplesExist
//     读 *_test.go 源码校验存在性——挪动/重命名测试必须同步本表（llm_golden_test.go
//     目录同款纪律，但这里是机读+程序校验）。
//
// 不做什么（有意边界）：
//   - P1-9 角色质量门与发布审计**明确延后**：其「未 PASS 不进入 synthesis」预设了
//     资产晋级/发布流程（draft→champion），该流程属 P2-1 champion/challenger 实验飞轮；
//     当前全部角色都是生产路径固定编排、无晋级动作可门控，且质量门的「代码硬检」
//     部分（字段/时效/来源/空值/截断）已作为 P0-1~P0-9+P1-2 运行时校验存在（每次调用
//     都检查，强于发布时抽检）。待 P2-1 建立实验流程后，P1-9 作为其发布门实施。
//   - 不引入 §4.5.1b 的方法角色（value_quality@r1 等）：那些是未启用的设计储备，
//     本表只登记真实接线的角色，防「登记了但代码里不存在」的假账。

// LLMRoleAsset 单个角色资产的机读描述（管理端只读透出）。
type LLMRoleAsset struct {
	// RoleID 与 llm_call_logs.module / llmRun.Module / llmModuleBudgets key 同名同值。
	RoleID string `json:"role_id"`
	Name   string `json:"name"` // 中文名（计划 §4.5.4 编排表口径）
	// Version 提示词版本锚（引用代码真实版本常量；用户自定义模板会追加 -custom.<hash8>
	// 后缀，见 P0-6 promptVersionFor——本字段是「基版本」）。
	Version string `json:"version"`
	// SchemaVersion 与 newLLMRun 调用处登记的 schema_version 一致（测试读源码对拍）。
	SchemaVersion string `json:"schema_version"`
	Purpose       string `json:"purpose"`
	// Market 适用市场；Horizons 适用周期（描述性——如「短线 5/10 日、长线 20/60 日」）。
	Market   string `json:"market"`
	Horizons string `json:"horizons"`
	// Trigger 适用性路由声明：何时调用/何时确定性跳过（对代码既有条件编排的描述）。
	Trigger string `json:"trigger"`
	// InputWhitelist 输入数据段白名单（角色只读这些冻结快照段——描述口径）。
	InputWhitelist []string `json:"input_whitelist"`
	// MustAnswer 必答问题/输出要求（未满足会触发 repair 或剥除的实际约束）。
	MustAnswer []string `json:"must_answer"`
	// ForbiddenActions 禁止动作（由程序剥除/校验/拒收兜底，非仅 prompt 约束）。
	ForbiddenActions []string `json:"forbidden_actions"`
	// Fallback 失败降级语义（best-effort 丢弃 / degraded 落库 / 拒答码 / 规则兜底）。
	Fallback string `json:"fallback"`
	// MaxTokens/RepairAttempts 由 llmModuleBudgets 回填（单一权威，不在本表重复声明）。
	MaxTokens      int `json:"max_tokens"`
	RepairAttempts int `json:"repair_attempts"`
	// CounterExamples 反例测试坐标（真实测试函数名，存在性由测试校验）。
	CounterExamples []string `json:"counter_examples"`
}

// llmRoleGlobalDisciplines 全局纪律（跨角色恒真，管理端与 registry 一并透出）：
// 与计划 §6.3 不采纳边界、§4.5.3 角色卡不变式一一对应，锁定测试断言关键条目存在。
var llmRoleGlobalDisciplines = []string{
	"不以角色/名人投票合成最终信号：judge 按证据质量裁决不按票数平均，panel 多数评级只用于语义校验（block ⇒ 不得 bullish），最终动作恒由程序约束（§6.3）",
	"模型口头 confidence 不当真实概率：只与程序合成置信度（SysConfidence）并排展示；其校准性由 P1-7 校准报表测量（§6.3/§9.2）",
	"服务端字段不可伪造：QuantScore/SysConfidence/Review/Bear/QualityGate/debate 等回填字段在 parse 层剥除模型自附值（P0-3/P1-1/P1-3 纪律）",
	"同一快照可回放：trace_id + prompt_hash + data_hash（llm_run manifest）+ prompt_template_revisions 不可变快照（P0-2/P0-6）构成回放锚",
	"新增 chatCompletion* 调用模块必须同时登记 llmModuleBudgets（预算）与本 registry（角色卡）——两表键集一致由测试锁定",
}

// llmRoleAssets 角色资产表：key 集与 llmModuleBudgets 完全一致（测试锁定；
// llm.go 探针 module=test 有意不进两表）。
var llmRoleAssets = map[string]LLMRoleAsset{
	"analysis": {
		RoleID: "analysis", Name: "论点分析师（thesis_analyst）",
		Version: analysisPromptVersion, SchemaVersion: "analysis.v1 / analysis_panel.v1",
		Purpose: "在冻结个股/市场/板块/自选/持仓快照内形成可证伪的投资论点（评级/亮点/风险/反方观点/失效条件/未知项）",
		Market:  "cn 为主（us/hk 请求可达但数据受限：非 cn 无日历走行情 fail-closed，多为拒绝或历史解释模式）", Horizons: "标准分析无固定持有期；panel 为四视角短评",
		Trigger:          "用户手动发起；个股 stale/unknown 默认拒绝（allow_stale 走历史解释硬约束）；panel 一律拒 stale；as_of 走回溯快照",
		InputWhitelist:   []string{"quote+freshness", "technicals(daily_bars)", "valuation", "finance(F10)", "news(window_meta)", "announcements", "org_view", "risk_gate", "unknowns", "board_valuation/board_flow(板块)", "mood(市场)"},
		MustAnswer:       []string{"rating 与证据/风险闸门一致（block ⇒ 不得 bullish，语义校验拒收）", "anti_thesis 反方观点与 kill_switches 失效条件", "unknowns 数据盲区如实声明", "引用数字须与快照吻合（证据核验 ev5）"},
		ForbiddenActions: []string{"补写快照外的价格/财务/新闻", "risk_gate=block 时输出 bullish", "自附 sys_confidence/evidence_check/trade_plan/debate 等服务端字段（parse 剥除）", "stale 快照下给当前行动建议"},
		Fallback:         "repair 打满走 degraded 落库（原文保留非成功）；拒答带机读码（stale_quote 等）",
		CounterExamples:  []string{"TestValidateAnalysisSemantics", "TestParseAnalysisResult_StripsTrustFields", "TestGoldenCustomTemplateCannotOverrideSafety", "TestGoldenAnalysisClaims"},
	},
	"trade_plan": {
		RoleID: "trade_plan", Name: "交易计划员（plan_interpreter）",
		Version: analysisPromptVersion, SchemaVersion: "trade_plan.v2",
		Purpose: "对已通过的个股分析提出买入区间/目标价/止损/持有周期候选值与操作清单；价位关系由服务端硬校验",
		Market:  "cn 为主（随主分析：us/hk 可达但数据受限）", Horizons: "计划自带持有周期字段",
		Trigger:          "个股标准分析成功后追加（非 panel/非 as_of）；评级偏空/风险闸门 block/现价缺失/行情非 fresh 四类确定性拒绝零调用（no_plan）",
		InputWhitelist:   []string{"分析结论", "现价（服务端冻结）", "risk_gate", "org_view 目标价对照锚"},
		MustAnswer:       []string{"止损<现价、止损<买入区间下沿、目标>区间上沿（validateTradePlan 硬纪律）", "invalidators 计划失效条件（P1-2；模型未输出如实为空，归一收集非硬校验）", "操作清单 checklist"},
		ForbiddenActions: []string{"计算仓位/RR/费用（Go 回填，模型自附值剥除）", "stale 行情下给精确价位", "盈亏比<2:1 时维持满仓（服务端减半记 discipline_notes）"},
		Fallback:         "repair 打满 best-effort 不带计划（主分析不受影响）",
		CounterExamples:  []string{"TestValidateTradePlan", "TestAttachTradePlan_DeterministicSkips", "TestParseTradePlan_StripServerFields", "TestGoldenTradePlanInvalidatorsParse"},
	},
	"analysis_review": {
		RoleID: "analysis_review", Name: "分析复核员（risk_reviewer）",
		Version: analysisPromptVersion, SchemaVersion: "analysis_review.v1",
		Purpose: "独立复核员视角对照数据快照审查分析结果，只挑刺不重写，输出 pass/warn/reject 与建议置信度",
		Market:  "cn 为主（随主分析）", Horizons: "同主分析",
		Trigger:          "用户开启 verify 时对成功分析追加一次；喂复核员前剥除 TradePlan（计划价不在快照防误 reject）",
		InputWhitelist:   []string{"主分析结果（剥 TradePlan）", "同一冻结快照"},
		MustAnswer:       []string{"verdict pass/warn/reject 与理由", "reject 必须级联（Action 压 watch、置信度 ≤25、SysConfidence=low）"},
		ForbiddenActions: []string{"直接改写主结果字段（只出 verdict，级联由程序执行）", "以复核通过替代证据核验"},
		Fallback:         "best-effort：失败只是没有复核结论，不影响主结果",
		CounterExamples:  []string{"TestAnalysisReviewRejectCascade", "TestReviewRepairTemperatureAndAttemptLimit"},
	},
	"recommendation": {
		RoleID: "recommendation", Name: "候选精选器（candidate_selector）",
		Version: recPromptVersion, SchemaVersion: "recommendation.v2",
		Purpose: "在量化 Top10 候选内精选/否决并给出理由、风险、失效条件；允许宁缺毋滥空选",
		Market:  "cn", Horizons: "短线 5/10 日、长线 20/60 日（标签 LabelHorizons）",
		Trigger:          "用户手动生成或日报自动推荐；建池→用户筛选→量化评分→行情时效硬门（qf3）之后只喂 Top10；全池 stale 宁可失败不推荐",
		InputWhitelist:   []string{"候选因子（candFactors 全量：技术/筹码/情绪/资金/盘中/财务）", "来源与量化分", "风险字段", "reflection 影子层不注入（三不纪律）"},
		MustAnswer:       []string{"picks 逐条理由/风险/证据数字", "长短线失效条件（invalidation，P1-2）", "rejected 落选理由", "短线 buy 盈亏比≥1.5（不足程序降 watch）"},
		ForbiddenActions: []string{"推荐池外/名单外标的（parseAndFilterPicks 剥除+coverage 诊断）", "自报 quant_score/sys_confidence/review/bear 等服务端字段", "为凑数量放宽门槛（空选合法）"},
		Fallback:         "超时/网络/5xx/流中断走量化降级（恒 watch+置信 35+SysConfidence low）；宁缺毋滥拒选语义不动",
		CounterExamples:  []string{"TestParseAndFilterPicks_DropsFabricated", "TestParseAndFilterPicksCoverageDiag", "TestParseAndFilterPicks_EmptyPicksLegal", "TestQuantFallbackKeepsScreenScore"},
	},
	"rec_review": {
		RoleID: "rec_review", Name: "推荐复核员（rec_reviewer）",
		Version: recPromptVersion, SchemaVersion: "rec_review.v1",
		Purpose: "对推荐名单二次挑刺（pass/warn/reject），reject 强制降级",
		Market:  "cn", Horizons: "同推荐",
		Trigger:          "用户开启 AI 复核时对成功名单追加一次；调用预算主调1+复核1+反方1 上限 3 次",
		InputWhitelist:   []string{"推荐名单与证据", "候选池快照"},
		MustAnswer:       []string{"逐条 verdict 与理由", "reject 级联（buy→watch、置信度压低）由程序执行"},
		ForbiddenActions: []string{"直接改写名单（只出 verdict）", "复核通过解除候选池边界"},
		Fallback:         "best-effort：失败无复核结论，token 计入批次",
		CounterExamples:  []string{"TestApplyReviews", "TestNormalizePick_Clamps"},
	},
	"rec_bear": {
		RoleID: "rec_bear", Name: "反方研究员（independent_bear）",
		Version: bearReviewVersion, SchemaVersion: "rec_bear.v1",
		Purpose: "对每只 buy 独立构建最强 bear case（影子：只展示不改写；high 危记影子事件）",
		Market:  "cn", Horizons: "同推荐",
		Trigger:          "BearCheck 开关（nil 默认关联 verify）；只对 action=buy 条目、放 verify 复核之后；无 buy 零调用",
		InputWhitelist:   []string{"buy 条目与其证据", "A 股空头论据框架（高位放量/拥挤/估值/T+1/技术背离）"},
		MustAnswer:       []string{"最强反方论证与 severity（high/med/low）", "解禁减持数据系统未提供——只能提示核查"},
		ForbiddenActions: []string{"改写 action/置信度（影子三不纪律）", "虚构解禁/质押/减持数据", "对 watch 条目消耗篇幅"},
		Fallback:         "best-effort 1 次 repair；失败该条无反方论证",
		CounterExamples:  []string{"TestBearPromptFramework", "TestApplyBearShadow", "TestNormalizePickStripsServerFields"},
	},
	"daily_report": {
		RoleID: "daily_report", Name: "收盘复盘员（market_summarizer）",
		Version: dailyReviewPromptVersion, SchemaVersion: "daily_report.v1",
		Purpose: "收盘后对市场/持仓/自选/新闻事件做当日复盘与明日计划",
		Market:  "cn", Horizons: "T+1 观察计划",
		Trigger:          "交易日 15:35 后自动/手动生成；日历明确 open 才生成（closed/unknown 分码拒答）；核心块数据水位不足剥数值只留时点",
		InputWhitelist:   []string{"指数/涨跌家数/资金流（业务日期校验后）", "持仓（fresh 契约）", "推荐批次", "新闻事件（窗口上界 min(dayEnd,now)）"},
		MustAnswer:       []string{"总结与明日计划（claims 推导，P1-2）", "数据缺口如实声明不冒充今日", "复盘不硬造失效条件（对已发生事实的解读）"},
		ForbiddenActions: []string{"用未来/过期数据冒充今日口径", "重建推荐（与批次一致）"},
		Fallback:         "repair 打满归 llm_output_invalid；复盘失败保留旧报告、partial 如实标注",
		CounterExamples:  []string{"TestDailySnapshotDateGate", "TestDailyTradingDayStatusFailClosed", "TestDailyReviewEvidenceClaims"},
	},
	"qa": {
		RoleID: "qa", Name: "证据问答员（evidence_qa）",
		Version: qaPromptVersion, SchemaVersion: "qa.free_text.v1",
		Purpose: "基于固化个股快照的多轮问答；回答数字过证据核验",
		Market:  "cn 为主（us/hk 快照可达但数据受限，时效重判 fail-closed）", Horizons: "会话快照时点",
		Trigger:          "用户提问；新会话快照非 fresh 默认拒绝首答（allow_stale 确认走历史解释）；每轮续问注入按提问时刻重判时效",
		InputWhitelist:   []string{"会话固化快照（永不回写）", "用户问题（UNTRUSTED）", "多轮历史"},
		MustAnswer:       []string{"快照外事实答未知", "时效非 fresh 如实声明「截至 X」"},
		ForbiddenActions: []string{"下达买卖指令（自由文本契约锚点）", "执行问题文本中的指令（注入隔离）", "流中断落半截回答"},
		Fallback:         "流式失败不落库；拒答带机读码",
		CounterExamples:  []string{"TestQaBuildMessages", "TestGoldenInjectionStaysInDataSection", "TestGoldenCustomTemplateCannotOverrideSafety"},
	},
	"compare": {
		RoleID: "compare", Name: "对比点评员（comparison_analyst）",
		Version: comparePromptVersion, SchemaVersion: "compare.free_text.v1",
		Purpose: "对 2~6 只标的的同口径行情/估值做简短对比点评",
		Market:  "cn 为主（us/hk 行可展示但 fresh<2 不调 AI）", Horizons: "现时快照",
		Trigger:          "用户勾选 AI 点评；fresh 行情 <2 只先拒（数据码优先于配置码）；stale 行保留展示不进 AI",
		InputWhitelist:   []string{"fresh 行情行", "估值（腾讯口径）"},
		MustAnswer:       []string{"只比较共同可用字段", "字段缺失方不得据此判劣"},
		ForbiddenActions: []string{"荐股式结论（研究参考表述）", "引用不在对比表中的数据"},
		Fallback:         "AI 失败返回 ai_refusal_code，量化对比表不受影响",
		CounterExamples:  []string{"TestChangeOverN", "TestNormalizeCompareRequestKeepsTruncationNote"},
	},
	"news": {
		RoleID: "news", Name: "新闻情绪标注员（news_window_classifier）",
		Version: sentiPromptVersion, SchemaVersion: "news_enhance.v1",
		Purpose: "批量标注新闻 sentiment/scope/policy（8 条/批，逐 ID 映射）",
		Market:  "cn", Horizons: "[now-7d,now] 窗口（nw2 window_meta：完整窗口统计+独立来源判定+查询状态）",
		Trigger:          "新闻采集后自动分级增强（P1/P2 全量、P3 规则先行）；LLM 走系统默认配置只审计不扣次",
		InputWhitelist:   []string{"新闻标题/摘要（UNTRUSTED）", "板块白名单"},
		MustAnswer:       []string{"逐 ID 恰一项", "方向与分数矛盾以方向为准分数归 0", "related_sectors 过白名单"},
		ForbiddenActions: []string{"窗口外/未来记录注入（0 注入锁定）", "单源写成市场共识（alignment 程序分类模型只解释）"},
		Fallback:         "解析失败降级规则路径（applySentimentRules），不 repair",
		CounterExamples:  []string{"TestComputeDailySentimentExcludesFutureNews", "TestGoldenNewsWindowZeroInjection", "TestGoldenNewsAlignment", "TestNormalizeEnhance"},
	},
	"screener_parse": {
		RoleID: "screener_parse", Name: "白话建策略解析器（factor_mapper）",
		Version: screenerParsePromptVersion, SchemaVersion: "screener_parse.v1",
		Purpose: "自然语言→条件树 {tree, unmatched, explain}；因子字典程序生成",
		Market:  "cn", Horizons: "n/a（选股条件）",
		Trigger:          "用户输入白话点「AI 生成」；树由用户确认「套用到编辑器」才落（AI 只生成不执行）",
		InputWhitelist:   []string{"factorDefs 因子字典（程序生成）", "用户白话（≤300 字，UNTRUSTED）"},
		MustAnswer:       []string{"无法映射的表述如实进 unmatched 禁硬凑", "tree=null 仅当 unmatched 非空合法（拦空手交差）"},
		ForbiddenActions: []string{"引用白名单外因子（validateCondTree 拒收）", "直接落库/扫描"},
		Fallback:         "repair 一次仍不过报错（不出降级半成品）",
		CounterExamples:  []string{"TestParseStrategyPromptFactorDict", "TestParseStrategyLLMOutput", "TestParseStrategyRepairStillBad"},
	},
	"debate_bull": {
		RoleID: "debate_bull", Name: "辩论看多方（bull_researcher）",
		Version: debateVersion, SchemaVersion: "debate_bull.v1",
		Purpose: "只建立当前快照下最强的看多论证（claims 引用 evidence_id+至少一条 invalidator）",
		Market:  "cn 为主（随主分析）", Horizons: "同主分析",
		Trigger:          "程序判定三条件之一（SysConfidence=low / claims 含 contradictory / risk_gate warn 级；block 排除——语义校验已硬约束辩论无从改变）；仅个股标准分析、fillAnalysisTrust 之后",
		InputWhitelist:   []string{"主分析 claims 与证据白名单（evidenceCheck.Items 的 ev-*）", "同一冻结快照"},
		MustAnswer:       []string{"每条 claim 必须引用至少一个合法 evidence_id（零引用剥除、全空触发 repair，db2）", "至少一条失效条件（全无报错触发 repair）"},
		ForbiddenActions: []string{"淡化 risk_gate", "把未知当利好", "自报 claim id（程序重编号 bu-*）"},
		Fallback:         "bull_failed 丢弃全部辩论产物（主分析不受影响），后续调用止损",
		CounterExamples:  []string{"TestDebateTriggerReasons", "TestDebateBullFailedDegrades", "TestNormalizeDebateClaims"},
	},
	"debate_bear": {
		RoleID: "debate_bear", Name: "辩论看空方（bear_researcher）",
		Version: debateVersion, SchemaVersion: "debate_bear.v1",
		Purpose: "逐条回应 BULL_CLAIMS 并建立看空论证，confirmed 区分已证实/假设风险",
		Market:  "cn 为主（随主分析）", Horizons: "同主分析",
		Trigger:          "bull 立论成功后串行调用（同一触发链）",
		InputWhitelist:   []string{"bull claims（程序重编号后）", "同一证据白名单与快照"},
		MustAnswer:       []string{"challenges 至少一条有效回应 bull claim id（空/全非法触发 repair，db2；引用校验剥除非法引用）", "每条 claim 必须引用合法 evidence_id（db2）", "区分已证实风险与假设风险（confirmed 缺省按假设风险 false 处理）"},
		ForbiddenActions: []string{"虚构证据 id", "重复 bull 的事实当反驳"},
		Fallback:         "bear_failed 连 bull 一起丢弃（单方论点有误导性）",
		CounterExamples:  []string{"TestDebateEndToEnd", "TestNormalizeDebateClaims"},
	},
	"debate_rebuttal": {
		RoleID: "debate_rebuttal", Name: "辩论反驳轮（bull_rebuttal）",
		Version: debateVersion, SchemaVersion: "debate_rebuttal.v1",
		Purpose: "bull 对 bear challenges 的一次反驳（仅同证据对立解读时）",
		Market:  "cn 为主（随主分析）", Horizons: "同主分析",
		Trigger:          "程序判定 hasSharedEvidence（双方 claims 引用同一 evidence_id）才触发；各说各话直接进 judge；轮次硬钳制 ≤3、调用预算 ≤4",
		InputWhitelist:   []string{"双方 claims 与共享证据"},
		MustAnswer:       []string{"针对共享证据的对立解读给出反驳"},
		ForbiddenActions: []string{"新增双方未引用的事实", "无条件追加轮次"},
		Fallback:         "rebuttal 失败 best-effort 不降级整体（judge 照常裁决）",
		CounterExamples:  []string{"TestHasSharedEvidence", "TestDebateRebuttalRound"},
	},
	"debate_judge": {
		RoleID: "debate_judge", Name: "辩论裁判（research_judge）",
		Version: debateVersion, SchemaVersion: "debate_judge.v1",
		Purpose: "按证据质量对双方论点裁决（verdict/decisive/rejected/unresolved/conflict_note）",
		Market:  "cn 为主（随主分析）", Horizons: "同主分析",
		Trigger:          "双方论点齐备后收口；裁决是附加复核不是替换——不改写主 rating，方向相反只压 SysConfidence=low 并点名",
		InputWhitelist:   []string{"双方已校验 claims（含 rebuttal）", "risk_gate 状态"},
		MustAnswer:       []string{"按证据质量排序不按角色票数平均（§6.3 反投票合成锚）", "block 时 verdict 不得 bullish（收口校验，repair 仍违纪 judge_invalid）", "三列表互斥且至少一个有效 claim 引用（空引用裁决触发 repair，db2）", "冲突输出 conflict_note 不和稀泥"},
		ForbiddenActions: []string{"以票数合成信号", "新增双方未引用的事实", "改写主分析 rating/summary/claims"},
		Fallback:         "judge_failed/judge_invalid 保留双方论点无裁决（对抗已完成只缺裁决）",
		CounterExamples:  []string{"TestDebateJudgeBlockGuard", "TestDebateOppositeVerdictLowersConfidence", "TestParseAnalysisResultStripsDebate"},
	},
	"reflection": {
		RoleID: "reflection", Name: "反思记忆生成器（reflection_writer）",
		Version: reflectionVersion, SchemaVersion: "reflection.v1",
		Purpose: "对成熟推荐标签批量生成可迁移教训（LLM 只写教训文本，不判输赢）",
		Market:  "cn", Horizons: "代表持有期：短线 h=10 / 长线 h=20",
		Trigger:          "tracking job 标签结算后；全库成熟标签（l2/next_open）≥30 才启用、每轮 ≤5 条；影子检索 available_from ≤ 检索时点（防回放泄漏铁律）",
		InputWhitelist:   []string{"成熟标签事实（outcome/factor_digest 程序产出）", "推荐当时论点"},
		MustAnswer:       []string{"方向对不对/论点哪部分成立失败/一条可迁移教训", "idx 关联程序校验（越界=伪造关联丢弃）"},
		ForbiddenActions: []string{"判定输赢（outcome 程序归类）", "注入推荐 prompt（影子三不）", "改写 action/置信度"},
		Fallback:         "生成失败本轮跳过（幂等下轮再试）；检索失败影子字段空串零噪声",
		CounterExamples:  []string{"TestReflectionAvailableFromFilter", "TestReflectionShadowJSONNotMutatePicks", "TestReflectionBelowThresholdNoCall"},
	},
	"experiment": {
		RoleID: "experiment", Name: "challenger 影子研究员（prompt_challenger）",
		Version: recPromptVersion, SchemaVersion: "recommendation.v2",
		Purpose: "P2-1 champion/challenger 实验的影子采样：同一批次候选名单只换 challenger 任务段再调一次，对照结构化质量/名单重合/成本（输出永不进业务结果）",
		Market:  "cn", Horizons: "同推荐",
		Trigger:          "flag llm_challenger（缺省关）开 + 该用户存在 running 实验 + 样本未达标 + 推荐主调成功；仅命中实验创建者本人的生成（单变量对照）",
		InputWhitelist:   []string{"与推荐主调完全相同的候选名单/市场环境/用户约束（仅任务段不同）"},
		MustAnswer:       []string{"与推荐主调同 schema（picks/rejected JSON）", "无 repair——影子对照测『一把过』的结构化质量"},
		ForbiddenActions: []string{"改写业务批次 picks/置信度（输出只落 llm_experiment_runs，测试锁定字节一致）", "跨用户采样", "晋级绕过 P1-9 发布质量门（PromoteLLMExperiment 硬检）"},
		Fallback:         "任何失败只落样本行 Error 字段（失败也是实验数据），业务批次零影响",
		CounterExamples:  []string{"TestChallengerShadowNotMutateBusiness", "TestLLMExperimentPromoteGate"},
	},
}

// LLMRoleRegistry 输出角色资产列表（按 RoleID 稳定排序，预算表回填 token/repair——
// llmModuleBudgets 是预算单一权威，本表不重复声明）。
func LLMRoleRegistry() []LLMRoleAsset {
	out := make([]LLMRoleAsset, 0, len(llmRoleAssets))
	for id, a := range llmRoleAssets {
		b := moduleBudget(id)
		a.MaxTokens = b.MaxTokens
		a.RepairAttempts = b.RepairAttempts
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RoleID < out[j].RoleID })
	return out
}

// LLMRoleDisciplines 输出全局纪律（管理端与 registry 一并展示）。
func LLMRoleDisciplines() []string {
	out := make([]string, len(llmRoleGlobalDisciplines))
	copy(out, llmRoleGlobalDisciplines)
	return out
}
