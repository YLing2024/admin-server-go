# admin-server-go 数据库（SQLite / MySQL 双支持）

本服务原有**两个** SQLite 库（`metrics.db` 指标、`notifications.db` 通知）。Go 重写用同一套业务代码
同时支持 SQLite（默认）与 MySQL，方言差异全部下沉到 `internal/sqlite` 与 `internal/mysql` 两套薄方言，
业务层（`internal/store` 及以上）不感知驱动。**不把业务 SQL 降级成两库公共子集**：DDL、upsert、自增主键
回填、列存在性探测、迁移方式都由方言层显式分支。

## 1. 选择与配置

- `DB_DRIVER=sqlite`（默认）| `mysql`。非法值启动即报错（fail fast）。
- **未配置 MySQL 时，`go-sql-driver/mysql` 不建连接、不影响启动**：只有 `DB_DRIVER=mysql` 分支才拼 DSN 并打开连接。
- 两个逻辑库（metrics / notifications）映射为：

  | | SQLite | MySQL |
  |---|---|---|
  | 指标库 | `ADMIN_METRICS_DB`（默认 `<repo>/data/metrics.db`） | schema `DB_METRICS_NAME` |
  | 通知库 | `ADMIN_NOTIFICATIONS_DB`（默认 `<repo>/data/notifications.db`） | schema `DB_NOTIFICATIONS_NAME` |

- MySQL 连接参数：`DB_HOST` / `DB_PORT`(默认 3306) / `DB_USER` / `DB_PASSWORD` + 两个库名；
  或一条 `DB_DSN`（go-sql-driver 原生 DSN，优先生效；其库名同时用于两库）。缺少必填项启动即报错。
- SQLite 打开时注入 pragma：`journal_mode=WAL`、`synchronous=NORMAL`、`busy_timeout=8000`
  （对齐旧 `db.ts`，写与聚合补算并发时避免 `SQLITE_BUSY`）。
- 连接池上限：SQLite 取 1（写锁敏感），MySQL 取 16。
- 连接串与口令只进返回值，**绝不落日志**（`config.Describe()` 只输出方言与库标识）。

## 2. 表与类型映射

指标库 4 张表、通知库 3 张表。

| 表 | 库 | 用途 | 键 |
|---|---|---|---|
| `metrics` | 指标 | 原始 5s 采样点（保留 30 天） | PK `ts` |
| `metrics_1m` | 指标 | 分钟物化桶（保留 90 天） | PK `bucket_start` |
| `metrics_1h` | 指标 | 小时物化桶（`P90`，直接来自 raw） | PK `bucket_start` |
| `metrics_1d` | 指标 | 天物化桶（`P99`，直接来自 raw） | PK `bucket_start` |
| `notifications` | 通知 | 通知账本 | PK `id`；索引 `ts` / `read_at` / `dedup_key` / `source` / `type` |
| `notification_types` | 通知 | 通知类别表 | PK `key` |
| `notification_attachments` | 通知 | 附件元信息（本体落盘） | PK `(notification_id, name)` |

指标列（raw 与三个物化桶共用）：`cpu` / `mem_percent` / `mem_used` / `mem_total` / `swap_percent` /
`psi_mem_avg10` / `psi_cpu_avg10` / `psi_io_avg10` / `net_rx_rate` / `net_tx_rate` / `disk_io_read` /
`disk_io_write`。物化桶另有 `sample_count` / `updated_at`。

| 逻辑字段 | SQLite | MySQL | 备注 |
|---|---|---|---|
| 时间（`ts` / `bucket_start` / `read_at` / `updated_at`） | `INTEGER` | `BIGINT` | epoch 秒（点）/ 桶起点 |
| 自增主键（`notifications.id`） | `INTEGER PRIMARY KEY AUTOINCREMENT` | `BIGINT AUTO_INCREMENT` | 回填方式见 §4 |
| 指标值（`cpu` 等） | `REAL` | `DOUBLE` | |
| 短文本（`level` / `source` / `type` / `dedup_key` / 类别 `key` / 附件 `name`/`mime`） | `TEXT` | `VARCHAR(...)` | |
| 长文本（`body` / `link` / `description`） | `TEXT` | `MEDIUMTEXT` / `TEXT` | |
| 布尔（`enabled`） | `INTEGER DEFAULT 1` | `TINYINT DEFAULT 1` | |
| 字符集 / 排序规则 | — | `utf8mb4` / `utf8mb4_unicode_ci` | |

> 通知 `id` 是 SQLite 行号（number），**不是**雪花 ID；项目级约定「雪花 ID 一律字符串」在本服务无对应字段
> （`/api/blog/*` 由别的后端提供）。

## 3. 迁移（只增量）

- 全部幂等：`CREATE TABLE IF NOT EXISTS`、`CREATE INDEX IF NOT EXISTS`、`ALTER TABLE ADD COLUMN`，
  **绝不 DROP / TRUNCATE / 重建表**。指标库建表在单事务内执行。
- 补列前先探测列是否存在：
  - SQLite：`SELECT name FROM pragma_table_info(?)`（参数绑定，无注入面）。
  - MySQL：查询 `information_schema.columns`（限定 `DATABASE()`）。
- **老库原地升级**：旧 `notifications.db` 缺 `type` 列 → 加列并 `UPDATE ... SET type = source WHERE type IS NULL`
  回填；旧 `metrics.db` 缺三个物化表 → 直接 `CREATE TABLE IF NOT EXISTS` 补建。二者都不动已有数据。
