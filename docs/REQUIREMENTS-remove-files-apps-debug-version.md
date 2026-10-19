# 需求：admin 再移除四块功能（文件区 / 分享链接 / 调试 / 软件版本 / 应用）

日期：2026-10-19　用户原话：「文件那个功能直接干掉，实际上 hermes 是可以直接管理文件系统的，调试也干掉」「软件版本也干掉」「应用也干掉，现在应用这个实质上已经移到了 home 子域名」「分享链接不用了」

## 目标

从 admin（admin-server-go + admin-web）里**整体删除**以下功能，其它功能一件不动：

1. **文件区**（浏览/上传/下载/重命名/新建/删除）
2. **分享链接**（`/s/<token>`、文件分享管理）
3. **调试页**
4. **软件版本**（VersionPanel）
5. **应用列表**（Apps；已迁到 home 子域，admin 里不再需要）

**必须保留**：指标图表（Chart）、数据库页（DBGate 嵌入）、探活面板、Web 终端票据、Hermes 历史、Blog 管理、登录/SSO、主题切换、命令面板中与保留功能相关的项。

## 一、admin-server-go

删除：

- `internal/files/`（handlers / service / upload / 测试）
- `internal/shares/`（shares / handlers / 测试）
- `internal/apps/`（如果只被 admin 的 Apps 页使用；**注意**：`/api/public/apps` 是 home 子域在用的公开接口，**必须保留**，只删管理端部分）
- 路由：`/api/admin/files*`、`/api/admin/download*`、`/api/admin/apps*`、`/s/`（分享）、调试与版本相关端点
- `internal/config` 中仅服务于上述功能的配置项（`FILE_ROOT_PROTECTED` / `FILE_ROOT_READONLY` 等若只给文件区用，一并移除；**先确认无其它调用方**）
- 启动时为文件区/分享做的初始化

保留：

- `/api/public/apps`（home 子域依赖，**不得删除**）
- `/api/blog/*`、`/api/admin/metrics*`、`/api/admin/probe*`、`/api/admin/term/*`、`/api/admin/history*`、`/api/admin/api-tokens*`、`/api/admin/auth-mode`、`/api/admin/me`

## 二、admin-web

删除：

- 组件：`Files.tsx`、`FileShare.tsx`、`Debug.tsx`、`VersionPanel.tsx`、`Apps.tsx`、`AppCard.tsx`、`AppIcon.tsx`、`PublicApps.tsx`（**PublicApps 若被 home 子域复用则保留并说明**）
- 导航项与路由：文件、分享、调试、版本、应用
- `api.ts` 中对应方法/类型
- `CommandPalette.tsx` 中指向上述页面的命令

保留：主布局、主题切换、指标、数据库、探活、终端、Hermes、Blog 管理。

## 三、数据与清理

- 不删任何数据库表；`admin_notifications` 三张空表保持原样（上一轮遗留，另行处理）
- 文件区数据（`/data` 下的用户文件）**一律不动** —— 只删代码与界面
- 分享链接记录（若在库里）随 shares 表一并保留，不再提供入口

## 四、纪律与验收

- 小步中文提交；**禁止** push / checkout -b / rebase / reset --hard / merge
- 不改仓库级 git 身份、提交正文不写署名
- 不装 systemd 单元、不碰 nginx、不改 `/var/www`（构建产物一律 `--outDir` 到临时目录）
- `make check` 全绿；`go vet` 无新增告警
- 验收：
  1. `grep -ri "api/admin/files\|api/admin/download\|/api/admin/apps" admin-server-go --include=*.go` → 只剩 public apps
  2. `grep -riE "Files\.tsx|FileShare|VersionPanel|Debug\.tsx" admin-web/src` → 0
  3. `/api/admin/files`、`/api/admin/apps`、`/s/x` → 404；`/api/public/apps` → 200
  4. 前端构建通过；页面剩余导航项可正常打开
  5. 其它保留功能的既有测试全绿

## 五、交付

- 变更文件清单
- `make check` 原始输出
- 上述 4 条验收命令的原始输出
- 需要人工确认的步骤（如有）
