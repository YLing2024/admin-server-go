# admin-server-go 接口文档

> 唯一权威接口文档。**与代码同一笔提交维护：改了路由不改本文件，`npm run check` / `make check` 会红。**
> 路由清单机检脚本：`scripts/check-api-docs.mjs`（从 `internal/httpapi/*.go` 提取 `{pattern: "METHOD /path"}`，
> 与本文的 `### METHOD 路径` 标题比对，缺失/多余都非 0 退出）。
> 旧实现（`/root/proj/admin-server`，Express 4）是行为权威，本文件逐条对齐其 `docs/API.md` 与源码。

## 一、通用约定

- **基址**：本文所有示例用占位符 `$BASE_URL`，部署环境注入（如 `http://` + 反代入口）。仓库不写任何真实域名 / 内网地址。
- **内容类型**：请求与响应均为 JSON（`Content-Type: application/json`）；SSE 为 `text/event-stream`；博客图片上传为 `multipart/form-data`。
- **鉴权凭证**：本文示例用 `$TOKEN` 占位。默认模式下为自带账号会话 token（`Authorization: Bearer <token>` 或 HttpOnly cookie `admin_session`，二者取其一）；`sso` 模式下为反代注入的 `X-Auth-User` 头。
- **错误响应**：统一 JSON `{ "error": "<中文可定位文案>" }`，部分接口附 `code` / `retryAfter`。未单独列出的错误分支按「错误码表」的通用项处理。
- **请求体解析时机**：请求体在鉴权之前解析（对齐 `app.use(express.json)`）。默认上限 100kb。解析失败 / 超限统一 `500 { "error": "服务器内部错误" }`。
- **未知路径**：返回 Express 默认 HTML 404 页（`Cannot <METHOD> <path>`），不是 JSON。
- **方法回退**（对齐 Express 自动行为）：已注册路径上的 `OPTIONS` **不走鉴权**，返回 `200`，`Allow` 为该方法路径命中的全部方法（逗号分隔、**无空格**，`GET` 后追加 `HEAD`），`Content-Type: text/html; charset=utf-8`，体即 `Allow` 串；`HEAD` 由对应 `GET` 路由承接（鉴权照常生效），状态码与响应头同 `GET`、无正文，`Content-Length` 取 `GET` 正文长度。未注册路径的 `OPTIONS`/`HEAD` 仍走上面的 HTML 404。
- **审计**：涉及写操作的接口会写 `audit.log`（记录动作、客户端 IP、成功/失败与资源标识；**绝不记录令牌明文与登记表中的 url**）。
- **时间单位**：见「数据模型约定」。
- **客户端 IP**：一律取自 `X-Real-IP`（反代注入），用于限流与审计定位。

## 二、认证模式

服务端环境变量 `AUTH_MODE` 决定鉴权通道，生产实际取值以部署环境为准（本文不写具体值）：

| `AUTH_MODE` | 语义 | 鉴权通道 |
|---|---|---|
| `builtin`（默认） | 自带账号口令：本服务自己的登录页 + 会话 cookie | `Authorization: Bearer <token>` 或 cookie `admin_session`；Redis 命中 `<ADMIN_REDIS_PREFIX><token>` 即通过并滑动续期。**忽略外部 `X-Auth-User`**（防提权） |
| `sso` | 关掉自带口令，管理端身份由前置认证层决定 | 只认反代注入的 `X-Auth-User`（网关会先剥掉客户端伪造的同名头） |

- 模式探测：`GET /api/admin/auth-mode`（免鉴权）。
- `builtin` 独有：`POST /api/admin/login`、`POST /api/admin/logout`、`GET /api/admin/me`；`sso` 下这三者一律 `404`。
- 鉴权失败一律 `401 JSON { "error": "未登录" }`（不 302、不 500）。会话存储异常为 `500 { "error": "会话服务异常" }`。
- 环境变量名：`AUTH_MODE`、`ADMIN_REDIS_PREFIX`（会话 key 前缀，测试实例隔离用）、`ADMIN_CONFIG_PATH`（配置文件路径）。具体取值由部署环境注入。

## 三、数据模型约定

### 3.1 单位与语义

| 字段（示例） | 单位 / 语义 |
|---|---|
| `mem_mb` | 进程**自身** RSS（MB，整数）；RSS 会把共享页在每个进程里各算一次 |
| `subtree_mem_mb` | 该进程**自身 + 全部后代**的 RSS 之和（MB，整数）；同样含共享页重复计算 |
| `subtree_cpu` | 该进程自身 + 全部后代的瞬时 CPU 之和（%），与 `cpu` 同源同基准 |
| `subtree_count` | 含自身的后代进程数（整数） |
| `thread_count` | 该进程的线程数（读 `/proc/<pid>/task` 条目数，整数）；进程已退出/读不到为 `null` |
| `cgroup_mem_mb` | 该 systemd 单位 cgroup 的**真值**（MB，整数，含页缓存）；与 RSS 口径不同、不可直接比 |
| `total_mem_mb` | 全部进程 RSS 之和（MB，整数）；含共享页重复计算 |
| `total_threads` | 全部进程线程数之和（整数，截断前口径）；与 `count` 是**两个口径，不得混算** |
| `total_cgroup_mb` | `system.slice` 的 cgroup 真值（MB，整数） |
| `total_cpu` | 系统**总** CPU 占用（%），**不是**列表之和 |
| `cpu.usage_15s` / `cpu.usage_60s` | 15 秒 / 60 秒窗口的 CPU 占用均值（%，一位小数） |
| `cpu.run_queue` / `cpu.blocked` | `/proc/stat` 的运行队列 / IO 阻塞进程数（整数） |
| `cpu.top_movers[].delta_percent` | 最近 15 秒窗口内该进程消耗的 CPU 占整机可用时间百分比 |
| `top_services[].cpu_percent` | 该 systemd 单元最近一个 tick 的 CPU 占整机可用时间百分比 |
| `top_services[].mem_mb` | 该 systemd 单元 cgroup 内存真值（MB，整数）；读不到为 `null` |
| `system/services[].cpu_percent` | 该运行中单元的 cgroup `cpu.stat` 两次采样差值占整机可用 CPU 时间百分比；单核满载约 `100 ÷ 核数` |
| `system/services[].mem_mb` | 该单元 cgroup `memory.current` 换算的 MB 整数；读不到为 `null` |
| `system/services[].since` | 本次启动时间，**epoch 秒**（取自 systemd invocation 文件 mtime）；读不到为 `0` |
| `ports[].protocol` / `ports[].state` | `tcp` / `udp`；`LISTEN`（TCP）/ `UNCONN`（UDP） |
| `disks[].inode_*` | inode 文件数口径：`inode_total` / `inode_used`（个）、`inode_percent`（%，一位小数） |
| `disk_percent` | 历史趋势里的根分区使用率（%，一位小数），与 `system/info` 的 `disks[]` 同口径 |
| `stall_score` | 0–100 卡顿指数（一位小数）；`stall_level` 为对应中文档位 |
| `mem_percent` / `cpu.usage_percent` / `cpu.load_percent` | 百分比（0–100） |

> 三个内存数（`total_mem_mb` / `total_cgroup_mb` / `free` 口径）都「对」，但**口径不同**：RSS 含共享页重复计算、cgroup 含页缓存。界面上必须并列标注，不能混用。

### 3.2 时间与 ID

- **时间**：`createdAt` / `expiresAt` / `lastUsedAt`（接口令牌）为 **epoch 毫秒**；`system.uptime` 为**秒**。一律 UTC 数值，由前端按本地时区展示。
- **ID 类型**：
  - 接口令牌 `id`：**string**（`sha256(token)` 十六进制），令牌明文仅生成时返回一次。
  - 会话 `id`：**string**（会话 token）；历史会话 `id`：**string**（Hermes 会话 id）。
  - SSH 公钥 `id`：**string**（`ssh_key.id` 的十进制字符串；BIGINT 转 Number 可能丢精度）。
  - **项目级约定**：博客等模块的**雪花 ID（`public_id`）一律为字符串，禁止转 Number**（会丢精度）；本服务当前无雪花 ID 字段，`/api/blog/*` 由别的后端提供。

