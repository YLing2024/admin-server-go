# admin-server-go

个人网站 Admin 系统的后端（Go 重写）。与旧实现 `/root/proj/admin-server`（Node.js + Express 4）
**逐条对齐 HTTP 契约**，用标准库 `net/http` 实现，单二进制、低内存。

职责：系统监控（实时 + 历史采样 + SSE）、软件版本与 systemd/docker 服务状态、文件区（浏览/上传/
下载/新建/重命名/删除）、公开分享链接、接口令牌管理、Web 终端口令与 ttyd 会话管理、
Hermes 历史会话只读浏览、应用探活面板。

## 技术栈

- Go 1.26，HTTP 仅标准库 `net/http`（SSE 自实现 `http.Flusher`），日志 `log/slog`
- MySQL `github.com/go-sql-driver/mysql`（**仅 MySQL**，无方言开关）；旧 SQLite 数据由 `tools/migrate-sqlite-to-mysql/` 一次性搬运
- Redis `github.com/redis/go-redis/v9`（会话 / 票据 / 限流 / 采样锁）
- TOTP / 终端口令哈希自实现（标准库 `crypto/*`），不依赖旧仓库的 JS 模块
- 无 web 框架、无 ORM

## 构建与运行

```bash
make build          # 产出 ./admin-server-go
make check          # 路由↔文档机检 + vet + 单测
make test           # go test ./... -count=1
make race           # go test ./... -count=1 -race
make run            # 构建并前台运行
```

直接运行时等价于 `admin-server-go serve`（可省略）。旧 SQLite 数据的一次性搬运见 `docs/DB.md` 的
`tools/migrate-sqlite-to-mysql/`。

默认监听 `127.0.0.1:19265`（**刻意与线上 3100 实例隔离**，两者可并存）。Redis 前缀、采样锁、
历史留样、TOTP secret 文件默认也都指向本仓库自己的位置；差异逐条记在 `docs/DB.md`。

首次运行会在 `ADMIN_CONFIG_PATH`（默认 `<cwd>/config.json`）生成随机初始口令（只存 PBKDF2 哈希，0600）。
设置 TOTP：`curl -X POST http://127.0.0.1:19265/api/admin/totp/setup`，用返回的 `otpauthUri` 扫码，
之后用 `POST /api/admin/login` + `{"code":"<动态码>"}` 换会话。

## 配置

环境变量（与旧实现同名同默认，除下表中标注的新增项与隔离项）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `HOST` / `PORT` | `0.0.0.0` / `19265` | 监听地址（部署用 systemd 设 `HOST=127.0.0.1`） |
| `AUTH_MODE` | `builtin` | `builtin`（自带账号 + TOTP + cookie 会话）/ `sso`（只认 `X-Auth-User`，自带登录入口 404） |
| `ADMIN_REPO_ROOT` | 当前工作目录 | 相对默认值的解析基准 |
| `ADMIN_CONFIG_PATH` | `<root>/config.json` | 配置文件路径（含口令哈希） |
| `ADMIN_UPLOAD_DIR` | `<root>/uploads` | 通用上传目录 |
| `ADMIN_FILE_DIR` | `/root/files/download` | 文件区根目录（所有路径严格限制在其内） |
| `ADMIN_APPS_FILE` | `<root>/data/apps.json` | 应用面板登记表（只读消费，不存在不生成） |
| `ADMIN_AUDIT_LOG` | `<root>/audit.log` | 审计日志 |
| `ADMIN_TERM_PW_FILE` | `~/.hermes/term_password` | 终端口令哈希（`sha256$<salt>$<hash>`，0600） |
| `ADMIN_TERM_UNLOCK` ← 新增 | `password` | 终端口令门开关：`password`（默认，需口令换票据）/ `off`（不要求口令，登录后台即可进终端；非法值回落 `password` 并告警）。仅关闭第二道门，`authRequired` 与票据校验不变 |
| `ADMIN_SHARE_BASE_URL` | 空 | 分享链接基址（拼 `url` 字段用） |
| `ADMIN_SHARE_FILE` | `<root>/data/file-shares.json` | 分享账本（本地 JSON） |
| `ADMIN_PROC_MAX_ROWS` | `2000` | 进程排行最多返回行数 |
| `ADMIN_REDIS_PREFIX` | `admin-go:session:` | 会话 key 前缀（测试隔离用） |
| `ADMIN_SAMPLER_LOCK` | 临时目录 `admin-server-go-sampler.lock` | 采样器互斥锁 |
| `ADMIN_HISTORY_FILE` | 临时目录 `admin-server-go-history.json` | 历史留样文件 |
| `ADMIN_TOTP_SECRET_FILE` ← 新增 | `<root>/data/totp-secret.json` | 自带账号模式的 TOTP secret 文件 |
| `REDIS_URL` ← 新增 | `redis://127.0.0.1:6379` | Redis 连接串 |
| `HERMES_STATE_DB` | `~/.hermes/state.db` | Hermes 历史只读数据源（服务运行期唯一的只读 SQLite） |
| `AUTH_CENTER_BASE_URL` | 空 | 认证中心基址（未配置时转发接口 502） |
| `AUTH_CENTER_INTERNAL_TOKEN_FILE` | `<root>/../auth-server/internal-token` | 内部令牌文件（只读、不打印） |
| `DB_HOST` | `127.0.0.1` | MySQL 地址 |
| `DB_PORT` | `3306` | MySQL 端口 |
| `DB_USER` | 必填 | MySQL 账号（缺失启动报错） |
| `DB_PASSWORD` | 空 | MySQL 口令 |
| `DB_METRICS_NAME` | `admin_metrics` | 指标库 schema |

数据库映射、建表口径与迁移工具见 `docs/DB.md`；接口清单见 `docs/API.md`；
与旧实现（Express）的已知差异（有意不复制 / 残余差异）见 `COMPAT.md`。

## 数据迁移

旧 SQLite 库用独立工具一次性搬进 MySQL（源库只读、不删不清；目标库先增量迁移再批量 upsert，可重复执行）：

```bash
go run ./tools/migrate-sqlite-to-mysql \
  --db all \
  --src-metrics ./data/metrics.db --src-notifications ./data/notifications.db \
  --dst-host 127.0.0.1 --dst-user root --dst-password "$MYSQL_PWD" \
  --verify
```

`--dry-run` 只统计不写；`--verify` 逐表比对条数并抽样比对行；详见 `docs/DB.md`。

## 部署

`deploy/admin-server-go.service` 与 `deploy/nginx.conf.example` 是**样例**，按需复制到 systemd / nginx，
仓库不代为安装。只监听回环，对外由 nginx 反代。

## 许可

MIT，见 `LICENSE`。维护约定见 `AGENTS.md`。
