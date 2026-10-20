#!/usr/bin/env bash
# 回滚 AuthorizedKeysCommand：恢复切换前的 sshd_config 整份备份并 reload。
#
# 用法：
#   rollback-authorized-keys-command.sh [备份文件]
# 不传备份文件时读取 $SSH_CONFIG_BACKUP_DIR/authorized-keys-command.latest。
# 恢复后先跑 `sshd -t`，通过才 reload；失败则保持现状并以非 0 退出，避免把 sshd 弄成不可用。
set -euo pipefail

SSHD_CONFIG="${SSHD_CONFIG:-/etc/ssh/sshd_config}"
SSHD_BIN="${SSHD_BIN:-/usr/sbin/sshd}"
BACKUP_DIR="${SSH_CONFIG_BACKUP_DIR:-/etc/ssh/backups}"
RELOAD_CMD="${SSHD_RELOAD_CMD:-systemctl reload ssh}"

fail() {
  echo "回滚失败：$*" >&2
  exit 1
}

backup="${1:-}"
if [[ -z "$backup" ]]; then
  [[ -f "$BACKUP_DIR/authorized-keys-command.latest" ]] || fail "找不到备份指针：$BACKUP_DIR/authorized-keys-command.latest"
  backup="$(tr -d '\n' <"$BACKUP_DIR/authorized-keys-command.latest")"
fi
[[ -f "$backup" ]] || fail "备份文件不存在：$backup"
[[ -f "$SSHD_CONFIG" ]] || fail "找不到 sshd_config：$SSHD_CONFIG"
[[ -x "$SSHD_BIN" ]] || fail "找不到可执行的 sshd：$SSHD_BIN"

# 先保留当前（已切换）版本，恢复失败时人工排查有据可查。
current="$BACKUP_DIR/sshd_config.before-rollback.$(date +%Y%m%d-%H%M%S).bak"
mkdir -p "$BACKUP_DIR"
cp -a "$SSHD_CONFIG" "$current"
cat "$backup" >"$SSHD_CONFIG"

if ! "$SSHD_BIN" -t -f "$SSHD_CONFIG"; then
  cat "$current" >"$SSHD_CONFIG"
  fail "还原后的配置未通过 sshd -t，已退回切换后状态（现场保存在 $current）"
fi

bash -c "$RELOAD_CMD"

echo "已回滚到：$backup"
echo "AuthorizedKeysFile 已恢复为备份中的取值（默认 .ssh/authorized_keys）"
