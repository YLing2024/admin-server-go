// Package logs 提供云 MySQL 里全量日志的只读检索。
//
// 数据由 loghub 采集（journald / nginx / docker / 文件源），表结构各不相同；
// 本包把它们统一成一条 DTO，供管理端按时间、关键字、来源查询。
// 只读：不建表、不写库、不改 loghub 的任何数据。
package logs

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Source 是日志来源标识，与 loghub 的表一一对应。
type Source string

const (
	SourceSystem    Source = "system"    // system_logs：journald 全单元
	SourceContainer Source = "container" // container_logs：docker
	SourceFile      Source = "file"      // file_logs：nginx 之外的业务文件日志
	SourceAccess    Source = "access"    // http_access：nginx access
	SourceError     Source = "error"     // http_error：nginx error
)

// 每张表在统一 DTO 里的字段映射：name 列、level 列。
var sourceSpec = map[Source]struct {
	table string
	// nameExpr / levelExpr / messageExpr 直接进 SQL：各表结构不同，
	// 尤其 http_access 没有 message 列，必须在这里拼出可读摘要。
	nameExpr    string
	levelExpr   string
	messageExpr string
	extraSearch []string
}{
	SourceSystem:    {table: "system_logs", nameExpr: "unit", levelExpr: "priority", messageExpr: "message", extraSearch: []string{"ident"}},
	SourceContainer: {table: "container_logs", nameExpr: "container", levelExpr: "stream", messageExpr: "message", extraSearch: []string{"image"}},
	SourceFile:      {table: "file_logs", nameExpr: "source_id", levelExpr: "level", messageExpr: "message", extraSearch: []string{"path"}},
	SourceAccess: {
		table:       "http_access",
		nameExpr:    "remote_addr",
		levelExpr:   "status",
		messageExpr: "CONCAT(IFNULL(method,'-'), ' ', IFNULL(path,'-'), ' → ', IFNULL(status,0), ' · ', IFNULL(bytes,0), 'B · ', ROUND(IFNULL(req_time,0),3), 's')",
		extraSearch: []string{"path", "method"},
	},
	SourceError: {table: "http_error", nameExpr: "'nginx'", levelExpr: "level", messageExpr: "message"},
}

// Entry 是统一后的日志行。
type Entry struct {
	TS      time.Time `json:"ts"`
	Source  string    `json:"source"`
	Name    string    `json:"name"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
}

// Query 是一次检索条件。零值表示不限。
type Query struct {
	Source Source
	// Q 是关键字，匹配 message 与各表的辅助列。
	Q string
	// Since 只取该时间之后的记录。
	Since time.Time
	// Limit 默认 200、上限 1000。
	Limit  int
	Offset int
}

// Result 是检索结果；HasMore 由多取一条判断，避免对大表做 COUNT。
type Result struct {
	Entries []Entry `json:"entries"`
	HasMore bool    `json:"hasMore"`
	Source  string  `json:"source"`
	Limit   int     `json:"limit"`
	Offset  int     `json:"offset"`
}

// Service 持有只读连接池。
type Service struct {
	db  *sql.DB
	log interface{ Warn(string, ...any) }
}

// New 构造检索服务；db 为指向日志库的连接池（由 main 负责打开与关闭）。
func New(db *sql.DB, log interface{ Warn(string, ...any) }) *Service {
	return &Service{db: db, log: log}
}

// Sources 返回支持的来源列表，供前端渲染切换。
func Sources() []string {
	return []string{string(SourceSystem), string(SourceContainer), string(SourceFile), string(SourceAccess), string(SourceError)}
}

// Query 执行检索。未识别的来源直接报错，避免拼出任意表名。
func (s *Service) Query(ctx context.Context, q Query) (Result, error) {
	spec, ok := sourceSpec[q.Source]
	if !ok {
		return Result{}, fmt.Errorf("未知日志来源: %q", q.Source)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	if limit > 1000 {
		limit = 1000
	}
	offset := q.Offset
	if offset < 0 {
		offset = 0
	}

	where := []string{"1=1"}
	args := []any{}
	if !q.Since.IsZero() {
		where = append(where, "ts >= ?")
		args = append(args, q.Since)
	}
	if kw := strings.TrimSpace(q.Q); kw != "" {
		like := "%" + escapeLike(kw) + "%"
		cols := append([]string{spec.messageExpr}, spec.extraSearch...)
		ors := make([]string, 0, len(cols))
		for _, c := range cols {
			ors = append(ors, c+" LIKE ?")
			args = append(args, like)
		}
		where = append(where, "("+strings.Join(ors, " OR ")+")")
	}

	// 多取一条用于判断是否还有下一页。
	sqlText := fmt.Sprintf(
		"SELECT ts, COALESCE(%s,''), COALESCE(%s,''), COALESCE(%s,'') FROM %s WHERE %s ORDER BY ts DESC LIMIT %d OFFSET %d",
		spec.nameExpr, spec.levelExpr, spec.messageExpr, spec.table, strings.Join(where, " AND "), limit+1, offset,
	)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = rows.Close() }()

	entries := make([]Entry, 0, limit)
	for rows.Next() {
		var (
			rawTS   []byte
			name    string
			level   string
			message string
		)
		if err := rows.Scan(&rawTS, &name, &level, &message); err != nil {
			return Result{}, err
		}
		ts, ok := parseDBTime(string(rawTS))
		if !ok {
			continue
		}
		if len(entries) == limit {
			// 第 limit+1 条只用于标记 hasMore，不返回。
			return Result{Entries: entries, HasMore: true, Source: string(q.Source), Limit: limit, Offset: offset}, nil
		}
		entries = append(entries, Entry{
			TS:      ts.UTC(),
			Source:  string(q.Source),
			Name:    name,
			Level:   normalizeLevel(string(q.Source), level),
			Message: message,
		})
	}
	if err := rows.Err(); err != nil {
		return Result{}, err
	}
	return Result{Entries: entries, HasMore: false, Source: string(q.Source), Limit: limit, Offset: offset}, nil
}

// normalizeLevel 把各表的 level 列统一成可读文本：
// journald 的 priority 是 0-7 数字，access 的 status 是 HTTP 码，其余原样。
func normalizeLevel(source, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	switch source {
	case string(SourceSystem):
		switch raw {
		case "0", "1", "2":
			return "error"
		case "3":
			return "warn"
		case "4":
			return "notice"
		case "5":
			return "info"
		case "6":
			return "info"
		case "7":
			return "debug"
		}
		return raw
	case string(SourceAccess):
		if code, err := strconv.Atoi(raw); err == nil {
			switch {
			case code >= 500:
				return "error"
			case code >= 400:
				return "warn"
			default:
				return "info"
			}
		}
	}
	return raw
}

// escapeLike 转义 LIKE 的通配符，避免用户输入 % 把整表扫出来。
func escapeLike(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_")
	return r.Replace(s)
}

// parseDBTime 解析 MySQL DATETIME(3) 的文本形式。
// 驱动未开 parseTime，这里统一按 UTC 解释（库内写入的就是 UTC 语义）。
func parseDBTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	layouts := []string{
		"2006-01-02 15:04:05.999999",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, raw, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