### 3.3 存储与迁移

- 存储**一律 MySQL**：指标库 schema 由 `DB_METRICS_NAME`（默认 `admin_metrics`）指向；连接参数与建表
  口径见 `docs/DB.md`。本服务没有 SQLite 自建库路径、方言分支或 `DB_DRIVER` 开关。
- 迁移只做增量（`CREATE TABLE IF NOT EXISTS` / `ADD COLUMN`），老库缺聚合表时原地补建可用；
  绝不 DROP / TRUNCATE / 重建表，不动云库已有数据。
- 旧 SQLite 数据由独立工具 `tools/migrate-sqlite-to-mysql/` 一次性搬运（该工具不被服务主程序 import）。
- `config.json`（含口令哈希）仍是本地文件，不入库；Redis 语义（会话 / 票据 / 限流 / 采样锁）不变。

## 四、系统与进程

### GET /

- 鉴权：无
- 请求参数：无
- curl：`curl -sS "$BASE_URL/"`
- 成功 200：`{ "name": "admin-server", "status": "ok" }`
- 错误：无
- 副作用：无
- 幂等：幂等；无显式限流

### GET /api/admin/system

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system"`
- 成功 200（节选；所有字段缺失时为 `null`/省略，前端需容错）：

```json
{
  "cpu": {
    "usage_percent": 12.3,
    "usage_15s": 12.1,
    "usage_60s": 10.8,
    "run_queue": 4,
    "blocked": 0,
    "per_core": [{ "id": 0, "usage_percent": 8.1 }],
    "top_movers": [{ "pid": 1234, "name": "compile", "delta_percent": 62.0, "current_percent": 63.5 }],
    "model": "…", "cores": 8, "loadavg": [0.5, 0.4, 0.3], "load_percent": 6.3
  },
  "memory": { "percent": 47, "used": 7239, "available": 8000, "buffCache": 2100, "free": 1000, "total": 16000,
              "swapTotal": 0, "swapUsed": 0, "swapFree": 0, "swapPercent": 0, "zram": null },
  "disk": { "mount": "/", "used": 120, "total": 500, "free": 380, "percent": 24 },
  "disks": [
    { "filesystem": "/dev/vda1", "mount": "/", "device": "vda", "percent": 24,
      "total": 500, "used": 120, "free": 380,
      "read_rate": 0, "write_rate": 0, "read_bytes": 0, "write_bytes": 0 }
  ],
  "network": { "rx_rate": 1024, "tx_rate": 512, "rx_bytes": 0, "tx_bytes": 0 },
  "disk_io": { "read_rate": 0, "write_rate": 0, "read_bytes": 0, "write_bytes": 0 },
  "processes": { "running": 1, "total": 344 },
  "psi": { "memory": { "some": { "avg10": 0 } }, "cpu": { "some": { "avg10": 1 } }, "io": { "some": { "avg10": 0 } } },
  "top_services": [{ "unit": "hermes-gateway.service", "cpu_percent": 12.3, "mem_mb": 550 }],
  "stall_score": 6.2,
  "stall_level": "顺畅",
  "uptime": 123456, "os": "Linux …", "hostname": "…"
}
```

- CPU 口径：`cpu.usage_percent` 与 `cpu.per_core` 同源于后台采样器的同一对 `/proc/stat` 快照（总量取 `cpu ` 聚合行、每核取 `cpuN` 行），不做 EMA；采样间隔默认 1 秒（`PROBE_INTERVAL_SEC`，最小 1），窗口不足 1 秒的样本直接丢弃。HTTP 请求只读快照，不会推进采样基线，因此连续调用与静置后调用结果一致。
- `cpu.load_percent`：`load1 ÷ 逻辑核数 × 100`（一位小数）；`loadavg` 仍为 `/proc/loadavg` 原始 1/5/15 分钟负载。
- `cpu.usage_15s` / `cpu.usage_60s`：同一采样 tick 序列的 15 秒 / 60 秒均值（固定长度环形缓冲，容量按 `PROBE_INTERVAL_SEC` 换算），启动初期返回已积累样本的均值；`usage_percent` 仍只表示最近一个 tick。
- `cpu.run_queue` / `cpu.blocked`：取自同一个 `/proc/stat` 的 `procs_running` / `procs_blocked`。`run_queue > cores` 表示已在排队；`blocked > 0` 表示有进程卡在 IO。
- `cpu.top_movers`：最近 15 秒 `Δ(utime+stime)` 最大的 5 个进程（按进程 CPU 同一整机口径换算为 `delta_percent`）。窗口内出现后又退出的进程仍保留在榜内并标 `"exited": true`，其 `current_percent` 为 0；`current_percent` 为最近一个 tick 的进程占用。
- `top_services`：`system.slice` 下各 `*.service` 的 cgroup v2 `cpu.stat` 中 `usage_usec` 两 tick 做差得到的 CPU 占比，最多 10 个。`cpu_percent` 按整机可用 CPU 时间归一（与进程 CPU 同口径，单核满载约 `100 ÷ 核数`）；`mem_mb` 读 `memory.current`，读不到为 `null`。
- `stall_score` / `stall_level`：由同一 tick 的 PSI（`some.avg60`）与运行队列合成的 0–100 卡顿指数：
  `0.40×min(psi_cpu,100) + 0.30×min(psi_io,100) + 0.20×min(psi_mem,100) + 0.10×min(run_queue÷cores×100,100)`。
  `<20` 为 `顺畅`、`20–50` 为 `有压力`、`>50` 为 `明显卡顿`；权重是后端常量，前端只展示文案。
  分数只描述等待时长，档位另有两条下限：`run_queue > cores×1.5`，或 `usage_15s ≥ 95` 且 `run_queue > cores` 时，
  档位至少抬到 `有压力`（`stall_score` 不变）。这样满载排队但 PSI 尚低时不会显示「顺畅」。
- 以上新增字段全部由后台 1 秒采样器产出（含 `psi` 也取该 tick 的快照），请求路径不读取 `/proc` 或 cgroup。

- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |
  | 500 | `{ "error": "获取系统信息失败: …" }` |

- `disks[]` 单盘字段：

  | 字段 | 类型 | 语义 |
  |---|---|---|
  | `mount` | string | 挂载点 |
  | `filesystem` | string | `df` 报出的设备路径（如 `/dev/vda1`） |
  | `device` | string | 基础设备名（与 `/proc/diskstats` 对齐）：`/dev/vda1`→`vda`、`/dev/nvme0n1p1`→`nvme0n1`、`/dev/mapper/x`→`x`；无法映射为 `""` |
  | `total` / `used` / `free` | number | 字节（`df -kP` × 1024） |
  | `percent` | number | 使用百分比（整数） |
  | `read_rate` / `write_rate` | number | 该设备读写速率（字节/秒；首次采样为 0） |
  | `read_bytes` / `write_bytes` | number | 该设备累计读写字节 |

- 速率口径：`disk_io`（全部整盘合计）、`disks[].read_rate/write_rate`、`network` 的速率**同源于唯一的 1s 后台采样快照**，
  不随请求次数重置（多客户端 / 多标签 / SSE 并发读到的是一致的数值）；快照尚未产出时速率一律 `0`。磁盘累计字节按设备名判定整盘，分区（`vda1`、`nvme0n1p1`）不计入。
- 副作用：同步写入历史采样 buffer（趋势图数据源）。幂等：是。限流：无。

