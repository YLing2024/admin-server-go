# 需求：admin「系统」页新增「系统信息」区块

日期：2026-10-20　用户原话：「Admin 的监控里面加一个呢，系统信息这种。」

## 目标

在 admin 的「系统」页（现有 CPU / 内存 / Swap / PSI / 磁盘 / 网络 趋势图之外）新增一块
**「系统信息」**，回答“这台机器现在是什么状态、在跑什么”，而不是只给曲线。

不改动现有趋势图与其它页面。

## 一、后端（admin-server-go）

新增只读接口：

```text
GET /api/admin/system/info        → 静态信息（缓存 60s 即可）
GET /api/admin/system/processes   → Top 进程（实时，不缓存）
```

`system/info` 返回（字段名用 json 小驼峰）：

| 字段 | 内容 | 数据来源 |
|---|---|---|
| `os` / `kernel` / `arch` | 发行版、内核版本、架构 | `/etc/os-release`、`/proc/sys/kernel/osrelease`、`runtime.GOARCH` |
| `hostname` | 主机名 | `os.Hostname()` |
| `bootTime` / `uptimeSeconds` | 启动时间、运行时长 | `/proc/stat` 的 `btime` + 当前时间 |
| `load` | 1/5/15 分钟负载 | `/proc/loadavg` |
| `cpuModel` / `cpuCores` | CPU 型号与逻辑核数 | `/proc/cpuinfo` |
| `memory` | total / available / used / cached | `/proc/meminfo` |
| `swap` | total / used | `/proc/meminfo` |
| `disks[]` | 挂载点、文件系统、总量、已用、inode 使用率 | `syscall.Statfs`（跳过 tmpfs/overlay，仅保留真实块设备） |
| `ports` | 监听端口总数 + 按进程分组的前 15 条（端口、进程名、监听地址） | `ss -tlnp`（解析输出，不引第三方库） |
| `systemd` | 失败单元列表（name / description） | `systemctl --failed --no-legend --plain` |
| `logins` | 最近 5 条登录（用户、来源 IP、时间） | `last -n 5 -w` |
| `temperatures` | 可选，有 `/sys/class/thermal/thermal_zone*/temp` 才返回 | sysfs |

`system/processes` 返回 Top 10（按 CPU 与按内存各 10，去重合并）：

```json
{"processes":[{"pid":123,"name":"nginx","cpuPercent":12.3,"memMB":45.2,"command":"..."}]}
```

数据来源：读 `/proc/<pid>/stat` + `/proc/<pid>/status`，两次采样间隔 500ms 计算 CPU 占比。
**不依赖 `ps` 外部命令**（解析更稳），`ss` / `systemctl` / `last` 用 `exec.Command` 调用，
任一失败只影响对应字段（返回空数组 + 该字段 `error` 提示），**不得让整个接口 500**。

约束：

- 只加依赖标准库；新增外部命令调用均为只读
- 接口挂在现有 `authRequired` 之后（与其它 admin 接口一致）
- 不允许阻塞超过 2 秒：`ss` / `systemctl` / `last` 均带 `context.WithTimeout(1500ms)`
- 不要读 `/proc/<pid>/environ`（可能含机密）

## 二、前端（admin-web）

在「系统」页的趋势图**上方**（或下方，按现有布局更顺的一侧）新增一个 **「系统信息」** 卡片区，
Ant Design 默认设计语言，响应式：

```text
┌ 系统信息 ───────────────────────────────┐
│ Ubuntu 24.04 · 内核 6.12.107 · x86_64   │   一行概要（次要文字色）
│ 运行 12 天 4 小时 · 负载 0.42 / 0.51 / 0.63 │
├─────────────┬─────────────┬─────────────┤
│ 内存         │ 磁盘         │ 端口 / 服务  │
│ 7.2 / 16 GB │ / 46% · /data 39% │ 18 个监听 · 2 个失败单元 │
└─────────────┴─────────────┴─────────────┘
```

- 概要行 + 三张小编卡片（内存/磁盘/端口与服务），点击卡片展开明细（Drawer 或 Collapse 均可）
- **Top 进程**用一个小表格（名称 / PID / CPU% / 内存），默认显示 8 行
- **失败单元**用 `Tag` 列表，空时显示「无失败单元」
- **最近登录**用简短列表（用户 · IP · 时间）
- 刷新：静态信息 60 秒、进程 10 秒；页面隐藏时（`document.hidden`）停止轮询
- 移动端：三张卡片纵向堆叠，表格横向滚动，不破版
- 不引新依赖、不用 emoji、不改现有趋势图

## 三、纪律

- 小步中文提交；禁止 push / checkout -b / rebase / reset --hard / merge
- `make check` 全绿；`go vet` 无新增告警
- 前端构建 `--outDir /tmp/...`，绝不写 `/var/www`
- 不改指标采集链路、不动现有 `system/metrics` 与 `system/stream`

## 四、验收

1. `GET /api/admin/system/info` 返回全部字段；`jq` 抽查 os/kernel/load/disks/ports/systemd 非空
2. 断网/无权限场景：临时把 `ss` 改名（或传错误参数）时，接口仍 200，`ports` 为空且有 error 提示
3. `GET /api/admin/system/processes` 返回 Top 进程，CPU 占比之和 ≤ 100×核数
4. 页面：概要行、三张卡片、Top 进程表、失败单元、最近登录全部渲染；移动端不横向溢出
5. 页面隐藏 30 秒后回到前台，不出现重复定时器（Network 面板确认请求频率未翻倍）
6. `make check` 原始输出
7. 与现有趋势图并存，不遮不叠
