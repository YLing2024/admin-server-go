# admin-server-go 数据库（仅 MySQL）

> **口径（2026-10-16，用户定，覆盖旧口径）**：本服务**只支持 MySQL**。SQLite 双支持已移除：
> 代码里没有方言分支、没有 `DB_DRIVER` 开关、没有自建 SQLite 库路径；`go.mod` 的
> `modernc.org/sqlite` 仅保留给 `internal/history` **只读外部 Hermes `state.db`**（见 §6）。
> 旧 SQLite 数据由独立工具 `tools/migrate-sqlite-to-mysql/` 一次性搬运。
>
> 建表语句的**唯一权威**是 `go-rewrite/docs/mysql-schema-cloud.md` 里 `admin_metrics` 库；服务端 DDL
> 与之一致，且只做增量迁移，绝不另立一套。通知库（`admin_notifications`）已随通知中心功能下线
> （2026-10-19）：服务不再连接它，云库里的三张表暂时保留（数据已清空），历史数据搬运仍由一次性
> 工具 `tools/migrate-sqlite-to-mysql/` 负责。

## 1. 连接与配置

服务只连接**一个逻辑库**（指标），映射为 MySQL 实例下的一个 schema，由连接串的库名选定。

| 环境变量 | 默认 | 说明 |
|---|---|---|
| `DB_HOST` | `127.0.0.1` | MySQL 主机 |
| `DB_PORT` | `3306` | MySQL 端口 |
| `DB_USER` | 必填 | MySQL 账号；缺失启动即报错（fail fast） |
| `DB_PASSWORD` | 空 | MySQL 口令（只进 DSN 返回值，绝不落日志） |
| `DB_METRICS_NAME` | `admin_metrics` | 指标库 schema（`ssh_key` 也在该库） |
| `SSH_KEYS_USER` | `root` | 辅助命令允许登录的唯一用户名 |
| `SSH_KEYS_ENV_FILE` | `/etc/ssh-keys-from-admin.env` | 辅助命令在 sshd 下读取凭证的文件（sshd 不继承服务环境） |
| `SSH_KEYS_DB_HOST` / `_PORT` / `_NAME` | 回退 `DB_*` | 辅助命令的只读连接目标 |
| `SSH_KEYS_DB_USER` / `_PASSWORD` | 回退 `DB_USER` / `DB_PASSWORD` | 辅助命令的只读账号；权限只需 `SELECT` |

- DSN 由 `internal/mysql.BuildDSN` 组装：`parseTime=false`（时间列一律整数 epoch）、`utf8mb4_unicode_ci`、
  连接/读/写超时 5s/15s/15s。连接池上限 16。
- **schema 需预先建好**：服务与迁移工具都只做**表级**增量迁移，绝不 `CREATE DATABASE`；
  建库语句以云库权威文档 `go-rewrite/docs/mysql-schema-cloud.md` 为准（账号需具备对应库的权限）。
- 启动时对指标库跑一次增量迁移；指标库连不上只降级「长期留样停用」，不影响进程启动，与旧实现一致。
- `config.json`（含口令哈希）仍是**本地文件**，不入库、不迁移。

## 2. 表

指标库 4 张表（与 `mysql-schema-cloud.md` 同名同形）。通知库 3 张表（`notifications` /
`notification_types` / `notification_attachments`）已不在服务职责内，仅由一次性搬运工具参考，见 §5。

| 表 | 库 | 用途 | 键 |
|---|---|---|---|
| `metrics` | 指标 | 原始 5s 采样点（保留 30 天） | PK `ts` |
| `metrics_1m` | 指标 | 分钟物化桶（`P100` = 桶内最大） | PK `bucket_start` |
| `metrics_1h` | 指标 | 小时物化桶（`P90`，直接来自 raw） | PK `bucket_start` |
| `metrics_1d` | 指标 | 天物化桶（`P99`，直接来自 raw） | PK `bucket_start` |
| `ssh_key` | 指标 | SSH 公钥白名单（管理端唯一事实源，sshd 实时查询） | PK `id`；UK `fingerprint` |

指标列（raw 与三个物化桶共用）：`cpu` / `mem_percent` / `mem_used` / `mem_total` / `swap_percent` /
`psi_mem_avg10` / `psi_cpu_avg10` / `psi_io_avg10` / `net_rx_rate` / `net_tx_rate` / `disk_io_read` /
`disk_io_write`（固定 12 列，后两列是**全部整盘合计**速率），之后**按启动时探测到的每块整盘**追加
`disk_io_<device>_read` / `disk_io_<device>_write`（`device` 为 `/proc/diskstats` 整盘名，按字典序稳定；
分区的流量含在整盘里）。设备集在服务启动时探测一次并由 `store.SetDiskDevices` 写进 `MetricKeys`；运行中
新出现的设备只计入合计列，不新增列。物化桶另有 `sample_count` / `updated_at`。

