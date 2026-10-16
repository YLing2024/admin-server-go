package metrics

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"admin-server-go/internal/mysql"
	"admin-server-go/internal/store"
	"admin-server-go/internal/testsupport"
)

// TestHistoryPointDeviceColumnsJSONRoundTrip 守住按盘速率列在 JSON 里与固定字段平铺，
// 再解码回 DiskDev；合计列 disk_io_read / disk_io_write 不被误收。
func TestHistoryPointDeviceColumnsJSONRoundTrip(t *testing.T) {
	point := HistoryPoint{
		TS:          1234,
		CPU:         1.5,
		DiskIORead:  10,
		DiskIOWrite: 20,
		DiskDev:     map[string]int64{"disk_io_vda_read": 11, "disk_io_vda_write": 22},
	}
	raw, err := json.Marshal(point)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("Unmarshal 到 map: %v", err)
	}
	for _, k := range []string{"disk_io_vda_read", "disk_io_vda_write"} {
		if _, ok := obj[k]; !ok {
			t.Errorf("JSON 缺少按盘列 %s: %s", k, raw)
		}
	}
	var back HistoryPoint
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if back.DiskDev["disk_io_vda_read"] != 11 || back.DiskDev["disk_io_vda_write"] != 22 {
		t.Errorf("DiskDev 往返错误: %+v", back.DiskDev)
	}
	if back.DiskIORead != 10 || back.DiskIOWrite != 20 {
		t.Errorf("合计列往返错误: %+v", back)
	}
	if len(back.DiskDev) != 2 {
		t.Errorf("DiskDev 不应包含合计列: %+v", back.DiskDev)
	}
}

// TestAggregateIncludesDeviceColumns 守住 §6.5：按盘列随 MetricKeys 自动进入 1m/1h/1d
// 三个物化档位的聚合与查询。
func TestAggregateIncludesDeviceColumns(t *testing.T) {
	store.SetDiskDevices([]string{"vda"})
	defer store.SetDiskDevices(nil)

	db := openMetricsDB(t)
	s := &Service{db: db, log: slog.Default(), diskDevices: map[string]bool{"vda": true}, diskDevWarned: map[string]bool{}}
	ctx := context.Background()
	now := time.Now().Unix()

	cases := []struct {
		step     string
		rangeKey string
		base     int64
		lookback int
	}{
		{"1m", "1h", (now/60 - 3) * 60, 10},
		{"1h", "7d", (now/3600 - 2) * 3600, 3},
		{"1d", "30d", (now/86400 - 2) * 86400, 3},
	}
	for _, c := range cases {
		for i := int64(0); i < 2; i++ {
			if _, err := db.SQL.ExecContext(ctx,
				"INSERT INTO metrics (ts, cpu, disk_io_read, disk_io_vda_read, disk_io_vda_write) VALUES (?, ?, ?, ?, ?)",
				c.base+i, 1.0, 10.0, 100.0, 200.0); err != nil {
				t.Fatalf("插入 raw 失败: %v", err)
			}
		}
		if _, err := s.aggregateLastComplete(c.step, c.lookback); err != nil {
			t.Fatalf("%s 桶聚合失败: %v", c.step, err)
		}
		points, _, err := s.queryMetricPoints(c.rangeKey, c.step)
		if err != nil {
			t.Fatalf("查询 %s 桶失败: %v", c.step, err)
		}
		found := false
		for _, p := range points {
			if p["ts"] != c.base*1000 {
				continue
			}
			found = true
			if got, ok := p["disk_io_vda_read"].(float64); !ok || got != 100 {
				t.Errorf("%s 桶 disk_io_vda_read=%v，应为 100", c.step, p["disk_io_vda_read"])
			}
			if got, ok := p["disk_io_vda_write"].(float64); !ok || got != 200 {
				t.Errorf("%s 桶 disk_io_vda_write=%v，应为 200", c.step, p["disk_io_vda_write"])
			}
		}
		if !found {
			t.Fatalf("%s 桶未找到 ts=%d，points=%v", c.step, c.base*1000, points)
		}
	}
}