### GET /api/admin/system/info

- 鉴权：`authRequired`
- 请求参数：

  | 名称 | 位置 | 类型 | 必填 | 默认 | 取值/校验 |
  |---|---|---|---|---|---|
  | `ports` | query | string | 否 | `all` | `all` 返回全部监听项；`15` 只返回前 15 条（`total` 仍为全量） |

- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system/info"`
- 成功 200（数组字段缺失时为空数组；`temperatures` 无热区时省略）：

```json
{
  "os": "Ubuntu 24.04.1 LTS",
  "kernel": "6.12.107",
  "arch": "x86_64",
  "hostname": "example",
  "bootTime": 1760000000000,
  "uptimeSeconds": 1055800,
  "load": [0.42, 0.51, 0.63],
  "cpuModel": "…",
  "cpuCores": 8,
  "memory": { "total": 17000000000, "available": 8000000000, "used": 9000000000, "cached": 2100000000 },
  "swap": { "total": 0, "used": 0 },
  "disks": [
    { "mount": "/", "filesystem": "/dev/vda1", "fstype": "ext4",
      "total": 500000000000, "used": 230000000000, "free": 250000000000,
      "percent": 46, "inode_total": 3268608, "inode_used": 605694, "inode_percent": 18.5 }
  ],
  "ports": {
    "total": 18,
    "items": [{ "port": 22, "protocol": "tcp", "state": "LISTEN", "process": "sshd", "pid": 812, "address": "0.0.0.0" }],
    "error": ""
  },
  "systemd": { "failed": [{ "name": "foo.service", "description": "…" }], "error": "" },
  "logins": { "items": [{ "user": "root", "ip": "127.0.0.1", "time": "Tue Oct 20 09:00 still logged in" }], "error": "" },
  "temperatures": [{ "zone": "thermal_zone0", "type": "x86_pkg_temp", "celsius": 42.5 }]
}
```

- 字段与来源：

  | 字段 | 类型 | 语义 / 来源 |
  |---|---|---|
  | `os` / `kernel` | string | `/etc/os-release` 的 `PRETTY_NAME`（缺失回退 `NAME + VERSION`）；`/proc/sys/kernel/osrelease` |
  | `arch` | string | `runtime.GOARCH`，展示时映射为 `uname -m` 风格（`amd64`→`x86_64`、`arm64`→`aarch64`、`386`→`i386`） |
  | `hostname` | string | `os.Hostname()` |
  | `bootTime` | number | 启动时间，**epoch 毫秒**（`/proc/stat` 的 `btime`；缺失时用当前时间 − uptime 补齐） |
  | `uptimeSeconds` | number | 运行秒数（`/proc/uptime`） |
  | `load` | number[] | 1/5/15 分钟负载（`/proc/loadavg` 前三列；读不到为 `[0,0,0]`） |
  | `cpuModel` / `cpuCores` | string / number | `/proc/cpuinfo` 型号；`runtime.NumCPU()` 逻辑核数 |
  | `memory` | object | 字节：`total`、`available`（`MemAvailable`）、`used`（`total − available`）、`cached`（`Buffers + Cached`） |
  | `swap` | object | 字节：`total` / `used`（`SwapTotal − SwapFree`） |
  | `disks` | array | `/proc/self/mounts` 中 device 以 `/dev/` 开头的真实块设备挂载点（**全部**挂载点，不截断），`syscall.Statfs` 读容量与 inode；跳过 `tmpfs` / `overlay` / `devtmpfs` / `squashfs` / `iso9660` / loop 设备与重复挂载点。`percent`、`inode_percent` 保留一位小数 |
  | `ports` | object | `ss -tulnp`：`total` 为全部 TCP/UDP 监听项数；`items` 默认返回全部（`?ports=15` 时只给前 15 条）。每项含 `port` / `protocol` / `state` / `process` / `pid` / `address` |
  | `systemd` | object | `systemctl --failed --no-legend --plain`：`failed[]` 含 `name` / `description` |
  | `logins` | object | `last -n 5 -w`：`items[]` 含 `user` / `ip` / `time`（`time` 为 `last` 原始文本，避免 locale 解析；`reboot`/`shutdown`/`wtmp` 伪记录被过滤） |
  | `temperatures` | array（可选） | `/sys/class/thermal/thermal_zone*/temp` 存在时才返回；`celsius` 保留一位小数 |

- 降级语义：`ss` / `systemctl` / `last` 任一失败（命令缺失、超时、非零退出）只让对应对象的数组为空并在该对象的 `error` 写原因；
  **接口仍返回 200**，静态字段仍正常返回。每条外部命令超时 1500ms，三条并发执行。
- 缓存：成功采集的快照在进程内缓存 60s；进程排行接口不缓存。
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |

- 副作用：只读 `/proc`、`/sys`、`/etc/os-release` 与三条只读命令；不写库。幂等：是。限流：无。

### GET /api/admin/system/processes

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system/processes"`
- 成功 200：

  ```json
  {
    "processes": [
      { "pid": 123, "name": "nginx", "cpuPercent": 12.3, "memMB": 45.2, "command": "nginx: worker process" }
    ]
  }
  ```

  | 字段 | 类型 | 语义 |
  |---|---|---|
  | `pid` | number | 进程 PID |
  | `name` | string | `/proc/<pid>/status` 的 `Name`（缺失回退 `/proc/<pid>/stat` 的 comm） |
  | `cpuPercent` | number | 采样区间（500ms）内占**整机** CPU 的百分比，保留一位小数；单进程上限 100 |
  | `memMB` | number | `VmRSS` 换算的 MB，保留一位小数 |
  | `command` | string | `/proc/<pid>/cmdline`（NUL 转空格，最多 300 字符；不读 `environ`） |

- 排序与去重：先按 `cpuPercent` 取前 10，再按 `memMB` 取前 10，按 `pid` 去重合并（CPU 榜在前），最多 20 条。
  CPU 值由两次采样（间隔 500ms）的 `/proc/stat` 与 `/proc/<pid>/stat` 滴答差计算，全部进程占比之和不超过 100。
- 降级：`/proc` 不可读时返回 `{ "processes": [], "error": "…" }`，状态码仍为 200。
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |

- 副作用：两次 `/proc` 只读遍历（间隔 500ms）；不调用 `ps`、不读写文件系统。幂等：是（值随采样时刻变化）。限流：无。

### GET /api/admin/system/services

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system/services"`
- 成功 200：全部 running 的 `.service` 单元数组（按 CPU 降序，CPU 相同按单元名）；没有单元时为 `[]`：

```json
[
  { "unit": "admin-server-go.service", "active": "active", "cpu_percent": 7.4, "mem_mb": 35, "since": 1792502132 }
]
```

- 字段：

  | 字段 | 类型 | 语义 |
  |---|---|---|
  | `unit` | string | systemd 单元名（含 `.service`） |
  | `active` | string | `systemctl list-units` 的 `active` 列；running 状态下通常为 `active` |
  | `cpu_percent` | number | cgroup `cpu.stat` 的 `usage_usec` 两次采样差值，按整机可用 CPU 时间归一（单核满载约 `100 ÷ 核数`），一位小数 |
  | `mem_mb` | number \| null | cgroup `memory.current` 换算的 MB 整数；读不到为 `null` |
  | `since` | number | 本次启动时间，**epoch 秒**（取自 `/run/systemd/units/invocation:<unit>` 的 mtime）；读不到为 `0` |

- 采集：`systemctl list-units --type=service --state=running --output=json` + 每个单元 cgroup 的
  `cpu.stat` / `memory.current`；两次 cgroup 采样间隔 80ms，整次请求硬超时 **300ms**。
- 降级：命令失败 / 超时统一返回 `[]`（状态码仍为 200）并写 warn 日志；单个单元 cgroup 读不到只影响该行。
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |

