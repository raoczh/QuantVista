# AI 研究、持仓闭环与全站界面复核

本轮开始于 2026-09-15，基于提交 `44323f2`。目标是继续扩展专门覆盖 AI 研究编排、证据评测、持仓退出和通知的外部项目，并重新审视全部页面的信息结构、样式和交互。本文记录实际源码证据、实施范围与验证结果；未完成项不会记为已验收。

## 1. 范围与验收依据

| 项目 | 验收依据 | 当前状态 |
| --- | --- | --- |
| 专项参考项目 | GitHub 查询、固定提交、相关源码入口、采纳或不采纳的理由 | 10 项已对照，见下表 |
| AI 研究与证据 | 真实调用路径、输入与输出契约、针对性本地回归 | 独立复核证据池与共享纪律已实现，定向回归通过 |
| 持仓退出与通知 | 保护条件、提醒状态与用户操作的闭环，失败和重复处理边界 | 退出语义已对照；通知隔离修复与定向回归通过 |
| 全站结构与视觉 | 38 个路由入口，桌面、平板与手机布局，明暗主题 | 基准矩阵与六套主题代表场景通过，见第 5 节 |
| 关键交互 | 导航、键盘焦点、页签、加载/空/失败/部分结果、显式 AI 操作 | 所列关键场景通过，见第 5 节 |
| 回归与交付 | 后端测试、前端测试与类型检查、浏览器验证、生产构建检查 | 本地检查通过；环境边界见第 5.3 节 |

外部源码用于静态对照，不运行其交易、采集和训练脚本，不直接复制不同许可证的实现。所有业务测试使用本地隔离数据或 mock，不读取应用 `server/.env`，不访问线上临时数据库，不调用真实 AI。用户已有的未提交修改单独保留。

## 2. 专项参考项目

通过 GitHub 公共 API 检索并核对项目元数据，当前选取下列十项。星数只用于确认关注规模，不作为准确率或实现质量的证明。源码位于 `D:\TestWorkSpace\_refs\ai-research-experience-20260915`；大型仓库按相关目录稀疏检出。

