# 需求：彻底移除 admin 的「通知中心」功能

日期：2026-10-19　发起人：用户（原话「直接干掉啊，功能都不要了」「历史也可以清掉了」）

## 目标

把 admin 的通知中心功能**整体删除**（前端入口 + 后端 API/SSE + 数据表使用），并清理历史数据。
**不影响**其它任何功能：指标图表、数据库页、文件区、分享链接、探活面板、Web 终端票据、Blog 管理。

## 一、admin-server-go

删除范围：

1. 整个包 `internal/notifications/`（`notifications.go` / `store.go` / `handlers.go` / `sse.go` / `attachments.go` / 测试）
2. 所有通知相关路由，至少包括：
   - `GET/POST/PATCH/DELETE /api/admin/notifications*`
   - `GET /api/admin/notifications/{id}/files/{name}`（附件下载）
   - 通知类型 CRUD、`read_all`、`bulk_delete`、SSE 流端点
3. 启动时初始化通知 SSE hub / 后台 goroutine 的代码
4. `internal/config` 中对通知库的连接与默认库名常量（`DB_NOTIFICATIONS_NAME` / `admin_notifications`）
5. `cmd/admin-server-go/main.go` 中注册通知路由与依赖注入的部分
6. `internal/mysql` 中仅供通知使用的辅助（若有，确认无其它调用方后再删）

**保留（不得改动）**：指标库连接与全部路由、文件区、分享链接、探活、Web 终端票据、`AUTH_MODE` 行为、`FILE_ROOT_*` 保护。

## 二、admin-web

删除范围：

1. 组件：`NotificationCenter.tsx`、`NotificationManage.tsx`、`NotificationDebug.tsx`、`PushWaitResult.tsx`
2. 逻辑：`notificationStream.ts`、`notificationPush.ts`
3. 引用清理：`Main.tsx`、`Debug.tsx`、`CommandPalette.tsx`、`main.tsx`、`api.ts` 中与通知相关的类型/方法/路由/入口
4. 导航与路由：去掉「通知」入口与对应 route
5. PWA：如果 service worker 里有 push/notification 逻辑，一并去掉（保留 PWA 安装与离线能力）

**保留**：其它所有页面与导航；antd 默认设计语言；响应式行为不变。

## 三、数据

1. **先备份**：把 `admin_notifications` 库三张表（`notifications` 146 行、`notification_attachments` 77 行、`notification_types` 10 行）导出为 SQL 到 `/data/backups/`，命名带时间戳
2. 备份校验（行数一致）后**清空数据**（`DELETE`，不 DROP 表；表结构先留着，确认几天没问题再删）
3. 备份文件权限 600，不进仓库

## 四、纪律与验收

- 小步中文提交；**禁止** push / checkout -b / rebase / reset --hard / merge
- 不改仓库级 git 身份、提交正文不写署名
- 不装 systemd 单元、不碰 nginx、不改 `/var/www`
- `make check` 必须全绿
- 验收口径：
  1. `grep -ri "notification" admin-server-go --include=*.go` → 只剩迁移工具与注释，主服务 0 命中
  2. `grep -ri "notification" admin-web/src` → 0 命中
  3. `/api/admin/notifications` → 404
  4. 前端构建通过、页面能正常打开（指标/文件区/探活/终端都正常）
  5. 云库三张表行数为 0，备份文件存在且行数与原表一致

## 五、交付

- 变更文件清单
- `make check` 原始输出
- 备份文件路径与行数核对结果
- 需要人工确认的步骤（如有）
