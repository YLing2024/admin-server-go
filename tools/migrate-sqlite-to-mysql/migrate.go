// Package main 是 SQLite → MySQL 的一次性数据搬运工具（独立 go run，不被服务主程序 import）。
//
// 用途：把旧实现的两个 SQLite 库（metrics / notifications）按表搬进 MySQL 的
// admin_metrics / admin_notifications。源库一律以只读方式打开（只发 SELECT），
// 目标库只做增量迁移（CREATE TABLE IF NOT EXISTS / ADD COLUMN）后以批量 upsert 写入，
// 可重复执行（幂等）。逐表执行「源计数 → 拷贝 → 目标计数 → 抽样比对」。
//
// 指标 raw 表可达 80 万行以上，拷贝采用「多行一条 INSERT + 有限并发写事务」：
// 批次避免超长 SQL，写并发上限避免瞬时压垮目标实例；同一主键在源表内唯一，不会互相覆盖。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"admin-server-go/internal/store"
)

// tableSpec 描述一张待搬运的表及其主键列（抽样比对据此定位行）。
type tableSpec struct {
	table string
	keys  []string
}

// progressStep 是拷贝进度日志的步长（行）。
const progressStep = 100000

// metricsTables 是指标库的搬运顺序（raw 先行，聚合表随后）。
var metricsTables = []tableSpec{
	{"metrics", []string{"ts"}},
	{"metrics_1m", []string{"bucket_start"}},
	{"metrics_1h", []string{"bucket_start"}},
	{"metrics_1d", []string{"bucket_start"}},
}

// notificationTables 是通知库的搬运顺序（类别先于通知，附件最后）。
var notificationTables = []tableSpec{
	{"notification_types", []string{"key"}},
	{"notifications", []string{"id"}},
	{"notification_attachments", []string{"notification_id", "name"}},
}

// logicalDB 是一个逻辑库（指标 / 通知）的搬运定义。
type logicalDB struct {
	name    string
	srcPath string
	dstDSN  string
	tables  []tableSpec
	migrate func(context.Context, *sql.DB) error
}

