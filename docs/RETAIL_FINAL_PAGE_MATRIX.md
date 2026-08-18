# 散户体验最终页面矩阵

> 更新时间：2026-08-18。本表是本地实现与检查清单，不是生产验收报告。

## 页面清单

| 页面组 | 路由 | 页面 | 本批结果 |
| --- | --- | --- | --- |
| 认证 | `/setup` | Setup | 复核初始化流程；认证错误统一脱敏 |
| 认证 | `/login` | Login | 保留安全 redirect；认证错误统一脱敏 |
| 认证 | `/login/callback` | OAuthCallback | 保留 OAuth/移动回调和通知目标恢复；错误脱敏 |
| 首页 | `/` | Home | 复核个人工作台；修复名称缺失时的代码冒充 |
| 市场 | `/mood` | Mood | 复核日期、排行、详情入口和失败态 |
| 市场 | `/news` | News | 增加最新发布时间；关联标的改用统一股票身份 |
| 市场 | `/heatmap` | Heatmap | 增加失败、未知时间和下一步；保留板块深链 |
| 市场 | `/boards/:code` | BoardDetail | 增加失败、数据截止时间和下一步 |
| 市场 | `/etf` | Etf | 明确行情时间、stale 估值和模拟账户边界；修复名称回退 |
| 股票 | `/stocks/:market/:symbol` | StockDetail | 拆分页签/移动操作；首屏接入统一持仓卖出评估 |
| 工作 | `/today` | Today | 复核 ledger 收件箱、深链和独立完成状态 |
| 工作 | `/daily-report` | DailyReport | 复核未持有推荐只做研究追踪 |
| 工作 | `/watchlist` | Watchlist | 复核分组、搜索、批量、详情、分析和提醒 |
| 研究 | `/screener` | Screener | 拆分快速选股、扫描结果和历史；保留高级条件/版本审计 |
| 研究 | `/backtest` | Backtest | 增加历史表现边界；保留参数、结果、明细和指标 |
| 账本 | `/positions` | Positions | 复核统一卖出决策中心；修复名称缺失文案 |
| 账本 | `/portfolio-risk` | PortfolioRisk | 拆分结论/专业指标；修复 `risk` 页签深链 |
| AI | `/analysis` | Analysis | 保留第三批工作台和显式触发边界 |
| 任务 | `/tasks` | Tasks | 复核排队、运行、成功、部分成功、失败、取消和重试 |
| AI | `/qa` | Qa | 复核股票上下文、快照时间、引用边界和显式发送 |
| 研究 | `/compare` | Compare | stale/unknown/失败不进入正常排名；增加截止时间和术语帮助 |
| 模拟 | `/paper` | Paper | 首屏明确模拟账户/持仓/订单与真实账本隔离 |
| 研究 | `/prompt-templates` | Prompts | 复核模板表单和错误态 |
| AI | `/recommendations` | Recommendations | 保留第三批工作台；修复名称缺失文案 |
| 工作 | `/alerts` | Alerts | 保留提醒规则/事件；修复作用域股票身份 |
| 工作 | `/thesis` | ThesisCards | 复核股票身份、详情深链、编辑状态 |
| 工作 | `/notes` | Notes | 复核股票身份、详情深链、编辑恢复 |
| 设置 | `/settings` | Settings | 保留 LLM、偏好、通知通道、浏览器通知和账号页签 |
| 管理 | `/admin` | AdminSettings | 复核紧凑表格、手机卡片、筛选和敏感字段边界 |
| 管理 | `/admin/llm-calls` | AdminLlmCalls | 复核筛选、分页、调用详情和横向滚动 |
| 管理 | `/admin/factor-ic` | AdminFactorIc | 复核空态、错误态和只读报表 |
| 管理 | `/admin/walk-forward` | AdminWalkForward | 复核空态、错误态和只读报表 |
| 管理 | `/admin/selection-eval` | AdminSelectionEval | 复核宽表、筛选、详情和错误态 |
| 管理 | `/admin/calibration` | AdminCalibration | 复核宽表、样本不足和错误态 |
| 管理 | `/admin/llm-roles` | AdminLlmRoles | 复核角色列表和错误态 |
| 管理 | `/admin/llm-experiments` | AdminLlmExperiments | 复核实验表单、运行状态和结果区 |
| 管理 | `/admin/joint-eval` | AdminJointEval | 复核只读联合评估和错误态 |
| 兜底 | `/:pathMatch(.*)*` | Placeholder | 保留站内返回入口 |

## 组件边界

- `components/screener/ScreenerQuickSelect.vue`：散户确定性模板、场景/风险/数据要求和最多三个参数。
- `components/screener/ScreenerScanResults.vue`：当前扫描结果、股票身份、命中原因、数据截止、批量选择和股票动作。
- `components/screener/ScreenerHistory.vue`：历史任务与不可变结果快照；不与当前结果混排。
- `components/stock-detail/StockDecisionSummary.vue`：价格、时效、本人关系、机会/风险和高频动作。
- `components/stock-detail/StockDetailResearchTabs.vue`：行情技术、新闻事件、财务估值和研究证据页签状态。
- `components/stock-detail/StockMobileActions.vue`：手机端固定高频动作和可访问性标签。
- `components/portfolio-risk/PortfolioRiskConclusion.vue`：白话结论、最需处理风险、时间和样本范围。
- `components/portfolio-risk/PortfolioProfessionalMetrics.vue`：Alpha、Beta、Sharpe、Sortino、回撤和波动指标。

## 全站规则

- 股票身份：名称为主，代码和市场为辅；缺名称统一显示“名称待补全”，不得复制代码充当名称。
- 术语：专业指标走 `TermHelp`/`termDictionary.ts`；简明模式不隐藏风险、缺失和时效。
- 数据状态：`loading`、`empty`、`partial`、`stale`、`unknown`、`failed` 分开表达；未知和部分结果不得显示为正常或成功。
- AI：只有明确按钮可以创建 AI 调用；加载、路由切换、标签切换和详情展开只允许 GET/恢复已有任务。
- 持仓：本人卖出风险只读 `PositionExitAssessment`；未持有标的明确为研究；模拟账户不写真实账本。
- 危险操作：删除、归档、重置和平仓继续使用确认；批量自选保留 100 只上限、服务端幂等和撤销冲突规则。

## 验收边界

本批本地检查范围以最终提交报告中的实际命令和视觉结果为准。生产真实数据、真实 Web Push、交易日盘后任务、长任务跨会话恢复、真实权限隔离、OAuth 生产回调和移动真机仍需线上验收；仓库未写入或伪造任何生产配置。
