# 杂项改进批次待办（2026-07-09，接力文档）

> 本批非主线开发。已完成 6 项（见下），剩 WP6/WP7/WP8 三项。新会话按顺序执行，每包独立提交，全部完成后 push、更新记忆、**删除本文档**（并入 ROADMAP 后不留孤本）。

## 已完成（勿重做）

| 包 | commit | 内容 |
|---|---|---|
| WP1 | force push `43fe96b...6eeae02` | git 历史去 Claude 署名（97 提交仅 1 个带，filter-branch 重写；备份分支 `backup-before-rewrite` + `../quantvista-backup-20260709.bundle`，**全批完成后删备份分支**） |
| WP2 | `2fb8357` | 推荐股价上限默认 ≤50 元（defaultRecFilters 短线长线 + 前端 emptyRecFilters/设置页初值；用户偏好里存过 10 的须自行改一次——交付说明提） |
| WP3 | `c7b82ad` | 新闻采集间隔可配（1~120 分钟默认 5，自调度循环下一轮生效）+ 自动 LLM 总闸 news_auto_llm（关=纯规则分析）；管理后台"新闻采集"卡片 |
| WP4 | `4a5c679` | 无 LLM 配置用户回退管理员默认配置（ResolveForUse；配额记发起用户；allowPrivate 按配置所有者 llmAllowPrivate） |
| WP4b | `5f06990` | 回退总闸 llm_fallback_enabled（缺省开）+ 指定回退配置 llm_fallback_config_id（0=自动首管默认；resolveSystemFallbackConfig 统一用户回退与新闻的系统默认 LLM；失效静默回落）；管理后台"LLM 回退"卡片 |
| WP5 | `046c2a9` | LLMConfig 加 endpoint_type（chat_completions/responses），ai_client_responses.go 按 new-api relayconvert 口径适配双端点（映射/流式/usage/错误），chat 复查对齐（extractErr 宽容/stream_options.include_usage+fallback/content_filter），测试连接分流，前端端点下拉 |

后端 go build/vet/test 全绿、前端 vue-tsc 通过（node 需 v24）。**本地提交尚未 push**（除 WP1 已 force push），新会话完成剩余任务后一起 push。

## WP6：LLM 调用审计日志 + 管理员查看页

需求：管理员后台可查看**所有用户**的 LLM 调用明细，可筛选用户，内容详细。用户已确认：**存请求/响应全文 + 90 天自动清理**。

现状（已探明）：无统一审计表；compare 点评与新闻增强完全不落明细；`chatCompletion`/`chatCompletionStream`（server/service/ai_client.go）是**唯一两个出口**，在此埋点全覆盖。

1. `server/model/llmlog.go` 新表 `LLMCallLog`：ID / UserID(index) / Module(size:32，取值 analysis|analysis_review|trade_plan|recommendation|rec_review|qa|compare|daily_report|news|test) / LLMConfigID / Provider(size:32) / Model(size:64) / EndpointType(size:24) / Stream(bool) / Status(size:16 success|error) / ErrorMsg(size:512) / PromptTokens / CompletionTokens / TotalTokens / LatencyMs / RequestBody(`gorm:"type:text"`，messages JSON，>60KB 截断) / ResponseBody(同上，正文或错误体) / CreatedAt(index)。注册进 `model/main.go` AllModels()
2. 埋点：`chatParams`（ai_client.go:51 附近）加 Meta 字段（CallerUserID int64 / Module string / ConfigID int64——**注意别与现有字段混淆，UserID 是发起用户**）；`chatCompletion` 与 `chatCompletionStream` 的成功/失败出口**同步**写一行（`common.DB == nil` 直接跳过——现有直调 ai_client 的单测不炸；写失败仅 SysWarn 不影响主流程）。responses 分支（ai_client_responses.go）走的是同两个入口函数，天然覆盖
3. 10 处 caller 组 chatParams 时补 Meta（都已透传 EndpointType，同位置加）：analysis.go:320（callWithRepair，module 按调用方——需从上层传或在 AnalysisService 记；trade_plan 复用同函数，可从 caller 传 module 串进 chatParams）/analysis.go:494（analysis_review）/compare.go:250/dailyreport.go:484,502/newsai.go:211（news，UserID=配置所有者 adminID）/qa.go:69,89/recommendation.go:525（recommendation）,594（rec_review）；llm.go testOpenAICompatible（module=test，UserID 需从 TestByID/TestByInput 传入——测试连接也记）
   - callWithRepair 内部循环多次调用会记多行（repair 各一行）——**这是特性不是 bug**（真实调用次数审计），module 相同即可
4. 清理 job：每日 03:25 删 90 天前记录（照 news.go 03:10 TTL goroutine 模式写 `StartLLMLogJobs()` 挂 main.go，或并进 StartNewsJobs 同级）
5. API（router/api.go 的 admin 组，AdminAuth 已就绪）：`GET /api/admin/llm-calls?user_id=&module=&status=&page=&page_size=`（列表 **Select 排除两 TEXT 列**防大响应；返回 {items,total}，items 带 username——users 表二查映射）；`GET /api/admin/llm-calls/:id`（全文详情）。controller/admin.go + service/admin.go 照 GetUserQuota 模式
6. 前端：新页 `web/src/pages/AdminLlmCalls.vue`（路由 /admin/llm-calls，meta:{admin:true}，router/index.ts 照 /admin 项）：筛选行（用户下拉复用 listUsers、模块下拉、状态下拉）+ n-data-table 分页（时间/用户/模块/provider/模型/端点/token/耗时/状态）+ 行点击弹窗看请求与响应全文（`<pre>` 滚动）；`AppShell.vue:137` 附近用户菜单 admin 组加"LLM 调用记录"入口；admin.ts 加类型与 API
   - **硬约束**：6 主题兼容（无硬编码色，用 useUi/Naive 变量）、页面单根节点（白屏防回归）、移动端适配（ARCHITECTURE §4.2/4.3）
