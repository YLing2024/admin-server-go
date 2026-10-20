// Package mysql 是 admin-server-go 存储层的 MySQL 方言实现（github.com/go-sql-driver/mysql，
// 自 2026-10-16 起为唯一实现）。集中处理：DSN 组装、DDL 类型（VARCHAR/BIGINT AUTO_INCREMENT/DOUBLE）、
// 列探测（information_schema.columns）、自增主键回填（LastInsertId）、upsert（ON DUPLICATE KEY UPDATE）。
// 迁移只做增量，绝不 DROP / TRUNCATE / 重建表。
//
// 服务连接单个指标逻辑库，映射为 MySQL 实例下的一个 schema，由 DSN 的库名选定。
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

// MigrateMetrics 建指标 raw 表与三个物化桶（幂等），并为老表补齐 MetricKeys 里新增的列。
// 只做 ADD COLUMN，绝不重建表、绝不丢旧行；旧行新列为 NULL。
func (d *Dialect) MigrateMetrics(ctx context.Context, db *sql.DB) error {
	for _, stmt := range metricsDDL() {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行指标建表语句失败: %w", err)
		}
	}
	tables := make([]string, 0, len(store.AggTables)+1)
	tables = append(tables, "metrics")
	for _, agg := range store.AggTables {
		tables = append(tables, agg.Table)
	}
	for _, table := range tables {
		for _, col := range store.MetricKeys {
			exists, err := d.ColumnExists(ctx, db, table, col)
			if err != nil {
				return err
			}
			if exists {
				continue
			}
			if _, err := db.ExecContext(ctx, "ALTER TABLE "+quoteIdent(table)+" ADD COLUMN "+quoteIdent(col)+" DOUBLE"); err != nil {
				return fmt.Errorf("补 %s.%s 列失败: %w", table, col, err)
			}
		}
	}
	return nil
}

// MigrateSSHKeys 建 SSH 公钥白名单表（幂等）。
//
// 表与指标表同库（DB_METRICS_NAME），这样服务与只读辅助命令共用一套连接参数。
// 只允许 CREATE TABLE IF NOT EXISTS，绝不 DROP / TRUNCATE / 重建。
func (d *Dialect) MigrateSSHKeys(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS ssh_key ("+
		"id BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,"+
		"title VARCHAR(100) NOT NULL,"+
		"public_key TEXT NOT NULL,"+
		"fingerprint CHAR(64) NOT NULL,"+
		"key_type VARCHAR(32) NOT NULL,"+
		"comment VARCHAR(200) NOT NULL DEFAULT '',"+
		"enabled TINYINT(1) NOT NULL DEFAULT 1,"+
		"created_at DATETIME NOT NULL,"+
		"last_used_at DATETIME NULL,"+
		"PRIMARY KEY (id),"+
		"UNIQUE KEY uk_fingerprint (fingerprint)"+
		") ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci")
	if err != nil {
		return fmt.Errorf("创建 ssh_key 表失败: %w", err)
	}
	return nil
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
		assignments = append(assignments, quoteIdent(c)+" = VALUES("+quoteIdent(c)+")")
	}
	base := "INSERT INTO " + quoteIdent(table) + " (" + quoteIdents(columns) + ") VALUES (" + strings.Join(placeholders, ", ") + ")"
	if len(assignments) == 0 {
		return base
	}
	return base + " ON DUPLICATE KEY UPDATE " + strings.Join(assignments, ", ")
}

// QuoteIdent 用反引号引用标识符（MySQL 里 key 等是保留字，必须引用）。
func (d *Dialect) QuoteIdent(name string) string { return quoteIdent(name) }

// quoteIdent 用反引号引用单个标识符，内部反引号翻倍转义。
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quoteIdents 逐项引用并逗号连接。
func quoteIdents(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return strings.Join(out, ", ")
}

// CastInt 把表达式截断取整（对齐旧 SQLite 的 CAST(... AS INTEGER) 语义）。
// 注意不能用 CAST(expr AS SIGNED)：MySQL 的 CAST 是「四舍五入到整数」，
// 会把 ts/60 的 2.98 进位成 3，导致分桶边界与旧实现不一致；先 TRUNCATE(x,0) 截断，
// 再 CAST 成 SIGNED 只是为了让后续乘法得到整数类型（否则返回 DECIMAL 字符串，扫描 int64 会失败）。
func (d *Dialect) CastInt(expr string) string {
	return "CAST(TRUNCATE(" + expr + ", 0) AS SIGNED)"
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