- 副作用：一次只读 `systemctl` 与两次 cgroup 文件读取；不写库。幂等：是（值随采样时刻变化）。限流：无。

### GET /api/admin/system/history

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system/history"`
- 成功 200：环形 buffer 中的采样点数组（最多约 120 点）`[{ "ts": <epoch ms>, "cpu": …, "mem_percent": …, "disk_percent": …, …, "disk_io_read": …, "disk_io_write": …, "disk_io_vda_read": …, "disk_io_vda_write": … }]`；无数据为 `[]`。
  除固定列外，还带每块整盘的速率键 `disk_io_<device>_read` / `disk_io_<device>_write`（`device` 同 `disks[].device`）；
  老采样点缺这些键时视为缺失，前端自行回退。
- 错误：401
- 副作用：无；幂等：是；限流：无

### GET /api/admin/system/metrics

- 鉴权：`authRequired`
- 请求参数：

  | 名称 | 位置 | 类型 | 必填 | 默认 | 取值/校验 |
  |---|---|---|---|---|---|
  | `range` | query | string | 否 | `1d` | `1h`/`6h`/`1d`/`7d`/`30d` 之一，未知值回退 `1d` |
  | `step` | query | string | 否 | `1m` | `1m`/`5m`/`1h`/`1d` 之一，未知值回退 `1m` |

- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system/metrics?range=1d&step=1m"`
- 成功 200：`{ "step": "1m", "range": "1d", "points": [{ "ts": <epoch ms>, … }], "meta": { "step": "1m", "range": "1d", "bucketCount": 0, "firstBucket": null, "lastBucket": null, "recordedSeconds": 0 } }`；无数据/单点也返回结构完整的 200（`points` 可为空/单点）。
  每个点含固定指标列 + 每块整盘的 `disk_io_<device>_read` / `disk_io_<device>_write` 列；老数据点缺这些列时值为 `null`（前端按缺失处理，可回退合计列）。
- 错误：401；查询失败不 500，降级为空 `points` + 完整 `meta`
- 副作用：无；幂等：是；限流：无

### GET /api/admin/system/stream

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -N -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/system/stream"`
- 成功 200（`text/event-stream`，每 1 秒一帧，每 10 帧一条 `: ping` 注释防代理超时）：

  ```
  event: snapshot
  data: {"system": { … },"services": {"services": [ … ],"total_cpu": 12.3},"history": [ … ]}
  ```

  帧结构固定为 `{ system, services: { services, total_cpu }, history }`；**不含进程明细**（进程树走 REST `/api/admin/services`）。
- 错误：401；单次采集失败不断流，下个 tick 重试
- 副作用：每次 tick 写历史采样；幂等：否（长连接流）；限流：无。客户端断开即停表

### GET /api/admin/services

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/services"`
- 成功 200：

  ```json
  {
    "services": [ { "name": "nginx", "status": "up", "pid": 1234 } ],
    "processes": [
      {
        "name": "systemd", "pid": 1, "ppid": 0,
        "mem_mb": 15, "cpu": 0.1,
        "subtree_mem_mb": 7845, "subtree_cpu": 12.3, "subtree_count": 300,
        "cgroup_mem_mb": null, "thread_count": 1
      }
    ],
    "count": 344,
    "truncated": false,
    "total_mem_mb": 7855,
    "total_cpu": 12.3,
    "parsed_count": 344,
    "skipped_count": 0,
    "total_cgroup_mb": 11459,
    "total_threads": 1401
  }
  ```

  顶层字段：

  | 字段 | 类型 | 语义 |
  |---|---|---|
  | `count` | number | `ps` 解析到的进程总数，**截断前** |
  | `truncated` | boolean | 是否因超过硬上限被截断（默认上限 2000，可用 `ADMIN_PROC_MAX_ROWS` 覆盖） |
  | `total_mem_mb` | number | 全部进程 RSS 之和（截断前口径）；含共享页重复计算 |
  | `total_cpu` | number | 系统总占用（不是列表之和） |
  | `parsed_count` | number | `ps` 输出成功解析的行数 |
  | `skipped_count` | number | 解析失败被丢弃的行数（`>0` 前端应提示） |
  | `total_cgroup_mb` | number \| null | `system.slice` 的 cgroup 真值；取不到为 `null` |
  | `total_threads` | number | 全部进程线程数之和（截断前口径）；与 `count` 为两个口径，**不得相加/混算** |

  单行字段：`name` / `pid` / `ppid` / `mem_mb`（自身 RSS）/ `cpu` / `subtree_mem_mb` / `subtree_cpu` / `subtree_count` / `cgroup_mem_mb`（仅 systemd 单位主进程行有值，否则 `null`）/ `thread_count`（读 `/proc/<pid>/task` 条目数，取不到为 `null`）。

- 进程 `cpu` 与顶层 `total_cpu` 同源于系统采样器的一个窗口：`Δ(utime+stime) ÷ Δtotal × 100`，不做 EMA；`total_cpu` 取自 `/proc/stat` 聚合行，等于同一窗口的 `cpu.usage_percent`。采样器尚未产出时 `cpu` 回退为 `ps` 的 `pcpu`；产出后新进程首帧不在快照里按 `0` 显示，下一 tick 起进入同一窗口。

- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 |
  | 500 | `{ "error": "获取服务状态失败: …" }` |

- 副作用：单次 `/proc` 全量遍历 + `ps`/`systemctl`/`docker` 只读调用；不写库。幂等：是。限流：无。**cgroup 取值优先 `/sys/fs/cgroup/system.slice/<unit>/memory.current`，回退 `systemctl show <unit> -p MemoryCurrent --value`，取不到为 `null`（绝不猜）。**

## 五、认证与账号

### GET /api/admin/auth-mode

- 鉴权：无
- 请求参数：无
- curl：`curl -sS "$BASE_URL/api/admin/auth-mode"`
- 成功 200：`{ "authMode": "builtin" }`（或 `"sso"`）
- 错误：无；副作用：无；幂等：是；限流：无

### POST /api/admin/login

- 鉴权：无（仅 `builtin`；`sso` → `404`）
- 请求参数（body，JSON）：

  | 名称 | 类型 | 必填 | 校验 |
  |---|---|---|---|
  | `code` | string | 是 | TOTP 动态验证码（30 秒一变，±1 步容忍） |

- curl：`curl -sS -X POST -H 'Content-Type: application/json' -d '{"code":"123456"}' "$BASE_URL/api/admin/login"`
- 成功 200：`{ "token": "<64 位 hex>" }`，并下发 `Set-Cookie: admin_session=<token>; Path=/; HttpOnly; SameSite=Lax`（HTTPS 下加 `Secure`）
- 错误码：

  | 状态码 | 语义 | 附加字段 |
  |---|---|---|
  | 403 | TOTP 未配置，需先设置 | `code: "totp_setup_required"` |
  | 401 | 验证码错误 | — |
  | 429 | 同 IP 连续失败 5 次锁定 | `code: "rate_limited"`、`retryAfter`（秒） |
  | 500 | `{ "error": "会话服务异常" }` | — |

- 副作用：写 Redis 会话、写审计；幂等：否；限流：**是**（同 IP 连续失败 5 次锁 60 秒）

### POST /api/admin/logout

- 鉴权：无（仅 `builtin`；`sso` → `404`）
- 请求参数：无（会话 token 取 `Authorization: Bearer` 或 cookie）
- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/logout"`
- 成功 200：`{ "ok": true }`（未登录也 200，幂等）；并清 cookie
- 错误：无（Redis 异常也不阻断，cookie 照清）
- 副作用：删 Redis 会话；幂等：**是**；限流：无

### GET /api/admin/me

