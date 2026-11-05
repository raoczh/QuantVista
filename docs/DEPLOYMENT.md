# 部署说明

本项目部署方式完全参照 `new-api`：本地构建镜像 → 推 Docker Hub → 宝塔容器编排更新重启。MySQL 由宝塔在宿主机运行，Redis 由编排内置。

## 1. 一次性准备

### 1.1 宝塔 MySQL 建库建账号

在宝塔 MySQL 里：

- 新建数据库 `quantvista`，字符集 `utf8mb4`，排序规则 `utf8mb4_general_ci`。
- 新建账号 `quantvista` 并授权该库（与 `deploy/.env` 的 `DB_USER` / `DB_PASSWORD` 一致）。
- 确认 MySQL 允许从 Docker 网段（`172.18.0.1` 对应的 host-gateway）连接。

> 应用**不会自动建库**，只会自动建表。库要先手动建好。

### 1.2 宿主机目录

```bash
mkdir -p /www/wwwroot/quantvista/{data,redis-data}
```

> 应用日志走容器 stdout（`docker logs` / 宝塔容器日志查看），无文件日志目录。

### 1.3 GitHub OAuth App（可选，登录需要时再配）

到 GitHub → Settings → Developer settings → OAuth Apps 新建应用。生产环境应先配置 HTTPS，
**Authorization callback URL 填 `https://<你的主域名>/login/callback`**（前端回调页，注意不是 `/api/...`，也不是容器内部端口）。

凭证（Client ID / Secret）**不必写进 `deploy/.env`**：首次用密码登录管理员后，到
**管理后台 → GitHub 登录** 填入并保存（secret 加密落库、可运行时修改）。
`deploy/.env` 里的 `GITHUB_CLIENT_ID/SECRET` 仅作首启种子（有则回填 DB，之后以后台为准）。

### 1.4 生成密钥

```bash
openssl rand -base64 36   # 生成 SESSION_SECRET
openssl rand -base64 36   # 再生成一个作 ENCRYPTION_KEY
```
分别填进 `deploy/.env`。

### 1.5 反向代理与真实 IP（用 nginx 反代时必配）

登录限流、访问日志按客户端 IP 统计。若前面挂了宝塔/nginx 反向代理，需要在
`deploy/.env` 设 `TRUSTED_PROXIES=<代理地址>`（如 `172.18.0.1`，逗号分隔可多个），
应用才会采信代理传来的 `X-Forwarded-For`。**不设置时不信任任何代理头**——反代场景下
所有请求会被视为来自代理 IP，限流会整体误伤；直连部署则保持留空即可。

### 1.6 浏览器 Web Push（可选）

页面仍在运行时可通过长轮询接收事件，不要求 VAPID。后台标签页可能被浏览器冻结；页面休眠或关闭后的投递需要有效 Web Push 订阅。要接收切到桌面后的提醒，应配置以下密钥，并在“设置 → 通知设置”确认当前设备显示“已订阅后台推送”。
在发布前生成一次 P-256 VAPID 密钥对：

```bash
cd server
go run ./cmd/vapidgen
```

把输出的 `VAPID_PUBLIC_KEY`、`VAPID_PRIVATE_KEY` 与联系地址
`VAPID_SUBJECT=mailto:ops@example.com` 一起写入长期保存的 `deploy/.env`。三项必须一起配置，密钥轮换会让
已有浏览器订阅全部失效；**禁止容器每次启动临时生成**。缺失、格式错误或公私钥不匹配时应用仍正常启动，
设置页会显示 Web Push 未配置，仅页面运行期间的通知继续可用。

生产站必须使用 HTTPS，`localhost` 只用于本地开发。反向代理需要正常提供根路径 `/sw.js`，不要给它设置
长期 immutable 缓存。iPhone/iPad 通常还需先“添加到主屏幕”再授权，具体支持取决于系统与浏览器版本。

