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
mkdir -p /www/wwwroot/quantvista/{data,logs,redis-data}
```

### 1.3 GitHub OAuth App

到 GitHub → Settings → Developer settings → OAuth Apps 新建应用，
回调地址填 `http://<你的域名或IP>:3002/api/oauth/github/callback`，
把 Client ID / Secret 填进 `deploy/.env`。

### 1.4 生成密钥

```bash
openssl rand -base64 36   # 生成 SESSION_SECRET
openssl rand -base64 36   # 再生成一个作 ENCRYPTION_KEY
```
分别填进 `deploy/.env`。

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

## 5. 与 new-api 并存注意

- 端口：new-api 用 `3001`，QuantVista 用 `3002`，不冲突。
- Redis：各自独立容器（`redis` vs `quantvista-redis`），不共用，避免 key 混淆。
- 网络：共用宝塔的 `baota_net` 外部网络。
- 数据库：同一个 MySQL 实例下不同库（`new-api` vs `quantvista`）。