- 鉴权：`authRequired`（仅 `builtin`；`sso` → `404`）
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/me"`
- 成功 200：`{ "name": "admin", "role": "admin" }`
- 错误：401 未登录
- 副作用：无；幂等：是；限流：无

### POST /api/admin/totp/setup

- 鉴权：无（仅 `builtin`；`sso` → `404`）。仅当 TOTP 尚未配置时可用。
- 请求参数：无
- curl：`curl -sS -X POST "$BASE_URL/api/admin/totp/setup"`
- 成功 200：`{ "secret": "<base32>", "otpauthUri": "otpauth://totp/…" }`
- 错误码：409 已配置（`{ "code": "totp_already_setup", "message": "TOTP already configured" }`）；500 生成/保存失败
- 副作用：写 TOTP secret 文件（0600，不入库）；幂等：否（已配置即 409）；限流：无

### POST /api/admin/totp/login

- 鉴权：无。`totp-auth` 模块的登录入口：校验 TOTP 后签发 HS256 JWT。
- 请求参数（body，JSON）：`{ "code": "<动态码>" }`
- curl：`curl -sS -X POST -H 'Content-Type: application/json' -d '{"code":"123456"}' "$BASE_URL/api/admin/totp/login"`
- 成功 200：`{ "token": "<jwt>", "expiresIn": "12h" }`
- 错误码：

  | 状态码 | 语义 | 附加字段 |
  |---|---|---|
  | 403 | TOTP 未配置 | `code: "totp_setup_required"` |
  | 401 | 验证码错误 | `code: "invalid_code"` |
  | 429 | 分档限速 | `code: "rate_limited"`、`retryAfter`（秒） |
  | 500 | 签发失败 | — |

- 副作用：无（凭 TOTP 换 JWT）；幂等：否；限流：**是**（连续 5 次失败后依次锁 60s / 300s / 900s）

### POST /api/admin/totp/reset

- 鉴权：`authRequired`
- 请求参数：无（身份取 `X-Auth-User` / 会话，转发时带内部令牌）
- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/totp/reset"`
- 成功：透传认证中心 `/api/internal/totp/reset?sub=<用户名>` 的响应（两阶段①：只写 pending，不覆盖正式 secret）
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 缺少身份 |
  | 500 | 内部令牌文件缺失/为空 → `{ "error": "服务未正确配置" }` |
  | 502 | 认证中心地址未配置 / 不可达 → `{ "error": "认证中心不可达" }` |

- 副作用：调用认证中心内部接口；幂等：否；限流：无（由认证中心侧决定）

### POST /api/admin/totp/confirm

- 鉴权：`authRequired`
- 请求参数：body `{ "code": "<动态码>" }`（透传认证中心）
- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"code":"123456"}' "$BASE_URL/api/admin/totp/confirm"`
- 成功：透传认证中心 `/api/internal/totp/confirm?sub=<用户名>`（两阶段②：pending 验证通过才转正）
- 错误：同 `/api/admin/totp/reset`（401 / 500 / 502，及认证中心返回的 4xx）
- 副作用：调用认证中心内部接口；幂等：否；限流：无

### ALL /api/admin/sessions*

- 鉴权：`authRequired`（挂载点，转发认证中心内部接口 `/api/internal/sessions*`）
- 子路径与语义（透传，`?sub=<用户名>` 由本服务补齐）：

  | 方法 | 路径 | 语义 |
  |---|---|---|
  | GET | `/api/admin/sessions` | 登录设备会话列表，`{ "sessions": [ … ] }` |
  | PUT | `/api/admin/sessions/:id/name` | body `{ "deviceName": "<名称>" }` 重命名会话 |
  | DELETE | `/api/admin/sessions/:id` | 踢下线 |

- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/sessions"`
- 错误码：401 未登录；500 内部令牌不可用；502 认证中心地址未配置/不可达；其余状态透传
- 副作用：调用认证中心内部接口；幂等：GET 是、DELETE 是、PUT 是；限流：无
- 备注：转发只带 `?sub=`，**不转发客户端凭证**；内部令牌读 `AUTH_CENTER_INTERNAL_TOKEN_FILE`，只读、不打印、不返回。

### POST /api/admin/password

- 鉴权：`authRequired`
- 请求参数（body）：

  | 名称 | 类型 | 必填 | 校验 |
  |---|---|---|---|
  | `old_password` | string | 是 | 必须与当前口令匹配 |
  | `new_password` | string | 是 | 长度 ≥ 6 |

- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"old_password":"…","new_password":"…"}' "$BASE_URL/api/admin/password"`
- 成功 200：`{ "ok": true, "msg": "密码已修改，请重新登录" }`（同步删除全部会话，所有已登录端失效）
- 错误码：400 旧密码错误 / 新密码不足 6 位；500 `{ "error": "会话服务异常" }`
- 副作用：写 `config.json`、删全部 Redis 会话、写审计；幂等：否；限流：无

## 六、接口令牌

### GET /api/admin/api-tokens

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/api-tokens"`
- 成功 200：`{ "tokens": [ { "id": "<sha256>", "name": "…", "note": "", "createdAt": <ms>, "expiresAt": <ms>, "lastUsedAt": <ms>, "canWrite": false } ] }`（**绝不含明文**，按 `createdAt` 倒序）
- 错误：401；500 `{ "error": "获取接口令牌失败" }`
- 副作用：写审计；幂等：是；限流：无

### POST /api/admin/api-tokens

- 鉴权：`authRequired`
- 请求参数（body）：

  | 名称 | 类型 | 必填 | 默认 | 校验 |
  |---|---|---|---|---|
  | `name` | string | 是 | — | 非空 |
  | `note` | string | 否 | `""` | — |
  | `expiresInDays` | number | 否 | `30` | 1~365 的整数 |
  | `canWrite` | boolean | 否 | `false` | 是否标记为可写令牌（保留位，当前无端点消费） |

- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"name":"ci","expiresInDays":30,"canWrite":true}' "$BASE_URL/api/admin/api-tokens"`
- 成功 200：`{ "id": "<sha256>", "token": "<64 位 hex，仅此一次>", "meta": { … } }`
- 错误码：400 名称空 / 有效期非 1~365 整数；500 `{ "error": "生成接口令牌失败" }`
- 副作用：写 Redis（固定过期，不滑动）、写审计；幂等：否；限流：无

### PATCH /api/admin/api-tokens/:id

- 鉴权：`authRequired`
- 请求参数：path `id`（string，sha256）；body 任选 `{ name?, note?, canWrite?, expiresInDays? }`（`expiresInDays` 需 1~365 整数，重设即刷新 TTL）
- curl：`curl -sS -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"note":"新备注"}' "$BASE_URL/api/admin/api-tokens/$ID"`
- 成功 200：更新后的令牌元数据（无明文）
- 错误码：400 名称空 / 有效期非法；404 令牌不存在或已过期；500 `{ "error": "更新接口令牌失败" }`
- 副作用：写 Redis、写审计；幂等：是（同入参结果一致）；限流：无

### DELETE /api/admin/api-tokens/:id

- 鉴权：`authRequired`
- 请求参数：path `id`（string）
- curl：`curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/api-tokens/$ID"`
- 成功 200：`{ "ok": true }`（立即失效）
- 错误：401；500 `{ "error": "吊销接口令牌失败" }`
- 副作用：删 Redis key、写审计；幂等：**是**；限流：无

## 七、公开应用中心

### GET /api/public/apps

