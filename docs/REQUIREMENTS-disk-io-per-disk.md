# 需求：磁盘 I/O 按盘分开统计（替代单一合计口径）

> 唯一权威需求书。实现方（admin-server-go + admin-web）以此为准；与本文冲突的旧实现行为一律以本文为准。
> 起因：用户观察到面板磁盘读写长期显示 0。排查结论见 §1——是判据写错导致**数据盘被整块漏掉**，
> 并且用户要求**不再有单独的「磁盘 I/O」栏**，改为**每块盘各自显示自己的读写**。

## 1. 背景与已确认的缺陷（必须一并修掉）

`internal/probe/system.go` 的 `DiskIOSample()` 用「minor == 0」认定整盘：

```go
// 分区 minor > 0：同一物理盘的分区与整盘会重复计数，只保留整盘。
if int(atof(cols[1])) != 0 { continue }
```

该假设只对 sd\*/hd\* 成立。本机是 virtio 盘，**整盘 minor 并不为 0**：

```
254   0 vda   ← 根盘（minor=0，被保留）
254  16 vdb   ← 数据盘（minor=16，被这条判据丢掉）
```

实测（往 /data 写 600MB）：当前口径只测到 806KB，正确口径测到 629MB。也就是说
**数据盘（/data：docker、全部项目、工具链、备份）的读写从来没进过统计**，根盘平时几乎无 I/O，
所以面板上永远是 0。`nvme0n1`、`dm-*`、`md*` 同样被漏掉（命名不匹配 `isWholeDisk`）。

### 1.1 实时值实测对照（同一时间窗，往 /data 持续写 4GB）

| 时刻 | 面板/库里存的写速率 | 同一秒真实 vda | 同一秒真实 vdb |
|---|---|---|---|
| …186 | **0.00 MB/s** | 0.00 MB/s | **634 MB/s** |
| …191 | **0.00 MB/s** | 0.00 MB/s | **660 MB/s** |
| …204 | 1.21 MB/s | 0.87 MB/s | 0.00 MB/s |

窗口峰值：库里最大写入 **1.21 MB/s**，真实 vdb 峰值 **660 MB/s**。可见实时值实际就是**根盘一个盘的
I/O 被当成整机 I/O 展示**——根盘安静时显示 0，根盘一抖就冒出一个与用户操作无关的小数字。
（另外：面板/前端历史上出现过的 ~1GB/s 尖峰，同样出自根盘瞬时突发被误当整机值。）

### 1.2 实时速率还存在「基线被偷」缺陷

`GET /api/admin/system`（REST）与 `GET /api/admin/system/stream`（SSE）**都**走
`Service.CollectSystem`，而 `diskioState` / `netState` 是**请求级共享**的单一基线：
速率 = (本次读数 − 上次读数) / dt，dt 由「上一次调用」决定。因此

- 第二个消费者（手机 App、另一个浏览器标签、任何轮询）一访问，就重置了基线；
- 于是 SSE 侧看到的 dt 变短、数值忽大忽小甚至偏小——**实时 IO「有点毛病」的另一半原因**（网速同款）。

修复方向见 §3.4：速率必须由**唯一后台采样线程**算好后供所有消费者读取，而不是每次请求各自做差。

## 2. 目标（用户原话拆解）

1. 「你这不是两个盘吗？你分开统计」→ 每块盘各自的读/写，不再只有一个合计值。
2. 「你不要单独去写一个磁盘 I/O 的栏」→ 面板上删除独立的「磁盘 I/O」卡片。
3. 「把磁盘 I/O 放到……两个磁盘的下面」→ 读/写数据挂在**对应磁盘自己的卡片里**。

## 3. 后端改动（admin-server-go）

### 3.1 probe：整盘判据与按设备采样

- 新增 `DiskIOSampleAll() map[string]DiskIOStat`（`DiskIOStat{ReadBytes, WriteBytes float64}`，均为**累计值**）：
  解析 `/proc/diskstats`，按**设备名**判定整盘并逐设备累加（第 6、10 列 × 512）。
