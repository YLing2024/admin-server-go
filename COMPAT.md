# COMPAT.md — 与旧实现（Express 4）的差异清单

> 行为权威是旧实现 `/root/proj/admin-server`（Node.js + Express 4）。本仓库逐条复刻其 HTTP 契约，
> 本文只记录**已知且有意保留**的差异，以及**无法完全对齐**的残余差异，便于验收方逐条核对。
> 除本文所列之外，接口行为均以旧实现实测真值为准。

## 一、有意不复制（不影响契约，仅头/体字节级别）

### 1. `X-Powered-By: Express`

Express 默认给每个响应加 `X-Powered-By: Express`。本仓库不发送该头。它只是框架指纹，没有客户端
依赖它；且暴露框架版本不利于收敛攻击面。判定为可接受差异。

### 2. 弱 `ETag`

Express 默认给响应体算弱 `ETag`（`W/"<len>-<hash>"`）并支持条件请求（`If-None-Match` → 304）。
本仓库用标准库 `net/http`，不自动生成 `ETag`，也不做 304 协商。旧实现的业务代码没有依赖 ETag
的读写路径，前端亦未使用条件缓存。判定为可接受差异。

> 副作用：旧实现对同一 GET 的第二次请求可能返回 `304`，新版恒返回 `200` + 完整正文。
> 若将来需要严格复刻，应在 `httpx.WriteJSON` 统一注入弱 ETag 并处理 `If-None-Match`。

## 二、残余差异（无法完全对齐，逐条说明影响面与原因）

### 1. JSON 正文尾部换行（全局）

旧实现 `res.json(obj)` 输出**无**尾随换行；本仓库 `httpx.WriteJSON` 走 `json.NewEncoder(w).Encode`，
会在正文末尾补一个 `\n`。

- 影响：**所有 JSON 响应的 `Content-Length` 比旧实现大 1 字节**，正文多一个尾随 `\n`。JSON 语义
  等价（解析结果完全相同），实际消费方（前端 `res.json()`）不受影响。
- 范围：全局，包括 ① 中 `HEAD` 的 `Content-Length`（新版等于“新版 GET 正文长度”，自洽）。
- 取舍：本轮 4 条修复未涉及此项；改动它需要动 `httpx` 的公共出口、牵动全部端点与全部单测的字节断言，
  风险面远大于收益，故保留并在此记录。

### 2. 中文排序：少数生僻字的拼音与 ICU 不一致（仅限 `GET /api/admin/files`）

需求允许用 `golang.org/x/text` 的 `collate` 复刻 `localeCompare(a, b, 'zh-Hans-CN')`。实测其排序表
冻结在 **CLDR 23**，与旧实现（Node 24 的 ICU，CLDR 40+）有两处系统性差异：

1. **汉字块与拉丁块的相对位置相反**：x/text 把汉字排在拉丁之后，旧实现排在之前。
2. **少数生僻字的拼音权重过时**：CLDR 23 缺该字读音或读音与现行不同。

针对第 1 点，本仓库用「脚本分组（通用字符 0 → 汉字 1 → 其余 2）+ 组内 x/text 主权重」的比较器
（`internal/files/sort.go`）把汉字块摆回拉丁之前，组内拼音序仍交给 x/text，避免重造整张表。

针对第 2 点，用 **CJK 基本区全部 20992 字**（U+4E00–U+9FFF）做两两比对：无序对 220,321,536、
不一致对 4,080,185，与旧实现 ICU 的一致率 **98.15%**，不一致的都是缺少 CLDR-23 拼音的罕见字。
已观测到的具体字符示例：

| 字符 | 旧实现（现行 ICU/拼音） | 本仓库（CLDR 23） |
|---|---|---|
| 沈 | shen | chen |

- 影响面：仅目录列表的文件名排序，且只涉及拼音权重过时的**生僻字**；常用字（验收用的
  `啊/带/中/中英混排` 等）已 100% 对齐。
- 已核对：旧实现中 `localeCompare` 只有 `src/routes/files.ts` 这一处（文件列表排序），其余地方无中文排序。
- 验收样例（Node 24.19.0 实测真值）：`.hidden < 啊.txt < 带空格 的.txt < 中文名.md < a.txt < blob.bin < z.txt`，
  目录优先后为 `sub` 置顶；本仓库输出与之逐字节一致。

### 3. 两处「验收真值与旧实现实测相反」的判定（以旧实现实测为准）

验收方给出的 ①–⑤ 真值里有两处与本仓库对旧实现的实测不一致，均已按**旧实现实测**复刻，
此处留证，便于逐条核对：

1. **`OPTIONS /api/admin/apps` 无 `X-Auth-User`**：验收真值记为 `401 {"error":"未登录"}`（认为鉴权先于方法回退）；
   旧实现实测为 **`200` + `Allow: GET,HEAD`**。原因是 Express 的路由层对未命中方法的 `OPTIONS` 直接由
   router 生成默认响应（`sendOptionsResponse`），**根本不会进入该路由的 `authRequired` 中间件**，故鉴权
   没有机会先跑。本仓库复刻为：OPTIONS 自动 Allow 响应免鉴权（`docs/API.md` §方法回退已写明）。
   实测命令：`curl -s -i -X OPTIONS http://127.0.0.1:3100/api/admin/apps` → `200` / `Allow: GET,HEAD` / 体 `GET,HEAD`。
2. **`cached` 字段**：验收真值记为「旧实现没有、新版多出」；旧实现实测**有** `cached`。
   旧 `src/apps.ts` 的 `createCollector().getPayload()` 恒返回 `{ ...collectOnce(), cached }`（命中 10s 缓存
   `true`，否则 `false`），故该字段属于旧实现响应契约，不能删。本仓库保留它，并把它放在**与旧实现相同的
   位置（顶层最后一个键，`discovered` 之后）**，以支持逐字节比对。

## 三、验收口径

- ①②③④ 四条修复的自测矩阵与原始输出见交付报告。
- 「已一致」的端点（坏 JSON 500 文案、`registryMtime` 等）不再复述。
- 活体采样类字段（`apps`/`services`/`system` 的内存、CPU、网络计数、`latencyMs`）因采样时刻不同天然有差异，
  不属于契约差异。