- 鉴权：无
- 请求参数：`refresh`（query，string，否）—— `1`/`true` 请求刷新，但**按来源 IP 每 10s 至多一次真正刷新**，超出按缓存读
- curl：`curl -sS "$BASE_URL/api/public/apps"`
- 成功 200：`{ "categories": [ { "id": "…", "name": "…" } ], "apps": [ { "id": "…", "name": "…", "status": "up" } ] }`（字段白名单：`id/name/category/desc/url/icon/status/latencyMs/onDemand`；登记表路径、mtime、notice、warning 与未登记端口发现一律不返回）；响应头 `Cache-Control: no-store`
- 错误：503 `{ "error": "应用数据暂不可用" }`（不返回栈信息）
- 副作用：探活只读（TCP/systemd/docker）、写审计（`public_apps_list`，不记 url）；幂等：是；限流：**是**（刷新窗口 10s / IP）

## 八、Hermes 历史（只读）

### GET /api/admin/history

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/history"`
- 成功 200：`{ "sessions": [ { "id": "…", "title": "…", "time": <epoch ms|null>, "message_count": 3 } ] }`（按最后活动倒序）
- 错误码：503 `{ "error": "历史记录暂不可用" }`（数据库不可用）；500 `{ "error": "获取历史会话失败" }`
- 副作用：只读打开 Hermes `state.db`（请求内 open→query→close，不长连接持有）；幂等：是；限流：无

### GET /api/admin/history/:id

- 鉴权：`authRequired`
- 请求参数：path `id`（string，会话 id）
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/history/$ID"`
- 成功 200：`{ "session": { "id": "…", "title": "…" }, "messages": [ { "role": "user|assistant", "content": "…", "ts": <epoch ms|null> } ] }`
- 错误码：404 `{ "error": "会话不存在" }`；503 数据库不可用；500 `{ "error": "获取会话消息失败" }`
- 副作用：只读；幂等：是；限流：无

## 九、Web 终端

### POST /api/admin/term/unlock

- 鉴权：`authRequired`
- 请求参数（body）：`{ "password": "<终端口令>" }`
- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"password":"…"}' "$BASE_URL/api/admin/term/unlock"`
- 成功 200：`{ "ticket": "<64 位 hex>", "expiresIn": 43200 }`
- 错误码：401 口令不正确；429 同 IP 连续失败 5 次锁 10 分钟（附 `retryAfter` 秒）；500 `{ "error": "票据存储失败: …" }`
- 副作用：写 Redis 票据（12 小时）、写审计；幂等：否；限流：**是**（5 次失败锁 10 分钟 / IP）
- 开关：`ADMIN_TERM_UNLOCK=off` 时跳过口令校验与失败计数（不再有 401/429），直接签发同样结构的票据；`authRequired` 不变，管理端鉴权仍是必经的第一道门

### GET /api/admin/term/mode

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/term/mode"`
- 成功 200：`{ "unlockRequired": true }`（`ADMIN_TERM_UNLOCK=off` 时为 `false`）
- 错误码：401 未登录
- 副作用：无（只读启动配置快照）；幂等：是；限流：无

### GET /api/admin/term/verify

- 鉴权：**仅本机**（来源 IP 必须为 `127.0.0.1`/`::1`/`::ffff:127.0.0.1`；否则 403）
- 请求参数：query `ticket`（string，必填）
- curl：`curl -sS "$BASE_URL/api/admin/term/verify?ticket=<ticket>"`
- 成功 200：`{ "ok": true }`
- 错误码：400 缺 ticket（`{ "ok": false }`）；401 票据无效/过期；403 非本机；500 Redis 异常
- 副作用：无（只读 Redis）；幂等：是；限流：无

### GET /api/admin/term/sessions

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/term/sessions"`
- 成功 200：`{ "sessions": [ { "name": "term-abc", "attached": false, "activity": <epoch 秒> } ] }`（仅 `term-*` 白名单会话）
- 错误：500 `{ "error": "读取终端会话失败: …" }`
- 副作用：执行 `tmux list-sessions`；幂等：是；限流：无

### POST /api/admin/term/sessions/close

- 鉴权：`authRequired`
- 请求参数（body）：`{ "names": ["term-abc", …] }`（最多前 32 个；非白名单名称忽略）
- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"names":["term-abc"]}' "$BASE_URL/api/admin/term/sessions/close"`
- 成功 200：`{ "ok": true, "killed": ["term-abc"] }`
- 错误：无（单个失败不影响整体）
- 副作用：`tmux kill-session`、写审计；幂等：是；限流：无

### DELETE /api/admin/term/sessions/:name

- 鉴权：`authRequired`
- 请求参数：path `name`（string，须匹配 `^term-[a-z0-9][a-z0-9-]{0,31}$`）
- curl：`curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/term/sessions/term-abc"`
- 成功 200：`{ "ok": true }`
- 错误码：400 非法会话名；500 `{ "error": "关闭终端会话失败: …" }`
- 副作用：`tmux kill-session`、写审计；幂等：是；限流：无

## 十、SSH 公钥白名单

公钥以 MySQL 表 `ssh_key` 为唯一事实源（与指标表同库）。sshd 通过 `AuthorizedKeysCommand` 调用独立命令
`/usr/local/bin/ssh-keys-from-admin %u %f` 实时查询，因此**停用 / 删除在下一次登录时立即生效**；
已建立的 SSH 连接不受影响。只有 `SSH_KEYS_USER`（默认 `root`）一个登录用户，最多保存 20 把公钥。

字段口径：

| 字段 | 类型 | 语义 |
|---|---|---|
| `id` | string | 自增主键的十进制字符串 |
| `title` | string | 管理页标题，1–100 字符 |
| `fingerprint` | string | GitHub 风格 `SHA256:` + 无填充 base64，与 `ssh-keygen -lf` 的指纹列一致 |
| `keyType` | string | `ssh-ed25519` / `ssh-rsa` / `ecdsa-sha2-nistp*` / `sk-*@openssh.com` |
| `comment` | string | 公钥行尾注释（可为空，≤200 字符） |
| `enabled` | boolean | 停用后辅助命令不再输出该公钥 |
| `createdAt` | number | epoch 毫秒 |
| `lastUsedAt` | number \| null | 最近一次通过该公钥尝试登录的时间（epoch 毫秒）；从未使用为 `null` |

校验（服务端强制，前端校验只是体验）：

- 只接受 `type base64 [comment]` 单行纯公钥；`command=` / `environment=` / `permitopen=` 等前置选项一律 400。
- base64 必须可解码，且解码后的 blob 算法名、字段长度须与声明类型自洽（不接受证书与未知算法）。
- 同一指纹唯一（409）；整行 ≤8KB；标题 ≤100 字符；注释 ≤200 字符；最多 20 把（400）。

### GET /api/admin/ssh-keys

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/ssh-keys"`
- 成功 200：`{ "keys": [ { "id": "1", "title": "办公笔记本", "fingerprint": "SHA256:…", "keyType": "ssh-ed25519", "comment": "me@example.com", "enabled": true, "createdAt": 1760000000000, "lastUsedAt": null } ] }`（按 `id` 倒序，不含公钥原文）
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |
  | 503 | `{ "error": "SSH 公钥库不可用" }`（指标库未连上） |
  | 500 | `{ "error": "获取 SSH 公钥失败" }` |

- 副作用：写审计（`ssh_keys_list`，只记数量）；幂等：是；限流：无

### POST /api/admin/ssh-keys

- 鉴权：`authRequired`
- 请求参数（body）：

  | 名称 | 类型 | 必填 | 默认 | 校验 |
  |---|---|---|---|---|
  | `title` | string | 是 | — | 去首尾空白后非空，≤100 字符 |
  | `public_key` | string | 是 | — | `type base64 [comment]` 单行纯公钥；整行 ≤8KB；不接受前置选项 |

- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"title":"办公笔记本","public_key":"ssh-ed25519 AAAA… me@example.com"}' "$BASE_URL/api/admin/ssh-keys"`
- 成功 201：单个公钥视图（字段同列表项）
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 400 | 标题空 / 超长；公钥格式非法、带前置选项、超长；已达 20 把上限 |
  | 409 | 同一指纹已存在（`{ "error": "该公钥已存在" }`） |
  | 401 / 503 / 500 | 同列表接口 |

