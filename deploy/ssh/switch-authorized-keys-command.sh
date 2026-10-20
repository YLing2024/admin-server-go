#!/usr/bin/env bash
# 把 sshd 的 authorized_keys 文件方式切换为 AuthorizedKeysCommand 实时查询。
#
# 设计要点：
#   - 改动前整份备份 sshd_config；`sshd -t` 失败时自动还原，绝不 reload 坏配置；
#   - 只改全局段（首个 Match 之前）的四条指令，Match 内的配置原样保留；
#   - 幂等：检测到托管标记后直接退出 0；回滚见 rollback-authorized-keys-command.sh。
#
# 可用环境变量覆盖（便于在临时配置上演练，脚本默认只动生产路径）：
#   SSHD_CONFIG / SSHD_BIN / SSH_KEYS_BIN / SSH_CONFIG_BACKUP_DIR /
#   SSHD_AUTHORIZED_KEYS_COMMAND_USER / SSHD_RELOAD_CMD
set -euo pipefail

SSHD_CONFIG="${SSHD_CONFIG:-/etc/ssh/sshd_config}"
SSHD_BIN="${SSHD_BIN:-/usr/sbin/sshd}"
SSH_KEYS_BIN="${SSH_KEYS_BIN:-/usr/local/bin/ssh-keys-from-admin}"
BACKUP_DIR="${SSH_CONFIG_BACKUP_DIR:-/etc/ssh/backups}"
COMMAND_USER="${SSHD_AUTHORIZED_KEYS_COMMAND_USER:-nobody}"
RELOAD_CMD="${SSHD_RELOAD_CMD:-systemctl reload ssh}"
MARKER="# BEGIN admin-server-go authorized keys command"

fail() {
  echo "切换失败：$*" >&2
  exit 1
}

[[ -f "$SSHD_CONFIG" ]] || fail "找不到 sshd_config：$SSHD_CONFIG"
[[ -x "$SSHD_BIN" ]] || fail "找不到可执行的 sshd：$SSHD_BIN"
[[ -x "$SSH_KEYS_BIN" ]] || fail "找不到可执行的辅助命令：$SSH_KEYS_BIN（先 make build-ssh-keys 并安装到该路径）"
id "$COMMAND_USER" >/dev/null 2>&1 || fail "AuthorizedKeysCommandUser 不存在：$COMMAND_USER"

if grep -qF "$MARKER" "$SSHD_CONFIG"; then
  echo "已切换过，无需重复操作：$SSHD_CONFIG"
  exit 0
fi

# sshd 对同一关键字取「第一个」值；Include 进来的 drop-in 可能抢先，必须在改之前拦住。
conf_dir="$(dirname "$SSHD_CONFIG")/sshd_config.d"
if [[ -d "$conf_dir" ]] && grep -rEq '^[[:space:]]*(AuthorizedKeysCommand|AuthorizedKeysCommandUser|AuthorizedKeysFile|PubkeyAuthentication)[[:space:]]' "$conf_dir"; then
  fail "$conf_dir 里有冲突的 sshd 指令，请先移除或合并后再切换"
fi

mkdir -p "$BACKUP_DIR"
stamp="$(date +%Y%m%d-%H%M%S)"
backup="$BACKUP_DIR/sshd_config.$stamp.bak"
cp -a "$SSHD_CONFIG" "$backup"
printf '%s\n' "$backup" >"$BACKUP_DIR/authorized-keys-command.latest"
echo "已备份 sshd_config：$backup"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT
awk -v bin="$SSH_KEYS_BIN" -v user="$COMMAND_USER" -v marker="$MARKER" '
  function emit_block() {
    if (emitted) return
    print marker
    print "AuthorizedKeysCommand " bin " %u %f"
    print "AuthorizedKeysCommandUser " user
    print "AuthorizedKeysFile none"
    print "PubkeyAuthentication yes"
    print "# END admin-server-go authorized keys command"
    emitted = 1
  }
  /^[[:space:]]*Match[[:space:]]/ {
    emit_block()
    in_match = 1
    print
    next
  }
  in_match { print; next }
  /^[[:space:]]*AuthorizedKeysCommand[[:space:]]/ { next }
  /^[[:space:]]*AuthorizedKeysCommandUser[[:space:]]/ { next }
  /^[[:space:]]*AuthorizedKeysFile[[:space:]]/ { next }
  /^[[:space:]]*PubkeyAuthentication[[:space:]]/ { next }
  { print }
  END { emit_block() }
' "$SSHD_CONFIG" >"$tmp"

cat "$tmp" >"$SSHD_CONFIG"

if ! "$SSHD_BIN" -t -f "$SSHD_CONFIG"; then
  cat "$backup" >"$SSHD_CONFIG"
  fail "sshd -t 未通过，已还原原配置"
fi

# reload 只在配置校验通过后执行；SSHD_RELOAD_CMD=true 可在演练时跳过。
bash -c "$RELOAD_CMD"

echo "已切换为 AuthorizedKeysCommand：$SSH_KEYS_BIN"
echo "回滚：SSHD_CONFIG=$SSHD_CONFIG $(dirname "$0")/rollback-authorized-keys-command.sh"