// Options 控制搬运行为。
type Options struct {
	// DryRun=true 只统计与校验，不写目标库。
	DryRun bool
	// Verify=true 搬运后比对条数与抽样行，不一致即报错。
	Verify bool
	// Sample 是每表首尾各抽样的条数（0 用默认 5）。
	Sample int
	// Batch 是单条多行 INSERT 的行数上限（0 用默认 500）。
	Batch int
	// Concurrency 是并发写事务数上限（0 用默认 2）；源表内主键唯一，写不冲突。
	Concurrency int
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

// Summary 生成人读摘要。
func (r Report) Summary() string {
	var b strings.Builder
	for _, t := range r.Tables {
		fmt.Fprintf(&b, "%s.%s: 源 %d → 目标 %d（拷贝 %d，抽样 %d）\n",
			t.DB, t.Table, t.SourceRows, t.TargetRows, t.Copied, t.SampleChecked)
	}
	return b.String()
}

// Run 执行搬运并返回报告。logf 为 nil 时静默。
func Run(ctx context.Context, dbs []logicalDB, opts Options) (Report, error) {
	if opts.Sample <= 0 {
		opts.Sample = 5
	}
	if opts.Batch <= 0 {
		opts.Batch = 500
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var report Report
	for _, logical := range dbs {
		rep, err := runLogical(ctx, logical, opts, logf)
		report.Tables = append(report.Tables, rep.Tables...)
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// runLogical 打开源（只读）与目标（跑增量迁移），逐表搬运。
func runLogical(ctx context.Context, logical logicalDB, opts Options, logf func(string, ...any)) (Report, error) {
	var report Report
	src, err := openSource(ctx, logical.srcPath)
	if err != nil {
		return report, fmt.Errorf("打开源库 %s 失败: %w", logical.name, err)
	}
	defer src.Close()

	dst, err := store.Open(ctx, mysqlDialect(), logical.dstDSN, opts.Concurrency+2, logical.migrate)
	if err != nil {
		return report, fmt.Errorf("打开目标库 %s 失败: %w", logical.name, err)
	}
	defer dst.Close()

	for _, spec := range logical.tables {
		tr, err := copyTable(ctx, logical.name, spec, src, dst, opts)
		report.Tables = append(report.Tables, tr)
		if err != nil {
			return report, err
		}
		logf("%s.%s 源 %d 行，目标 %d 行，拷贝 %d 行", logical.name, spec.table, tr.SourceRows, tr.TargetRows, tr.Copied)
		if opts.Verify {
			logf("%s.%s 抽样校验 %d 行通过", logical.name, spec.table, tr.SampleChecked)
		}
	}
	return report, nil
}

// openSource 以只读方式打开 SQLite 源库（绝不写入源库）。
func openSource(ctx context.Context, path string) (*sql.DB, error) {
	db, err := sql.Open(sqliteDriverName, "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	// 只读源库单连接即可，避免多连接各自持有快照带来的不一致。
	db.SetMaxOpenConns(1)
	return db, nil
}

// copyTable 拷贝单表并（可选）校验：源计数 → 拷贝 → 目标计数 → 抽样比对。
func copyTable(ctx context.Context, dbName string, spec tableSpec, src *sql.DB, dst *store.DB, opts Options) (TableReport, error) {
	tr := TableReport{DB: dbName, Table: spec.table}
	srcCols, err := sqliteColumns(ctx, src, spec.table)
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
	keys := filterPresent(spec.keys, srcSet)
	if len(keys) == 0 {
		return tr, fmt.Errorf("%s.%s 主键列缺失（%v）", dbName, spec.table, spec.keys)
	}

	if err := countRows(ctx, src, spec.table, &tr.SourceRows); err != nil {
		return tr, fmt.Errorf("%s.%s 统计源表条数失败: %w", dbName, spec.table, err)
	}
	if !opts.DryRun {
		copied, err := copyRows(ctx, src, dst, spec.table, cols, keys, opts)
		if err != nil {
			return tr, fmt.Errorf("%s.%s 拷贝失败: %w", dbName, spec.table, err)
		}
		tr.Copied = copied
	}
	if err := countRows(ctx, dst.SQL, spec.table, &tr.TargetRows); err != nil {
		return tr, fmt.Errorf("%s.%s 统计目标表条数失败: %w", dbName, spec.table, err)
	}
	if opts.Verify {
		if !opts.DryRun && tr.SourceRows != tr.TargetRows {
			return tr, fmt.Errorf("%s.%s 条数不一致：源 %d，目标 %d", dbName, spec.table, tr.SourceRows, tr.TargetRows)
		}
		checked, err := sampleVerify(ctx, spec, cols, keys, src, dst, opts.Sample)
		if err != nil {
			return tr, fmt.Errorf("%s.%s 抽样校验失败: %w", dbName, spec.table, err)
		}
		tr.SampleChecked = checked
	}
	return tr, nil
}

// copyRows 以「多行 INSERT + 有限并发事务」把源表全部行 upsert 进目标，返回写入行数。
func copyRows(ctx context.Context, src *sql.DB, dst *store.DB, table string, cols, keys []string, opts Options) (int64, error) {
	selectSQL := "SELECT " + quoteList(cols) + " FROM " + quoteIdent(table)
	rows, err := src.QueryContext(ctx, selectSQL)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		copied   int64
		firstErr error
		errOnce  sync.Once
		wg       sync.WaitGroup
	)
	batches := make(chan [][]any, opts.Concurrency)
	fail := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}

	for i := 0; i < opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 每批行数可能不同（末批更短），按行数缓存语句，避免重复拼 SQL。
			cache := map[int]string{}
			for batch := range batches {
				if runCtx.Err() != nil {
					continue
				}
				upsert, ok := cache[len(batch)]
				if !ok {
					upsert = multiUpsertSQL(table, cols, keys, len(batch))
					cache[len(batch)] = upsert
				}
				if err := execBatch(runCtx, dst, table, upsert, cols, batch); err != nil {
					fail(err)
					continue
				}
				n := atomic.AddInt64(&copied, int64(len(batch)))
				if opts.Logf != nil && n/progressStep != (n-int64(len(batch)))/progressStep {
					opts.Logf("%s 已拷贝 %d 行", table, n)
				}
			}
		}()
	}

	batch := make([][]any, 0, opts.Batch)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			fail(err)
			break
		}
		batch = append(batch, vals)
		if len(batch) == opts.Batch {
			if !sendBatch(runCtx, batches, batch) {
				batch = nil
				break
			}
			batch = make([][]any, 0, opts.Batch)
		}
	}
	if err := rows.Err(); err != nil {
		fail(err)
	}
	if len(batch) > 0 {
		_ = sendBatch(runCtx, batches, batch)
	}
	close(batches)
	wg.Wait()
	if firstErr != nil {
		return atomic.LoadInt64(&copied), firstErr
	}
	if err := ctx.Err(); err != nil {
		return atomic.LoadInt64(&copied), err
	}
	return atomic.LoadInt64(&copied), nil
}

// sendBatch 投递一批数据；上下文取消时返回 false（调用方停止读取）。
func sendBatch(ctx context.Context, ch chan<- [][]any, batch [][]any) bool {
	select {
	case ch <- batch:
		return true
	case <-ctx.Done():
		return false
	}
}

