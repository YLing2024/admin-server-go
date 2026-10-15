// Package sqlite 是 admin-server-go 存储层的 SQLite 方言（modernc.org/sqlite，纯 Go、无 cgo）。
//
// 与 MySQL 方言的差异全部收在这里：DSN 与 PRAGMA、DDL 类型、列存在性探测（pragma_table_info）、
// 自增主键回填（RETURNING id）、upsert 形态（ON CONFLICT ... DO UPDATE）、seed 语句（INSERT OR IGNORE）。
// 迁移只做增量（CREATE TABLE IF NOT EXISTS / ADD COLUMN），绝不 DROP、TRUNCATE 或重建表 —
// 现网老库（缺 notifications.type 列、缺聚合表）必须原地继续可用。
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"admin-server-go/internal/store"

	// 注册 database/sql 驱动名 "sqlite"（modernc 纯 Go 实现，无 cgo）。
	_ "modernc.org/sqlite"
)

// Dialect 实现 store.Dialect。
type Dialect struct{}

// New 返回 SQLite 方言。
func New() *Dialect { return &Dialect{} }

// Name 返回方言名。
func (d *Dialect) Name() string { return "sqlite" }

// DriverName 返回 modernc.org/sqlite 注册的驱动名。
func (d *Dialect) DriverName() string { return "sqlite" }

// DSN 组装连接串。PRAGMA 口径对齐旧实现 db.ts：WAL + synchronous=NORMAL + busy_timeout=8000
// （写与聚合补算并发时避免 SQLITE_BUSY）。
func DSN(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=busy_timeout(8000)"
}

// ReadOnlyDSN 是 migrate-data 读取源库用的只读连接串（mode=ro，绝不写源库）。
func ReadOnlyDSN(path string) string {
	return "file:" + path + "?mode=ro&_pragma=busy_timeout(8000)"
}

func metricsDDL() []string {
	cols := make([]string, len(store.MetricKeys))
	for i, k := range store.MetricKeys {
		cols[i] = k + " REAL"
	}
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS metrics (" +
			"ts INTEGER PRIMARY KEY," + strings.Join(cols, ", ") + ")",
	}
	for _, agg := range store.AggTables {
		stmts = append(stmts, "CREATE TABLE IF NOT EXISTS "+agg.Table+" ("+
			"bucket_start INTEGER PRIMARY KEY,"+strings.Join(cols, ", ")+","+
			"sample_count INTEGER, updated_at INTEGER)")
	}
	return stmts
}

var notificationsDDL = []string{
	`CREATE TABLE IF NOT EXISTS notifications (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL,
		level TEXT NOT NULL,
		source TEXT NOT NULL,
		title TEXT NOT NULL,
		body TEXT,
		link TEXT,
		dedup_key TEXT,
		read_at INTEGER
	)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_ts ON notifications(ts DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_read_at ON notifications(read_at)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_dedup_key ON notifications(dedup_key)`,
	`CREATE INDEX IF NOT EXISTS idx_notifications_source ON notifications(source)`,
	`CREATE TABLE IF NOT EXISTS notification_types (
		key TEXT PRIMARY KEY,
		label TEXT NOT NULL,
		description TEXT,
		default_level TEXT,
		sort INTEGER DEFAULT 0,
		enabled INTEGER DEFAULT 1,
		archived_at INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS notification_attachments (
		notification_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		mime TEXT NOT NULL,
		size INTEGER NOT NULL,
		PRIMARY KEY (notification_id, name)
	)`,
}

// MigrateMetrics 建指标 raw 表与三个物化桶（幂等）。
func (d *Dialect) MigrateMetrics(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	for _, stmt := range metricsDDL() {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("执行指标建表语句失败: %w", err)
		}
	}
	return tx.Commit()
}

// MigrateNotifications 建通知相关表并补齐 type 列（老库缺列时加列 + 按 source 回填）。
func (d *Dialect) MigrateNotifications(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启迁移事务失败: %w", err)
	}
	for _, stmt := range notificationsDDL {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("执行通知建表语句失败: %w", err)
		}
	}
	hasType, err := d.ColumnExists(ctx, tx, "notifications", "type")
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if !hasType {
		if _, err := tx.ExecContext(ctx, "ALTER TABLE notifications ADD COLUMN type TEXT"); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("补 notifications.type 列失败: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "UPDATE notifications SET type = source WHERE type IS NULL"); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("回填 notifications.type 失败: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, "CREATE INDEX IF NOT EXISTS idx_notifications_type ON notifications(type)"); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("建 notifications.type 索引失败: %w", err)
	}
	seed := `INSERT OR IGNORE INTO notification_types (key, label, description, default_level, sort, enabled, archived_at) VALUES (?, ?, NULL, ?, ?, 1, NULL)`
	for _, row := range defaultTypes {
		if _, err := tx.ExecContext(ctx, seed, row.key, row.label, row.level, row.sort); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("预置通知类别失败: %w", err)
		}
	}
	return tx.Commit()
}

type typeSeed struct {
	key, label, level string
	sort              int
}

var defaultTypes = []typeSeed{
	{"watchdog", "看门狗", "urgent", 10},
	{"monitor", "站点监控", "normal", 20},
	{"cron", "定时播报", "normal", 30},
	{"hermes", "Hermes", "normal", 40},
	{"admin", "管理后台", "normal", 50},
}

// ColumnExists 用 pragma_table_info 探测列（表名为受控常量，无注入面）。
func (d *Dialect) ColumnExists(ctx context.Context, q store.Querier, table, column string) (bool, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return false, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// TableColumns 返回表列名（按定义顺序）。
func (d *Dialect) TableColumns(ctx context.Context, q store.Querier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}

// InsertReturningID 用 RETURNING 一次拿回自增主键。
func (d *Dialect) InsertReturningID(ctx context.Context, q store.Querier, insertSQL string, args ...any) (int64, error) {
	var id int64
	if err := q.QueryRowContext(ctx, insertSQL+" RETURNING id", args...).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// UpsertSQL 生成 ON CONFLICT 形态。
func (d *Dialect) UpsertSQL(table string, columns, keyColumns []string) string {
	return upsert(table, columns, keyColumns, "excluded.")
}

// CastInt 用 SQLite 的整数类型名。
func (d *Dialect) CastInt(expr string) string {
	return "CAST(" + expr + " AS INTEGER)"
}

// PrepareCopy 关闭本连接的 SQLite 外键强制（忠实拷贝用）。
func (d *Dialect) PrepareCopy(ctx context.Context, q store.Querier) error {
	_, err := q.ExecContext(ctx, "PRAGMA foreign_keys=OFF")
	return err
}

func upsert(table string, columns, keyColumns []string, prefix string) string {
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = "?"
	}
	var assignments []string
	for _, c := range columns {
		if contains(keyColumns, c) {
			continue
		}
		assignments = append(assignments, c+" = "+prefix+c)
	}
	base := "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
	if len(assignments) == 0 {
		return base
	}
	return base + " ON CONFLICT(" + strings.Join(keyColumns, ", ") + ") DO UPDATE SET " + strings.Join(assignments, ", ")
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