| 项目与固定提交 | 实际源码入口 | 结论与落地范围 | 根许可证 |
| --- | --- | --- | --- |
| [virattt/dexter](https://github.com/virattt/dexter/tree/ecaed3011f24ea24ef687ab536aa7f22f7294038) `ecaed3011f24` | `src/agent/agent.ts`、`src/evals/evaluator.ts` | 逐项正确性/矛盾判定、上下文裁剪和任务完成状态。借鉴独立事实与反证核对；未引入开放式工具执行，也不采用缺失矛盾项默认无矛盾的评分作为放行条件。 | 根未见许可证 |
| [TauricResearch/TradingAgents](https://github.com/TauricResearch/TradingAgents/tree/be952b8eccb49720509af544c6675233bc1f10d0) `be952b8eccb4` | `tradingagents/agents/managers/portfolio_manager.py` | 新版允许证据冲突或不足时 Hold，要求按证据而非发言顺序判断。补强本项目多空复核的共享研究纪律及独立证据池；不引入五档评级来代替已有执行门槛。 | Apache-2.0 |
| [AI4Finance-Foundation/FinRobot](https://github.com/AI4Finance-Foundation/FinRobot/tree/6d6ccd32c1b8b1904dc656cf06897438aba3daec) `6d6ccd32c1b8` | `finrobot/functional/ragquery.py`、`finrobot/agents/prompts.py` | 财报/电话会按报告和说话人筛选上下文。证据必须保留证券、期间与来源身份；本项目继续使用冻结快照，不接入共享向量库或自动执行分析脚本。 | Apache-2.0 |
| [confident-ai/deepeval](https://github.com/confident-ai/deepeval/tree/1e4f9e6e9f0bdf01a88f9e4d69d0eb5d0de634c1) `1e4f9e6e9f0b` | `deepeval/metrics/faithfulness/faithfulness.py` | 区分支持、矛盾和不确定；所读实现的无论断得分为 1、不确定默认可计分，不直接用于金融研究放行。本轮将无独立证据明确标为无法复核，保留数值存在性与结论正确性的区别。 | Apache-2.0 |
| [freqtrade/freqtrade](https://github.com/freqtrade/freqtrade/tree/eec4eb074bd919d405fb60be8eaee49d3a49b511) `eec4eb074bd9` | `freqtrade/persistence/trade_model.py`、`freqtrade/strategy/interface.py` | 对照止损只收紧、成交后刷新、退出信号/止损/ROI/移动保护的排序。其做空、杠杆及加仓刷新规则不适配普通 A 股；保留本项目 T+1、真实成交账本和独立价格保护。 | GPL-3.0 |
| [vnpy/vnpy](https://github.com/vnpy/vnpy/tree/fa5206fe63836f3f8cd1ebd7168fbd19a5e2ff09) `fa5206fe6383` | `vnpy/trader/engine.py`、`vnpy/trader/event.py` | 订单、成交、持仓分别维护事件状态。继续区分“触发保护”“通知已投递”“实际已卖出”，不以阅读提醒或 AI 意见替代真实成交。 | MIT |
| [caronc/apprise](https://github.com/caronc/apprise/tree/5c2b104025ad09b63298ee32b6d838b55aa243fb) `5c2b104025ad` | `apprise/apprise.py` 的 `_notify_parallel_asyncio` | 通道并发与失败隔离。落实独立并发投递，修复慢通道耗尽后续通道预算；取消/超时结果使用短预算落库。未照搬对交付状态未知的外部消息自动重发。 | BSD-2-Clause |
| [novuhq/novu](https://github.com/novuhq/novu/tree/a3d9eeca7077da31462457e648ac19ce6c6a5803) `a3d9eeca7077` | `apps/worker/src/app/workflow/usecases/send-message/send-message-in-app.usecase.ts` | 消息创建、投递、已读与执行明细分离。保留本项目收件箱完成状态与通知投递状态分离，外部通道结果不得伪装成用户已处理。 | MIT，另有企业目录许可 |
| [ghostfolio/ghostfolio](https://github.com/ghostfolio/ghostfolio/tree/02e74d6e1dfdc77d51a1c971f734ce0b59501885) `02e74d6e1dfd` | `apps/client/src/app/pages/home/home-page.component.ts` | 首页按概览、持仓、汇总、自选和市场分层，页签与路由相连。用于本轮工作流导航及研究结果/历史视图的结构参考，未迁入 Angular 组件。 | AGPL-3.0 |
| [Fincept-Corporation/FinceptTerminal](https://github.com/Fincept-Corporation/FinceptTerminal/tree/09b70f3bc5c751d0e9507cb877de0270445034fb) `09b70f3bc5c7` | `fincept-qt/src/ui/theme/ThemeTokens.h`、`fincept-qt/src/ui/navigation/NavigationBar.cpp` | 主题统一声明表面、文本、边界和语义色。将主题变量集中下发到应用及认证页面，统一页面骨架；不照搬固定 LIVE 标签或桌面终端的高密度小字号。 | AGPL-3.0 |

## 3. 界面方向

以现有 Vue 3 与 Naive UI 为基础，统一工作区导航、页头、主操作与内容层级。桌面优先呈现高频工作流，手机保留直接可达的导航和完整操作；六套主题继续通过统一主题变量生效。专业指标按任务组织，解释和系统版本不挤占日常操作位置。

页面改造必须保留数据时效、未知、部分失败、实际与模拟账户边界；路由访问、页签展开和筛选不能隐式创建 AI 调用或交易。设计验收同时检查内容丰富、空数据和错误场景。

## 4. 本轮实施

### 4.1 复核能看到主分析遗漏的事实

原证据白名单来自主分析已经引用的数字。主分析遗漏负利润，后续看空角色可能也没有可引用的亏损证据；模型计划价等复述来源还可能混入事实白名单。

`db3` 改为直接消费冻结快照的行情、技术、估值、财报和机构观点事实。关键价格、盈利金额和可比期间优先，在原有最多 30 项的预算内保留真实零、负数、来源与报告期；相同路径去重，派生重复值、模型计划价和用户阈值不充当独立事实。只有路径、数值均与快照一致的主分析引用才复用编号。无独立事实时返回 `evidence_unavailable`，不继续调用模型。

四个辩论角色共享研究与盈利纪律，明确区分“引用了真实数字”和“该数字支持结论”。结果新增可展开的证据索引，旧记录仍可查看。没有增加角色或交锋轮次；新增纪律会增加少量输入文本。程序索引可验证引用来源，不能独自判定所有自然语言因果结论。

实现：[事实索引](../server/service/analysis_debate_evidence.go)、[编排与提示词](../server/service/analysis_debate.go)、[结果展示](../web/src/components/analysis/AnalysisResultWorkspace.vue)。回归覆盖遗漏亏损、真实零、伪造路径值、重复引用、模型自证、预算截断、无证据零调用及旧记录兼容。

### 4.2 通知通道独立投递和记录

原逐通道串行投递共用超时，第一个慢通道可能耗尽后续通道的预算。现在在每用户最多 10 个通道的范围内使用有界并发，各通道分别尝试投递。取消和 HTTP 超时后使用独立的 3 秒短预算记录已尝试通道的结果，避免界面继续留下旧的成功状态；落库仍核对通道、用户、类型和加密目标身份。

不会在取消后继续发送，也不自动重发交付状态未知的外部消息。失败通道的时间标为“最近尝试”，成功通道保留“最近发送”；失败详情继续采用安全提示。站内完成状态、外部投递和真实成交保持独立。

实现：[通知服务](../server/service/notify.go)、[隔离回归](../server/service/notify_isolation_test.go)、[通知设置](../web/src/components/settings/NotificationSettings.vue)。回归通过阻塞一个通道，验证另一个通道在前者结束前已经收到消息，并分别保存成功和取消结果。

### 4.3 全站工作区与信息层级

| 范围 | 最终行为 |
| --- | --- |
| 导航 | 桌面 232px 侧栏按日常、研究、市场、持仓复盘、工具、管理分组；当前组自动展开，普通用户不显示管理入口；平板和手机使用抽屉，手机保留原底部导航。 |
| 页面骨架 | 集中下发主题变量，统一页头、主操作、卡片间距、数字层级、焦点与减少动画设置。每页保留一个主标题，嵌入的组合风险使用二级标题，卡片避免嵌套重复 heading。 |
| 首页 | AI 快捷入口收进页头，个人持仓和待办先于市场数据；有行情缺口时明确标出“已定价持仓盈亏”，不能把部分金额当成完整组合盈亏。 |
| 推荐与分析 | 共用本次结果/历史复盘工作区，URL 保存视图；完成结果后研究条件默认收起；成功任务记录折叠，运行、失败及降级状态仍直接展示。 |
| 研究交互 | 页签支持左右箭头、Home/End 和焦点移动；全局搜索具有对话框与组合框语义，支持键盘选中、关闭和焦点返回。浏览和展开操作不提交研究任务。 |
| 结果与风险 | 等待入场、数据不足和实际持仓的语义保留；主要风险使用警示色，版本字段集中在审计详情，历史推荐统一称“推荐结果”。 |
| 个股首屏 | 简明模式解释数据完整、部分可用和未知状态，原始证据与专业模式保留程序字段；平板上持仓指标整组换行，完整时间不再被挤在过窄的列中；风险等级与涨跌颜色分开表达。 |
| 认证入口 | 桌面介绍与表单双栏、手机简化；登录、初始化与回调共用标题结构，消除重复标题，并补齐输入名称和自动填充属性。 |

实现主要在 [AppShell](../web/src/components/AppShell.vue)、[导航目录](../web/src/navigation/workspace.ts)、[ResearchWorkspace](../web/src/components/ResearchWorkspace.vue)、[PageContainer](../web/src/components/PageContainer.vue)、[SectionCard](../web/src/components/SectionCard.vue)、[AuthShell](../web/src/components/AuthShell.vue) 和各页面。保留 Vue 3、Naive UI、六套主题与移动端技术栈。

## 5. 页面覆盖与验证口径

### 5.1 路由入口

以下 38 个入口均纳入同一浏览器检查：有主内容与唯一主标题、无脚本异常、无页面级横向溢出、无 `NaN`、无未明确模拟的 API、无浏览引起的写请求。宽表可以在自己的容器内滚动。

| 分组 | 入口 | 数量 |
| --- | --- | ---: |
| 日常 | `/`、`/today`、`/watchlist`、`/tasks` | 4 |
| 发现与研究 | `/screener`、`/recommendations`、`/analysis`、`/qa`、`/compare` | 5 |
| 市场与详情 | `/mood`、`/news`、`/heatmap`、`/etf`、`/stocks/cn/600100`、`/boards/BK0477` | 6 |
| 持仓与复盘 | `/positions`、`/portfolio-risk`、`/alerts`、`/daily-report`、`/thesis`、`/notes`、`/paper` | 7 |
| 工具与设置 | `/backtest`、`/prompt-templates`、`/settings` | 3 |
| 管理与评估 | `/admin`、`/admin/llm-calls`、`/admin/factor-ic`、`/admin/walk-forward`、`/admin/selection-eval`、`/admin/calibration`、`/admin/llm-roles`、`/admin/llm-experiments`、`/admin/joint-eval` | 9 |
| 认证与兜底 | `/login`、`/setup`、`/login/callback`、`/page-not-found` | 4 |

基准矩阵使用桌面 1440×1000、平板 834×1112、手机 390×844，分别运行浅蓝与深蓝。本轮均为系统 Edge 的视口验证，不代表跨浏览器或真机验收。六套外观另在推荐结果场景逐项验证；这不表示每个页面的每套主题都执行了所有交互。

### 5.2 有数据、失败和关键交互

固定合成日期为 2026-09-15。全部 `/api/` 请求在浏览器内拦截；不启动后端、不调用真实 AI、不发送真实通知。没有行情的持仓与部分失败场景不会用完整成功响应替代。

| 场景 | 检查重点 |
| --- | --- |
| 首页、待办、自选 | 重点标的、部分行情、部分待办来源失败、加载期间不能出现零风险结论。 |
| 推荐结果与历史 | 等待区间及风险可见，不显示为已满足价格条件；历史视图与指定批次刷新后保留。 |
| AI 分析与复核 | 负利润、报告期和来源可核对；无证据明确停止复核；页签键盘操作与历史恢复。 |
| 持仓与组合 | 紧急保护优先；未知行情不产生 0% 盈亏；全部持仓、嵌入风险、风险图表和真实/模拟现金流边界。 |
| 个股、新闻、任务与设置 | 长内容、失败任务、行情时效、通知通道分别显示成败；管理调用表在窄屏容器内滚动。 |
| 导航与搜索 | 普通用户入口、移动抽屉关闭、当前路由标记、搜索键盘打开个股与关闭后焦点返回。 |
| 请求失败 | 自选、新闻、设置和模型调用页展示错误；不把读取失败当作空成功。 |

自动化位于 [workspace.spec.ts](../web/e2e/workspace.spec.ts) 和 [research-workspace.spec.ts](../web/e2e/research-workspace.spec.ts)。人工视觉复核使用 [inspect-page.mjs](../web/e2e/inspect-page.mjs)，通过 `QV_VIEW_PATH`、`QV_VIEW_WIDTH`、`QV_VIEW_HEIGHT`、`QV_VIEW_THEME` 和 `QV_VIEW_RICH=1` 指定场景；截图只在内存中传回，不生成临时图片文件。

### 5.3 最终执行结果

2026-09-15 完成以下检查：

| 检查 | 结果 |
| --- | --- |
| `go test -json -p 1 ./... -count=1` | 全量通过；2,336 个通过事件（含子用例）、94 个跳过事件。子进程清除 `LIVE_*`、`QV_REVIEW_MYSQL`、`SQL_DSN`、`MYSQL_DSN`。 |
| `go vet -p 1 ./...` | 通过。 |
| `go mod verify` | 全部模块校验通过。 |
| `npm test` | 现有前端回归全部通过。 |
| `npm run type-check` | 通过。 |
| `npm run test:ui` | 456 例通过，约 8 分钟。 |
| 个股与推荐局部终验 | 54 例通过，覆盖个股简明文案、三尺寸持仓指标、等待条件与六套主题。 |
| Vite 生产构建 | 通过 Node API `build({ build: { write: false } })` 完成，114 项输出在内存中校验。 |
| 文档与文件 | 新报告的十个引用提交均与本地参考仓库一致；README/报告本地链接有效，本轮文件通过 UTF-8 与常见凭据模式检查。 |

456 例由 228 个路由布局、96 个有数据视图、24 个失败视图、36 个外观检查和 72 个关键交互/加载场景组成。个股页最后的简明文案及平板指标调整另做局部终验。

人工查看了桌面推荐与分析、平板个股、手机首页与持仓、桌面及手机认证页，涵盖浅蓝、深蓝、极客绿、典雅紫、暖夜橙、樱桃红六套外观。截图未写入仓库。

环境边界：当前 Go 为 `CGO_ENABLED=0`，没有可用的 GCC/Clang，因此无法执行 `go test -race`。不为此安装系统编译器；通知并发行为由确定性的本地阻塞/取消测试验证。MySQL 专项、真实模型/行情、连续交易日运行和 Android 真机体验不计入本轮本地验收。

## 6. 与原 Top50 研究的关系及后续方向

原 [Top50 业务审查](TOP50_BUSINESS_ALGORITHM_REVIEW.md) 覆盖 50 个固定样本，另补充 Backtrader、Qlib、Alphalens；本轮再补充上表 10 个专项仓库。已对照的内容包括算法、财报事实、提示词、证据、任务编排、持仓保护、通知、信息结构和主题系统。

此前已落地的盈利分类、30 个内置策略、共同研究价、时间外排序研究、持仓分阶段保护和任务恢复继续使用。本轮增量集中在独立复核事实、通知失败隔离和全站工作区；没有仅凭外部项目的阈值或展示置信度重新调权重。

后续最有价值的工作仍是用成熟的时间外样本评估推荐增量、积累覆盖完整的财务/行业历史，以及用真实运行记录评估证据支持率和通知效果。基金持仓穿透、现金流硬筛选、更复杂的形态或开放式代理编排需要额外数据和验证。本轮没有把这些储备方向记为已实现，也不能据此宣布实盘准确率已经提高。
