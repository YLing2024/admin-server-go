# 需求：admin 管理 SSH 公钥白名单（GitHub 风格）

日期：2026-10-20　用户原话：「你能给这个在 Admin 做一个管理吗，管理 SSH 公钥白名单这种，类似 github 这种」

## 目标

在 admin 后台新增「SSH 公钥」管理页：**增 / 删 / 停用 / 查看** 一组允许登录服务器的公钥，
以数据库为唯一事实源，由服务端把公钥渲染成 sshd 实际读取的文件。

**不做**：用户体系、多账号、审批流、Web 端生成密钥对。只有一个管理员（SSO 身份 `linden`）。

## 架构（关键取舍）

服务器上 `/root/.ssh/authorized_keys` 是 sshd 读的地方。两种同步方式：

- 方案 A（定时渲染文件）：后台写库 + 定时任务渲染 `authorized_keys`
- **方案 B（AuthorizedKeysCommand，采用）**：sshd 每次登录时**实时询问**一个辅助程序，
  辅助程序查库返回该用户可用的公钥。

选 B 的理由：**撤销即时生效**（GitHub 的体验就是这样），不依赖定时任务，也不会出现
「库里删了但文件里还在」的中间态。代价是要改 `sshd_config`（一次性改动，必须可回滚）。

```text
sshd  ← AuthorizedKeysCommand /usr/local/bin/ssh-keys-from-admin %u %f
                                    │
                                    ├── 查 MySQL（admin 库，表 ssh_key）
                                    └── 输出该用户名的公钥（每行一个）
```

## 一、数据库（只允许 CREATE TABLE IF NOT EXISTS，禁止 DROP / TRUNCATE）

库：沿用现有 admin 库（`DB_METRICS_NAME` 所在库，或单独的 `admin_auth` 库，按现有连接池走）。
表：

```sql
CREATE TABLE IF NOT EXISTS ssh_key (
  id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
  title         VARCHAR(100)  NOT NULL,
  public_key    TEXT          NOT NULL,
  fingerprint   CHAR(64)      NOT NULL,          -- SHA256(base64 解码后的 blob)，hex
  key_type      VARCHAR(32)   NOT NULL,          -- ssh-ed25519 / ssh-rsa / ecdsa-...
  comment       VARCHAR(200)  NOT NULL DEFAULT '',
  enabled       TINYINT(1)    NOT NULL DEFAULT 1,
  created_at    DATETIME      NOT NULL,
  last_used_at  DATETIME      NULL,
  PRIMARY KEY (id),
  UNIQUE KEY uk_fingerprint (fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;
```

## 二、后端接口（admin-server-go）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/ssh-keys` | 列表（id / title / 指纹 / 类型 / 注释 / 启用 / 创建时间 / 最近使用） |
| POST | `/api/admin/ssh-keys` | 新增：`{title, public_key}` |
| PATCH | `/api/admin/ssh-keys/:id` | 改 `title` / `enabled` |
| DELETE | `/api/admin/ssh-keys/:id` | 删除 |

校验（服务端必须做，不能只靠前端）：

1. **解析公钥**：必须是 `type base64 [comment]` 形式，base64 能解码，且 `ssh.ParseAuthorizedKey` 能解析
2. **去重**：按 `fingerprint`（`SHA256:` + base64）唯一，重复返回 409
3. **上限**：同一用户最多 20 把，超出返回 400
4. **长度**：title ≤ 100，comment ≤ 200，整行 ≤ 8KB
5. **拒绝**：`command=` / `environment=` / `permitopen=` 等前置选项一律拒绝（只接受纯公钥，防止被塞入强制命令）

响应里的指纹格式用 GitHub 风格：`SHA256:xxxxxxxx...`（无填充的 base64）。

## 三、辅助程序（关键）

新增独立小命令 `cmd/ssh-keys-from-admin`（**不被服务主程序 import**，独立 `go build`）：

```text
ssh-keys-from-admin <username> <fingerprint>
```

- 只允许 `username == "root"`（当前唯一登录用户；写成可配置项 `SSH_KEYS_USER`，默认 root）
- 读库（连接串复用服务端的环境变量），输出 `enabled=1` 的公钥，每行 `type base64 comment`
- sshd 调用时是 `%u %f`：`%f` 只在 key 验证阶段有值，可用来回写 `last_used_at`（尽力而为，失败不影响输出）
- **任何异常都必须输出空并退出 0**（宁可登录失败，也不能因程序报错放行或阻塞）
- 二进制放 `/usr/local/bin/`，权限 755，只读凭证：额外给一个 `SELECT` 权限的只读账号（若现有账号权限过大，用独立只读连接配置）

sshd 配置改动（一次性，改完 `sshd -t` 通过才 reload）：

```text
AuthorizedKeysCommand /usr/local/bin/ssh-keys-from-admin %u %f
AuthorizedKeysCommandUser nobody
AuthorizedKeysFile none
PubkeyAuthentication yes
```

回滚脚本必须一起交付：把 `sshd_config` 恢复备份 + 还原 `AuthorizedKeysFile .ssh/authorized_keys`。

## 四、前端（admin-web）

新增侧栏项 **「SSH 公钥」**（放在「管理」之后、「终端」之前）：

- 列表：标题 / 指纹（等宽字体，点击复制）/ 类型 / 注释 / 状态（启用/停用）/ 创建时间 / 最近使用 / 操作（编辑标题、停用、删除）
- 新增：弹窗，`标题` + `公钥（textarea，粘贴整行）`，提交后立即出现在列表
- 删除：二次确认，文案说明「删除后该密钥立即无法登录，已建立的连接不受影响」
- 停用：与删除的区别写清楚（停用即从 authorized_keys 消失，可随时恢复）
- 空态：给出「在本地 `ssh-keygen -t ed25519 -C "备注"` 然后把 `.pub` 内容粘到这里」的说明
- 必须是 **Ant Design 默认设计语言**，响应式（≥992px 与移动端都可用），不引新依赖，不用 emoji

## 五、纪律

- 小步中文提交；**禁止** push / checkout -b / rebase / reset --hard / merge
- `make check` 全绿；`go vet` 无新增告警
- 前端构建产物输出到临时目录（`--outDir /tmp/...`），**绝不写 `/var/www`**
- 不改动其它功能（指标 / 博客 / 探活 / 终端 / Hermes / 数据库）
- 云库只许 `CREATE TABLE IF NOT EXISTS` 与 `ALTER TABLE ... ADD COLUMN`，不得 DROP / TRUNCATE / 重建

## 六、验收

1. 迁移后表存在，字段齐全
2. `POST` 一把真实公钥 → 列表出现 → `sha256sum` 指纹与 `ssh-keygen -lf` 一致
3. 重复提交同一公钥 → 409
4. 提交带 `command="..."` 的公钥 → 400
5. 停用后：辅助程序对该公钥不再输出（用 `ssh-keys-from-admin root` 直接验证）
6. 删除后：同上
7. `sshd -t` 通过；用一把新公钥实测能登录；删除该公钥后**新连接**被拒（已建立的连接不受影响）
8. 回滚脚本执行后 `authorized_keys` 方式恢复原状
9. 前端：列表/新增/停用/删除全部可用，移动端不破版
10. `make check` 原始输出

## 七、交付

- 变更文件清单
- `make check` 原始输出
- 第 2~8 条验收命令的原始输出（含 `ssh-keygen -lf` 对指纹的比对）
- sshd 改动前后的 diff 与回滚脚本路径
