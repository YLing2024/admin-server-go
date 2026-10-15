package notifications

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"admin-server-go/internal/store"
)

// cleanupInterval 是保留期清理循环的间隔（旧实现为每小时一次）。
const cleanupInterval = time.Hour

// notificationColumns 是通知表的显式列清单（列顺序与扫描顺序一致）。
const notificationColumns = "id, ts, level, source, type, title, body, link, read_at"

// placeholders 生成 n 个 `?` 的逗号分隔串（n 由内部长度决定，无注入面）。
func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func isNoRows(err error) bool { return err == sql.ErrNoRows }

// writeInput 是写入通知的入参（level 已由调用方校验；缺省时取类别默认级别）。
type writeInput struct {
	levelInput string
	source     string
	typeKey    string
	title      string
	body       *string
	link       *string
	dedupKey   *string
}

// writeResult 是写入结果；deduped 表示命中去重、更新了既有行。
type writeResult struct {
	ID      int64
	Ts      int64
	Level   string
	Deduped bool
	Item    NotificationItem
}

// ensureNotificationType 未注册类别自动注册：key 当 label、enabled=1。
// 失败只 warn，绝不阻断写入（注册表出问题也不能丢通知）。
func (s *Service) ensureNotificationType(ctx context.Context, key string) {
	if key == "" || s.db == nil {
		return
	}
	if _, err := s.db.SQL.ExecContext(ctx,
		"INSERT IGNORE INTO notification_types (`key`, label, description, default_level, sort, enabled, archived_at) VALUES (?, ?, NULL, NULL, 0, 1, NULL)",
		key, key); err != nil {
		s.log.Warn("通知类别自动注册失败（不影响写入）", "key", key, "error", err)
	}
}

// notificationDefaultLevel 取类别默认级别：default_level 合法才用，否则 normal。
func (s *Service) notificationDefaultLevel(ctx context.Context, key string) string {
	if s.db == nil || key == "" {
		return "normal"
	}
	var lvl sql.NullString
	if err := s.db.SQL.QueryRowContext(ctx, "SELECT default_level FROM notification_types WHERE `key` = ?", key).Scan(&lvl); err != nil {
		return "normal"
	}
	if lvl.Valid && store.NotificationLevels[lvl.String] {
		return lvl.String
	}
	return "normal"
}

// writeNotification 写入通知（含 dedupKey 去重）：同一 dedupKey 10 分钟内更新该行并置未读，不新增。
func (s *Service) writeNotification(ctx context.Context, in writeInput) (writeResult, error) {
	now := nowSeconds()
	s.ensureNotificationType(ctx, in.typeKey)
	level := in.levelInput
	if level == "" {
		level = s.notificationDefaultLevel(ctx, in.typeKey)
	}
	if in.dedupKey != nil && *in.dedupKey != "" {
		var existingID int64
		err := s.db.SQL.QueryRowContext(ctx,
			"SELECT id FROM notifications WHERE dedup_key = ? AND ts >= ? ORDER BY id DESC LIMIT 1",
			*in.dedupKey, now-store.NotificationDedupWindowSeconds).Scan(&existingID)
		switch {
		case err == nil:
			if _, err := s.db.SQL.ExecContext(ctx,
				"UPDATE notifications SET ts=?, level=?, source=?, type=?, title=?, body=?, link=?, read_at=NULL WHERE id=?",
				now, level, in.source, in.typeKey, in.title, in.body, in.link, existingID); err != nil {
				return writeResult{}, err
			}
			row, err := s.fetchRow(ctx, existingID)
			if err != nil {
				return writeResult{}, err
			}
			return writeResult{ID: existingID, Ts: now, Level: level, Deduped: true, Item: s.view(row, nil)}, nil
		case err != sql.ErrNoRows:
			return writeResult{}, err
		}
	}
	id, err := s.db.Dialect().InsertReturningID(ctx, s.db.SQL,
		"INSERT INTO notifications (ts, level, source, type, title, body, link, dedup_key, read_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)",
		now, level, in.source, in.typeKey, in.title, in.body, in.link, in.dedupKey)
	if err != nil {
		return writeResult{}, err
	}
	row, err := s.fetchRow(ctx, id)
	if err != nil {
		return writeResult{}, err
	}
	return writeResult{ID: id, Ts: now, Level: level, Item: s.view(row, nil)}, nil
}