事件 API 单次长轮询最长 25 秒，前端请求超时 35 秒；反向代理读取超时应至少为 40 秒。部署后让浏览器完成新 Worker 激活。在线事件与 Push 由同一 Worker 展示并去重；IndexedDB 只保存已展示事件编号和时刻。服务端每 15 秒承接待重试投递，临时错误按 15/30/60/120 秒退避，404/410 会禁用失效订阅，超过 5 分钟的旧行情通知不补弹。

部署验收应覆盖首次授权、在线通知、后台页面、Worker 更新和跨账号切换。测试按钮“事件已创建”及推送服务 HTTP 成功都不等同于操作系统已经显示横幅；系统通知权限、勿扰模式及浏览器后台运行设置仍会影响展示。行情规则的评估周期仍约 2 分钟，不是逐笔行情推送。

## 2. 配置文件说明

| 文件 | 是否提交 | 作用 |
| --- | --- | --- |
| `deploy/.env.example` | 提交 | 环境变量模板，占位值 |
| `deploy/.env` | **不提交**（gitignore） | 真实密钥与连接串 |
| `deploy/docker-compose.example.yml` | 提交 | 编排模板 |
| `deploy/docker-compose.yml` | **不提交**（gitignore） | 真实编排，密钥从 `.env` 注入 |

`docker-compose.yml` 本身不含明文密钥，所有敏感值都用 `${...}` 从 `.env` 读取。

## 3. 日常发布流程

见 [`编译推送步骤.md`](../编译推送步骤.md)。简述：

1. 改 `deploy/.env` 的 `IMAGE` tag。
2. 本地 `docker buildx build ... --load .`
3. `docker push ...`
4. 宝塔容器编排更新 tag → 重启。
5. 启动时自动迁移数据库，等健康检查变绿。

## 4. 数据库自动迁移（重点）

后端在监听端口前执行 GORM `AutoMigrate`：**每次启动检查表结构，自动建表、加列、加索引，并执行当前 GORM 支持的兼容性列变更**。表结构和账户、身份、策略归属迁移失败会终止启动；提示词历史基线与代次锚的补齐失败目前会告警、留待下次启动重试，须同时检查这些告警，不能只凭健康检查判断全部补齐。

**能自动做的：**

- 新建不存在的表。
- 给已有表加新列。
- 加新索引。
- 将现有非空列放宽为可空列。本轮统一系统作业会自动把 `job_runs.user_id`、`job_events.user_id` 调整为可空，并创建 `research_artifacts`、补齐 owner、触发人和结果引用字段。
- 本轮 P0-5 会自动给 `alert_events` 增加命中上下文字段，并创建 `job_failure_notifications` 及其 JobRun 唯一键、短窗 group key/merge 索引；不需要额外 SQL 文件。
- 本轮 P0-4 策略结果阶段会自动创建 `strategy_run_results` 及其 owner/kind/状态/时间索引；有 16KiB 上限的规范化请求映射为 MySQL TEXT，最大 2MiB 的结果正文映射为 LONGTEXT。扫描/回测 handler 在作业恢复前完成注册，部署不需要另建队列或执行 SQL。
- 本轮 Top50 U13/U18 会自动创建 `todo_inbox_states` 及 `(user_id, source_kind, source_id)` 唯一索引。该表只保存来源版本、已读、稍后和当日静默；提醒/推荐/任务正文不会复制。模型已进入 `model.AllModels()`，Docker 正常重部署会自动完成，不提供也不需要手工 SQL。
- 价格/策略/通知升级会为 `finance_indicators` 增加 `value_mask`，为 `browser_notification_deliveries` 增加重试时点与租约字段。旧财务缓存不回填猜测的可用性，按正常采集刷新；历史价格计划和已完成结果不重写。规则与特征版本已更新，旧算法的未完成推荐任务会明确要求重新生成，旧学习模型须重新评估后启用。

**不会自动做的（需写迁移代码）：**