- 副作用：写 `ssh_key`、写审计（只记 id 与指纹，不记公钥内容）；幂等：否（重复提交返回 409）；限流：无

### PATCH /api/admin/ssh-keys/:id

- 鉴权：`authRequired`
- 请求参数：path `id`（string，十进制正整数）；body 至少包含 `title` / `enabled` 之一：

  | 名称 | 类型 | 必填 | 默认 | 校验 |
  |---|---|---|---|---|
  | `title` | string | 否 | 保持原值 | 去首尾空白后非空，≤100 字符 |
  | `enabled` | boolean | 否 | 保持原值 | 必须是布尔值；`false` 即停用 |

- curl：`curl -sS -X PATCH -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"enabled":false}' "$BASE_URL/api/admin/ssh-keys/1"`
- 成功 200：更新后的公钥视图
- 错误码：400 ID 非法 / 无可更新字段 / 字段类型或长度非法；404 公钥不存在；401 / 503 / 500
- 副作用：写 `ssh_key`、写审计（`ssh_keys_update`）；幂等：是（同入参结果一致）；限流：无

### DELETE /api/admin/ssh-keys/:id

- 鉴权：`authRequired`
- 请求参数：path `id`（string，十进制正整数）
- curl：`curl -sS -X DELETE -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/ssh-keys/1"`
- 成功 200：`{ "ok": true }`
- 错误码：400 ID 非法；404 公钥不存在；401 / 503 / 500
- 副作用：删除 `ssh_key` 行（辅助命令下一次查询即不再返回该公钥）、写审计；幂等：**否**（重复删除 404）；限流：无

> 部署侧：辅助命令的安装、只读数据库账号、`AuthorizedKeysCommand` 切换与回滚见
> `deploy/ssh/README.md`。辅助程序把任何异常都转成「stdout 为空 + 退出码 0」。

## 十一、工具面板

工具清单由 `GET /api/admin/tools` 数据驱动；当前只有 Bitwarden 解锁一项。接口只负责把主密码
换成会话 token 落到服务器文件，**不记录请求体、不写审计、不在日志里输出密码或上游响应**。

服务端环境变量（写入 `/etc/admin-server-go.env`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `BITWARDEN_SERVER` | 空（必填） | 身份服务基址，如 `https://vault.example.com`；代码会拼 `/identity/connect/token` |
| `BITWARDEN_EMAIL` | 空（必填） | Bitwarden 账号邮箱 |
| `BITWARDEN_DEVICE_ID` | 空（必填） | 安装时生成一次并固定的设备标识 |
| `BITWARDEN_SESSION_FILE` | `$HOME/.bw_session` | token 落盘路径；写入时权限固定 0600、原子替换 |

前三项缺任一即「未配置」：清单里 `configured=false`，除清单外的三个接口返回 `501`，不影响其它工具。

字段口径：

| 字段 | 类型 | 语义 |
|---|---|---|
| `configured` | boolean | 三项必填环境变量是否齐全 |
| `unlocked` | boolean | 会话文件存在、非空且 token 未过期（非 JWT 按已解锁处理） |
| `expiresAt` | number \| null | token JWT 的 `exp`（epoch 毫秒）；非 JWT / 未解锁为 `null` |
| `updatedAt` | number \| null | 会话文件最后修改时间（epoch 毫秒）；文件不存在为 `null` |
| `expiresInSeconds` | number \| null | 距过期的剩余秒数；已过期为 `0`，非 JWT 为 `null` |

### GET /api/admin/tools

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/tools"`
- 成功 200：`[ { "id": "bitwarden", "name": "Bitwarden 解锁", "configured": true, "unlocked": false, "expiresAt": null } ]`
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |

- 副作用：无（只读会话文件元信息）；幂等：是；限流：无

### GET /api/admin/tools/bitwarden/status

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/tools/bitwarden/status"`
- 成功 200：`{ "configured": true, "unlocked": true, "updatedAt": 1760000000000, "expiresInSeconds": 13320 }`
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |
  | 501 | `{ "error": "未配置 BITWARDEN_*" }` |

- 副作用：无；幂等：是；限流：无。**响应绝不含 token 或其片段。**

### POST /api/admin/tools/bitwarden/unlock

- 鉴权：`authRequired`
- 请求参数（body）：

  | 名称 | 类型 | 必填 | 默认 | 校验 |
  |---|---|---|---|---|
  | `password` | string | 是 | — | 非空；只用于本次请求内换 token，不落盘、不记日志 |

- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -d '{"password":"<主密码>"}' "$BASE_URL/api/admin/tools/bitwarden/unlock"`
- 成功 200：`{ "ok": true, "expiresInSeconds": 3600 }`（非 JWT 时为 `null`）；同时原子写入 `BITWARDEN_SESSION_FILE`（0600）
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | `{ "ok": false, "error": "密码错误" }`——账号不存在 / 密码错误 / 网络错误 / 上游 5xx **一律同一响应**，防枚举 |
  | 429 | `{ "ok": false, "error": "尝试过多，请稍后再试", "retryAfter": 42 }`——同 IP 每分钟最多 5 次 |
  | 501 | `{ "error": "未配置 BITWARDEN_*" }` |

- 副作用：写 / 覆盖会话文件（临时文件 + rename，权限 0600）；**不写审计、不记录请求体**；幂等：是（重复解锁只会覆盖 token）；限流：同 IP 每分钟 5 次（进程内计数，成功与失败都计入）

### POST /api/admin/tools/bitwarden/lock

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -X POST -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/tools/bitwarden/lock"`
- 成功 200：`{ "ok": true }`（文件本就不存在也算成功）
- 错误码：

  | 状态码 | 语义 |
  |---|---|
  | 401 | 未登录 / 会话失效 |
  | 500 | `{ "ok": false, "error": "清除会话失败" }` |
  | 501 | `{ "error": "未配置 BITWARDEN_*" }` |

- 副作用：删除会话文件（删除即失效）；幂等：是；限流：无

## 十二、错误码表

| 状态码 | 通用语义 |
|---|---|
| 400 | 请求参数非法（具体文案见各端点） |
| 401 | 未登录 / 凭证无效 / 会话失效 |
| 403 | 已登录但无权（只读令牌写入、非本机访问） |
| 404 | 资源不存在（或 `sso` 模式下 `builtin` 专属入口） |
| 409 | 冲突（同名已存在 / TOTP 已配置） |
| 413 | 请求体超限 |
| 429 | 触发限流（附 `retryAfter` 秒） |
| 500 | 服务器内部错误（含请求体解析失败） |
| 501 | 功能未配置（如 Bitwarden 环境变量缺失） |
| 502 | 认证中心地址未配置 / 不可达 |
| 503 | 依赖不可用（应用数据 / 历史库） |

### GET /api/admin/logs

- 鉴权：`authRequired`
- 数据源：云 MySQL 日志库（`DB_LOGS_NAME`，默认 `admin_logs`，由 loghub 写入；本接口只读）
- 请求参数：
  - `source`：`system`（journald，默认）| `container`（docker）| `file`（业务文件）| `access`（nginx access）| `error`（nginx error）
  - `q`：关键字，匹配 message 与该来源的辅助列（unit/ident、container/image、source_id/path、path/method）
  - `since`：相对时长，如 `30m` / `24h` / `7d`
  - `limit`：默认 200，上限 1000；`offset`：默认 0
- 成功 200：

```json
{
  "entries": [
    {
      "ts": "2026-10-20T16:21:25.123Z",
      "source": "system",
      "name": "nginx.service",
      "level": "info",
      "message": "...",
      "host": "web-1",
      "unit": "nginx.service",
      "process": "nginx",
      "pid": "1234"
    }
  ],
  "hasMore": true,
  "source": "system",
  "limit": 200,
  "offset": 0
}
```