- 整盘判定（不再看 minor）：设备名匹配 `^(sd[a-z]+|vd[a-z]+|hd[a-z]+|nvme\d+n\d+|mmcblk\d+|md\d+|dm-\d+)$`
  且**不是分区**（分区 = 在上述名字后再跟 `p?\d+`，如 `vda1`、`nvme0n1p1`）。`dm-\d+` 需排除 `dm-\d+` 的分区形态。
- `DiskIOSample() (read, write float64)` 保留为**所有整盘之和**（合计口径，供历史合计列与兼容字段使用），内部改为调用 `DiskIOSampleAll()`。
- `getDisks()`：每项新增 `"device"` 字段 = 该挂载点对应的**基础设备名**（去掉 `/dev/` 前缀与分区后缀：
  `/dev/vda1` → `vda`，`/dev/nvme0n1p1` → `nvme0n1`，`/dev/mapper/x` → `x`；无法映射时为空串）。

### 3.2 metrics：按设备留样

- 新增按设备的速率基线（`map[设备名]已有基线`），与现有 `diskioPrev` 合计基线并存、互不影响。
- `MetricKeys` 动态扩展：在固定 12 列之后追加每块整盘的
  `disk_io_<dev>_read` / `disk_io_<dev>_write`（`dev` = 基础设备名，**按字典序排序保证列序稳定**）。
  设备集在服务启动时探测一次并缓存；运行中新出现的设备只计入合计，不新增列（记一条 warn 日志）。
- **保留 `disk_io_read` / `disk_io_write` 两列不动**（合计口径，旧客户端与旧数据仍可读）。
- raw 与三个聚合档位（`metrics_1m/1h/1d`）共用 `MetricKeys` → 新列自动进入建表、聚合与查询；
  已有表按幂等方式补列（沿用 `internal/mysql` 的 `ColumnExists` + `ALTER TABLE ... ADD COLUMN`，
  与 `notifications.type` 的既有迁移同款），旧行新列为 NULL，读取时按 0 处理，**不得报错、不得重建表**。
- `MetricRound`：新列小数位为 0。

### 3.3 接口

- `GET /api/admin/system`：`disks[]` 每项新增
  `read_rate`、`write_rate`（字节/秒，按该设备算，首次采样为 0）、`read_bytes`、`write_bytes`（累计）。
  `disk_io`（合计）字段**保留不动**（兼容旧 App 与旧前端），但前端不再单独展示（见 §4）。
- 历史接口（各粒度）：新列随 `MetricKeys` 自动出现在返回点里；老数据点缺这些列时前端按缺失处理。
- `docs/API.md`、`docs/DB.md` 必须同步（`make check` 会机检路由↔文档）。

### 3.4 速率计算与展示解耦（修「基线被偷」）

- 新增唯一后台采样循环（1s 节拍，随服务启动；与 5s 的历史采样器各司其职、互不干扰），
  在该循环内统一计算：**每块整盘的读/写速率**、网络 rx/tx 速率，结果写入一个受锁保护的
  「最近一次速率快照」（含 `ts`）。
- `CollectSystem`（REST 与 SSE 共同入口）**不再自己做差**，改为读取该快照：多个客户端、
  多标签、手机 App 同时访问时，读到的是**同一份数值**；不再因为「谁后调用谁重置基线」而抖动。
- 快照是唯一的速率来源；`disk_io`（合计）、`disks[].read_rate/write_rate`、网速均取自它。
- 采样循环尚未产出第一个点（服务刚启动/循环异常）时，速率一律返回 `0`（不得返回 NaN/垃圾值），
  且必须保持接口契约不变。
- 采样循环异常要按现有日志约定记 warn（不得 panic、不得影响其它指标）。

### 3.5 兼容与文档

- `GET /api/admin/system` 的 `disk_io`、`network` 字段结构不变（仅数值口径修正 + 快照来源）。
- `docs/API.md`、`docs/DB.md` 同步（路由与列口径）。

## 4. 前端改动（admin-web，`src/components/System.tsx`）

1. **删除**独立的「磁盘 I/O」卡片（读 / 写 / 累计读 / 累计写 四行那一个）。
2. `DiskCard` 内新增三行（沿用 `InfoRow`，风格与其它卡片一致）：
   - `设备` = `vda` / `vdb`（取 `disk.device`，缺失显示 `—`）
   - `读 / 写` = 速率（复用 `fmtRate`）
   - `累计读 / 累计写` = 累计字节（复用 `fmtBytes`）
   - 卡片标题保持「磁盘 <mount>」不变。