// TestMigrateMetricsAddsDiskColumnsToLegacyTables 守住 §6.5：对只有旧列的库执行启动迁移后
// 不报错、不丢旧行，四张表都补齐按盘速率列，旧行新列为 NULL。
func TestMigrateMetricsAddsDiskColumnsToLegacyTables(t *testing.T) {
	store.SetDiskDevices([]string{"vda", "vdb"})
	defer store.SetDiskDevices(nil)

	dsn := testsupport.SchemaDSN(t, "legacymetrics")
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	stmts := []string{
		"CREATE TABLE metrics (ts BIGINT NOT NULL, cpu DOUBLE, disk_io_read DOUBLE, disk_io_write DOUBLE, PRIMARY KEY (ts))",
		"CREATE TABLE metrics_1m (bucket_start BIGINT NOT NULL, cpu DOUBLE, PRIMARY KEY (bucket_start))",
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("建旧表失败: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO metrics (ts, cpu) VALUES (1000, 1.5)"); err != nil {
		t.Fatalf("写旧行失败: %v", err)
	}
	if _, err := db.ExecContext(ctx, "INSERT INTO metrics_1m (bucket_start, cpu) VALUES (60, 2.5)"); err != nil {
		t.Fatalf("写旧桶失败: %v", err)
	}

	if err := mysql.New().MigrateMetrics(ctx, db); err != nil {
		t.Fatalf("迁移失败: %v", err)
	}

	for _, table := range []string{"metrics", "metrics_1m", "metrics_1h", "metrics_1d"} {
		for _, col := range []string{"disk_io_vda_read", "disk_io_vda_write", "disk_io_vdb_read", "disk_io_vdb_write"} {
			var n int
			if err := db.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = ? AND column_name = ?",
				table, col).Scan(&n); err != nil {
				t.Fatalf("探测列失败: %v", err)
			}
			if n == 0 {
				t.Errorf("表 %s 缺列 %s", table, col)
			}
		}
	}

	var cnt int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM metrics").Scan(&cnt); err != nil {
		t.Fatalf("统计旧行失败: %v", err)
	}
	if cnt != 1 {
		t.Errorf("旧行应保留 1 行，实际 %d", cnt)
	}
	var v sql.NullFloat64
	if err := db.QueryRowContext(ctx, "SELECT disk_io_vda_read FROM metrics WHERE ts = 1000").Scan(&v); err != nil {
		t.Fatalf("读旧行新列失败: %v", err)
	}
	if v.Valid {
		t.Errorf("旧行新列应为 NULL，实际 %v", v.Float64)
	}
}

// TestPersistMetricPointWritesDeviceColumns 守住 raw 写入按 MetricKeys 动态取参：
// 按盘列写进对应列，未提供的按盘列写 NULL。
func TestPersistMetricPointWritesDeviceColumns(t *testing.T) {
	store.SetDiskDevices([]string{"vda", "vdb"})
	defer store.SetDiskDevices(nil)

	db := openMetricsDB(t)
	s := &Service{
		db:            db,
		log:           slog.Default(),
		diskDevices:   map[string]bool{"vda": true, "vdb": true},
		diskDevWarned: map[string]bool{},
	}
	point := HistoryPoint{
		TS:          time.Now().UnixMilli(),
		CPU:         1,
		DiskIORead:  2,
		DiskIOWrite: 3,
		DiskDev:     map[string]int64{"disk_io_vda_read": 11, "disk_io_vda_write": 22},
	}
	s.persistMetricPoint(point, 100, 200)

	var vdaRead, vdaWrite, vdbRead sql.NullFloat64
	if err := db.SQL.QueryRowContext(context.Background(),
		"SELECT disk_io_vda_read, disk_io_vda_write, disk_io_vdb_read FROM metrics WHERE ts = ?", point.TS/1000).
		Scan(&vdaRead, &vdaWrite, &vdbRead); err != nil {
		t.Fatalf("读回按盘列失败: %v", err)
	}
	if !vdaRead.Valid || vdaRead.Float64 != 11 || !vdaWrite.Valid || vdaWrite.Float64 != 22 {
		t.Errorf("vda 按盘列写错: read=%+v write=%+v", vdaRead, vdaWrite)
	}
	if vdbRead.Valid {
		t.Errorf("未提供的 vdb 列应为 NULL，实际 %v", vdbRead.Float64)
	}
}
