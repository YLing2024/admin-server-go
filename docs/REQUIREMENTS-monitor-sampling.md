# 需求：修正系统监控采样（借鉴 1Panel 的单窗口设计）

日期：2026-10-20
用户：「感觉我的监控不是很准，借鉴一下 1Panel 的实现」

## 一、实测证据（问题是真的）

同一时刻对比（`/proc/stat` 3 秒窗口为真值）：

```text
真值 per-core 均值 9.0%   汇总 9.0%

接口单次（静置 4 秒后）  total=5.5   per_core 均值=8.3     ← 接近真值
接口连续快速调用         total=17.4  per_core 均值=78.3    ← 严重失真
                         total=19.4  per_core 均值=30.1
                         total=7.6   per_core 均值=18.6
                         total=11.5  per_core 均值=21.9
                         total=11.8  per_core 均值=24.6
```

两处硬伤：

1. **采样窗口 = 距上次请求的时间**。`cpuPerCorePrev` / `procCpuSample` 是**共享且每次请求都更新**的，
   连续调用时窗口被压到几十毫秒，Jiffies 量化误差把占用率放大成 78% 这种数字。
2. **两个数字基准不同**：`usage_percent` 由"所有进程 delta 之和 ÷ 总 delta"得到（还带 EMA 平滑），
   `per_core` 由 `cpuN` 行 delta 得到。两者窗口不同源，同一帧里能差一倍以上。

## 二、1Panel 的做法（已读源码 `backend/app/service/monitor.go`）

```go
totalPercent, _ := cpu.Percent(3*time.Second, false)   // 固定 3 秒窗口，阻塞采样
memoryInfo, _ := mem.VirtualMemory()                    // UsedPercent
loadInfo, _ := load.Avg()                               // load1/5/15
itemModel.LoadUsage = loadInfo.Load1 / (float64(cpuCount*2) * 0.75) * 100
// 磁盘/网络 IO 用两次采样的差值 ÷ interval 得到速率
```

要点只有三条，全都值得抄：

1. **固定窗口**：一次采样横跨 3 秒，不依赖"上次请求时间"
2. **单一来源**：总量和每核用同一套快照，必然自洽
3. **不做 EMA**：显示原始窗口值，所以和 `top` 对得上

## 三、改造方案（admin-server-go）

### 1. 独立采样器（核心）

新增 `internal/probe/sampler.go`：

```text
每 1 秒一次 tick（time.Ticker，进程内单实例）：
  读 /proc/stat 的 cpu 行与 cpuN 行 → 与上一次快照做差
  → total_percent 与 per_core[*].usage_percent 同源同窗口
  读一次 /proc/<pid>/stat 全量 → 进程 CPU（同一窗口）
  读 /proc/meminfo、/proc/net/dev、/proc/diskstats → 缓存
HTTP 请求只读缓存，绝不修改采样基准
```

要求：

- **请求路径不再写任何 prev 状态**（这是当前 bug 的根源）
- 采样间隔可配：`PROBE_INTERVAL_SEC`，默认 1；最小 1
- 窗口不足 1 秒的样本直接丢弃，不上报
- ticker 启动失败或读文件失败时，保留上一次值并在日志里 warn（不影响接口 200）

### 2. 计算口径

| 指标 | 口径 |
|---|---|
| `cpu.usage_percent` | `(Δtotal − Δidle) / Δtotal × 100`，**取自 `cpu ` 聚合行** |
| `cpu.per_core[i]` | 同上，取自 `cpuN` 行，**同一对快照** |
| 进程 CPU | `Δ(utime+stime) / Δtotal × 100`，同一窗口 |
| 内存 | `MemTotal − MemAvailable`（现有口径不变，与 1Panel 的 UsedPercent 等价） |
| 负载 | 原始 load1/5/15；**新增** `load_percent = load1 / cores × 100`（1Panel 用 cores×2×0.75 做"压力指数"，我们用 cores 更直观） |

### 3. 去掉 headline 的 EMA

- `cpu.usage_percent` 与 `per_core` **不再平滑**，直接给窗口值
- 进程列表的 CPU 也去 EMA（或 α=1）
- 历史趋势入库时如需平滑，由 metrics 聚合层自己做，不污染实时值

### 4. 保留

- 现有接口形状与字段名全部不变（只新增 `load_percent`）
- SSE 帧结构不变
- 不引第三方依赖（不引入 gopsutil，继续直接读 /proc）

## 四、验收

1. 连续快速调用 10 次，`usage_percent` 与 `per_core` 均值**相互偏差 ≤ 3 个百分点**，
   且与同期 `/proc/stat` 3 秒真值偏差 ≤ 3 个百分点（贴 10 组数据）
2. 静置 10 秒后调用，结果与连续调用同样稳定（窗口不再依赖请求间隔）
3. 进程列表里 CPU 最高的进程，与 `top -bn1` 排序前三一致（贴对比）
4. 新增单测：同一对快照下 total 与 per_core 自洽；窗口 < 1s 的样本被丢弃
5. `make check` 全绿
6. 接口响应时间不因采样变慢（仍 < 50ms）

## 五、纪律

- 小步中文提交、不 push
- 不动其它模块
