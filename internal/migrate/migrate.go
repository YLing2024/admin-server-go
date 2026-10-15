// Package migrate 实现 `admin-server-go migrate-data` 子命令的跨库搬运：
// 逐库逐表拷贝 + 条数比对 + 抽样校验。源库只读（SQLite 以 mode=ro 打开，MySQL 只发 SELECT），
// 绝不删除、清空或改动源库；目标库先跑增量迁移再以 upsert 写入，可重复执行（幂等）。
package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strconv"
	"strings"

	"admin-server-go/internal/mysql"
	"admin-server-go/internal/sqlite"
	"admin-server-go/internal/store"
)

// Side 描述迁移一侧（源或目标）。Metrics / Notifications 在 sqlite 下是文件路径，
// 在 mysql 下是 go-sql-driver 原生 DSN（含库名）。
type Side struct {
	Driver        string
	Metrics       string
	Notifications string
}

// Tables 与键。
var (
	metricsTables = []tableSpec{
		{"metrics", []string{"ts"}},
		{"metrics_1m", []string{"bucket_start"}},
		{"metrics_1h", []string{"bucket_start"}},
		{"metrics_1d", []string{"bucket_start"}},
	}
	notificationTables = []tableSpec{
		{"notification_types", []string{"key"}},
		{"notifications", []string{"id"}},
		{"notification_attachments", []string{"notification_id", "name"}},
	}
)

type tableSpec struct {
	table string
	keys  []string
}

// Options 控制搬运行为。
type Options struct {
	// DryRun=true 只统计与校验，不写目标库。
	DryRun bool
	// Verify=true 搬运后比对条数与抽样行，不一致即报错。
	Verify bool
	// Sample 是每表首尾各抽样的条数（0 用默认 5）。
	Sample int
	// Logf 输出进度（可为 nil）。
	Logf func(format string, args ...any)
}

// TableReport 是单表搬运结果。
type TableReport struct {
	DB            string
	Table         string
	SourceRows    int64
	TargetRows    int64
	Copied        int64
	SampleChecked int
}

// Report 是整次搬运结果。
type Report struct {
	Tables []TableReport
}

// logicalDB 描述一个逻辑库（两库共用一个搬运流程）。
type logicalDB struct {
	name   string
	tables []tableSpec
	// migrate 在目标库上跑该库的增量迁移。
	migrate func(context.Context, *sql.DB) error
}

func logicalDBs(d store.Dialect) []logicalDB {
	return []logicalDB{
		{name: "metrics", tables: metricsTables, migrate: d.MigrateMetrics},
		{name: "notifications", tables: notificationTables, migrate: d.MigrateNotifications},
	}
}

// Run 执行搬运并返回报告。
func Run(ctx context.Context, from, to Side, opts Options) (Report, error) {
	if opts.Sample <= 0 {
		opts.Sample = 5
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	srcDialect, err := dialectFor(from.Driver)
	if err != nil {
		return Report{}, err
	}
	dstDialect, err := dialectFor(to.Driver)
	if err != nil {
		return Report{}, err
	}

	var report Report
	for _, logical := range logicalDBs(dstDialect) {
		srcDB, err := openSide(ctx, srcDialect, from, logical, true)
		if err != nil {
			return report, fmt.Errorf("打开源库 %s 失败: %w", logical.name, err)
		}
		dstDB, err := openSide(ctx, dstDialect, to, logical, false)
		if err != nil {
			srcDB.Close()
			return report, fmt.Errorf("打开目标库 %s 失败: %w", logical.name, err)
		}
		for _, spec := range logical.tables {
			tr, err := copyTable(ctx, logical.name, spec, srcDB, dstDB, opts, logf)
			report.Tables = append(report.Tables, tr)
			if err != nil {
				dstDB.Close()
				srcDB.Close()
				return report, err
			}
		}
		dstDB.Close()
		srcDB.Close()
	}
	return report, nil
}

func dialectFor(driver string) (store.Dialect, error) {
	switch strings.ToLower(strings.TrimSpace(driver)) {
	case "sqlite":
		return sqlite.New(), nil
	case "mysql":
		return mysql.New(), nil
	default:
		return nil, fmt.Errorf("不支持的驱动「%s」（只能是 sqlite / mysql）", driver)
	}
}

// openSide 打开一侧的逻辑库。source=true 时只读且不跑迁移。
func openSide(ctx context.Context, d store.Dialect, side Side, logical logicalDB, source bool) (*store.DB, error) {
	dsn := side.Metrics
	if logical.name == "notifications" {
		dsn = side.Notifications
	}
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("缺少 %s 库的连接串", logical.name)
	}
	maxConns := 16
	var migrateFn func(context.Context, *sql.DB) error
	if d.Name() == "sqlite" {
		maxConns = 1
		if source {
			dsn = sqlite.ReadOnlyDSN(dsn)
		}
	}
	if !source {
		migrateFn = logical.migrate
	}
	return store.Open(ctx, d, dsn, maxConns, migrateFn)
}

