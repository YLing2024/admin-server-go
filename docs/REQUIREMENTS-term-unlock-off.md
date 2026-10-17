# 需求：终端口令门可关闭（`ADMIN_TERM_UNLOCK`）

日期：2026-10-17　提出人：Linden　承接：OpenCode

## 1. 背景

后台「终端」Tab 现在是**两道门**：

1. 前置 Auth Gateway（SSO）—— 已满足「必须登录过管理端」；
2. 应用自己的**终端口令门**：`POST /api/admin/term/unlock` 校验 `ADMIN_TERM_PW_FILE`
   里的口令哈希，成功才签发 12h 票据（Redis `term:ticket:<sha256>`），前端把票据作为
   ttyd 的第 2 个 `--url-arg` 传下去，`webterm-session.sh` 侧再经
   `GET /api/admin/term/verify`（仅本机）校验票据。

用户要求：**本人部署里把第 2 道门去掉**（登录了后台就能直接进终端）。
但不能把特性从开源项目里删掉 —— 别人可能正需要它。

## 2. 目标

新增环境变量开关，**默认行为一字不变**；关闭时不需要输任何口令即可拿到票据。

```
ADMIN_TERM_UNLOCK   password（默认，现状） | off（不要求终端口令）
```

## 3. 后端（admin-server-go）要求

1. `internal/config`：解析 `ADMIN_TERM_UNLOCK` → `Config.TermUnlock`（字符串或枚举，
   取值只有 `password` / `off`；**非法值按 `password` 处理并 `slog.Warn` 一次**，不要 fatal）。
   默认必须是 `password`（缺省不变）。`ADMIN_TERM_PW_FILE` 字段与语义保持原样。
2. `internal/term`：
   - `Unlock`：当 `TermUnlock == "off"` 时**跳过口令校验与失败锁定计数**，直接按现有成功分支
     签发 12h 票据；**响应结构与成功路径完全一致**（字段名、类型、状态码都不许变），
     不得因为「没校验口令」就多/少字段。
   - `password` 模式下 `Unlock`、`Verify`、`ListSessions`、`CloseSessions`、`KillSession`
     的行为、错误文案、限流（5 次失败锁 10 分钟）**全部保持现状**。
   - 新增路由 `GET /api/admin/term/mode`（`authRequired`）→
     `{"unlockRequired": true|false}`（`off` → `false`）。字段名就用 `unlockRequired`。
3. 安全边界（必须保住，不许放宽）：
   - `Verify` 仍然**只允许本机**调用（127.0.0.1 / ::1，现状判断保持）；
   - 票据仍然是「随机 → 只存 sha256 → Redis TTL 12h」；
   - `off` 模式下 unlock 依然要求**通过管理端鉴权**（`authRequired` 不变）。
4. 单测（`internal/term` + `internal/config`）：
   - `off`：unlock 传任意口令/空口令 → 200 且带票据；连续 6 次都不触发限流（不计数）。
   - `password`：错口令 → 现状错误；5 次后 → 429；对口令 → 200 且带票据（回归）。
   - 配置解析：非法值回落 `password`。

## 4. 前端（admin-web）要求

1. `src/components/Terminal.tsx`：进入终端流程前先探测 `GET /api/admin/term/mode`：
   - `unlockRequired === false` → **不渲染口令表单**，直接调 `POST /api/admin/term/unlock`
     拿票据并进入终端（票据存续逻辑、多标签、存活点轮询、`pagehide` sendBeacon 一律不变）；
   - `unlockRequired === true` → 完全按现状（口令表单 → 票据）；
   - 探测失败（网络错 / 404 / 字段缺失，如旧后端）→ **按 `true` 兜底**（保持现状，不白屏）。
2. 不新增依赖、不新增文案区块（禁用 emoji / 技术说明性文案）；样式沿用既有 `.term-gate-*`，
   不加新色板（antd 默认设计语言不变）。
3. `npm run check` 全绿（typecheck + lint + lint:css + check:tokens + test）。
   模式判定逻辑抽成可测纯函数（如 `src/termGate.ts`）并补单测：`false` / `true` / 缺字段 /
   非布尔值 → 期望 `false` / `true` / `true` / `true`。
4. 探针时序：探测与 Tab 激活解耦（进入 Tab 时探一次即可，失败允许下次重探），
   不要因为探测而让「Tab 切换变慢」（历史坑：存活点轮询曾被投诉变绿慢）。

## 5. 文档与工程基线

- `docs/API.md` 必须新增 `### GET /api/admin/term/mode` 一节（`make check` 会机检路由↔文档）。
- `README.md` 环境变量表补 `ADMIN_TERM_UNLOCK`（默认 `password`，说明 `off` 的含义与风险）。
- 若 AGENTS.md 的「已知坑」需要，补一行说明开关语义（不长篇）。
- 不改其它 HTTP 契约；**不 push**；小步提交、conventional commits 中文描述；
  使用全局 git 身份，不得改仓库级 `user.name`/`user.email`。
- 源码/文档/提交信息不得出现真实域名、公私网 IP、邮箱、密钥、真实口令（示例 `example.com` / `127.0.0.1`）。
- `make check` 必须通过（含 `scripts/check-api-docs.mjs` 路由文档比对）。

## 6. 验收（由 Hermes 执行，不通过不算完成）

1. `make check` 退出码 0；前端 `npm run check` 退出码 0。
2. **口令模式回归**（默认）：不带该环境变量启动 → `/api/admin/term/mode` 返回 `unlockRequired: true`；
   unlock 错口令 401、连错 5 次 429；对口令 200 带票据；前端仍显示口令表单。
3. **关闭模式**：`ADMIN_TERM_UNLOCK=off` 启动 → `/mode` 返回 `false`；
   unlock 空口令 200 带票据；前端**不出现口令表单**、直接进终端。
4. 生产部署必须真机验证：管理后台 →「终端」Tab → **登录后台后直接打开终端**（无口令提示），
   且 `/term/` 在未登录（无 SSO 会话）时仍被网关拦下（两道门里的第一道不能松）。
5. 落地后 `ADMIN_TERM_UNLOCK=off` 写进 systemd 单元（Hermes 负责），并确认
   `systemctl restart admin-server-go` 后行为生效。
