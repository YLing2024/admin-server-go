# 需求：admin 新增「站点」页（站点 / 证书 / 备份，只读）

日期：2026-10-21
背景：监控重构与告警、受控操作已完成，最后一块是「站点 / 证书 / 备份」的可见性。

## 一、范围（只读，不做任何修改动作）

新增一个页面 `/sites`，三块内容：

### 1. 站点（nginx）

从 `nginx -T` 的展开配置里解析，每行一个 server 块：

```json
{
  "serverName": "admin.zhangyunling.cn",
  "listen": ["443 ssl http2", "80"],
  "root": "/var/www/admin",
  "proxyPass": "http://127.0.0.1:18920",
  "certPath": "/etc/letsencrypt/live/admin.zhangyunling.cn/fullchain.pem",
  "configFile": "/etc/nginx/conf.d/homepage.conf",
  "redirect": null
}
```

- 正则 server_name（`~^.+\.zhangyunling\.cn$` 这类）单独标 `isRegex: true`，排在最后
- `return 301/302` 的块 `redirect` 填目标地址
- 解析失败时返回空数组 + error，不 500

### 2. 证书（Let's Encrypt）

扫描 `/etc/letsencrypt/live/*/fullchain.pem`，对每个证书用 `openssl x509 -noout -subject -enddate -issuer` 取：

```json
{
  "domain": "admin.zhangyunling.cn",
  "notAfter": "2027-01-18T12:00:00Z",
  "daysLeft": 89,
  "issuer": "Let's Encrypt",
  "autoRenew": true
}
```

- `daysLeft < 21` 标 warning，`< 7` 标 critical（前端用 Tag 颜色区分）
- `autoRenew`：检查 `certbot.timer` 是否 active
- openssl 失败（文件损坏）时该条 `error` 字段说明，不影响其它条

### 3. 备份

- 列出 `/data/backups/` 下的一级目录：名称、大小（du 汇总）、最后修改时间，按时间倒序，最多 50 条
- 列出备份相关的 systemd timer（名字含 backup 的 timer）及其 `NextElapseUSecRealtime`、`LastTriggerUSec`
- 目录大小用 `du -sb` 单次批量算，禁止对每个目录反复 fork

## 二、接口

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/sites/nginx` | 站点列表 |
| GET | `/api/admin/sites/certs` | 证书列表 |
| GET | `/api/admin/sites/backups` | 备份列表 |

- 全部 `authRequired`
- 每个接口内部 10 秒超时；任一子命令失败只影响该字段
- 结果缓存 60 秒（这些数据变化慢，避免每次刷新都 fork openssl/du）

## 三、前端

- 侧栏新增「站点」，路径 `/sites`，沿用现有 router（SectionPaths/SEGMENT_SECTIONS 加一项）
- 三个卡片纵向排列：
  1. **站点**：表格列 = 域名 / 监听 / 上游或根目录 / 证书到期 / 配置文件；支持搜索域名
  2. **证书**：表格列 = 域名 / 到期日 / 剩余天数（Tag：>21 绿、7-21 橙、<7 红）/ 签发者 / 自动续期
  3. **备份**：表格列 = 名称 / 大小 / 时间；上方一行摘要「共 N 份，占用 X GB，最近一次 X 小时前」
- 表格统一：`size="small"`、`pagination={false}`、`scroll={{ y: 360 }}`、空态 `—`
- 390px 不横向溢出
- 不新增依赖

## 四、验收

1. 三个接口都返回真实数据（贴 curl 输出）
2. 站点数 ≥ 20（本机 nginx 的 server 块数量级）
3. 证书条目 ≥ 20，且至少一条能算出正确的剩余天数（与 `openssl x509 -enddate` 对照）
4. 备份条目 ≥ 5，大小与 `du -sh` 一致（误差 < 5%）
5. `/sites` 页面渲染三张卡，390px 无溢出（贴截图）
6. `make check` 全绿，前端 build + typecheck 通过
7. 提交后 **push 到 GitHub**，回报远端 commit