- 删除列、重命名列，以及需要清洗或重写存量数据的类型/约束变更。
- 这类变更必须在代码中增加幂等迁移函数，跟 `AutoMigrate` 一起在启动时执行，不能依赖运维人员临时手改生产库。

因此本轮只需正常构建镜像并重新部署，**无需手工执行 SQL**。上线前仍应备份数据库，并在健康检查转绿前查看启动日志确认迁移成功；连续重启一次可核对迁移幂等，随后分别跑一项扫描和回测验证 `strategy_run_results`、JobRun 引用及 Artifact，并用两个账号验证 `todo_inbox_states` 用户隔离、稍后跨日恢复和来源版本升级重现。不要把这条结论扩展成“任何未来的类型或数据迁移都能由 AutoMigrate 自动完成”。

## 5. 系统配置（管理后台，运行时可改）

以下配置存 `options` 表、管理后台改动即时生效，**不需要改环境变量或重启**：

- **注册策略**：开放/关闭 GitHub 新用户注册。
- **GitHub 登录**：Client ID / Secret（secret 加密落库；`deploy/.env` 里的同名变量仅作首启种子）。
- **新闻采集**：快讯轮询间隔（1~120 分钟，默认 5，下一轮生效）；「自动 LLM 分析」总闸（关闭时新闻情绪走纯关键词规则，零 token）。
- **LLM 回退**：「允许回退」总闸（未配置 LLM 的用户自动用系统回退配置，次数配额仍记本人）；指定回退配置（0=自动取首个启用管理员的默认配置；该配置同时是新闻情绪分析等后台任务的系统默认 LLM，后台任务不受总闸影响）。
- **LLM 调用审计**：无开关，全量落 `llm_call_logs`（请求/响应全文，仅管理员经 `/admin/llm-calls` 可见），每日 03:25 自动清理 90 天前记录。

## 6. 与 new-api 并存注意

- 端口：new-api 用 `3001`，QuantVista 用 `3002`，不冲突。
- Redis：各自独立容器（`redis` vs `quantvista-redis`），不共用，避免 key 混淆。
- 网络：共用宝塔的 `baota_net` 外部网络。
- 数据库：同一个 MySQL 实例下不同库（`new-api` vs `quantvista`）。

## 7. 数据备份与恢复

业务数据保存在 MySQL 单库 `quantvista`；应备份完整数据库，并另外安全保管对应的 `ENCRYPTION_KEY` 和部署配置。镜像可以重建，数据库和加密主密钥丢失不能靠重建容器恢复。启用 ntfy 时，其独立用户库的备份边界见 8.1。

### 7.1 表的两类：必须备份 vs 可重建

**用户数据表（必须备份——丢了无法找回）：**

- 账号与配置：`users`、`user_preferences`、`user_quotas`、`refresh_tokens`（可不备，重新登录即可）、`options`（系统设置，GitHub secret 为密文）、`llm_configs`（API Key 为密文，恢复后需同一 `ENCRYPTION_KEY` 才能解密）、`prompt_templates`、`notify_channels`（target 为密文，同上）、`browser_notification_preferences`、`browser_notification_devices`、`web_push_subscriptions`（endpoint/p256dh/auth 为密文）、`browser_notification_events`、`browser_notification_deliveries`
- 研究与交易记录：`watchlists`、`watchlist_items`、`watchlist_batches`、`watchlist_batch_items`、`portfolio_accounts`、`positions`、`position_trades`、`portfolio_cash_flows`、`portfolio_snapshots`、`target_allocation_revisions`、`thesis_cards`、`research_notes`、`screener_strategies` 及其不可变版本 `screener_strategy_revisions`
- 导入与调整审计：`import_batches`、`import_rows`、`import_row_claims`、`import_effects`、`position_corp_adjusts`、`paper_corp_adjusts`；缺失会破坏去重、回滚及权益确认依据
- 任务与版本归因：`job_runs`、`job_steps`、`job_events`、`research_artifacts`、`strategy_run_results`、`todo_inbox_states`、`onboarding_progresses`、`prompt_template_revisions`、`prompt_champion_states`，以及模型实验、路由和发布审计记录
- AI 产出：`analysis_records`、`recommendation_batches`、`recommendations`、`recommendation_statuses`、`ai_conversations`、`ai_conversation_messages`、`daily_reports`
- 提醒与任务通知：`alert_rules`、`alert_events`、`job_failure_notifications`
- 模拟盘：`paper_accounts`、`paper_holdings`、`paper_trades`