// copyTable 拷贝单表并（可选）校验。
func copyTable(ctx context.Context, dbName string, spec tableSpec, src, dst *store.DB, opts Options, logf func(string, ...any)) (TableReport, error) {
	tr := TableReport{DB: dbName, Table: spec.table}
	srcCols, err := src.Dialect().TableColumns(ctx, src.SQL, spec.table)
	if err != nil {
		return tr, fmt.Errorf("%s.%s 读取源表结构失败: %w", dbName, spec.table, err)
	}
	dstCols, err := dst.Dialect().TableColumns(ctx, dst.SQL, spec.table)
	if err != nil {
		return tr, fmt.Errorf("%s.%s 读取目标表结构失败: %w", dbName, spec.table, err)
	}
	srcSet := map[string]bool{}
	for _, c := range srcCols {
		srcSet[c] = true
	}
	cols := make([]string, 0, len(dstCols))
	for _, c := range dstCols {
		if srcSet[c] {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return tr, fmt.Errorf("%s.%s 源表与目标表没有公共列", dbName, spec.table)
	}
	keys := filterKeys(spec.keys, srcSet)
	if len(keys) == 0 {
		return tr, fmt.Errorf("%s.%s 主键列缺失（%v）", dbName, spec.table, spec.keys)
	}

	if err := countRows(ctx, src, spec.table, &tr.SourceRows); err != nil {
		return tr, fmt.Errorf("%s.%s 统计源表条数失败: %w", dbName, spec.table, err)
	}
	if !opts.DryRun {
		copied, err := performCopy(ctx, spec, cols, keys, src, dst)
		if err != nil {
			return tr, err
		}
		tr.Copied = copied
	}
	if err := countRows(ctx, dst, spec.table, &tr.TargetRows); err != nil {
		return tr, fmt.Errorf("%s.%s 统计目标表条数失败: %w", dbName, spec.table, err)
	}
	logf("%s.%s 源 %d 行，目标 %d 行，拷贝 %d 行", dbName, spec.table, tr.SourceRows, tr.TargetRows, tr.Copied)

	if opts.Verify {
		if !opts.DryRun && tr.SourceRows != tr.TargetRows {
			return tr, fmt.Errorf("%s.%s 条数不一致：源 %d，目标 %d", dbName, spec.table, tr.SourceRows, tr.TargetRows)
		}
		checked, err := sampleVerify(ctx, spec, cols, keys, src, dst, opts.Sample)
		if err != nil {
			return tr, fmt.Errorf("%s.%s 抽样校验失败: %w", dbName, spec.table, err)
		}
		tr.SampleChecked = checked
		logf("%s.%s 抽样校验 %d 行通过", dbName, spec.table, checked)
	}
	return tr, nil
}

// performCopy 在单事务内以 upsert 逐行拷入目标库。
func performCopy(ctx context.Context, spec tableSpec, cols, keys []string, src, dst *store.DB) (int64, error) {
	selectSQL := "SELECT " + qi(src.Dialect(), cols) + " FROM " + src.Dialect().QuoteIdent(spec.table)
	upsert := dst.Dialect().UpsertSQL(spec.table, cols, keys)
	var copied int64
	err := dst.Tx(ctx, func(q store.Querier) error {
		if err := dst.Dialect().PrepareCopy(ctx, q); err != nil {
			return err
		}
		rows, err := src.SQL.QueryContext(ctx, selectSQL)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				return err
			}
			if _, err := q.ExecContext(ctx, upsert, vals...); err != nil {
				return fmt.Errorf("写入 %s 失败: %w", spec.table, err)
			}
			copied++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		// 老库缺 type 列时，目标新建的 type 列为 NULL，按 source 回填一次。
		if spec.table == "notifications" && contains(cols, "source") && !contains(cols, "type") {
			if _, err := q.ExecContext(ctx, "UPDATE notifications SET type = source WHERE type IS NULL"); err != nil {
				return fmt.Errorf("回填 notifications.type 失败: %w", err)
			}
		}
		return nil
	})
	return copied, err
}