// fetchRow 按 id 取原始行。
func (s *Service) fetchRow(ctx context.Context, id int64) (notificationRow, error) {
	var row notificationRow
	err := s.db.SQL.QueryRowContext(ctx,
		"SELECT "+notificationColumns+" FROM notifications WHERE id = ?", id).
		Scan(&row.ID, &row.Ts, &row.Level, &row.Source, &row.Type, &row.Title, &row.Body, &row.Link, &row.ReadAt)
	return row, err
}

// listFilters 是列表筛选条件（unread 与 total 为全库计数）。
type listFilters struct {
	limit      int64
	before     *int64
	level      *string
	source     *string
	typ        *string
	unreadOnly bool
}

// listResult 是列表响应体。
type listResult struct {
	Items  []NotificationItem `json:"items"`
	Unread int64              `json:"unread"`
	Total  int64              `json:"total"`
}

// listNotifications 返回筛选后的通知、全库未读数与总数。
func (s *Service) listNotifications(ctx context.Context, f listFilters) (listResult, error) {
	where := []string{}
	args := []any{}
	if f.level != nil {
		where = append(where, "level = ?")
		args = append(args, *f.level)
	}
	if f.source != nil {
		where = append(where, "source = ?")
		args = append(args, *f.source)
	}
	if f.typ != nil {
		where = append(where, "type = ?")
		args = append(args, *f.typ)
	}
	if f.before != nil {
		where = append(where, "id < ?")
		args = append(args, *f.before)
	}
	if f.unreadOnly {
		where = append(where, "read_at IS NULL")
	}
	sqlText := "SELECT " + notificationColumns + " FROM notifications"
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	sqlText += " ORDER BY id DESC LIMIT ?"
	args = append(args, f.limit)
	rows, err := s.db.SQL.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return listResult{}, err
	}
	defer rows.Close()
	raw := []notificationRow{}
	ids := []int64{}
	for rows.Next() {
		var row notificationRow
		if err := rows.Scan(&row.ID, &row.Ts, &row.Level, &row.Source, &row.Type, &row.Title, &row.Body, &row.Link, &row.ReadAt); err != nil {
			return listResult{}, err
		}
		raw = append(raw, row)
		ids = append(ids, row.ID)
	}
	if err := rows.Err(); err != nil {
		return listResult{}, err
	}
	attMap, err := s.attachmentsForIds(ctx, ids)
	if err != nil {
		return listResult{}, err
	}
	items := make([]NotificationItem, 0, len(raw))
	for _, row := range raw {
		items = append(items, s.view(row, attMap[row.ID]))
	}
	unread, err := s.count(ctx, "SELECT COUNT(*) FROM notifications WHERE read_at IS NULL")
	if err != nil {
		return listResult{}, err
	}
	total, err := s.count(ctx, "SELECT COUNT(*) FROM notifications")
	if err != nil {
		return listResult{}, err
	}
	return listResult{Items: items, Unread: unread, Total: total}, nil
}

// count 执行单值 COUNT 查询。
func (s *Service) count(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := s.db.SQL.QueryRowContext(ctx, query, args...).Scan(&n)
	return n, err
}

// getNotification 单条详情：命中返回带附件的 item，未命中 false。
func (s *Service) getNotification(ctx context.Context, id int64) (NotificationItem, bool, error) {
	row, err := s.fetchRow(ctx, id)
	if err != nil {
		if isNoRows(err) {
			return NotificationItem{}, false, nil
		}
		return NotificationItem{}, false, err
	}
	metas, err := s.listAttachments(ctx, id)
	if err != nil {
		return NotificationItem{}, false, err
	}
	return s.view(row, metas), true, nil
}

