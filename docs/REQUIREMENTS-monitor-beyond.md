# 需求（第二阶段）：监控「超越」——不只是准，而是能解释卡顿

日期：2026-10-20
用户：「凭借自己的能力思考，比它好的不管，不如它的借鉴，我要的不是准确，是超越」

前置：第一阶段（固定窗口采样器）已另行委托，本阶段在其之上增量开发。

## 一、我方的判断

**我们已经强于 1Panel（保留，不返工）**

| 能力 | 我们 | 1Panel |
|---|---|---|
| PSI 压力（cpu/io/memory 三档 avg10/60/300） | ✅ 已有 | ❌ 没有，只有 load/CPU% |
| cgroup 内存真值（systemd 单元级） | ✅ 已有 | 仅容器（docker API） |
| 历史入库 MySQL + 多档聚合 | ✅ | SQLite，固定分钟粒度 |
| 进程树 + cgroup 归属 | ✅ | 平铺列表 |

**1Panel 强于我们（借鉴）**

| 点 | 说明 |
|---|---|
| 固定采样窗口 | 第一阶段已抄 |
| `load / cores` 压力指数 | 我们只给裸 load1/5/15，读者不知道 0.7 算高还是低 |
| 磁盘/网络按设备的速率 | 我们有 read_rate/write_rate，需确认口径正确 |

## 二、本阶段要做的五件事（这些是超越点）

### 1. 运行队列

读 `/proc/stat` 的 `procs_running` / `procs_blocked`（本机实测 4 / 0）：

```json
"cpu": { "run_queue": 4, "blocked": 0 }
```

- `run_queue > cores` 说明排队，比 load 更及时
- `blocked > 0` 说明有进程卡在 IO

### 2. CPU 三档窗口

单一窗口无法区分「瞬时抖动」和「持续满载」。用采样器已有的 1 秒 tick 维护滚动缓冲：

```json
"cpu": {
  "usage_percent": 16.4,        // 1 秒窗口（保持现有字段，仍是瞬时）
  "usage_15s": 12.1,            // 15 秒均值
  "usage_60s": 10.8,            // 60 秒均值
  "run_queue": 4,
  "load_percent": 12.5
}
```

- 三个值来自同一个 ticker，不额外读文件
- 缓冲用环形数组，固定长度（15 / 60），不无限增长

### 3. 窗口归因：谁在涨

只给「当前占用 Top」回答不了「刚才为什么卡」。新增窗口内增量榜：

```json
"cpu": {
  "top_movers": [
    {"pid": 1234, "name": "compile", "delta_percent": 62.0, "current_percent": 63.5}
  ]
}
```

- 取最近 15 秒内 `Δ(utime+stime)` 最大的 5 个进程
- 已退出的进程如果贡献明显（窗口内出现后消失，且 Δ 大），也列出来，标 `exited: true`
- 这条是 1Panel 完全没有的：它只存总量，不存归因

### 4. cgroup CPU 归属（补齐内存那块）

已有 `cgroup_mem_mb`，但没有 CPU。读 `/sys/fs/cgroup/<slice>/<unit>/cpu.stat` 的 `usage_usec`，
两个 tick 做差得到单元级 CPU%（只对 Top N 服务取，避免遍历开销）：

```json
"top_services": [
  {"unit": "hermes-gateway.service", "cpu_percent": 12.3, "mem_mb": 550}
]
```

- 与进程维度互补：进程 fork 后退出会被 cgroup 捕获
- 最多取 10 个 CPU 最高的单元

### 5. 合成「卡顿指数」

一眼判断"现在到底卡不卡"，把多个信号压成一个 0–100 的分数：

```text
score = 0.40*min(psi_cpu_avg60,100)
      + 0.30*min(psi_io_avg60,100)
      + 0.20*min(psi_mem_avg60,100)
      + 0.10*min(run_queue/cores*100,100)

< 20  顺畅
20-50 有压力
> 50  明显卡顿
```

- 只在接口里给数值与档位文案，前端不重复计算
- 权重写进常量并注释理由，避免后人拍脑袋改

## 三、约束

- 接口**只新增字段**，不改不删现有字段（前端老版本仍可用）
- 全部来自第一阶段采样器的同一 tick，**请求路径不读 /proc**
- 不引第三方依赖
- 单次采集在 1 秒 tick 内完成，超过 200ms 记 warn

## 四、验收

1. 连续快速调用 10 次，`usage_percent` / `usage_15s` / `usage_60s` 单调关系合理且不跳变（贴 10 组）
2. `run_queue` 与 `grep procs_running /proc/stat` 一致（贴对比）
3. 制造一次 CPU 尖峰（`yes > /dev/null` 5 秒），`top_movers` 第一名是它，`usage_percent` 明显上升，`usage_60s` 变化平缓
4. `top_services` 里能出现 `hermes-gateway.service` 等真实单元，数值与 `systemctl status` 的 CPU 用量量级一致
5. `stall_score` 在空载时 < 20，跑满一个核时明显上升（贴两组）
6. 接口响应 < 50ms
7. `make check` 全绿
