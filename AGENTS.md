# AGENTS.md — admin-server-go（维护约定）

> 面向维护者。README.md 面向使用者。行为权威是旧实现 `/root/proj/admin-server`（Express 4），
> 本仓库逐条对齐其 HTTP 契约；冲突时以旧实现 + 需求书 `/root/proj/go-rewrite/docs/admin-server.md` 为准。

## 分层

```
cmd/admin-server-go/       入口：依赖装配 + 路由挂载 + 启动
internal/config/           环境变量解析、config.json 读写（PBKDF2 口令哈希）
internal/store/            存储端口 + DB 句柄（两库共用；唯一实现是 MySQL）
internal/mysql/            MySQL 方言（DDL / 列探测 / 自增回填 / upsert）
internal/probe/            系统与服务探活采集（只读 /proc、ps、systemctl、docker）
internal/metrics/          指标留样与物化聚合、系统接口
internal/notifications/    通知中心（表读写、附件落盘、SSE）
internal/files/            文件区与通用上传下载
internal/shares/           文件区临时链接（本地 JSON 账本）
internal/apps/             应用探活面板（登记表只读消费 + 探活）
internal/term/             Web 终端口令与票据、ttyd 会话管理
internal/history/          Hermes 历史只读浏览（只读外部 state.db）
internal/auth/             鉴权中间件 + 接口令牌 + TOTP / JWT
internal/httpx/            HTTP 契约小工具（JSON、请求体、路径参数、身份）
internal/httpapi/          路由表与分发、鉴权包装、全局行为
internal/testsupport/      单测夹具：临时 MySQL schema（仅测试引用）
tools/migrate-sqlite-to-mysql/  独立迁移工具（一次性搬运旧 SQLite → MySQL；不被主程序 import）
```

**别把业务逻辑塞回 `httpapi`**：它只做装配与分发；业务放对应 `internal/<domain>`。

## 铁律

- **路由改了必须同步 `docs/API.md`**，`make check` 会机检（`scripts/check-api-docs.mjs` 从
  `internal/httpapi/*.go` 的 `{pattern: "METHOD /path"}` 与文档的 `### METHOD 路径` 比对）。
- **改动存储层必须同步 `docs/DB.md`**（表、列、映射、迁移）。
- 写操作、鉴权、限流的语义对齐旧实现；错误文案用旧实现的中文串，不要自创。
- **源码/文档/提交信息不得出现真实域名、公私网 IP、邮箱、密钥、真实口令、真实分享 token**；
  示例统一 `example.com` / `127.0.0.1`。
- 只监听 `127.0.0.1`（默认端口 19265）；Redis 前缀 / 采样锁 / 历史留样与线上实例隔离。
- 依赖白名单：`github.com/go-sql-driver/mysql`、`redis/go-redis/v9`、
  `modernc.org/sqlite`（**仅**用于只读打开**外部** SQLite：`internal/history` 读 Hermes `state.db`、
  迁移工具读旧库源文件；本服务自身存储不得用 SQLite）；
  新增依赖先写进 `go-rewrite/docs/00-CONVENTIONS.md` 再用。
- **存储一律 MySQL**（2026-10-16 用户口径）：运行期只有 MySQL 一条路径，不得引入方言分支或 `DB_DRIVER` 开关；
  任何写进本服务库表的数据都必须经 MySQL。旧 SQLite 数据由 `tools/migrate-sqlite-to-mysql/` 搬运。

## 命令

```bash
make check     # 路由↔文档机检 + go vet ./... + 单测（脚本起临时 MySQL 8.4 容器，验完即删）
make race      # 并发（SSE / 采样器）用 -race 再跑一遍
make build
```

## 提交

- 小步提交、conventional commits（中文描述）。
- **禁止** `git push`、`checkout -b`、`rebase`、`reset --hard`、`merge`。
- 使用全局 git 身份，不得改仓库级 `user.name` / `user.email`。
- 不提交构建产物、运行时数据（`config.json` / `data/` / `audit.log` / 数据库文件）。

## 已知坑

- 请求体在鉴权之前解析（对齐 Express `express.json`）；仅 `POST /api/admin/notifications` 放宽到 24mb。
  未知路径返回 Express 默认 HTML 404 页，不是 JSON。
- `builtin` 模式**忽略**外部 `X-Auth-User`（防提权）；`sso` 模式自带登录入口一律 404。
- 客户端 IP 一律读 `X-Real-IP`（缺失才退化），否则限流会退化成全局桶。
- 文件区：受保护根项隐身且整树禁写、只读根项仅根禁删、跳过软链、严格防路径穿越。
- 历史浏览只读打开 `state.db`，请求内 open→query→close，**不可写、不可长连接持有**（这是本仓库唯一的 SQLite，且是别人的只读文件）。
- Hermes `state.db` 禁止被本服务写入；旧 SQLite 源库在迁移工具里一律只读打开。

## 工程基线

见 `/root/proj/ENGINEERING_BASELINE.md`（v2）：导出符号必须有文档注释、注释写「为什么」、
不留死代码/调试输出、不新增 TODO/FIXME、禁止 any 逃逸、禁止空 catch、单文件超约 600 行要拆层。
