# sshd 接入 admin 的 SSH 公钥白名单

sshd 通过 `AuthorizedKeysCommand` 实时调用独立辅助程序查询 MySQL，因此后台停用 / 删除公钥后，
**下一次登录立即失效**；已建立的连接不受影响。

```text
sshd ──AuthorizedKeysCommand──> /usr/local/bin/ssh-keys-from-admin %u %f
                                      ├── 查 MySQL（admin_metrics.ssh_key）
                                      └── 输出 enabled=1 的 type base64 comment
```

## 1. 构建与安装

```bash
# 在 admin-server-go 仓库根目录
make build-ssh-keys
install -o root -g root -m 0755 ssh-keys-from-admin /usr/local/bin/ssh-keys-from-admin
```

辅助程序独立 `go build`，不被服务主程序 import；服务启动时会自动 `CREATE TABLE IF NOT EXISTS ssh_key`。

## 2. 只读数据库账号

若现有服务账号权限过大，建一个只读账号（示例，把占位符换成部署环境的值）：

```sql
CREATE USER 'ssh_keys_ro'@'127.0.0.1' IDENTIFIED BY '<只读口令>';
GRANT SELECT ON `admin_metrics`.`ssh_key` TO 'ssh_keys_ro'@'127.0.0.1';
```

`last_used_at` 的回写是可选项；若希望管理页显示「最近使用」，额外授予：

```sql
GRANT UPDATE (last_used_at) ON `admin_metrics`.`ssh_key` TO 'ssh_keys_ro'@'127.0.0.1';
```

## 3. 凭证文件（sshd 不继承服务环境）

sshd 拉起 `AuthorizedKeysCommand` 时不会带上 admin-server-go 的 systemd 环境变量，因此辅助程序
从 `SSH_KEYS_ENV_FILE`（默认 `/etc/ssh-keys-from-admin.env`）读取连接参数：

```ini
# /etc/ssh-keys-from-admin.env
DB_HOST=127.0.0.1
DB_PORT=3306
DB_USER=ssh_keys_ro
DB_PASSWORD=<只读口令>
DB_METRICS_NAME=admin_metrics
SSH_KEYS_USER=root

# 也可用 SSH_KEYS_DB_* 单独覆盖，未设置时回退到 DB_*
# SSH_KEYS_DB_USER=ssh_keys_ro
# SSH_KEYS_DB_PASSWORD=<只读口令>
```

`AuthorizedKeysCommandUser nobody` 时，该文件必须对 `nobody` 可读；文件里只放只读账号：

```bash
chown root:nogroup /etc/ssh-keys-from-admin.env
chmod 0640 /etc/ssh-keys-from-admin.env
sudo -u nobody cat /etc/ssh-keys-from-admin.env >/dev/null   # 自检可读
```

手工验证（不经过 sshd）：

```bash
SSH_KEYS_ENV_FILE=/etc/ssh-keys-from-admin.env \
  /usr/local/bin/ssh-keys-from-admin root 'SHA256:...'
```

## 4. 切换

```bash
sudo deploy/ssh/switch-authorized-keys-command.sh
```

脚本会：

1. 整份备份 `sshd_config` 到 `/etc/ssh/backups/sshd_config.<时间>.bak`，并写 `authorized-keys-command.latest`；
2. 在全局段写入 `AuthorizedKeysCommand` / `AuthorizedKeysCommandUser nobody` / `AuthorizedKeysFile none` /
   `PubkeyAuthentication yes`（`Match` 段原样保留）；
3. `sshd -t` 通过后才 `systemctl reload ssh`；失败自动还原。

演练（临时配置、不 reload 生产 sshd）：

```bash
SSHD_CONFIG=/tmp/sshd_config.test \
SSH_CONFIG_BACKUP_DIR=/tmp/ssh-backups \
SSHD_RELOAD_CMD=true \
deploy/ssh/switch-authorized-keys-command.sh
```

## 5. 回滚

```bash
sudo deploy/ssh/rollback-authorized-keys-command.sh
# 或指定备份：sudo deploy/ssh/rollback-authorized-keys-command.sh /etc/ssh/backups/sshd_config.20261020-120000.bak
```

恢复整份备份（`AuthorizedKeysFile` 回到备份中的 `.ssh/authorized_keys`），`sshd -t` 通过后 reload。

## 6. 变更前后对照

```diff
-#AuthorizedKeysFile	.ssh/authorized_keys .ssh/authorized_keys2
-#AuthorizedKeysCommand none
-#AuthorizedKeysCommandUser nobody
-PubkeyAuthentication yes
+# BEGIN admin-server-go authorized keys command
+AuthorizedKeysCommand /usr/local/bin/ssh-keys-from-admin %u %f
+AuthorizedKeysCommandUser nobody
+AuthorizedKeysFile none
+PubkeyAuthentication yes
+# END admin-server-go authorized keys command
```

本仓库只交付脚本与辅助程序，不在安装时自动修改 `/etc/ssh`；是否切换由部署者执行上面两步。