**行情与派生表（部分可重建，但不能据此假定历史都能恢复）：**

- `stocks`、`stock_quotes`、`daily_bars`（个股查询/批量同步自动回填）
- `trading_calendars`（管理端「回填交易日历」一键重建）
- `market_snapshots`、`data_sync_logs`、`stock_scores`（后台任务自动再生）
- 采集与派生表包括新闻与情绪、财报/财务、同步状态、龙虎榜/涨停池/人气/资金流/盘中因子、机构观点和板块估值。上游可回溯范围各不相同；盘中因子、历史股票宇宙和因子快照等时间点事实不能用当前数据完整重建。推荐标签、退出结果等历史评估依据也应保留，不能因为属于派生结果就默认排除。
- `llm_call_logs`（LLM 调用审计，90 天滚动自清理；如需长期留存审计证据则纳入备份）

### 7.2 备份命令

```bash
# 全库一致性备份，避免遗漏新增账户、流水、版本与审计表
docker exec mysql mysqldump -uquantvista -p'密码' --single-transaction quantvista | gzip > qv-$(date +%F).sql.gz
```

上面的表分类用于说明数据性质，不是可直接复制的完整备份白名单。若必须排除大表，应按当前 `model.AllModels()` 及实际恢复需求逐项核对，并在隔离库验证恢复后账户、流水、版本和审计关联完整；不能沿用旧版有限表名清单。示例中的密码是占位说明，实际备份应使用运维环境已有的安全凭证注入方式。

宝塔用户也可直接用面板的「数据库 → 备份」定时任务（等效全库 dump）。

### 7.3 恢复

```bash
gunzip < qv-2026-07-03.sql.gz | docker exec -i mysql mysql -uquantvista -p'密码' quantvista
```

先在隔离环境恢复并核对完整性，再按部署流程使用备份。恢复后启动应用时，`AutoMigrate` 会补齐结构；它不能补回漏备的交易流水、账户关联、不可变版本或历史快照。

**两个密钥必须与备份时一致，否则密文字段作废：**

- `ENCRYPTION_KEY`：解密 `llm_configs.api_key`、`notify_channels.target`、Web Push 订阅敏感字段和 `options` 里的 GitHub secret。丢失则需在页面重新录入这些密钥并重新订阅浏览器设备。
- `SESSION_SECRET`：影响 JWT 与 OAuth state 签名，换掉只是让所有人重新登录，无数据损失。

另：页面内的「设置 → 数据导出」可随时导出持仓/自选/推荐/分析历史为 CSV，作为轻量的二级备份（人可读，但不含账号/密钥，不能替代 SQL 备份）。

## 8. ntfy 推送服务（可选，Android App 系统级推送）