// sampleVerify 按主键取源表首尾各 N 行，逐行到目标表按主键取回并逐列比对（跨库类型归一比较）。
func sampleVerify(ctx context.Context, spec tableSpec, cols, keys []string, src, dst *store.DB, n int) (int, error) {
	order := " ORDER BY " + qi(src.Dialect(), keys)
	head, err := sampleRows(ctx, src.SQL, src.Dialect(), spec.table, cols, order+" ASC LIMIT "+strconv.Itoa(n))
	if err != nil {
		return 0, err
	}
	tail, err := sampleRows(ctx, src.SQL, src.Dialect(), spec.table, cols, order+" DESC LIMIT "+strconv.Itoa(n))
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	checked := 0
	for _, row := range append(head, tail...) {
		keyVals := keyValues(cols, keys, row)
		keyID := canonKey(keyVals)
		if seen[keyID] {
			continue
		}
		seen[keyID] = true
		target, err := lookupRow(ctx, dst.SQL, dst.Dialect(), spec.table, cols, keys, keyVals)
		if err != nil {
			return checked, err
		}
		if target == nil {
			return checked, fmt.Errorf("目标缺少主键 %v 的行", keyVals)
		}
		for i, c := range cols {
			if canon(row[i]) != canon(target[i]) {
				return checked, fmt.Errorf("列 %s 不一致：源 %q，目标 %q（主键 %v）", c, canon(row[i]), canon(target[i]), keyVals)
			}
		}
		checked++
	}
	return checked, nil
}

func sampleRows(ctx context.Context, db *sql.DB, d store.Dialect, table string, cols []string, suffix string) ([][]any, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+qi(d, cols)+" FROM "+d.QuoteIdent(table)+suffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func lookupRow(ctx context.Context, db *sql.DB, d store.Dialect, table string, cols, keys []string, keyVals []any) ([]any, error) {
	where := make([]string, len(keys))
	for i, k := range keys {
		where[i] = d.QuoteIdent(k) + " = ?"
	}
	q := "SELECT " + qi(d, cols) + " FROM " + d.QuoteIdent(table) + " WHERE " + strings.Join(where, " AND ") + " LIMIT 1"
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	err := db.QueryRowContext(ctx, q, keyVals...).Scan(ptrs...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return vals, nil
}

func countRows(ctx context.Context, db *store.DB, table string, out *int64) error {
	return db.SQL.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+db.Dialect().QuoteIdent(table)).Scan(out)
}

// qi 用方言逐项引用标识符再逗号连接（列名可能撞关键字，如通知类别的 key）。
func qi(d store.Dialect, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = d.QuoteIdent(n)
	}
	return strings.Join(out, ", ")
}

func keyValues(cols, keys []string, row []any) []any {
	out := make([]any, len(keys))
	for i, k := range keys {
		for j, c := range cols {
			if c == k {
				out[i] = row[j]
				break
			}
		}
	}
	return out
}

func canonKey(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = canon(v)
	}
	return strings.Join(parts, "\x1f")
}

// canon 把跨驱动读到的值归一到可比较文本：null / 字节串 / 整数 / 浮点 / 布尔。
func canon(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", x)
	}
}

func filterKeys(keys []string, present map[string]bool) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if present[k] {
			out = append(out, k)
		}
	}
	return out
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// Summary 生成人读摘要。
func (r Report) Summary() string {
	var b strings.Builder
	for _, t := range r.Tables {
		fmt.Fprintf(&b, "%s.%s: 源 %d → 目标 %d（拷贝 %d，抽样 %d）\n",
			t.DB, t.Table, t.SourceRows, t.TargetRows, t.Copied, t.SampleChecked)
	}
	return b.String()
}
