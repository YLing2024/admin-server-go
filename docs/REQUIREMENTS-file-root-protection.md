# 需求：补齐文件区的根项保护名单（含密钥/凭据的目录不得经面板可读可删）

> 本轮只改 **admin-server-go**（后端）。

## 一、背景（已核实的事实）

文件区根目录是 `/data`（`ADMIN_FILE_DIR`）。根这一层有两张名单，实现在
`internal/files/service.go`：

- `fileRootProtected`：**不展示**（隐身）+ 整棵子树禁止写操作；
- `fileRootReadonly`：正常展示可浏览，但**根这一项**不允许删/改名/移动，子路径照常读写。

实测：文件区接口当前会把下列根项原样返回，也就是说**在持有后台会话的情况下可以浏览、下载、
甚至删除**它们——其中多个目录里放的是密钥与凭据：

| 根项 | 内容（实测） | 风险 |
|---|---|---|
| `auth-server-go` | `oidc-keys.json`、`jwt-secret`、`internal-token`、`totp-secret.json`、`clients.json` | 认证中心私钥与共享令牌，泄露即可伪造身份 |
| `davbox-dev` | `admin-password.txt`、`secret.key`、`accounts.json` | 明文口令与密钥 |
| `drop-forensics` | `admin.json`、`key`、`blobs` | 凭据与取证数据 |
| `dbgate` | `.env`（云 MySQL 口令）、`docker-compose.yml` | 生产库口令 |
| `admin-server-go` | `config.json`（口令哈希）、`audit.log`、`data/` | 后台自身配置与审计 |
| `keygate-go` | `data/`、`hooks/` | 密钥网关运行数据 |
| `chrome-shared` | 浏览器 profile（Cookie / 登录态） | 会话凭据 |
| `legacy-sqlite-20261016.tar.gz` | 迁移前的旧库快照 | 历史业务数据 |

另有若干「服务数据目录」未登记，当前根项可被误删（一次误点即毁数据）：`quotahub-go`、
`blog-go`、`cloud`、`super-productivity`。

## 二、需求（逐条实现）

1. `internal/files/service.go` 的 `fileRootProtected` 追加这些根项（**稳定排序**，按现有风格的字母序插入）：
   `admin-server-go`、`auth-server-go`、`chrome-shared`、`davbox-dev`、`dbgate`、
   `drop-forensics`、`keygate-go`、`legacy-sqlite-20261016.tar.gz`。
   - 后缀为文件（tarball）同样适用：根这一层的名字命中即受保护。
   - 注释里写清「为什么」（哪些是密钥/凭据、哪些是自身配置、哪些是历史数据），沿用文件里既有的注释风格与
     `与旧实现 …一字不差` 那类说明；**新名单是新增约定，不要在注释里谎称与旧实现一致**。
2. `fileRootReadonly` 追加：`quotahub-go`、`blog-go`、`cloud`、`super-productivity`。
3. 行为不变的部分：受保护项仍然「隐身 + 整树禁写」，只读项仍然「可见但根项禁删」；子路径读写、软链跳过、
   路径穿越防护等既有语义**一个字都不要改**。
4. 若仓库里有文档描述这两张名单（`docs/`、`README.md`、`AGENTS.md` 任一处），同步更新；没有就不新增文档文件。
5. 单测：`internal/files` 现有用例若断言名单内容或数量，同步更新；**新增用例**至少覆盖两条：
   - 受保护的根项不出现在列表结果里，且对其写操作被拒；
   - 只读的根项出现在列表结果里，但删除/改名被拒。
   用例要满足「改坏了会红」（例如把 `dbgate` 从名单里删掉，用例必须失败）。

## 三、边界与禁止

- 不改路由、不改响应结构、不改其它 `internal/*` 包；不改 `/data` 下任何实际文件。
- 不新增依赖（`go.mod` 不应变化）。
- 源码 / 文档 / 提交信息不得出现真实域名、公网 IP、口令、密钥、token；示例一律占位。
- 禁止把 `/data` 的真实目录清单以外的私有信息写进仓库（本需求已按需列出目录名，属可公开的部署结构）。

## 四、交付标准（硬性）

1. `make check`（机检路由↔文档 + `go vet` + 单测）退出码必须 0，报告里贴**原始输出**。
2. 新增/改动逻辑必须有对应单测，且「改坏了会红」；报告里贴跑测试的原始输出。
3. 导出符号必须有文档注释（Go：以导出名开头），写用途/参数/返回/错误；注释写「为什么」，不写「这行在做什么」。
4. 不留注释掉的死代码、不留 `fmt.Println`/调试残留；不新增 TODO/FIXME；禁止空 `catch`（Go：错误必须处理）。
5. **不改 `docs/API.md` 的行为描述**（本轮无接口契约变化）；若 `make check` 的文档机检报错，报告里说明原因。
6. 报告四要素：改了什么 / 新增了哪些依赖 / 哪些没做到 / 留下什么风险；发现既有问题只报告不修。

## 五、提交纪律

- 小步多次提交，conventional commits（中文描述）；**禁止** `git push`、`checkout -b`、`rebase`、
  `reset --hard`、`merge`；在当前分支工作。
- 提交使用仓库既有 git 身份，不得自报署名、不得写 `Co-authored-by` / `Generated-by` 之类水印。

## 六、自检（逐条贴原始输出）

```bash
make check; echo "CHECK_EXIT=$?"
grep -n "auth-server-go\|davbox-dev\|dbgate\|legacy-sqlite\|keygate-go\|chrome-shared\|drop-forensics\|admin-server-go" internal/files/service.go
grep -n "quotahub-go\|blog-go\|cloud\|super-productivity" internal/files/service.go
git status --short
```