// listResultTypes 是类别列表响应体。
type listResultTypes struct {
	Types []NotificationType `json:"types"`
}

// listNotificationTypes 列出未软删的类别（按 sort、label 排序，含当前库内统计）。
func (s *Service) listNotificationTypes(ctx context.Context) (listResultTypes, error) {
	rows, err := s.db.SQL.QueryContext(ctx,
		"SELECT t.`key`, t.label, t.description, t.default_level, t.sort, t.enabled, "+
			"(SELECT COUNT(*) FROM notifications n WHERE n.type = t.`key`) AS count, "+
			"(SELECT COUNT(*) FROM notifications n WHERE n.type = t.`key` AND n.read_at IS NULL) AS unread "+
			"FROM notification_types t WHERE t.archived_at IS NULL ORDER BY t.sort ASC, t.label ASC")
	if err != nil {
		return listResultTypes{}, err
	}
	defer rows.Close()
	out := []NotificationType{}
	for rows.Next() {
		var t NotificationType
		if err := rows.Scan(&t.Key, &t.Label, &t.Description, &t.DefaultLevel, &t.Sort, &t.Enabled, &t.Count, &t.Unread); err != nil {
			return listResultTypes{}, err
		}
		out = append(out, t)
	}
	return listResultTypes{Types: out}, rows.Err()
}

// typePatch 累积类别更新的 SET 片段；空 patch 表示无字段可改。
type typePatch struct {
	sets []string
	args []any
}

func (p *typePatch) set(column string, value any) {
	p.sets = append(p.sets, column+" = ?")
	p.args = append(p.args, value)
}

func (p *typePatch) empty() bool { return len(p.sets) == 0 }

func (p *typePatch) fields() int { return len(p.sets) }

// updateNotificationType 只更新传入的字段；返回受影响行数（0 表示类别不存在）。
func (s *Service) updateNotificationType(ctx context.Context, key string, patch typePatch) (int64, error) {
	if patch.empty() {
		return 0, nil
	}
	args := append(append([]any{}, patch.args...), key)
	res, err := s.db.SQL.ExecContext(ctx, "UPDATE notification_types SET "+strings.Join(patch.sets, ", ")+" WHERE `key` = ?", args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		// MySQL 未启用 CLIENT_FOUND_ROWS 时「值未变化」也返回 0；以存在性判定，避免把「无变化」误报成「不存在」。
		cnt, err := s.count(ctx, "SELECT COUNT(*) FROM notification_types WHERE `key` = ?", key)
		if err != nil {
			return 0, err
		}
		if cnt > 0 {
			return 1, nil
		}
	}
	return n, nil
}

// markNotificationRead 单条置已读（已读的不再改时间）。
func (s *Service) markNotificationRead(ctx context.Context, id int64) error {
	_, err := s.db.SQL.ExecContext(ctx, "UPDATE notifications SET read_at = ? WHERE id = ? AND read_at IS NULL", nowSeconds(), id)
	return err
}

