#!/usr/bin/env bash
# test-mysql.sh —— 用临时 MySQL 8.4 容器跑单测，结束即删除容器，绝不留常驻实例。
#
# 用法：scripts/test-mysql.sh [go test 的额外参数，例如 -race]
#   - 已设置 TEST_MYSQL_HOST 时直接复用该实例（不另起容器）。
#   - 未装 docker 时优雅退化为「跳过 MySQL 用例」（go test 仍会跑纯函数用例）。
#
# 镜像固定 mysql:8.4（与云库同款）：MariaDB 不认 INSERT ... ON DUPLICATE KEY UPDATE 等
# MySQL 8.0.19+ 语法，用错引擎会把实现误判成 bug。
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PKGS="./cmd/... ./internal/... ./tools/..."
PORT="${TEST_MYSQL_DOCKER_PORT:-13579}"
NAME="asg-test-mysql-$PORT"
IMG="${MYSQL_TEST_IMG:-mysql:8.4}"
PW="asgpw$RANDOM$RANDOM"
CONTAINER_STARTED=0

cleanup() {
  if [ "$CONTAINER_STARTED" = "1" ]; then
    docker rm -f "$NAME" >/dev/null 2>&1 && echo "[test-mysql] 已删除临时容器 $NAME"
  fi
}
trap cleanup EXIT

cd "$ROOT"

if [ -z "${TEST_MYSQL_HOST:-}" ]; then
  if command -v docker >/dev/null 2>&1; then
    echo "[test-mysql] 启动临时 MySQL 8.4：127.0.0.1:$PORT"
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    docker run -d --name "$NAME" -p "127.0.0.1:$PORT:3306" \
      -e MYSQL_ROOT_PASSWORD="$PW" \
      --health-cmd="mysqladmin ping -h127.0.0.1 -uroot -p$PW" \
      --health-interval=2s --health-timeout=4s --health-retries=60 \
      "$IMG" >/dev/null
    CONTAINER_STARTED=1
    for _ in $(seq 1 90); do
      st=$(docker inspect -f '{{.State.Health.Status}}' "$NAME" 2>/dev/null || echo none)
      [ "$st" = "healthy" ] && break
      sleep 2
    done
    if [ "$(docker inspect -f '{{.State.Health.Status}}' "$NAME" 2>/dev/null)" != "healthy" ]; then
      echo "[test-mysql] 容器未就绪，跳过 MySQL 用例" >&2
    else
      export TEST_MYSQL_HOST=127.0.0.1
      export TEST_MYSQL_PORT="$PORT"
      export TEST_MYSQL_USER=root
      export TEST_MYSQL_PASSWORD="$PW"
    fi
  else
    echo "[test-mysql] 未找到 docker，跳过 MySQL 用例" >&2
  fi
fi

CGO_ENABLED=0 go test $PKGS -count=1 "$@"
rc=$?
exit $rc