// execBatch 在单事务内执行一条多行 upsert；写前关掉该会话的外键强制（忠实拷贝）。
func execBatch(ctx context.Context, dst *store.DB, table, upsert string, cols []string, batch [][]any) error {
	tx, err := dst.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := dst.Dialect().PrepareCopy(ctx, tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	args := make([]any, 0, len(batch)*len(cols))
	for _, row := range batch {
		args = append(args, row...)
	}
	if _, err := tx.ExecContext(ctx, upsert, args...); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("写入 %s 失败: %w", table, err)
	}
	return tx.Commit()
}

// multiUpsertSQL 生成一次可写 n 行的 INSERT ... ON DUPLICATE KEY UPDATE。
// n 行共享同一条预编译语句，减少往返；主键列不参与更新。
func multiUpsertSQL(table string, cols, keys []string, n int) string {
	if n < 1 {
		n = 1
	}
	placeholders := make([]string, len(cols))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	tuple := "(" + strings.Join(placeholders, ", ") + ")"
	values := strings.TrimSuffix(strings.Repeat(tuple+",", n), ",")
	base := "INSERT INTO " + quoteIdent(table) + " (" + quoteList(cols) + ") VALUES " + values
	var assignments []string
	for _, c := range cols {
		if contains(keys, c) {
			continue
		}
		assignments = append(assignments, quoteIdent(c)+" = VALUES("+quoteIdent(c)+")")
	}
	if len(assignments) == 0 {
		return base
	}
	return base + " ON DUPLICATE KEY UPDATE " + strings.Join(assignments, ", ")
}

// sampleVerify 按主键取源表首尾各 N 行，逐行到目标表按主键取回并逐列比对。
func sampleVerify(ctx context.Context, spec tableSpec, cols, keys []string, src *sql.DB, dst *store.DB, n int) (int, error) {
	order := " ORDER BY " + quoteList(keys)
	head, err := sampleRows(ctx, src, spec.table, cols, order+" ASC LIMIT "+itoa(n))
	if err != nil {
		return 0, err
	}
	tail, err := sampleRows(ctx, src, spec.table, cols, order+" DESC LIMIT "+itoa(n))
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
		target, err := lookupRow(ctx, dst, spec.table, cols, keys, keyVals)
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

// sampleRows 读取源表的头/尾若干行。
func sampleRows(ctx context.Context, src *sql.DB, table string, cols []string, suffix string) ([][]any, error) {
	rows, err := src.QueryContext(ctx, "SELECT "+quoteList(cols)+" FROM "+quoteIdent(table)+suffix)
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

// lookupRow 按主键从目标表取回一行（无则返回 nil）。
func lookupRow(ctx context.Context, dst *store.DB, table string, cols, keys []string, keyVals []any) ([]any, error) {
	where := make([]string, len(keys))
	for i, k := range keys {
		where[i] = quoteIdent(k) + " = ?"
	}
	q := "SELECT " + quoteList(cols) + " FROM " + quoteIdent(table) + " WHERE " + strings.Join(where, " AND ") + " LIMIT 1"
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	err := dst.SQL.QueryRowContext(ctx, q, keyVals...).Scan(ptrs...)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return vals, nil
}

// sqliteColumns 用 pragma_table_info 探测 SQLite 表列（表名为受控常量，无注入面）。
func sqliteColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return nil, err
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

// countRows 统计一张表的行数。
func countRows(ctx context.Context, db *sql.DB, table string, out *int64) error {
	return db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(table)).Scan(out)
}

// quoteIdent 用反引号引用标识符（SQLite 与 MySQL 都接受；key 是 MySQL 保留字）。
func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// quoteList 逐项引用并逗号连接。
func quoteList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = quoteIdent(n)
	}
	return strings.Join(out, ", ")
}

// keyValues 从整行中按 keys 顺序取出主键值。
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

// canonKey 把主键值序列拼成可比字符串。
func canonKey(vals []any) string {
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = canon(v)
	}
	return strings.Join(parts, "\x1f")
}

// canon 把跨驱动读到的值归一到可比较文本：null / 字节串 / 整数 / 浮点 / 布尔。
// SQLite 与 MySQL 驱动对同一逻辑值可能返回不同 Go 类型（如 []byte vs int64），必须归一。
func canon(v any) string {
	switch x := v.(type) {
	case nil:
		return "<nil>"
	case []byte:
		return string(x)
	case string:
		return x
	case int64:
		return fmt.Sprintf("%d", x)
	case int:
		return fmt.Sprintf("%d", x)
	case float64:
		if x == float64(int64(x)) {
			return fmt.Sprintf("%d", int64(x))
		}
		return fmt.Sprintf("%g", x)
	case bool:
		if x {
			return "1"
		}
		return "0"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// filterPresent 只保留源表实际存在的键列。
func filterPresent(keys []string, present map[string]bool) []string {
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

// itoa 避免为一个简单整数再引 strconv（与旧实现保持一致的轻量风格）。
func itoa(n int) string { return fmt.Sprintf("%d", n) }