> 指标表主键（`ts` / `bucket_start`）为 BIGINT 整数；项目级约定「雪花 ID 一律字符串」在本服务
> 无对应字段（`/api/blog/*` 由别的后端提供）。

### 2.1 `ssh_key`（SSH 公钥白名单）

```sql
CREATE TABLE IF NOT EXISTS ssh_key (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  title         VARCHAR(100)  NOT NULL,
  public_key    TEXT          NOT NULL,
  fingerprint   CHAR(64)      NOT NULL,          -- SHA256(blob) 的 hex，用于去重
  key_type      VARCHAR(32)   NOT NULL,
  comment       VARCHAR(200)  NOT NULL DEFAULT '',
  enabled       TINYINT(1)    NOT NULL DEFAULT 1,
  created_at    DATETIME      NOT NULL,
  last_used_at  DATETIME      NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_fingerprint (fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
```

- `public_key` 存归一化后的 `type base64 comment` 单行（sshd 直接读取），指纹列存 hex；接口对外展示
  `SHA256:` + 无填充 base64（与 `ssh-keygen -lf` 一致）。
- `created_at` 写入时取 `NOW()`；`last_used_at` 由辅助命令在收到 sshd `%f` 时尽力回写（失败不影响输出）。
- 表由服务启动时的增量迁移创建；辅助命令**只建连接、不建表**，查询走 `SELECT`，仅额外尝试一次
  `UPDATE last_used_at`。只读账号若严格只给 `SELECT`，只是「最近使用」不更新，登录不受影响。
- 管理端最多 20 行，上限在应用层校验；数据库不设 CHECK，避免与旧版本 MySQL 的兼容性分歧。

## 3. 迁移（只增量）

- 全部幂等：`CREATE TABLE IF NOT EXISTS`、`ADD COLUMN`，**绝不 DROP / TRUNCATE / 重建表**；
  `ssh_key` 与指标表在同一轮启动迁移里创建（先指标、后公钥，互不依赖）。
  886k 行的指标表尤其不做整表重写。
- 补列前先探测列是否存在：查询 `information_schema.columns`（限定 `DATABASE()`）。
- **老库原地升级**：旧指标库缺三个物化表 → `CREATE TABLE IF NOT EXISTS` 补建；旧指标表缺按盘速率列 →
  逐表逐列 `information_schema.columns` 探测后 `ALTER TABLE ... ADD COLUMN ... DOUBLE` 补列
  （旧行新列为 NULL，读取按缺失处理）。都不动已有数据。
- MySQL 无 `CREATE INDEX IF NOT EXISTS`：重复建索引的 `Duplicate key name` 直接忽略。

## 4. MySQL 行为差异（相对旧 SQLite 口径，已在方言层修正）

陈旧的「双库支持」有两个 MySQL 语义坑，直接照搬 SQLite 写法会悄悄改变结果，现已在 `internal/mysql`
与聚合 SQL 修正并加单测：

| 坑 | 旧写法 | 后果 | 现写法 |
|---|---|---|---|
| 取整 | `CAST(ts / ? AS SIGNED)` | MySQL 的 `CAST ... AS SIGNED` 是**四舍五入**（179/60 → 3），与 SQLite `CAST(... AS INTEGER)` 的截断不一致，分桶边界偏移 | `CAST(TRUNCATE(ts / ?, 0) AS SIGNED)`：先截断再转整数 |
| 取整除法 | `(pct * cnt + 99) / 100` | MySQL 的 `/` 是**真除法**（返回 DECIMAL），最近秩 `3.9900` 永不等于 `ROW_NUMBER`，分位值全为 NULL | `(pct * cnt + 99) DIV 100`：整数除法 |

其它显式事实：自增主键用 `Exec` + `LastInsertId()` 回填；upsert 用 `ON DUPLICATE KEY UPDATE`；
标识符一律反引号引用（`key` 是保留字）；时间语义为整数 epoch（`parseTime=false`）。

## 5. 跨库搬运：`tools/migrate-sqlite-to-mysql/`

独立 `go run` 的一次性工具，**不被服务主程序 import**。把旧 SQLite 库按表搬进 MySQL，逐表执行
「源计数 → 拷贝 → 目标计数 → 抽样比对」；源库以 `mode=ro` 只读打开（只发 SELECT），目标库只做增量迁移
后以**批量 upsert** 写入，可重复执行（幂等）。

