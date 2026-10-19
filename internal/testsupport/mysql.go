// Package testsupport 为需要真实 MySQL 的单测提供临时库夹具。
//
// 连接参数来自环境变量 TEST_MYSQL_HOST / TEST_MYSQL_PORT / TEST_MYSQL_USER /
// TEST_MYSQL_PASSWORD，由 scripts/test-mysql.sh 注入一个临时 MySQL 8.4 容器。
// 未设置 TEST_MYSQL_HOST 时用例自行跳过，因此直接 `go test` 不会因缺库而变红。
//
// 每个夹具建一个唯一命名的空 schema 并在测试结束 DROP，互不干扰；绝不使用 DB_DRIVER
// 或任何 SQLite 路径（存储一律 MySQL）。
package testsupport

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"admin-server-go/internal/mysql"
	"admin-server-go/internal/store"
)

// seq 为同一进程内的重复调用去重（t.TempDir 之外还要保证 schema 名唯一）。
var seq atomic.Uint64

// Metrics 建一个空的指标库 schema 并跑增量迁移，测试结束自动 DROP。
func Metrics(t *testing.T) *store.DB {
	t.Helper()
	dsn := SchemaDSN(t, "metrics")
	db, err := store.Open(context.Background(), mysql.New(), dsn, 8, func(ctx context.Context, db *sql.DB) error {
		return mysql.New().MigrateMetrics(ctx, db)
	})
	if err != nil {
		t.Fatalf("打开指标测试库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// SchemaDSN 建一个唯一命名的空 schema 并返回其 MySQL DSN，测试结束自动 DROP。
// 供需要自行打开目标的用例（如迁移工具）使用。
func SchemaDSN(t *testing.T, prefix string) string {
	t.Helper()
	env := readEnv(t)
	ctx := context.Background()
	name := fmt.Sprintf("asg_test_%s_%d_%d", prefix, os.Getpid(), seq.Add(1))
	admin, err := sql.Open("mysql", mysql.BuildDSN(env.host, env.port, "", env.user, env.password))
	if err != nil {
		t.Fatalf("打开 MySQL 管理连接失败: %v", err)
	}
	if _, err := admin.ExecContext(ctx, "CREATE DATABASE `"+name+"` DEFAULT CHARSET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
		_ = admin.Close()
		t.Fatalf("创建测试库 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.ExecContext(context.Background(), "DROP DATABASE IF EXISTS `"+name+"`")
		_ = admin.Close()
	})
	return mysql.BuildDSN(env.host, env.port, name, env.user, env.password)
}

type connEnv struct {
	host     string
	port     int
	user     string
	password string
}

// readEnv 读取并校验 TEST_MYSQL_* 环境变量；缺 host 时跳过用例。
func readEnv(t *testing.T) connEnv {
	t.Helper()
	host := strings.TrimSpace(os.Getenv("TEST_MYSQL_HOST"))
	if host == "" {
		t.Skip("未设置 TEST_MYSQL_HOST；请经 scripts/test-mysql.sh 运行以启用 MySQL 用例")
	}
	port := 3306
	if raw := strings.TrimSpace(os.Getenv("TEST_MYSQL_PORT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 65535 {
			t.Fatalf("TEST_MYSQL_PORT 非法: %q", raw)
		}
		port = n
	}
	user := strings.TrimSpace(os.Getenv("TEST_MYSQL_USER"))
	if user == "" {
		user = "root"
	}
	return connEnv{host: host, port: port, user: user, password: os.Getenv("TEST_MYSQL_PASSWORD")}
}