3. 趋势图（现「磁盘 I/O（/s）」）：改为**按盘分色**——每块盘各两条线（读 / 写），
   **本机两块盘 → 图上就是 4 根线**：`vda 读` / `vda 写` / `vdb 读` / `vdb 写`（legend 与颜色一一对应，
   读/写用同色系深浅或 existing tone 体系区分，不得四根线撞色）。数据取 `disk_io_<dev>_read|write` 列；
   若这些列不存在（老数据或单盘环境）**回退**为合计两条线（`disk_io_read`/`disk_io_write`），不允许空白图。
   卡片标题相应改为「磁盘 I/O（/s，按盘）」。三个粒度档位（秒/分钟/小时/天）都要正常，
   **四个档位都必须能看到这 4 根线**。
4. `src/api.ts` 的 `DiskInfo` 补 `device`、`read_rate`、`write_rate`、`read_bytes`、`write_bytes`（均选填）。
5. 设计语言：B 端沿用 antd 默认 token，不引入新依赖、不加自定义色板；文案克制、禁 emoji。

## 5. 明确不做

- 不改网速 / CPU / 内存 / PSI 的口径与展示。
- 不做磁盘的增删改、不做挂载管理。
- 不重命名 `disk_io_read` / `disk_io_write`，不删除 `disk_io` 响应字段（兼容面）。
- 不引入新的第三方依赖（Go 只用标准库；前端不加库）。
- 不改 `metrics` 的留存窗口与聚合规则。

## 6. 验收清单（实现方自证 + Hermes 独立复验）

1. `go test ./...` 新增 probe 单测：喂 `/proc/diskstats` 夹具（含 `vda` minor=0、`vdb` minor=16、
   `vda1`/`vdb1` 分区、`nvme0n1` + `nvme0n1p1`）→ 整盘集 = {vda, vdb, nvme0n1}，分区全部被排除。
2. 速率口径单测：夹具两次采样、`vdb` 增量 600MB / 10s → `write_rate ≈ 60MB/s`，`vda` 不串味。
3. `GET /api/admin/system` 实测：`disks[]` 每项含 `device` 与四个 I/O 字段；本机应为
   `/`（vda）与 `/data`（vdb）两条，且 `device` 分别为 `vda`/`vdb`。
4. 实测证据：往 `/data` 持续写入时，`disks[]` 里 **vdb 的 write_rate 明显 > 0**（≥ 数十 MB/s），
   而 vda 维持低位——修掉 §1 缺陷的直接证据。
5. 历史留样：新列出现在三个聚合档位的返回里；对已有数据的库执行启动迁移后**不报错、不丢旧行**
   （旧行新列按 0 处理）。
6. 前端：面板上**不再存在**独立的磁盘 I/O 卡片；每块盘卡片内有「设备 / 读·写 / 累计读·累计写」；
   趋势图按盘分色且四个粒度档位都不空白。
7. `make check`（admin-server-go：路由↔文档机检 + vet + 单测 + race；admin-web：`npm run check`）全绿。
8. **速率来源唯一性**：并发访问 `GET /api/admin/system` 与 `/api/admin/system/stream` 时，
   两路返回的速率取自同一份快照（单测：并发调用后快照 ts 不变、数值不为 0 抖动）；
   采样循环未启动时速率返回 `0` 而非 NaN。
9. 提交：小步、conventional commits、中文描述；**不 push**；不得改仓库级 git 身份；
   源码/文档不得出现真实域名、公网 IP、口令。

## 7. 交付物

- admin-server-go：probe 按设备采样 + 整盘判据修复、唯一速率采样循环与快照、metrics 动态列与迁移、
  `docs/API.md`/`docs/DB.md` 同步、单测（含 §6.1/6.2/6.8）。
- admin-web：`System.tsx` 展示改造 + `api.ts` 类型 + 构建验证（`npx vite build --outDir /tmp/admin-web-verify`）。
- 一份自证报告（逐条对应 §6，附原始命令与输出）。
