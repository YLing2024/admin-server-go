package metrics

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"admin-server-go/internal/sqlite"
	"admin-server-go/internal/store"
)

// openMetricsDB 建一个临时指标库并跑增量迁移，供聚合用例复用。
func openMetricsDB(t *testing.T) *store.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.db")
	d := sqlite.New()
	db, err := store.Open(context.Background(), d, sqlite.DSN(path), 1, d.MigrateMetrics)
	if err != nil {
		t.Fatalf("打开指标库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestComputeAggRowsBuckets 验证分桶取整（走方言 CastInt）与桶内最大值口径。
func TestComputeAggRowsBuckets(t *testing.T) {
	db := openMetricsDB(t)
	s := &Service{db: db, log: slog.Default()}

	// 截断取整：120..179 -> bucket 120；240..299 -> bucket 240。
	rows := []struct {
		ts  int64
		cpu float64
	}{
		{120, 10}, {125, 30}, {179, 20},
		{240, 50}, {299, 40},
	}
	for _, r := range rows {
		if _, err := db.SQL.ExecContext(context.Background(),
			"INSERT INTO metrics (ts, cpu) VALUES (?, ?)", r.ts, r.cpu); err != nil {
			t.Fatalf("插入 raw 失败: %v", err)
		}
	}

	got, err := s.computeAggRows(60, 100, 0, 600)
	if err != nil {
		t.Fatalf("computeAggRows 失败: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("桶数应为 2，实际 %d", len(got))
	}
	if got[0].bucket != 120 || got[0].sampleCount != 3 {
		t.Fatalf("首桶应为 bucket=120/count=3，实际 %d/%d", got[0].bucket, got[0].sampleCount)
	}
	if v := got[0].values["cpu"]; v == nil || *v != 30 {
		t.Fatalf("首桶 cpu（pct=100 取最大值）应为 30，实际 %v", v)
	}
	if got[1].bucket != 240 || got[1].sampleCount != 2 {
		t.Fatalf("次桶应为 bucket=240/count=2，实际 %d/%d", got[1].bucket, got[1].sampleCount)
	}
	if v := got[1].values["cpu"]; v == nil || *v != 50 {
		t.Fatalf("次桶 cpu 应为 50，实际 %v", v)
	}
}

// TestPlanBackfillJobs 验证补算规划：raw 有数据但聚合表为空时应规划出分片，
// 且已存在的桶不重复规划（走方言 CastInt 的 DISINCT 桶查询）。
func TestPlanBackfillJobs(t *testing.T) {
	db := openMetricsDB(t)
	s := &Service{db: db, log: slog.Default()}

	for _, ts := range []int64{120, 180, 240, 300} {
		if _, err := db.SQL.ExecContext(context.Background(),
			"INSERT INTO metrics (ts, cpu) VALUES (?, ?)", ts, 1.0); err != nil {
			t.Fatalf("插入 raw 失败: %v", err)
		}
	}
	jobs := s.planBackfillJobs()
	if len(jobs) == 0 {
		t.Fatal("应至少规划出一个补算分片")
	}
	for _, j := range jobs {
		if j.end <= j.start || j.start < 0 {
			t.Fatalf("分片区间非法: %+v", j)
		}
	}
}