- 详情字段按来源可选，均为 `omitempty`（没有该列时不出现）：`host`（journald 主机名）、`unit`（journald 单元）、`container`（容器名）、`file_path`（业务文件路径）、`process`（journald `ident`）、`pid`、`method` / `path` / `status` / `duration`（秒，字符串）/ `client_ip`（仅 nginx access）
- `level` 已归一：journald 的 priority 映射为 error/warn/notice/info/debug；access 的 status 映射为 5xx=error、4xx=warn、其余 info
- 库未配置或连不上时返回 503；参数非法返回 400

### GET /api/admin/anomalies

- 鉴权：`authRequired`
- 说明：返回基线异常事件（各指标偏离自身历史 > 4σ 时记录），不是阈值告警
- 请求参数：`limit`（默认 100，上限 500）
- 成功 200：

```json
{ "items": [ { "ts": "2026-10-21T02:30:00Z", "metric": "cpu_percent", "value": 61.2, "mean": 12.4, "stddev": 8.1, "z": 6.0 } ] }
```

- 未启用检测时返回 `{"items": []}`

### GET /api/admin/alerts/rules

- 鉴权：`authRequired`；返回全部规则 `{"items":[{id,name,metric,op,threshold,durationSec,severity,enabled,cooldownSec}]}`

### POST /api/admin/alerts/rules

- 鉴权：`authRequired`；body `{"name","metric","op","threshold","durationSec","severity","cooldownSec"}`；返回创建的规则
- metric 取值：`cpu_percent` / `mem_percent` / `disk_percent` / `stall_score` / `run_queue` / `psi_cpu` / `psi_io` / `psi_mem`；op 取值：`gt` / `gte` / `lt` / `lte`

### PATCH /api/admin/alerts/rules/:id

- 鉴权：`authRequired`；body `{"enabled": true|false}`；返回 `{"ok":true}`

### DELETE /api/admin/alerts/rules/:id

- 鉴权：`authRequired`；返回 `{"ok":true}`

### GET /api/admin/alerts/events

- 鉴权：`authRequired`；参数 `limit`（默认 100）；返回 `{"items":[{ruleName,severity,value,threshold,startedAt,endedAt,notified}]}`
- 判定每分钟一次，连续满足 `durationSec` 才触发，`cooldownSec` 内不重复推送

### POST /api/admin/ops

- 鉴权：`authRequired`
- 用途：受控运维动作——服务启停、容器重启
- body：`{"kind":"service|container","name":"<单元名或容器名>","action":"start|stop|restart","confirm":"<必须原样等于 name>"}`
- 成功 200：`{"ok":true,"kind":"service","name":"x.service","action":"restart","output":"...","elapsedMs":123}`
- 失败 400：`{"ok":false,"error":"...","result":{...}}`
- 安全约束：`confirm` 必须等于 `name`；`ssh/sshd/networking/systemd-*` 等服务在拒绝名单；全局 3 秒限速；命令 30 秒超时；每个动作写审计 `/data/admin-server-go/audit.log`

## 五、站点 / 证书 / 备份（只读）

三条接口都只读：不写 nginx 配置、不触发 certbot、不清理备份。每条接口内部 10 秒超时，
结果进程内缓存 60 秒（不同接口各自缓存）；缓存期内重复请求不会再次 fork 外部命令。
路径可用环境变量覆盖：`SITES_NGINX_BIN`（默认 `nginx`）、`SITES_CERT_DIR`（默认
`/etc/letsencrypt/live`）、`SITES_BACKUP_DIR`（默认 `/data/backups`）。

### GET /api/admin/sites/nginx

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/sites/nginx"`
- 数据来源：`nginx -T` 展开后的配置；每个 `server {}` 块一行，正则形式的 `server_name`
  （以 `~` 开头）排在最后。
- 成功 200：

```json
{
  "sites": [
    {
      "serverName": "admin.example.com",
      "listen": ["443 ssl http2", "80"],
      "root": "/var/www/admin",
      "proxyPass": "http://127.0.0.1:18920",
      "certPath": "/etc/letsencrypt/live/admin.example.com/fullchain.pem",
      "configFile": "/etc/nginx/conf.d/admin.conf",
      "redirect": null,
      "isRegex": false
    }
  ]
}
```

- 字段：`root` / `proxyPass` / `certPath` / `redirect` 缺失时为 `null`；`listen` 始终是数组；
  `configFile` 取自 `nginx -T` 输出里的 `# configuration file <path>:` 标记。
- 失败 200：`nginx -T` 失败或未解析出 server 块时返回 `{"sites":[],"error":"…"}`（不返回 5xx）。
- 副作用：无；只执行 `nginx -T`（不会 reload）。
- 幂等：幂等；无显式限流。

### GET /api/admin/sites/certs

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/sites/certs"`
- 数据来源：`<SITES_CERT_DIR>/*/fullchain.pem`，逐张执行
  `openssl x509 -noout -subject -enddate -issuer`；`autoRenew` 取 `systemctl is-active certbot.timer`
  （所有条目共用一次探测）。
- 成功 200：

```json
{
  "certs": [
    {
      "domain": "admin.example.com",
      "notAfter": "2027-01-19T12:00:00Z",
      "daysLeft": 90,
      "issuer": "Let's Encrypt",
      "autoRenew": true
    }
  ]
}
```

- `daysLeft`：`notAfter - 当前时间` 的完整天数（向下取整）；已过期为负数。前端分级：
  `> 21` 绿、`7–21` 橙、`< 7` 红。
- `issuer`：优先取签发者组织名（`O=`），缺失时退回 CN。
- 单张证书损坏时该条带 `error`（`notAfter` / `daysLeft` 为 `null`），不影响其它条目；
  证书目录不可读时返回 `{"certs":[],"error":"…"}`。
- 副作用：无；只读证书文件与 systemd 状态。
- 幂等：幂等；无显式限流。

### GET /api/admin/sites/backups

- 鉴权：`authRequired`
- 请求参数：无
- curl：`curl -sS -H "Authorization: Bearer $TOKEN" "$BASE_URL/api/admin/sites/backups"`
- 数据来源：`<SITES_BACKUP_DIR>` 下的一级目录 + 名字含 `backup` 的 systemd timer。
  目录大小用**一次** `du -sb -- <全部目录>` 批量计算；`systemctl list-units` 一次、
  `systemctl show -p Id,NextElapseUSecRealtime,LastTriggerUSec` 一次。
- 成功 200：

```json
{
  "backups": [
    { "name": "backup-2026-10-21", "size": 1073741824, "modifiedAt": 1792544400000 }
  ],
  "timers": [
    {
      "name": "daily-full-backup.timer",
      "nextElapseUSecRealtime": "Thu 2026-10-22 04:02:53 HKT",
      "lastTriggerUSec": "Wed 2026-10-21 04:03:31 HKT"
    }
  ],
  "total": 47,
  "totalSize": 6499815374,
  "latestModifiedAt": 1792544400000
}
```

- `backups` 按 `modifiedAt` 倒序，最多 50 条；`total` / `totalSize` / `latestModifiedAt`
  统计截断前的全部一级目录，供前端摘要使用。
- `size` 为字节（`du -sb` 口径）；`modifiedAt` 为 epoch 毫秒。
- `timers[].nextElapseUSecRealtime` / `lastTriggerUSec` 保留 `systemctl show` 的原始文本。
- 子命令失败只影响对应字段：目录读取或 `du` 失败时 `backups` 为空并带 `error`，
  timer 仍尽量返回；timer 采集失败不影响目录列表。
- 副作用：无；只执行只读命令与目录遍历。
- 幂等：幂等；无显式限流。