// markAllNotificationsRead 全部已读，返回影响条数。
func (s *Service) markAllNotificationsRead(ctx context.Context) (int64, error) {
	res, err := s.db.SQL.ExecContext(ctx, "UPDATE notifications SET read_at = ? WHERE read_at IS NULL", nowSeconds())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// deleteNotification 删除单条（连同附件目录与元信息）。
func (s *Service) deleteNotification(ctx context.Context, id int64) error {
	s.deleteNotificationAttachments(ctx, []int64{id})
	_, err := s.db.SQL.ExecContext(ctx, "DELETE FROM notifications WHERE id = ?", id)
	return err
}

// bulkDeleteFilters 是批量删除的筛选（未读与已读互斥由调用方保证）。
type bulkDeleteFilters struct {
	level      *string
	source     *string
	unreadOnly bool
	readOnly   bool
}

// bulkDeleteNotifications 按筛选统计/删除；dryRun 只统计不删。
func (s *Service) bulkDeleteNotifications(ctx context.Context, f bulkDeleteFilters, dryRun bool) (int64, error) {
	where := []string{}
	args := []any{}
	if f.level != nil {
		where = append(where, "level = ?")
		args = append(args, *f.level)
	}
	if f.source != nil {
		where = append(where, "source = ?")
		args = append(args, *f.source)
	}
	if f.unreadOnly {
		where = append(where, "read_at IS NULL")
	}
	if f.readOnly {
		where = append(where, "read_at IS NOT NULL")
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = " WHERE " + strings.Join(where, " AND ")
	}
	if dryRun {
		return s.count(ctx, "SELECT COUNT(*) FROM notifications"+whereSQL, args...)
	}
	rows, err := s.db.SQL.QueryContext(ctx, "SELECT id FROM notifications"+whereSQL, args...)
	if err != nil {
		return 0, err
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	if len(ids) > 0 {
		s.deleteNotificationAttachments(ctx, ids)
	}
	res, err := s.db.SQL.ExecContext(ctx, "DELETE FROM notifications"+whereSQL, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// statsResult 是统计响应体。
type statsResult struct {
	Total   int64         `json:"total"`
	Unread  int64         `json:"unread"`
	Sources []sourceCount `json:"sources"`
}

// sourceCount 是来源计数项。
type sourceCount struct {
	Source string `json:"source"`
	Count  int64  `json:"count"`
}

// notificationStats 返回总数、未读数与按条数降序（同数按字典序）的来源分布。
func (s *Service) notificationStats(ctx context.Context) (statsResult, error) {
	total, err := s.count(ctx, "SELECT COUNT(*) FROM notifications")
	if err != nil {
		return statsResult{}, err
	}
	unread, err := s.count(ctx, "SELECT COUNT(*) FROM notifications WHERE read_at IS NULL")
	if err != nil {
		return statsResult{}, err
	}
	rows, err := s.db.SQL.QueryContext(ctx,
		"SELECT source, COUNT(*) AS count FROM notifications GROUP BY source ORDER BY count DESC, source ASC")
	if err != nil {
		return statsResult{}, err
	}
	defer rows.Close()
	sources := []sourceCount{}
	for rows.Next() {
		var sc sourceCount
		if err := rows.Scan(&sc.Source, &sc.Count); err != nil {
			return statsResult{}, err
		}
		sources = append(sources, sc)
	}
	return statsResult{Total: total, Unread: unread, Sources: sources}, rows.Err()
}

// cleanupExpired 删除保留期外的通知（连同附件目录与元信息）；失败只 warn，不影响其它接口。
func (s *Service) cleanupExpired() {
	if s.db == nil || s.cfg == nil {
		return
	}
	ctx := context.Background()
	cutoff := nowSeconds() - int64(s.cfg.NotificationRetentionSeconds)
	rows, err := s.db.SQL.QueryContext(ctx, "SELECT id FROM notifications WHERE ts < ?", cutoff)
	if err != nil {
		s.log.Warn("通知清理失败（不影响其它接口）", "error", err)
		return
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			s.log.Warn("通知清理失败（不影响其它接口）", "error", err)
			return
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		s.log.Warn("通知清理失败（不影响其它接口）", "error", err)
		return
	}
	if len(ids) > 0 {
		s.deleteNotificationAttachments(ctx, ids)
	}
	if _, err := s.db.SQL.ExecContext(ctx, "DELETE FROM notifications WHERE ts < ?", cutoff); err != nil {
		s.log.Warn("通知清理失败（不影响其它接口）", "error", err)
	}
}
