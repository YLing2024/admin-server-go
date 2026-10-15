// Package mysql 是 admin-server-go 存储层的 MySQL 方言（github.com/go-sql-driver/mysql）。
//
// 差异全部收在这里：DSN 组装、DDL 类型（VARCHAR/BIGINT AUTO_INCREMENT/DOUBLE）、列探测
// （information_schema.columns）、自增主键回填（LastInsertId）、upsert（ON DUPLICATE KEY UPDATE）、
// seed（INSERT IGNORE）。迁移只做增量，绝不 DROP / TRUNCATE / 重建表。
//
// MySQL 下两个库映射为同一实例下的两个 schema，由 DSN 的库名选定；连接各自独立。
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"admin-server-go/internal/store"
)

// Dialect 实现 store.Dialect。
type Dialect struct{}

// New 返回 MySQL 方言。
func New() *Dialect { return &Dialect{} }

// Name 返回方言名。
func (d *Dialect) Name() string { return "mysql" }

// DriverName 返回 go-sql-driver/mysql 注册的驱动名。
func (d *Dialect) DriverName() string { return "mysql" }

// BuildDSN 由分段参数拼出 go-sql-driver 的 DSN。口令只进返回值，绝不落日志。
func BuildDSN(host string, port int, name, user, password string) string {
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = password
	cfg.Net = "tcp"
	cfg.Addr = fmt.Sprintf("%s:%d", host, port)
	cfg.DBName = name
	cfg.ParseTime = false
	cfg.Collation = "utf8mb4_unicode_ci"
	cfg.Loc = time.Local
	cfg.Timeout = 5 * time.Second
	cfg.ReadTimeout = 15 * time.Second
	cfg.WriteTimeout = 15 * time.Second
	return cfg.FormatDSN()
}

func metricsDDL() []string {
	cols := make([]string, len(store.MetricKeys))
	for i, k := range store.MetricKeys {
		cols[i] = k + " DOUBLE"
	}
	stmts := []string{
		"CREATE TABLE IF NOT EXISTS metrics (" +
			"ts BIGINT NOT NULL," + strings.Join(cols, ", ") + "," +
			"PRIMARY KEY (ts)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci",
	}
	for _, agg := range store.AggTables {
		stmts = append(stmts, "CREATE TABLE IF NOT EXISTS "+agg.Table+" ("+
			"bucket_start BIGINT NOT NULL,"+strings.Join(cols, ", ")+","+
			"sample_count BIGINT, updated_at BIGINT,"+
			"PRIMARY KEY (bucket_start)) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")
	}
	return stmts
}

var notificationsDDL = []string{
	`CREATE TABLE IF NOT EXISTS notifications (
		id         BIGINT NOT NULL AUTO_INCREMENT,
		ts         BIGINT NOT NULL,
		level      VARCHAR(16) NOT NULL,
		source     VARCHAR(190) NOT NULL,
		title      VARCHAR(255) NOT NULL,
		body       MEDIUMTEXT NULL,
		link       TEXT NULL,
		dedup_key  VARCHAR(190) NULL,
		read_at    BIGINT NULL,
		PRIMARY KEY (id),
		KEY idx_notifications_ts (ts),
		KEY idx_notifications_read_at (read_at),
		KEY idx_notifications_dedup_key (dedup_key),
		KEY idx_notifications_source (source)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS notification_types (
		key           VARCHAR(64) NOT NULL,
		label         VARCHAR(190) NOT NULL,
		description   TEXT NULL,
		default_level VARCHAR(16) NULL,
		sort          INT DEFAULT 0,
		enabled       TINYINT DEFAULT 1,
		archived_at   BIGINT NULL,
		PRIMARY KEY (key)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS notification_attachments (
		notification_id BIGINT NOT NULL,
		name            VARCHAR(255) NOT NULL,
		mime            VARCHAR(190) NOT NULL,
		size            BIGINT NOT NULL,
		PRIMARY KEY (notification_id, name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// MigrateMetrics 建指标 raw 表与三个物化桶（幂等）。
func (d *Dialect) MigrateMetrics(ctx context.Context, db *sql.DB) error {
	for _, stmt := range metricsDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行指标建表语句失败: %w", err)
		}
	}
	return nil
}

// MigrateNotifications 建通知相关表并补齐 type 列（老库缺列时加列 + 按 source 回填）。
func (d *Dialect) MigrateNotifications(ctx context.Context, db *sql.DB) error {
	for _, stmt := range notificationsDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行通知建表语句失败: %w", err)
		}
	}
	hasType, err := d.ColumnExists(ctx, db, "notifications", "type")
	if err != nil {
		return err
	}
	if !hasType {
		if _, err := db.ExecContext(ctx, "ALTER TABLE notifications ADD COLUMN type VARCHAR(64) NULL"); err != nil {
			return fmt.Errorf("补 notifications.type 列失败: %w", err)
		}
		if _, err := db.ExecContext(ctx, "UPDATE notifications SET type = source WHERE type IS NULL"); err != nil {
			return fmt.Errorf("回填 notifications.type 失败: %w", err)
		}
	}
	// MySQL 无 CREATE INDEX IF NOT EXISTS：重复建索引的 1061 直接忽略。
	if _, err := db.ExecContext(ctx, "CREATE INDEX idx_notifications_type ON notifications(type)"); err != nil && !strings.Contains(err.Error(), "Duplicate key name") {
		return fmt.Errorf("建 notifications.type 索引失败: %w", err)
	}
	for _, row := range defaultTypes {
		if _, err := db.ExecContext(ctx,
			"INSERT IGNORE INTO notification_types (key, label, description, default_level, sort, enabled, archived_at) VALUES (?, ?, NULL, ?, ?, 1, NULL)",
			row.key, row.label, row.level, row.sort); err != nil {
			return fmt.Errorf("预置通知类别失败: %w", err)
		}
	}
	return nil
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

// ColumnExists 用 information_schema.columns 探测列（限定当前库）。
func (d *Dialect) ColumnExists(ctx context.Context, q store.Querier, table, column string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?",
		table, column).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("读取表 %s 结构失败: %w", table, err)
	}
	return n > 0, nil
}

// TableColumns 返回表列名（按定义顺序）。
func (d *Dialect) TableColumns(ctx context.Context, q store.Querier, table string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT column_name FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? ORDER BY ordinal_position",
		table)
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

// InsertReturningID 用 Exec + LastInsertId 回填自增主键。
func (d *Dialect) InsertReturningID(ctx context.Context, q store.Querier, insertSQL string, args ...any) (int64, error) {
	res, err := q.ExecContext(ctx, insertSQL, args...)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// UpsertSQL 生成 ON DUPLICATE KEY UPDATE 形态。
func (d *Dialect) UpsertSQL(table string, columns, keyColumns []string) string {
	placeholders := make([]string, len(columns))
	for i := range columns {
		placeholders[i] = "?"
	}
	var assignments []string
	for _, c := range columns {
		if contains(keyColumns, c) {
			continue
		}
		assignments = append(assignments, c+" = VALUES("+c+")")
	}
	base := "INSERT INTO " + table + " (" + strings.Join(columns, ", ") + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
	if len(assignments) == 0 {
		return base
	}
	return base + " ON DUPLICATE KEY UPDATE " + strings.Join(assignments, ", ")
}

// PrepareCopy 关闭本会话的外键检查（忠实拷贝用）。
func (d *Dialect) PrepareCopy(ctx context.Context, q store.Querier) error {
	_, err := q.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS=0")
	return err
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}