自建 [ntfy](https://ntfy.sh) 作为推送通道（大陆网络下 FCM 不可靠，决策记录见 `docs/ANDROID_APP_PLAN.md` §2.3）。
服务端发布消息 → 手机 ntfy App 经 WebSocket 长连接接收并弹系统通知，点击直达 QuantVista App 对应页面。

### 8.1 容器编排

`deploy/docker-compose.example.yml` 已含 `quantvista-ntfy` 服务（镜像 `binwiederhier/ntfy`，宿主机端口 `3003`，
`NTFY_AUTH_DEFAULT_ACCESS: deny-all` 私有实例默认全拒）。同步进你的 `docker-compose.yml`，
并在 `deploy/.env` 里设 `NTFY_BASE_URL=https://ntfy.<你的域名>`，然后更新编排启动容器。

数据目录：`${APP_PATH}/ntfy`（auth.db 用户库 + cache.db 消息缓存，跟主应用数据一起备份可选——丢了重新初始化用户即可）。

### 8.2 用户与 token 初始化（容器内执行一次）

```bash
docker exec -it quantvista-ntfy ntfy user add --role=user quantvista   # 交互设密码（手机 App 登录用）
docker exec -it quantvista-ntfy ntfy access quantvista "qv-*" rw       # 仅授权 qv- 前缀 topic
docker exec -it quantvista-ntfy ntfy token add quantvista              # 输出 tk_xxx，后端发布用
```

- `tk_xxx` token 填在 QuantVista「设置 → 通知设置」的 ntfy 通道里（整串配置加密落库）；
- 手机 ntfy App 用「用户名 quantvista + 密码」登录订阅；
- 验证 deny-all 生效：`curl -d test https://ntfy.<域名>/qv-test` 应返回 403。

### 8.3 宝塔反代（必须带 WebSocket）

宝塔新建站点 `ntfy.<域名>` → 反向代理到 `http://127.0.0.1:3003`，配置文件确认三点：

```nginx
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;      # WebSocket 升级头（宝塔模板可能缺，手补）
proxy_set_header Connection "upgrade";
proxy_read_timeout 300s;                     # ≥120s：ntfy 服务端默认 45s keepalive，足够刷新空闲计时
```

### 8.4 Cloudflare 子域

CF 加 `ntfy` 子域 A 记录指向服务器（橙云代理即可——免费版支持 WebSocket，空闲超时约 100s，
被 45s keepalive 安全穿过；国内直连 CF 可达是本方案成立的关键）。

### 8.5 应用侧接线

- **管理后台 → 站点地址**：填 `https://<主域名>`（SiteBaseURL）。推送通知的点击跳转链接由它拼接；不填则通知不带跳转。
- **用户侧**：设置页 → 通知设置 → 类型选「ntfy」，填服务地址 `https://ntfy.<域名>`、
  topic（如 `qv-u1`，须 `qv-` 前缀）、token（8.2 生成的 `tk_xxx`），添加后点「测试」验证可达。
- **手机端**：ntfy App 安装、订阅与国产 ROM 保活配置见 `mobile/README.md`。

## 9. 数据源升级与排障

本轮新增能力默认启用，不需要购买服务、填写 Tushare Token、安装 Python 或另起数据服务。按原流程构建新镜像并重部署即可；2026-11-04 日线修复会自动增加 `daily_bars.updated_at/amount_source/turnover_source` 与 `daily_bar_write_locks.basis_unverified`。旧数据不猜测回填来源或收盘完成时间，需通过补采重新核验。先备份数据库和 `ENCRYPTION_KEY`；大表迁移可能需要时间，请观察启动日志，不要用删除数据库、Redis 卷或重置历史的方式排障。

升级后建议按顺序检查：

1. 查看启动日志，确认迁移成功。`/api/status` 变绿只说明应用状态，不代表外部行情数据全部可用。
2. 在“管理后台 → 数据源能力矩阵”分别探测 `eastmoney/daily_bars`、`tencent/daily_bars`、`sina/daily_bars` 与 `sohu/daily_facts`，再测试所需其他能力。搜狐只补历史量额，不是价格备用；新浪是经过因子计算的比例前复权。探测指定来源不会偷偷换源；主源失败但备用源成功，是需要观察的正常降级场景。
3. 打开一只股票的日线与行情，检查实际来源、最新交易日及旧数据提示。1 分钟分时仍只由腾讯提供；新浪 5 分钟线不会被当成 1 分钟分时。
4. “数据健康”是本地覆盖报告，不是连通性测试。先点“回填日历”，起止日期全部留空即可预演近 400 个自然日（包含今天），为 250 日研究窗口准备日历；显式范围也最多 400 个自然日。确认后等待任务结束，不要把“已入队”当成补齐。日历仍未知时不会把 250 根旧数据认证为初始化完成。
5. 已有全市场清单时，直接选择“日线范围：本地已知全市场（含已跟踪）”并点击“日线补采”，日期留空默认近 250 个已完成交易日。每轮至多修复 800 只，包含旧记录的完成性核验；结束后重新预演，已完整标的会跳过，直到没有其他待修复标的。只修复关注股票可改选 tracked（默认 45 日）。没有全市场清单时先盘后全市场增量同步；历史初始化可续跑 pending 并重试 failed，但不代替对已有 done 标的逐日检查。
6. 全市场同步从北京时间 15:05 后才接受当日完整收盘快照，不能在盘中用当前量价冒充昨天或今天的全日数据；任务还会核对实际源日期与确认的目标。若提示目标不符，检查日历/上游水位再预演，不要改数据库日期强行通过。
7. 免费接口共享限速后，全市场任务会比高并发抓取慢。管理页会显示本次任务最终状态并链接任务中心；观察 `success/degraded/failed` 及同步日志中的 `success/partial/failed`。暂停和取消会终止后续等待，已保存历史不会因为临时失败被清空。刷新页面后可从任务中心继续查结果。
8. 带范围确认的失败补采要回管理后台重新预演，不能从任务中心复制旧计划直接重跑。管理员的系统任务链接会带 `include_system=1`；普通用户仍无系统作业权限。“涨跌家数快照”只采本项，不补涨停池历史。
9. 核对日志中的实际保存数、剩余缺口和“数据交易日”，再等待因子重建并刷新推荐追踪。`799/800` 是本轮标的结果，不是全市场覆盖率；日期有一根更新也不表示全部历史齐全。某只追踪仍缺数据会单独报错，但其他合法结果继续更新。旧推荐正文、生成时质量警告与已冻结结算不重写。

本次取数复核不新增数据库表或必填环境变量；新增列由正常启动迁移处理，无需手工 SQL。收盘完成时间未知的旧行保留原样，待补采核验，不能直接填当前时间“消除缺口”。完整修复范围及仍需线上验证的项目见 [取数链路复核第八节](DATA_PIPELINE_REVIEW.md#8-2026-11-04-日线补采与推荐追踪修复)。

### 网络与限流

- `EOF`、超时、403、429、5xx：查看具体来源和能力。不要不断重启或解冷；等待退避并先确认备用源能否工作。东财主域和数字节点都可能断连，仅更换域名不是可靠修复。
- `HTTP_PROXY`、`HTTPS_PROXY`、`NO_PROXY`：行情 HTTP 客户端遵循进程代理变量，Compose 示例现在显式传入。已有编排也必须合并这三个 `environment` 映射；只改 Compose 的 `.env` 不会自动注入容器。全部可选，直连正常时保持代理地址留空。容器里的 `127.0.0.1` 指容器自己，宿主机代理使用可达的宿主机地址并允许容器网段；`NO_PROXY` 补充实际内网地址。代理凭据不要粘贴进日志、截图或提交文件。
- 保持 `TZ=Asia/Shanghai`，同步宿主机时钟，并保证容器有 CA 证书。行情解析显式使用北京时间，但全站调度仍依赖部署时区。
- 无 Redis 时，热点行情的进程内缓存与请求合并仍工作。已配置 Redis 但不可达时，缓存访问最多 250ms，随后短路 15 秒；这不是清空 Redis，也不是绕过业务权限缓存。
- 区分应用自身 429 与数据源 429。宝塔/nginx 反代应按 1.5 节正确配置 `TRUSTED_PROXIES`，否则所有访问可能共享反代 IP 的应用限流桶。

详细免费接口清单、开源依据和本地实测边界见 [数据源稳定性说明](DATA_SOURCE_RELIABILITY.md)。本地接口成功不能代替生产服务器的连通性与连续交易日验收。
