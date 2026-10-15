// Package store 是 admin-server-go 两个数据库（指标 metrics、通知 notifications）的统一访问层。
//
// 两库的差异（DDL、列存在性探测、自增主键回填、upsert 形态、时间列读取）全部下沉到 Dialect
// 接口的两套实现（internal/sqlite / internal/mysql），业务层不感知驱动。所有查询使用 `?`
// 占位符，不做字符串拼接。迁移只做增量：绝不 DROP / TRUNCATE / 重建表。
package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Querier 是 *sql.DB 与 *sql.Tx 的公共子集。
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Dialect 抽象两库差异。两套实现见 internal/sqlite 与 internal/mysql。
type Dialect interface {
	// Name 返回 "sqlite" / "mysql"。
	Name() string
	// DriverName 返回 database/sql 驱动名。
	DriverName() string
	// MigrateMetrics 增量迁移指标库（raw + 分钟/小时/天物化桶）。
	MigrateMetrics(ctx context.Context, db *sql.DB) error
	// MigrateNotifications 增量迁移通知库（通知表 / 类别表 / 附件元信息表 + type 列回填）。
	MigrateNotifications(ctx context.Context, db *sql.DB) error
	// ColumnExists 探测列是否存在。
	ColumnExists(ctx context.Context, q Querier, table, column string) (bool, error)
	// TableColumns 返回表列名（按定义顺序）。
	TableColumns(ctx context.Context, q Querier, table string) ([]string, error)
	// QuoteIdent 引用标识符（表/列名）以防撞上关键字保留字。
	// 两库都用反引号：MySQL 必需（如 key 是保留字），SQLite 兼容接受。
	QuoteIdent(name string) string
	// InsertReturningID 执行单行 INSERT 并回填自增主键。
	InsertReturningID(ctx context.Context, q Querier, insertSQL string, args ...any) (int64, error)
	// UpsertSQL 返回按主键冲突即更新的 INSERT（跨库搬运幂等写入）。
	UpsertSQL(table string, columns, keyColumns []string) string
	// CastInt 把表达式截断取整（SQLite 用 CAST(... AS INTEGER)，MySQL 用 CAST(... AS SIGNED)）。
	// 两库的 CAST 目标类型名不同，业务 SQL 不得自行写死，否则另一库直接语法报错。
	CastInt(expr string) string
	// PrepareCopy 在跨库搬运前把连接置为「忠实拷贝」模式（关外键强制）。
	PrepareCopy(ctx context.Context, q Querier) error
}

// DB 是数据库句柄 + 方言。
type DB struct {
	SQL     *sql.DB
	dialect Dialect
}

// Dialect 返回当前方言。
func (db *DB) Dialect() Dialect { return db.dialect }

// Driver 返回驱动名。
func (db *DB) Driver() string { return db.dialect.Name() }

// Open 打开数据库并跑指定迁移。maxOpenConns 对 SQLite 场景取 1，避免写锁竞争。
func Open(ctx context.Context, dialect Dialect, dsn string, maxOpenConns int, migrate func(context.Context, *sql.DB) error) (*DB, error) {
	sqlDB, err := sql.Open(dialect.DriverName(), dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	if maxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(maxOpenConns)
		sqlDB.SetMaxIdleConns(maxOpenConns)
	}
	sqlDB.SetConnMaxLifetime(0)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	if migrate != nil {
		if err := migrate(ctx, sqlDB); err != nil {
			_ = sqlDB.Close()
			return nil, err
		}
	}
	return &DB{SQL: sqlDB, dialect: dialect}, nil
}

// Close 关闭连接池。
func (db *DB) Close() error {
	if db == nil || db.SQL == nil {
		return nil
	}
	return db.SQL.Close()
}

// Tx 在单事务内执行 fn；fn 返回错误则回滚。
func (db *DB) Tx(ctx context.Context, fn func(q Querier) error) error {
	tx, err := db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	return tx.Commit()
}