7. 单测：埋点三态（非流式成功/失败、流式成功）落行字段正确（含 module/endpoint_type/token）；列表筛选 user_id/module/分页 total；清理边界（90 天内外各一行）。测试里 `common.EncryptionKey = "unit-test-key"`、httptest 服务器要 `AllowPrivate: true`（SSRF 拦 127.0.0.1，本批 WP5 测试踩过）

## WP7：README 重写（GitHub 通用格式）+ 全文档更新

1. **README.md 重写**：简介定位（个人自用 AI 股票研究平台）→ ✨ 功能特性（对齐现状全量：市场看板/个股详情（K线+MACD/BOLL+筹码峰+主力资金+龙虎榜+财务+公告）/新闻情绪（间隔可配+LLM 开关）/AI 分析（五模块+多角色+回溯诊断+交易计划）/问答流式/对比/推荐四阶段流水线+信任层/选股 21 策略/回测时光机/推荐追踪/收盘日报/提醒待办/持仓+模拟盘+ETF/板块热力图/管理后台（用户配额+LLM 调用审计+新闻配置+LLM 回退））→ 🏗️ 技术栈 → 🚀 快速开始（源码：Go + Node≥20（实测 v24）、`SQL_DSN=local`；Docker：buildx→宝塔 compose，详链 DEPLOYMENT）→ ⚙️ 环境变量表（从 main.go/common 核对：SQL_DSN/ENCRYPTION_KEY/GITHUB_CLIENT_ID/SECRET/ALLOWED_ORIGINS/PORT 等）→ 🔐 安全特性（**LLM API Key AES-256-GCM 加密落库 + json:"-" 不回显——用户点名要记录**；JWT 双 token+TokenVersion；SSRF 防护；认证限流；备份须同 ENCRYPTION_KEY）→ 📚 文档索引 → 📄 License（Apache-2.0，配 WP8）→ ⚠️ 风险与免责（保留现有合规口径：个人自用/研究参考非投资建议/无牌照不得公开荐股）
2. **docs/ARCHITECTURE.md**：§5 后端模块补 M1~M3c 与本批（factortable/screener/backtest/mood/fundflow/intraday/board/analysis_trader、llm_call_logs 审计）；§6 AI 调用设计补：管理员回退与回退配置（resolveSystemFallbackConfig）、endpoint_type 双端点（ai_client_responses.go）、调用审计埋点、新闻 LLM 总闸；§9 前端页面补 /news /screener /backtest /heatmap /boards/:code /admin/llm-calls
3. **docs/ROADMAP.md**：§1 当前状态刷至 2026-07-09（M3c + 本杂项批）；§5 欠人工验证清单加本批目验项（管理后台三张新卡片/LLM 调用记录页/回退真实命中/responses 端点真实上游冒烟/推荐默认 ≤50 生效）
4. **docs/DATA_SOURCES.md**：补已接入扩展源（腾讯 mkline 5 分钟线、东财板块热度榜/龙虎榜/涨停池/人气榜/资金流），§7 待解锁清单勾掉已实现项
5. **docs/DEPLOYMENT.md**：环境变量核对；新增"系统配置（管理后台）"小节：新闻采集间隔/自动 LLM 开关/LLM 回退开关与指定配置/调用日志 90 天自动清理
6. **docs/DEVELOPMENT_PLAN.md**：9 个旧 hash 替换为新 hash（见下表）
7. 记忆文件更新（quantvista-progress.md 本批条目已建，补 WP6~8 完成态）

### git 历史重写 hash 映射表（WP1 产物，文档/记忆更新用）

| 旧 | 新 | 批次 |
|---|---|---|
| dc26e7f | 5460aea | N1 后端 |
| 69e7b3a | 4a46ef9 | N1 前端 |
| 11657d1 | 2448abd | N2 |
| 7074ec6 | bc1614a | F1 |
| b03bee3 | 77b0ed3 | T1 |
| 3b5212f | cc9d76f | S1 |
| 53b7646 | 1d8baec | F2 |
| b47648e | 7a0eeab | M1 一 |
| 9e494f7 | 8bd06d0 | M1 二 |
| fd2e7cb | 5d5c5ef | M2 |
| 0250fdd | a35aa13 | M3a |
| 3525182 | 6b2f505 | M3b |
| 29c58e5 | 1411128 | M3c |
| 43fe96b | 6eeae02 | M3c docs |

（提交**消息里**引用的旧 hash 不再二次重写——避免再一轮 hash 级联；只改文档文件与记忆。）

## WP8：Apache License 2.0

- 仓库根新建 `LICENSE`：Apache License 2.0 标准全文（版权行 `Copyright 2026 raoczh`）
- README 加 License 徽章/段落（与 WP7 同一提交即可）

## 验证与纪律（每包必做）

- 后端：`cd server && go build ./... && go vet ./... && go test ./...` 全绿
- 前端：`cd web && npm run type-check`；最后一包跑 `npm run build` 后 **`git checkout -- server/web/dist/index.html`**（构建会改写它，防混入提交——历史踩坑）
- ai_client.go 是全 AI 链路心脏：改动保持 chatCompletion/chatCompletionStream 对外签名与语义不变，QA AskStream 端到端测试是回归底线
- llm_call_logs 的 RequestBody 含用户数据（持仓等），仅 AdminAuth 可见，文档注明
- 提交消息不带任何 AI 署名；全部完成后 `git push origin main` + 删 `backup-before-rewrite` 分支 + 更新记忆 + 删本文档