- 预置通知类别（`watchdog` / `monitor` / `cron` / `hermes` / `admin`）用 `INSERT OR IGNORE`（SQLite）/
  `INSERT IGNORE`（MySQL），不覆盖用户改动。

## 4. 两库差异的显式分支

| 能力 | SQLite | MySQL |
|---|---|---|
| DDL 自增主键 | `INTEGER PRIMARY KEY AUTOINCREMENT` | `BIGINT AUTO_INCREMENT` |
| 自增主键回填 | INSERT + `RETURNING id` | `Exec` + `LastInsertId()` |
| Upsert | `ON CONFLICT(<keys>) DO UPDATE SET …=excluded.…` | `ON DUPLICATE KEY UPDATE …=VALUES(…)` |
| 列存在性 | `pragma_table_info` | `information_schema.columns` |
| 建索引 | `CREATE INDEX IF NOT EXISTS` | `CREATE INDEX`，重复的 `Duplicate key name` 忽略 |
| 标识符引用 | 反引号（兼容接受） | 反引号（`key` 是保留字，必须引用） |
| 时间语义 | 整数 epoch | 整数 epoch（`parseTime=false`） |
| 字符集 | — | `utf8mb4_unicode_ci` |

> **标识符一律反引号引用**：通知类别表主键列名为 `key`，而 `KEY` 是 MySQL 保留字，裸写会让
> `CREATE TABLE` / `SELECT` / `UPDATE` 在服务端直接报 `ERROR 1064`。两库都用反引号（SQLite 兼容接受），
> 由 `Dialect.QuoteIdent` 统一提供，DDL、固定查询与动态拼装的 `UpsertSQL` / `migrate-data` 都走它。

## 5. 跨库搬运：`migrate-data`

把源库逐表拷贝到目标库：**拷贝不是清洗**（保留原始值）。逐库逐表拷贝 + 条数比对 + 抽样校验；
**源库只读、不删、不清**。

```bash
# sqlite → mysql：源库以只读模式打开（只发 SELECT），目标库跑增量迁移再写入
admin-server-go migrate-data \
  --from-driver sqlite \
  --from-metrics ./data/metrics.db --from-notifications ./data/notifications.db \
  --to-driver mysql \
  --to-metrics "user:pass@tcp(127.0.0.1:3306)/admin_metrics" \
  --to-notifications "user:pass@tcp(127.0.0.1:3306)/admin_notifications" \
  --verify
```

- `--from-driver` / `--to-driver`：`sqlite` | `mysql`。
- `--from-metrics` / `--from-notifications` / `--to-metrics` / `--to-notifications`：`sqlite` 时是文件路径；
  `mysql` 时是 go-sql-driver 原生 DSN（含库名）。
- `--dry-run`：只统计与校验，**不写目标库**。
- `--verify`：搬运后逐表比对目标条数与源条数，并抽样比对行；不一致 → 非 0 退出。
- `--sample N`：抽样校验条数（默认 5，按主键排序取首尾若干条）。
- 顺序：指标库 `metrics → metrics_1m → metrics_1h → metrics_1d`；通知库 `notification_types →
  notifications → notification_attachments`。
- 每表单事务 upsert，天然幂等，可重复执行。
- 源库**绝不删除、绝不清空、绝不改结构**（只读打开，不跑迁移）。
- 写入固定在同一条目标连接上，写前调用方言 `PrepareCopy` 关掉该连接的外键强制
  （SQLite `PRAGMA foreign_keys=OFF` / MySQL `SET FOREIGN_KEY_CHECKS=0`）。

## 6. 有意差异（相对旧实现）

新仓库必须与线上实例隔离（需求书 §5），因此**默认值**与旧实现有意不同；环境变量**同名**，需要旧行为时显式覆盖：

| 项 | 旧默认 | 本仓库默认 | 说明 |
|---|---|---|---|
| `PORT` | `3100` | `19265` | 新旧并存，避免抢占线上端口 |
| `ADMIN_REDIS_PREFIX` | `admin:session:` | `admin-go:session:` | Redis key 隔离，避免污染线上会话 |
| `ADMIN_SAMPLER_LOCK` | 线上采样锁 | `admin-server-go-sampler.lock`（临时目录） | 采样锁隔离，避免与线上互斥 |
| `ADMIN_HISTORY_FILE` | 线上历史文件 | `admin-server-go-history.json`（临时目录） | 历史留样隔离 |
| TOTP secret 文件 | 认证中心模块管理 | `<repo>/data/totp-secret.json` | 自带账号模式下的本地 secret（新增 `ADMIN_TOTP_SECRET_FILE`） |

其它新增（旧实现没有）：`DB_DRIVER` / `DB_DSN` / `DB_HOST` / `DB_PORT` / `DB_USER` / `DB_PASSWORD` /
`DB_METRICS_NAME` / `DB_NOTIFICATIONS_NAME`（双库支持）；`REDIS_URL`（默认 `redis://127.0.0.1:6379`，
可覆盖 Redis 连接目标）。其余环境变量名与默认值以旧实现实际读取为准（见 `README.md` 与旧 `AGENTS.md`）。

## 7. 从旧实现迁移的操作建议

1. 停旧实例（由验收方决定，执行者不得操作生产）。
2. 复制两个库作为源（副本），`sha256sum` 记录原件与副本。
3. `migrate-data` 以副本为 `--from`、以新库为 `--to`，带 `--verify`。
4. 比对源副本 `sha256sum` 迁移前后不变（证明源只读）。
