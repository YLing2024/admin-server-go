# 需求：admin 新增「工具」面板 —— 第一个工具：Bitwarden 解锁

日期：2026-10-20　用户原话：「你做一个工具面板，第一个工具就是 bitwarden 解锁的，做出来吧」
「不用，那就开源呗，但是你要保证没有问题」「就是隐私相关的」

## 目标

在 admin（admin-server-go + admin-web）新增一个**「工具」**页：`⚙ 工具` 侧栏项，内含若干独立小工具，
第一个工具是 **Bitwarden 解锁** —— 用户在页面上输一次主密码，服务端换出 token 落到
`/root/.bw_session`，供服务器上的 `bw` CLI 与 Hermes 使用。

**本功能的隐私边界是硬要求，见第四节。代码可以开源，但不得包含任何密钥、不得记录任何输入。**

## 一、后端（admin-server-go）

新增包 `internal/tools/bitwarden`（不是 `xpack`，只是普通功能模块）。

### 配置（环境变量，写进 `/etc/admin-server-go.env`）

```ini
BITWARDEN_SERVER=https://password.zhangyunling.cn
BITWARDEN_EMAIL=<账号邮箱>
BITWARDEN_DEVICE_ID=<安装时生成一次并固定>
BITWARDEN_SESSION_FILE=/root/.bw_session
```

三项缺任一 → 该工具在 `/api/admin/tools` 列表里标记为「未配置」，接口返回 501，不影响其它工具。

### 解锁原理（不走 `bw unlock`）

`bw unlock` 只接受交互式 TTY；直接调身份接口：

```http
POST {BITWARDEN_SERVER}/identity/connect/token
Content-Type: application/x-www-form-urlencoded

grant_type=password&username={EMAIL}&password={输入}&scope=api offline_access
&client_id=cli&deviceType=10&deviceIdentifier={DEVICE_ID}&deviceName=admin-tools
```

返回的 `access_token` 即等价 `BW_SESSION`。**只落盘 token，不落盘密码。**

### 接口

| 方法 | 路径 | 行为 |
|---|---|---|
| GET | `/api/admin/tools` | 工具清单：`[{id:"bitwarden",name,configured,unlocked,expiresAt}]` |
| GET | `/api/admin/tools/bitwarden/status` | `{configured,unlocked,updatedAt,expiresInSeconds}`（解析 token 的 JWT exp，不返回 token） |
| POST | `/api/admin/tools/bitwarden/unlock` | body `{"password":"..."}` → 成功写文件（0600）→ `{ok:true,expiresInSeconds}`；失败 401 `{ok:false,error:"密码错误"}` |
| POST | `/api/admin/tools/bitwarden/lock` | 删 `/root/.bw_session` → `{ok:true}` |

必须做到：

1. 全部路由 `authRequired`（与其它 admin 接口一致，SSO 注入 `X-Auth-User`）
2. **10 秒超时**；失败一律回「密码错误」，不区分账号不存在 / 网络错误 / 服务端 5xx（防枚举与探测）
3. **同 IP 每分钟最多 5 次**，超出 429（用进程内计数即可，不引 Redis）
4. `/root/.bw_session` 0600、属主 root；写入用「临时文件 + rename」原子替换
5. token 解析失败（非 JWT）时按「已解锁但我们读不出有效期」处理，不要报错

## 二、前端（admin-web）

侧栏新增 **「工具」**（放在「数据库」之后），页面为工具卡片列表：

```text
工具

┌ Bitwarden 解锁 ─────────────────────────────┐
│ 状态：已锁定 / 已解锁（剩 3h42m）             │
│ 用途：把主密码换成 session，供服务器上的       │
│       bw CLI 与本机助手使用。主密码不保存。    │
│  ┌───────────────────────────┐              │
│  │ 主密码                     │              │
│  └───────────────────────────┘              │
│  [ 解锁 ]                    [ 清除会话 ]    │
└─────────────────────────────────────────────┘
```

- 解锁成功后：状态行刷新、密码框清空、`message.success` 提示剩余有效期
- 解锁失败：`message.error` 显示「密码错误」，**不对输入做任何保留**
- 未配置时：卡片置灰，提示「未配置 BITWARDEN_*，见运维文档」
- 输入框 `type="password"`、`autoComplete="off"`，**不写入 localStorage / sessionStorage**
- 移动端三列堆叠，不破版
- 工具列表用数据驱动（`GET /api/admin/tools` 渲染），后续加工具只加一个卡片组件

## 三、隐私与安全（硬要求，验收会逐条查）

1. **不记录请求体**：该路由必须显式跳过任何请求体日志/审计；`journalctl` 里 grep 不到密码
2. **不落盘密码**：只在内存里存在一次；panic 恢复里也不得打印 body
3. **不返回密码**：任何响应、错误信息、调试接口都不含密码或其片段
4. **不写前端存储**：密码不进 localStorage、sessionStorage、URL、Referer
5. **不引第三方**：不新增 Go 依赖；不引 CDN、不引外部字体
6. **token 文件**：0600、原子写、删除即失效
7. **限速与超时**：见上
8. **不把 token 暴露给前端**：`/status` 只返回布尔与剩余时间

## 四、纪律

- 小步中文提交；禁止 push / checkout -b / rebase / reset --hard / merge
- `make check` 全绿；`go vet` 无新增告警
- 前端构建 `--outDir /tmp/...`，绝不写 `/var/www`
- 不动现有功能（指标 / 博客 / 探活 / 终端 / Hermes / 数据库 / SSH 公钥）

## 五、验收

1. `GET /api/admin/tools` 返回 `bitwarden` 一项，`configured=true`
2. 未解锁时 `/status` 的 `unlocked=false`；错误密码 → 401，响应体只有「密码错误」
3. 正确密码 → 200、`/root/.bw_session` 存在且 0600、`BW_SESSION=$(cat ...) bw status` 显示 `unlocked`
4. `POST .../lock` 后文件消失、`bw status` 回到 `locked`
5. 连续 6 次错误密码 → 第 6 次 429
6. `journalctl -u admin-server-go --since '10 min ago' | grep -c "<本次输入的密码>"` → **0**
7. 浏览器 DevTools：Network 里请求体只在 unlock 那一条出现；Application → Storage 里搜不到密码
8. 页面：工具页渲染、移动端不溢出、清空会话可用
9. `make check` 原始输出

## 六、交付

- 变更文件清单 + 两个仓库的提交记录
- 第 1~9 条的原始输出（含第 6 条 grep 的 0 结果）
- 需要运维补的环境变量清单