> 服务已于 2026-10-19 下线通知中心并不再连接通知库，但通知库的搬运链路保留在本工具内（自行携带
> 其建表语句），以保留旧数据的一次性搬运能力。

```bash
go run ./tools/migrate-sqlite-to-mysql \
  --db all \
  --src-metrics ./data/metrics.db --src-notifications ./data/notifications.db \
  --dst-host 127.0.0.1 --dst-port 3306 --dst-user root --dst-password "$MYSQL_PWD" \
  --verify
```

- `--db metrics|notifications|all`：只搬某个逻辑库或两个都搬（默认 `all`）。
- `--src-metrics` / `--src-notifications`：SQLite 源文件（只读；按 `--db` 选中的库必填）。
- `--dst-host` / `--dst-port` / `--dst-user` / `--dst-password` / `--dst-metrics-name` /
  `--dst-notifications-name`：目标连接；或为单库给 `--dst-metrics-dsn` / `--dst-notifications-dsn`。
- `--dry-run`：只统计与校验，**不写目标库**。
- `--verify`：搬运后逐表比对目标条数与源条数，并抽样比对行；不一致 → 非 0 退出。
- `--sample N`：抽样条数（默认 5，按主键取首尾若干条）。
- `--batch N`：单条多行 `INSERT` 的行数（默认 500）——避免超长 SQL，也避免逐行往返。
- `--concurrency N`：并发写事务数上限（默认 2）——写入有批次与并发控制，不把目标实例打爆；
  源表内主键唯一，写不冲突。
- 顺序：指标库 `metrics → metrics_1m → metrics_1h → metrics_1d`；通知库 `notification_types →
  notifications → notification_attachments`。
- 源库**绝不删除、绝不清空、绝不改结构**（只读打开，不跑迁移）。每批写事务前关掉该会话的外键强制。

## 6. 唯一的 SQLite：`internal/history` 只读外部文件

`internal/history` 要浏览 Hermes 的历史会话，数据源是 **Hermes 自己的 `state.db`**（别人的 SQLite 文件），
每次请求独立「只读打开 → 查询 → 关闭」，绝不长连接持有。这是**只读别人的文件**，不属于「本服务支持
SQLite」：**服务自身存储一律 MySQL**，没有任何自建 SQLite 库路径、方言分支或 `DB_DRIVER` 开关。
`go.mod` 因此保留 `modernc.org/sqlite`，仅用于此处的只读打开（`HERMES_STATE_DB` 可覆盖路径）。

## 7. 有意差异（相对旧实现）

新仓库必须与线上实例隔离（需求书 §5），因此**默认值**与旧实现有意不同；环境变量**同名**，需要旧行为时显式覆盖：

| 项 | 旧默认 | 本仓库默认 | 说明 |
|---|---|---|---|
| `PORT` | `3100` | `19265` | 新旧并存，避免抢占线上端口 |
| `ADMIN_REDIS_PREFIX` | `admin:session:` | `admin-go:session:` | Redis key 隔离，避免污染线上会话 |
| `ADMIN_SAMPLER_LOCK` | 线上采样锁 | `admin-server-go-sampler.lock`（临时目录） | 采样锁隔离，避免与线上互斥 |
| `ADMIN_HISTORY_FILE` | 线上历史文件 | `admin-server-go-history.json`（临时目录） | 历史留样隔离 |
| TOTP secret 文件 | 认证中心模块管理 | `<repo>/data/totp-secret.json` | 自带账号模式下的本地 secret |
| 存储 | SQLite 文件 | MySQL（`DB_HOST` 等） | 2026-10-16 用户口径变更 |

新增（旧实现没有）：`DB_HOST` / `DB_PORT` / `DB_USER` / `DB_PASSWORD` / `DB_METRICS_NAME`
（MySQL 连接）、`REDIS_URL`（默认 `redis://127.0.0.1:6379`）。
其余环境变量名与默认值以旧实现实际读取为准（见 `README.md` 与旧 `AGENTS.md`）。

## 8. 从旧实现迁移的操作建议

1. 停旧实例（由验收方决定，执行者不得操作生产）。
2. 复制两个库作为源（副本），记录业务表行数快照。
3. 用 `tools/migrate-sqlite-to-mysql` 以副本为源、以新库为目标，带 `--verify`。
4. 迁移前后核对源副本业务表行数不变（证明源只读）；目标库逐表条数与抽样比对通过。
