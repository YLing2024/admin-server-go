# 需求：监控页 v2「大而全」——恢复全部能力并补齐维度

日期：2026-10-20　用户明确不满：
> 「可监控的功能反而比以前少了……进程和服务也显示不全，服务的排序也无法自定义……我要一个大而全的美观面板」

## 一、已核实的回归（必须修）

| 项 | 数据实际 | 新版页面 | 问题 |
|---|---|---|---|
| 进程 | `/api/admin/services` 返回 **285 条全量**（`truncated=false`） | 只显示 **8 条** | 前端 `slice(0,8)` 且 `pagination={false}` |
| 服务 | 运行中 systemd 单元 **44 个** | 只显示 **10 个**（`top_services`） | 后端只给 top10，前端无分页 |
| 端口 | `/api/admin/system/info` 有 **60 条**（返回 15，`total=60`） | 未展示 | 数据被丢 |
| 排序 | — | 全部列无 `sorter` | 旧版有排序，新版删了 |
| 磁盘 | 3 个挂载点 | 只显示 `/` | 其余挂载点没展示 |

## 二、目标信息架构（单页，分区清楚，可锚点跳转）

```text
① 页头      系统监控 · 主机名 · 内核 · 已运行 · [自动刷新 5s/关] [刷新]
② KPI 带     CPU / 内存 / 磁盘(/) / Swap / 卡顿指数 / 运行队列      （6 张，带 sparkline）
③ 趋势       指标切换：CPU 内存 磁盘 网络 磁盘IO   范围：1h 6h 24h 7d
④ 压力与负载  PSI 三条 + load1/5/15 + 阻塞进程 + CPU 温度（有则显示）
⑤ 进程      全量表（285 行）· 搜索 · 分页 20/50/100 · 全列可排序
⑥ 服务      全量运行中单元（44 个）· 搜索 · 分页 · 按 CPU/内存/名称排序
⑦ 端口      全量 60 条 · 过滤（TCP/UDP/仅监听）· 排序
⑧ 磁盘      每个挂载点：使用率 / 已用总 / inode 使用率 / 读写速率
⑨ 审计      失败单元 · 最近登录（last 不可用时灰色说明）
```

## 三、表能力（每个表都要）

- **排序**：点表头切换升降序，默认 CPU 降序；数值列按数值排（不是字符串）
- **搜索**：进程按名称/PID；服务按单元名；端口按端口/进程；输入即筛（本地过滤即可）
- **分页**：`pageSize` 可选 20/50/100，显示 `共 N 条`；`showSizeChanger`
- **列可见性**：进程表提供「列」下拉，可隐藏列（名称/PID/CPU/内存/线程/cgroup 内存/子树内存）
- **行高**：`size="small"`，数值 `tabular-nums` 右对齐
- **空态**：`—`，不显示 0

## 四、后端改动（admin-server-go）

1. `/api/admin/system/info` 的端口：**返回全量**（去掉 15 条上限），保留 `total`；如担心体积，加 `?ports=all|15`，默认 all
2. `/api/admin/system/info` 的磁盘：返回**全部挂载点**（含 inode 字段 `inode_percent` / `inode_used` / `inode_total`）
3. 新增 `GET /api/admin/system/services`：返回**全部** `.service` 单元
   ```json
   [{"unit":"admin-server-go.service","active":"active","cpu_percent":7.4,"mem_mb":35,"since":1792502132}]
   ```
   - 数据源：`systemctl list-units --type=service --state=running --output=json` + cgroup `cpu.stat`/`memory.current`
   - 单次采集在 300ms 内；失败时返回 `[]` 并 warn，不 500
4. SSE 的 `top_services` 保留（前端用于「最耗 CPU 的服务」小榜），不扩成 44 条以免帧过大

## 五、视觉要求（延续上一版，但不许牺牲信息量）

- 大而全但分区明确：每个区块一张卡，标题一行，右上角可放操作（排序/过滤/分页）
- 数字 `tabular-nums`，KPI 28px，表格 13px
- 只用 antd 状态色；图表单色
- 暗色跟随主题
- **390px 不横向溢出**（表格自身可横向滚动，但整页不行）
- 首屏必须看到：KPI + 趋势 + 压力；滚动到下面才是表格区

## 六、验收

1. 进程表显示 `共 285 个进程`，可选 50/页 并能翻到最后一页；按内存排序第一行是当前内存最大的进程（与 `ps aux --sort=-rss | head -3` 对比）
2. 服务表显示 `共 44 个服务`（与 `systemctl list-units --type=service --state=running | wc -l` 一致），可按 CPU/内存/名称三向排序
3. 端口表显示 `共 60 条`，TCP/UDP 过滤生效
4. 磁盘区显示 3 个挂载点，含 inode 使用率
5. 列可见性下拉能隐藏/恢复至少 2 列
6. 390 / 768 / 1440 三档整页无横向滚动条（贴三张截图）
7. 暗色模式截图一张
8. `make check` + `npm run build` + typecheck 全过
