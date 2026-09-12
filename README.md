# QuantVista

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](server/go.mod)
[![Vue](https://img.shields.io/badge/Vue-3-4FC08D?logo=vuedotjs&logoColor=white)](web/package.json)
[![TypeScript](https://img.shields.io/badge/TypeScript-5-3178C6?logo=typescript&logoColor=white)](web/package.json)

**面向 A 股的自托管研究工作台，整合行情、策略选股、AI 分析、持仓管理与研究复盘。**

QuantVista 将市场数据、研究结论和后续跟踪放在同一工作流中：从自选与条件选股发现候选，通过数据快照和 AI 辅助研究，再记录交易计划、持仓变化与实际表现。

后端使用 Go，前端使用 Vue 3 与 TypeScript。生产构建通过 `go:embed` 将前端打包进 Go 程序，可用一个应用容器提供页面和 API；支持 MySQL、SQLite，以及可选的 Redis 缓存。Android 客户端使用 Capacitor 加载已部署的站点。

[功能概览](#功能概览) · [快速开始](#快速开始) · [配置](#配置) · [开发与验证](#开发与验证) · [文档](#文档) · [参与贡献](#参与贡献)

## 功能概览

| 能力 | 主要内容 |
| --- | --- |
| 行情与市场 | 实时行情、前复权日线、分时图、技术指标、板块热力图、资金流、财务与公告、新闻情绪、公司行动日历 |
| 选股与回测 | 内置策略、自定义条件树、自然语言生成策略、因子扫描、历史时点回测与执行约束模拟 |
| AI 研究 | 个股、市场、板块、自选和持仓分析，多轮问答、横向对比、收盘日报、可配置提示词与模型路由 |
| 候选与跟踪 | 全市场日更发现、策略相关预选与质量排序、候选记忆、入场条件复核、收益跟踪、候选召回和效果评估 |
| 持仓与组合 | 多组合账户、交易流水、成本与盈亏、买入前退出规划、动态止盈止损、公司行动确认、资产曲线、组合风险、模拟交易与 ETF 行情 |
| 日常工作流 | 个人首页、自选分组、Today 收件箱、条件提醒、投资逻辑卡、笔记、CSV 导入导出 |
| 通知与后台任务 | Server酱、Webhook、ntfy、浏览器通知与 Web Push；后台作业进度、取消、重试、重启恢复与结果跳转 |
| 管理与评估 | 用户权限与配额、GitHub 登录、LLM 调用审计、数据源健康、因子 IC、滚动验证、概率校准与影子实验 |

界面提供简明与专业显示模式、多套明暗主题和响应式布局。Android 壳支持返回键、站内深链与离线恢复，构建和签名方式见 [移动端说明](mobile/README.md)。

### 研究结果如何追溯

- **数据快照与版本**：研究任务记录输入快照、来源、时间与策略或提示词版本，历史结果保留当时依据。
- **程序化校验**：对模型引用的证据、候选范围、数值与交易计划关系进行校验，展示数据缺口和降级状态。
- **明确失效条件**：结论附带风险、未知项与跟踪条件；数据缺失或请求失败时，根据业务场景返回量化观察或拒绝生成。
- **区分评估口径**：模型模拟表现与用户实际成交分别统计，回测考虑交易日历、涨跌停、停牌、费税等约束。

这些机制用于提高结果的可核查性，不能保证模型结论正确或投资收益。当前历史研究依赖已积累的快照与前复权数据；完整的不复权价格和每日复权因子回放仍在规划中。

推荐排序支持质量规则、同机会集的原加法对照，以及经过时间外验证后显式启用的学习基线。管理员可以只读评估和切换版本，已排队任务保留提交时的完整策略与模型；独立 CLI 支持以只读方式评估 SQLite 输入。算法、样本边界和回退方式见 [推荐排序与评估](docs/RECOMMENDATION_RANKING.md)。

持仓退出规划根据完整日线、策略侧重、实际成本与费税估算初始止损和分阶段目标，盈利后逐步提高保护价。建仓前可以预览，保存后在持仓处理中心统一跟踪；到价提醒会说明可卖数量与执行限制，成交仍由用户登记。算法、操作流程和验证范围见 [持仓退出规划](docs/POSITION_EXIT_OPTIMIZATION_PLAN.md)。

## 快速开始

### 环境要求

| 组件 | 要求 |
| --- | --- |
| Go | 最低 1.26，仓库工具链为 1.26.8 |
| Node.js | 24 LTS，使用 npm 与仓库锁文件安装依赖 |
| 数据库 | 本地开发可使用内置 SQLite；MySQL 部署见部署文档 |
| Docker | 仅容器构建和运行需要 |
| Android 工具链 | 仅移动端构建需要 JDK 21 与 Android SDK，详见移动端说明 |

### 本地开发

克隆仓库：

```bash
git clone https://github.com/raoczh/QuantVista.git
cd QuantVista
```

在 `server/.env` 中配置本地开发环境。已有配置时按需调整；以下使用独立的开发数据库文件：

```dotenv
SQL_DSN=local
SQLITE_PATH=quantvista.dev.db
SESSION_SECRET=please-replace-with-generated-session-secret
ENCRYPTION_KEY=please-replace-with-generated-encryption-key
```

运行以下命令两次，分别生成上述两个密钥并替换占位值。密钥生成后固定保存，`ENCRYPTION_KEY` 用于解密已保存的 API Key 等敏感配置。

```bash
node -e "console.log(require('node:crypto').randomBytes(36).toString('base64'))"
```

在第一个终端启动后端：

```bash
cd server
go mod download
go run .
```

在另一个终端从仓库根目录启动前端：

```bash
cd web
npm ci
npm run dev
```

打开 <http://localhost:5173>。Vite 会将 `/api` 请求代理到后端的 `http://localhost:3000`。

### 首次使用

1. 首次访问会进入初始化页面，创建第一个管理员账号；项目没有预设管理员密码。
2. 登录后设置投资偏好，添加自选或持仓，按需创建提醒。
3. 需要 AI 功能时，在设置中添加兼容 OpenAI 的 API 地址、模型和 API Key，并测试连接；支持 Chat Completions 与 Responses 两类端点。
4. 数据同步和研究生成进度可在任务中心查看。行情、账本和规则选股等功能可以独立于 AI 使用。

### Docker 运行

从仓库根目录构建当前源码：

```bash
docker build -t quantvista:local .
```

准备好上面的 `server/.env` 后，可用独立数据卷运行 SQLite 版本：

```bash
docker run -d --name quantvista --restart unless-stopped -p 3000:3000 --env-file server/.env -e SQL_DSN=local -e SQLITE_PATH=/data/quantvista.db -v quantvista-data:/data quantvista:local
```

打开 <http://localhost:3000>，页面与 API 由同一应用提供，数据保存在 `quantvista-data` 卷中。构建镜像时会安装前端依赖、构建页面并编译 Go 程序，无需先在宿主机生成前端产物。

生产 MySQL、反向代理、HTTPS、备份恢复和 ntfy 配置见 [部署说明](docs/DEPLOYMENT.md)。仓库提供 [环境变量示例](deploy/.env.example) 和 [Compose 示例](deploy/docker-compose.example.yml)；Compose 示例使用外部 MySQL 与 `baota_net` 网络，需要按部署环境调整。

**应用启动会自动迁移所配置的数据库并启动后台任务。** 升级已有实例前，应备份完整数据库并保管对应的 `ENCRYPTION_KEY`。

## 配置

配置优先级为：进程环境变量优先，其次加载当前工作目录的 `.env`；该文件不存在时才加载 `../deploy/.env`。两个环境文件不会合并。从 `server/` 运行时，对应 `server/.env` 与 `deploy/.env`。

| 变量 | 默认值或用途 |
| --- | --- |
| `SQL_DSN` | 留空或 `local` 使用 SQLite；其他值按 MySQL DSN 解析 |
| `SQLITE_PATH` | SQLite 文件位置，默认当前工作目录的 `quantvista.db` |
| `SESSION_SECRET` | JWT 签名密钥；连接 MySQL 时必须配置强密钥 |
| `ENCRYPTION_KEY` | 敏感字段加密主密钥；连接 MySQL 时必须配置，备份恢复须保留同一密钥 |
| `PORT` | HTTP 监听端口，默认 `3000` |
| `REDIS_CONN_STRING` | 可选 Redis 连接串；未配置时关闭 Redis 缓存 |
| `ALLOWED_ORIGINS` | 跨域访问白名单，多个来源用逗号分隔 |
| `TRUSTED_PROXIES` | 可信反向代理 IP 或 CIDR，默认不信任代理头 |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | GitHub OAuth 首次配置种子，之后以管理后台保存的配置为准 |
| `VAPID_PUBLIC_KEY` / `VAPID_PRIVATE_KEY` / `VAPID_SUBJECT` | Web Push 配置；未配置时可使用网站打开期间的浏览器通知 |
| `TUSHARE_TOKEN` | 可选 Tushare 数据源凭证 |
| `DEFAULT_MARKET` | 默认 `cn`，当前主要支持 A 股 |
| `DEBUG` | 调试日志开关，默认 `false` |

LLM 地址、模型、API Key、通知通道和用户偏好在应用内管理。GitHub 登录、用户配额及系统开关由管理员配置。完整说明见 [部署文档](docs/DEPLOYMENT.md)。

## 开发与验证

### 后端

在 `server/` 目录执行：

```bash
go test -p 1 ./... -count=1
go vet -p 1 ./...
go mod verify
```

支持 race detector 的环境还可运行：

```bash
go test -race -p 1 ./... -count=1 -timeout=20m
```

MySQL 专项回归默认跳过，仅在 `QV_REVIEW_MYSQL=1` 时启用，并固定使用 `127.0.0.1:33317` 的专用测试数据库服务。它会创建和清理临时测试库；运行前按 [测试约束](server/service/mysql_review_test.go) 准备隔离环境。

### 前端

在 `web/` 目录安装依赖后执行：

```bash
npm test
npm run type-check
```

仓库的 [PowerShell 验证脚本](scripts/verify.ps1) 也支持从根目录统一检查：

```powershell
./scripts/verify.ps1 -Mode quick
./scripts/verify.ps1 -Mode full
```

`quick` 编译后端包并检查前端类型；`full` 运行后端和前端测试，并将前端验证构建输出到临时目录，结束后清理该次产物。

### 构建发布程序

从仓库根目录执行：

```bash
cd web
npm ci
npm run build
cd ../server
go build -o quantvista .
```

前端构建输出到 `server/web/dist/`，随后由 Go 嵌入程序。Windows 可将输出文件名改为 `quantvista.exe`。部署时同时配置运行目录、数据持久化与环境变量；容器构建可直接使用上述 Dockerfile。

Android 平台同步、签名、App Links 和推送接入见 [移动端说明](mobile/README.md)。Web 与原生端的 Capacitor 依赖需保持兼容。

## 项目结构

```text
QuantVista/
├── server/
│   ├── cmd/          # 数据检查、迁移验证与密钥生成工具
│   ├── common/       # 配置、数据库、加密与公共能力
│   ├── controller/   # HTTP 接口
│   ├── datasource/   # 行情与资讯数据源
│   ├── model/        # 数据模型与迁移
│   ├── service/      # 研究、交易账本、后台作业与业务规则
│   ├── router/       # API 和嵌入页面路由
│   └── web/dist/     # 前端构建输出
├── web/
│   ├── src/          # 页面、组件、API、状态与交互逻辑
│   └── scripts/      # 前端回归检查
├── mobile/           # Capacitor Android 工程
├── deploy/           # 部署配置示例
├── scripts/          # 开发验证脚本
└── docs/             # 架构、部署、审查记录与规划
```

## 文档

| 文档 | 内容 |
| --- | --- |
| [部署说明](docs/DEPLOYMENT.md) | MySQL、Docker Compose、反向代理、迁移与备份 |
| [技术架构](docs/ARCHITECTURE.md) | 服务分层、数据流、界面约定与 AI 调用设计 |
| [数据源说明](docs/DATA_SOURCES.md) | 数据源能力、覆盖范围与降级策略 |
| [移动端说明](mobile/README.md) | Android 构建、签名、深链与通知 |
| [项目规划与验收边界](docs/ROADMAP.md) | 业务规划、未完成项与线上验收清单 |
| [代码审查报告](docs/CODE_REVIEW.md) | 全量审查范围、修复与验证记录 |
| [LLM 准确性工程](docs/LLM_ACCURACY_OPTIMIZATION_PLAN.md) | 提示词、证据校验、角色资产与评测设计 |
| [推荐准确性规划](docs/RECOMMENDATION_ACCURACY_PLAN.md) | 标签、回测、影子运行与校准 |
| [推荐排序与评估](docs/RECOMMENDATION_RANKING.md) | 策略预选、质量规则、只读研究、模型启用与回退 |
| [推荐优化验收清单](docs/RECOMMENDATION_OPTIMIZATION_PLAN.md) | 本轮 O01～O21 的实现范围、验证与效果证据边界 |
| [候选发现与持仓卖出方案](docs/RECOMMENDATION_DISCOVERY_AND_EXIT_PLAN.md) | 候选召回、每日复盘与持仓风险工作流 |
| [持仓退出规划](docs/POSITION_EXIT_OPTIMIZATION_PLAN.md) | 买入前预览、初始止损、分阶段目标、盈利保护、通知与验收记录 |

规划文档保留历史阶段记录，具体实现以当前代码为准。离线测试和构建验证的范围见审查报告，真实模型、连续交易日任务和 Android 真机体验仍需在对应环境验收。

## 参与贡献

欢迎通过 [Issues](https://github.com/raoczh/QuantVista/issues) 反馈问题或讨论改进，通过 Pull Request 提交代码与文档。

- 问题报告请提供版本、运行环境、复现步骤、预期行为和脱敏日志。
- 行为修复应附带覆盖实际问题的回归测试，并运行相关模块检查。
- 修改页面时检查桌面与移动尺寸，以及加载、空数据、失败和异步响应状态。
- 配置示例使用占位值；不要提交真实环境文件、数据库、API Key、签名材料或个人部署配置。

## 使用边界

QuantVista 面向个人研究与自托管使用。行情数据可能延迟或缺失，AI 输出和回测结果仅供研究参考，不构成投资建议，也不代表未来收益。涉及对外提供服务或证券咨询时，使用者应自行确认数据授权及适用的法律要求。

## 许可证

本项目采用 [Apache License 2.0](LICENSE)。
