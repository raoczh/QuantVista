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

到 GitHub → Settings → Developer settings → OAuth Apps 新建应用，
**Authorization callback URL 填 `http://<你的域名或IP>:3002/login/callback`**（前端回调页，注意不是 `/api/...`）。

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

后端用 GORM 的 `AutoMigrate`，与 new-api 相同：**每次启动检查表结构，自动建表、加列、加索引**，你不用手动改表。

**能自动做的：**

- 新建不存在的表。
- 给已有表加新列。
- 加新索引。

**不会自动做的（需写迁移代码）：**

- 删除列、改列类型、重命名列、改非空约束 —— GORM 出于安全不做破坏性变更。
- 这类变更参照 new-api 的做法：在迁移函数里写一段一次性 SQL（如 `ALTER TABLE` 改类型），跟 `AutoMigrate` 一起在启动时执行。

所以日常加字段/加表 = 改好 model 代码、构建、重启即可，**无需手动动数据库**。涉及改类型/删列这类，才需要在代码里补一段迁移逻辑。

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

个人自用部署，数据全在 MySQL 单库 `quantvista`；容器与镜像可随时重建，**只有数据库需要备份**。

### 7.1 表的两类：必须备份 vs 可重建

**用户数据表（必须备份——丢了无法找回）：**

- 账号与配置：`users`、`user_preferences`、`user_quotas`、`refresh_tokens`（可不备，重新登录即可）、`options`（系统设置，GitHub secret 为密文）、`llm_configs`（API Key 为密文，恢复后需同一 `ENCRYPTION_KEY` 才能解密）、`prompt_templates`、`notify_channels`（target 为密文，同上）
- 研究与交易记录：`watchlists`、`watchlist_items`、`positions`、`thesis_cards`、`research_notes`
- AI 产出：`analysis_records`、`recommendation_batches`、`recommendations`、`recommendation_statuses`、`ai_conversations`、`ai_conversation_messages`、`daily_reports`
- 提醒：`alert_rules`、`alert_events`
- 模拟盘：`paper_accounts`、`paper_holdings`、`paper_trades`

**行情缓存表（可不备份——均能从数据源重建）：**

- `stocks`、`stock_quotes`、`daily_bars`（个股查询/批量同步自动回填）
- `trading_calendar`（管理端「回填交易日历」一键重建）
- `market_snapshots`、`data_sync_logs`、`stock_scores`（后台任务自动再生）
- N/F/M 批次的采集与派生表：新闻（`news_items` 等）、财报/财务（`earnings_*`/`finance_*`）、全市场宽表与状态（`factor_tables`/`market_sync_states`）、龙虎榜/涨停池/人气/资金流/盘中因子（`lhb_*`/`zt_*`/`popularity_*`/`fund_flows`/`intraday_factor_dailies`）、机构观点（`report_ratings`/`org_surveys`，P3a 按需拉取缓存）——均由每日 job 或按需拉取重建；注意涨停池/盘中因子上游**不可回溯**，重建只能从当天起积累，历史断档是诚实缺失
- `llm_call_logs`（LLM 调用审计，90 天滚动自清理；如需长期留存审计证据则纳入备份）

### 7.2 备份命令

```bash
# 全库备份（最简单，推荐；行情缓存表体积有限，一起备份省心）
docker exec mysql mysqldump -uquantvista -p'密码' --single-transaction quantvista | gzip > qv-$(date +%F).sql.gz

# 只备用户数据表（体积敏感时）
docker exec mysql mysqldump -uquantvista -p'密码' --single-transaction quantvista \
  users user_preferences user_quotas options llm_configs prompt_templates notify_channels \
  watchlists watchlist_items positions thesis_cards research_notes \
  analysis_records recommendation_batches recommendations recommendation_statuses \
  ai_conversations ai_conversation_messages daily_reports alert_rules alert_events \
  paper_accounts paper_holdings paper_trades | gzip > qv-user-$(date +%F).sql.gz
```

宝塔用户也可直接用面板的「数据库 → 备份」定时任务（等效全库 dump）。

### 7.3 恢复

```bash
gunzip < qv-2026-07-03.sql.gz | docker exec -i mysql mysql -uquantvista -p'密码' quantvista
```

恢复后启动应用，`AutoMigrate` 会补齐缺的表/列（只备了用户数据表时，行情缓存表自动重建）。

**两个密钥必须与备份时一致，否则密文字段作废：**

- `ENCRYPTION_KEY`：解密 `llm_configs.api_key`、`notify_channels.target`、`options` 里的 GitHub secret。丢失则需在页面重新录入这些密钥。
- `SESSION_SECRET`：影响 JWT 与 OAuth state 签名，换掉只是让所有人重新登录，无数据损失。

另：页面内的「设置 → 数据导出」可随时导出持仓/自选/推荐/分析历史为 CSV，作为轻量的二级备份（人可读，但不含账号/密钥，不能替代 SQL 备份）。
