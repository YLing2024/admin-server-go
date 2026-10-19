package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// 通知库的建表与增量迁移。
//
// 通知中心功能已于 2026-10-19 从主服务整体下线（见 docs/REQUIREMENTS-remove-notifications.md），
// 服务不再连接 admin_notifications，故这份 schema 知识不再属于 internal/mysql；但旧 SQLite 通知库到
// MySQL 的一次性搬运链路保留在本工具内，于是随工具携带自己的一份 DDL 与迁移逻辑（工具不被服务主程序
// import，不会把通知 schema 带回服务运行期）。
//
// 口径与原 internal/mysql.MigrateNotifications 完全一致：CREATE TABLE IF NOT EXISTS、列探测后补
// type 列并按 source 回填、忽略重复建索引、INSERT IGNORE 预置类别；绝不 DROP / TRUNCATE。

// typeKeyCol 是通知类别表主键列名。key 在 MySQL 是保留字必须引用；
// 而 DDL 用反引号原生字符串书写（无法内嵌反引号），故对该标识符单独拼接。
const typeKeyCol = "`key`"

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
		` + typeKeyCol + `         VARCHAR(64) NOT NULL,
		label         VARCHAR(190) NOT NULL,
		description   TEXT NULL,
		default_level VARCHAR(16) NULL,
		sort          INT DEFAULT 0,
		enabled       TINYINT DEFAULT 1,
		archived_at   BIGINT NULL,
		PRIMARY KEY (` + typeKeyCol + `)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
	`CREATE TABLE IF NOT EXISTS notification_attachments (
		notification_id BIGINT NOT NULL,
		name            VARCHAR(255) NOT NULL,
		mime            VARCHAR(190) NOT NULL,
		size            BIGINT NOT NULL,
		PRIMARY KEY (notification_id, name)
	) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci`,
}

// migrateNotifications 建通知相关表并补齐 type 列（老库缺列时加列 + 按 source 回填），
// 再以 INSERT IGNORE 预置默认类别。幂等，绝不丢已有行。
func migrateNotifications(ctx context.Context, db *sql.DB) error {
	for _, stmt := range notificationsDDL {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("执行通知建表语句失败: %w", err)
		}
	}
	dialect := mysqlDialect()
	hasType, err := dialect.ColumnExists(ctx, db, "notifications", "type")
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
	for _, row := range defaultNotificationTypes {
		if _, err := db.ExecContext(ctx,
			"INSERT IGNORE INTO notification_types ("+typeKeyCol+", label, description, default_level, sort, enabled, archived_at) VALUES (?, ?, NULL, ?, ?, 1, NULL)",
			row.key, row.label, row.level, row.sort); err != nil {
			return fmt.Errorf("预置通知类别失败: %w", err)
		}
	}
	return nil
}

type notificationTypeSeed struct {
	key, label, level string
	sort              int
}

var defaultNotificationTypes = []notificationTypeSeed{
	{"watchdog", "看门狗", "urgent", 10},
	{"monitor", "站点监控", "normal", 20},
	{"cron", "定时播报", "normal", 30},
	{"hermes", "Hermes", "normal", 40},
	{"admin", "管理后台", "normal", 50},
}
