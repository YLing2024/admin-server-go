package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"admin-server-go/internal/testsupport"
)

// TestMultiUpsertSQLShape 守住批量 upsert 的形态：n 行 VALUES 元组 + 主键不更新。
func TestMultiUpsertSQLShape(t *testing.T) {
	got := multiUpsertSQL("t", []string{"a", "b"}, []string{"a"}, 2)
	want := "INSERT INTO `t` (`a`, `b`) VALUES (?, ?),(?, ?) ON DUPLICATE KEY UPDATE `b` = VALUES(`b`)"
	if got != want {
		t.Fatalf("多行 upsert 形态不符：\n got %q\nwant %q", got, want)
	}
}

// createSQLiteSource 造一个只含 (ts, cpu) 两列的 SQLite 源库，写入 n 行。
func createSQLiteSource(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.db")
	db, err := sql.Open(sqliteDriverName, "file:"+path)
	if err != nil {
		t.Fatalf("建源库失败: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE metrics (ts INTEGER PRIMARY KEY, cpu REAL)`); err != nil {
		t.Fatalf("建源表失败: %v", err)
	}
	for i := 0; i < n; i++ {
		if _, err := db.Exec(`INSERT INTO metrics (ts, cpu) VALUES (?, ?)`, 1000+i, float64(i)); err != nil {
			t.Fatalf("写源行失败: %v", err)
		}
	}
	return path
}

// metricsLogical 用给定源路径与目标 DSN 组装「只搬 metrics 一张表」的搬运定义。
// 测试源库只建了 metrics 表；真实施工时 metricsTables 会带齐三个物化桶。
func metricsLogical(srcPath, dstDSN string) logicalDB {
	return logicalDB{
		name:    "metrics",
		srcPath: srcPath,
		dstDSN:  dstDSN,
		tables:  []tableSpec{{"metrics", []string{"ts"}}},
		migrate: mysqlDialect().MigrateMetrics,
	}
}

// countTarget 统计目标表行数。
func countTarget(t *testing.T, dsn, table string) int64 {
	t.Helper()
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开目标库失败: %v", err)
	}
	defer db.Close()
	var n int64
	if err := db.QueryRow("SELECT COUNT(*) FROM `" + table + "`").Scan(&n); err != nil {
		t.Fatalf("统计 %s 失败: %v", table, err)
	}
	return n
}

// TestRunCopiesAndVerifies 端到端：源计数 → 批量拷贝 → 目标计数 → 抽样比对，且可重复执行（幂等）。
func TestRunCopiesAndVerifies(t *testing.T) {
	dsn := testsupport.SchemaDSN(t, "migmetrics")
	src := createSQLiteSource(t, 1234)
	logical := metricsLogical(src, dsn)

	report, err := Run(context.Background(), []logicalDB{logical}, Options{Verify: true, Batch: 100, Concurrency: 2})
	if err != nil {
		t.Fatalf("Run 失败: %v", err)
	}
	if len(report.Tables) != 1 {
		t.Fatalf("应报告 1 张表，实际 %d", len(report.Tables))
	}
	first := report.Tables[0]
	if first.Table != "metrics" || first.SourceRows != 1234 || first.TargetRows != 1234 || first.Copied != 1234 {
		t.Fatalf("metrics 搬运结果不符: %+v", first)
	}
	if first.SampleChecked == 0 {
		t.Fatal("应至少抽样校验一行")
	}
	if n := countTarget(t, dsn, "metrics"); n != 1234 {
		t.Fatalf("目标 metrics 行数应为 1234，实际 %d", n)
	}

	// 幂等：再跑一次仍应 1234 行（upsert 不产生重复）。
	if _, err := Run(context.Background(), []logicalDB{logical}, Options{Verify: true, Batch: 100}); err != nil {
		t.Fatalf("重复 Run 失败: %v", err)
	}
	if n := countTarget(t, dsn, "metrics"); n != 1234 {
		t.Fatalf("幂等重跑后目标行数应为 1234，实际 %d", n)
	}
}

// TestDryRunCountsWithoutWriting 守住 --dry-run：只统计、不写目标库。
func TestDryRunCountsWithoutWriting(t *testing.T) {
	dsn := testsupport.SchemaDSN(t, "migdry")
	src := createSQLiteSource(t, 50)
	logical := metricsLogical(src, dsn)

	report, err := Run(context.Background(), []logicalDB{logical}, Options{DryRun: true, Batch: 10})
	if err != nil {
		t.Fatalf("dry-run 失败: %v", err)
	}
	if report.Tables[0].SourceRows != 50 {
		t.Fatalf("dry-run 应统计源 50 行，实际 %d", report.Tables[0].SourceRows)
	}
	if report.Tables[0].Copied != 0 {
		t.Fatalf("dry-run 不应拷贝，实际 %d", report.Tables[0].Copied)
	}
	if n := countTarget(t, dsn, "metrics"); n != 0 {
		t.Fatalf("dry-run 后目标应为空，实际 %d", n)
	}
}

// TestVerifyDetectsCountMismatch 守住 --verify 的条数硬闸：
// 人为在目标多写一行，校验必须报错（改坏了会红）。
func TestVerifyDetectsCountMismatch(t *testing.T) {
	dsn := testsupport.SchemaDSN(t, "migmismatch")
	src := createSQLiteSource(t, 20)
	logical := metricsLogical(src, dsn)
	if _, err := Run(context.Background(), []logicalDB{logical}, Options{Batch: 10}); err != nil {
		t.Fatalf("首轮拷贝失败: %v", err)
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开目标库失败: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO metrics (ts) VALUES (999999)`); err != nil {
		_ = db.Close()
		t.Fatalf("注入多余行失败: %v", err)
	}
	_ = db.Close()

	if _, err := Run(context.Background(), []logicalDB{logical}, Options{Verify: true, Batch: 10}); err == nil {
		t.Fatal("条数不一致时 --verify 必须报错")
	} else if want := "条数不一致"; !strings.Contains(err.Error(), want) {
		t.Fatalf("错误信息应包含 %q，实际 %q", want, err.Error())
	}
}
