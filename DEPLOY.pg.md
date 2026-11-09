# Resin-pg 部署单（Northflank）

本仓库是 [Resinat/Resin](https://github.com/Resinat/Resin) v1.3.0 的改造版：**配置与运行状态可以存进外部 PostgreSQL**，容器重建不再丢平台、订阅、端点、热配置、节点健康与粘性租约。

- 镜像：`ghcr.io/kakaw-1/resin-pg:latest`（公开包，Northflank 无需填 registry 凭据）
- 平台：`linux/amd64`
- 版本 tag：`pg-v1.3.0-1`，另有 `sha-<7位>` 便于回滚钉版本
- 监听：容器内 `2260/tcp`，健康检查 `GET /healthz`

---

## 1. 与上游的差异

| 项 | 改动 |
|---|---|
| 存储后端 | 新增 `RESIN_DB_DRIVER=postgres`，把 state.db / cache.db 放进同一 PostgreSQL 库的两个 schema |
| 迁移 | 新增 PostgreSQL 版迁移集（state 9 个版本、cache 2 个版本，与 SQLite 版版本号一一对应） |
| 数据搬迁 | 新增子命令 `resin db-migrate`（`--dry-run`、逐表 upsert、搬后行数核对） |
| 系统信息 | `GET /api/v1/system/config/env` 增加 `db_driver` / `db_state_schema` / `db_cache_schema` / `db_target`（密码脱敏） |
| 不动的部分 | 请求日志（`request_logs-*.db`，按大小轮转）与指标（`metrics.db`）**始终保存在本地** `RESIN_LOG_DIR`；GeoIP `.mmdb` 仍在 `RESIN_CACHE_DIR` |

## 2. Northflank 配置

### 2.1 服务

1. **Create service → Combined / Deployment**，来源选 *External image*。
2. Image：`ghcr.io/kakaw-1/resin-pg:latest`（生产建议钉 `:pg-v1.3.0-1` 或 `:sha-xxxxxxx`）。
3. **Ports**：新建端口 `2260`，协议 **HTTP**，公共暴露（要跑代理流量就把同一端口再开一条 **TCP** 入口；Resin 一个端口同时支持 HTTP 正向、反向与 SOCKS5，按需开）。
4. **Health check**：类型 HTTP，路径 `/healthz`，端口 `2260`，初始延迟 20s。
5. **Resources**：最小 0.2 vCPU / 256 MB 可用；节点多（>5k）建议 1 vCPU / 1 GB。

> ⚠️ Northflank 会注入 `PORT` 变量，但 Resin 只认 `RESIN_PORT`。所以**必须显式设置 `RESIN_PORT`，且与上表端口号一致**（默认 2260 就直接用 2260）。

### 2.2 环境变量

必填：

| 变量 | 值 |
|---|---|
| `RESIN_ADMIN_TOKEN` | 管理面板/API 密码（自己定，强随机） |
| `RESIN_PROXY_TOKEN` | 代理密码（不能为空，可为空字符串但必须显式设置） |
| `RESIN_LISTEN_ADDRESS` | `0.0.0.0` |
| `RESIN_PORT` | `2260`（与 Northflank 端口一致） |

外置数据库（Aiven / Neon / Supabase / 自建 PG 同理）：

| 变量 | 值 |
|---|---|
| `RESIN_DB_DRIVER` | `postgres` |
| `RESIN_DB_DSN` | `postgres://user:pass@host:port/db?sslmode=require`（**存为 secret**） |
| `RESIN_DB_STATE_SCHEMA` | `resin_state`（首次启动自动 `CREATE SCHEMA`） |
| `RESIN_DB_CACHE_SCHEMA` | `resin_cache` |
| `RESIN_DB_MAX_OPEN_CONNS` | `3`（远端小实例务必调小） |
| `RESIN_DB_MAX_IDLE_CONNS` | `1` |
| `RESIN_DB_CONNECT_TIMEOUT` | `20s`（跨区冷连接可能 400ms 以上） |

可选：`RESIN_LOG_DIR=/var/log/resin`、`RESIN_CACHE_DIR=/var/cache/resin`（GeoIP）、`TZ=Asia/Shanghai`。

### 2.3 卷

有外置库时 `state` 目录不再需要持久化，只建议挂两个：

| 容器路径 | 用途 | 是否必须 |
|---|---|---|
| `/var/log/resin` | 请求日志 + 指标（`metrics.db`、`request_logs-*.db`） | 想保留历史就挂 |
| `/var/cache/resin` | GeoIP `.mmdb` 缓存 | 不挂每次重建重新下载 |

`/var/lib/resin` 只在外置库关闭（退回 SQLite）时才需要。

## 3. 首次部署后有数据吗

- **全新库**：启动即空库，自动建两个 schema、跑 11 个迁移，UI 里会有内置的 `Default` 平台。
- **从旧实例搬数据**（本地已有 SQLite 的 `state.db` / `cache.db`）：

```bash
RESIN_DB_DSN='postgres://user:pass@host:port/db?sslmode=require' \
  resin db-migrate \
    --from-state /path/to/state.db \
    --from-cache /path/to/cache.db \
    --state-schema resin_state --cache-schema resin_cache \
    --dry-run          # 先看行数，再去掉这行正式搬
```

在没有本地 shell 的平台上，可以临时起一个 one-off 容器（同镜像）执行上面的命令，或用 Northflank 的 *Job* 功能跑同一镜像、命令填 `resin db-migrate ...`。

搬完的校验：命令会打印每张表 `source=… written=…`，最后输出 `row counts verified for 10 tables`。

## 4. 校验清单

```bash
# 健康
curl -fsS https://<service>.code.run/healthz          # 期望 200
# 后端确认（需要 admin token）
curl -fsS -H "Authorization: Bearer $RESIN_ADMIN_TOKEN" \
  https://<service>.code.run/api/v1/system/config/env | jq '{db_driver,db_state_schema,db_cache_schema,db_target}'
# 期望 db_driver=postgres，db_target 里密码显示为 ***
# 平台列表
curl -fsS -H "Authorization: Bearer $RESIN_ADMIN_TOKEN" \
  https://<service>.code.run/api/v1/platforms | jq '.items[].name'
```

**重建验证**（这是外置库的意义所在）：Northflank 里 Redeploy / 换版本，重启后 `platforms`、`subscriptions`、`endpoints` 与热配置应原样回来，只有日志页与 Dashboard 历史曲线归零。

## 5. 已知边界

1. **一个库接一个实例**：脏集 flush 遇序列化失败会整批重放，但同一平台的多实例共享租约属未定义行为。
2. **state 与 cache 必须同驱动、同库不同 schema**：启动时的一致性清理要在一条事务里 JOIN 两侧。
3. **数据库不可达时进程启动失败**，不会静默退回本地 SQLite —— 这是有意设计，避免出现两份真源。
4. **上游 `release.yml` 仍在仓库里**，会响应 `v*` tag。本仓库的镜像由 `docker-publish.yml` 构建，**不要推送 `v*` 形式的 tag**，否则会连带触发上游的五平台二进制发布。
5. 请求日志与指标不进外部库；需要长期审计另行归档。

## 6. 回滚

- 换回旧镜像 tag 即可（配置与状态都在外部库，不受镜像回滚影响）。
- 若要从外置库退回 SQLite：删掉 `RESIN_DB_DRIVER` / `RESIN_DB_DSN` 两个变量，挂上 `/var/lib/resin` 卷，重启 —— 会得到一个空库（数据留在 PG 里，未删除）。
