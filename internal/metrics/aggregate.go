package metrics

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"admin-server-go/internal/store"
)

// MetricsStepSeconds 是粒度白名单；未知参数由调用方回退默认（1d/1m）并正常返回 200。
var MetricsStepSeconds = map[string]int64{
	"1m": 60,
	"5m": 300,
	"1h": 3600,
	"1d": 86400,
}

// MetricsRangeSeconds 是区间白名单。
var MetricsRangeSeconds = map[string]int64{
	"1h":  3600,
	"6h":  6 * 3600,
	"1d":  86400,
	"7d":  7 * 86400,
	"30d": 30 * 86400,
}

// aggByKey 是物化档位的按 key 索引（1m / 1h / 1d）。
var aggByKey = func() map[string]store.AggDef {
	m := make(map[string]store.AggDef, len(store.AggTables))
	for _, def := range store.AggTables {
		m[def.Key] = def
	}
	return m
}()

// MetricMeta 是粒度查询的元信息；无数据时各字段仍给出完整结构，绝不返回 null。
type MetricMeta struct {
	Step            string `json:"step"`
	Range           string `json:"range"`
	BucketCount     int    `json:"bucketCount"`
	FirstBucket     *int64 `json:"firstBucket"`
	LastBucket      *int64 `json:"lastBucket"`
	RecordedSeconds int64  `json:"recordedSeconds"`
}

// aggRow 是一个聚合桶：bucket（Unix 秒）+ 各指标值（可能为 null）+ 样本数。
type aggRow struct {
	bucket      int64
	sampleCount int64
	values      map[string]*float64
}

// computeAggRows 用窗口函数直接从 raw metrics 计算分位分桶。
// 桶内各指标独立排序，取「最近秩」第 ceil(pct/100 * N) 个样本；pct=100 即桶内最大值。
// 取数绝不把 raw 全量拉进进程内存。
func (s *Service) computeAggRows(stepSec int64, pct int, sinceSec, untilSec int64) ([]aggRow, error) {
	if s.db == nil {
		return nil, nil
	}
	cols := strings.Join(store.MetricKeys, ", ")
	// 最近秩：index = ceil(pct/100 * N)。MySQL 的 `/` 是真除法（返回 DECIMAL），
	// 这里必须用整数除法 DIV，否则 r_xxx = 3.9900 永不等于 ROW_NUMBER，分位值全为 NULL。
	rank := fmt.Sprintf("(%d * cnt + 99) DIV 100", pct)
	rowNums := make([]string, len(store.MetricKeys))
	p99 := make([]string, len(store.MetricKeys))
	for i, k := range store.MetricKeys {
		rowNums[i] = "ROW_NUMBER() OVER (PARTITION BY bucket ORDER BY " + k + ") AS r_" + k
		p99[i] = fmt.Sprintf("ROUND(MAX(CASE WHEN r_%s = %s THEN %s END), %d) AS %s",
			k, rank, k, store.MetricRound[k], k)
	}
	sqlText := "WITH base AS (" +
		"SELECT " + s.db.Dialect().CastInt("ts / ?") + " * ? AS bucket, " + cols + " " +
		"FROM metrics WHERE ts >= ? AND ts < ?" +
		"), ranked AS (" +
		"SELECT bucket, COUNT(*) OVER (PARTITION BY bucket) AS cnt, " + strings.Join(rowNums, ", ") + ", " + cols + " " +
		"FROM base" +
		") SELECT bucket, MAX(cnt) AS sample_count, " + strings.Join(p99, ", ") + " FROM ranked GROUP BY bucket ORDER BY bucket"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rows, err := s.db.SQL.QueryContext(ctx, sqlText, stepSec, stepSec, sinceSec, untilSec)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	columnNames, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	return scanAggRows(rows, columnNames)
}

// scanAggRows 把结果行扫成 aggRow；数值列一律按可空浮点读取。
//
// 列按名字识别而非位置：computeAggRows 的结果列是 [bucket, sample_count, <指标…>]，
// 而 queryMetricPoints 直读聚合表时列是 [bucket, <指标…>]（没有 sample_count）。
// 若按位置把第 1 列当 sample_count，就会把首个指标（cpu）当成样本数吞掉，cpu 恒为 null。
func scanAggRows(rows *sql.Rows, columnNames []string) ([]aggRow, error) {
	var out []aggRow
	for rows.Next() {
		vals := make([]sql.NullFloat64, len(columnNames))
		ptrs := make([]any, len(columnNames))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		row := aggRow{values: map[string]*float64{}}
		for i, name := range columnNames {
			switch name {
			case "bucket":
				row.bucket = int64(vals[i].Float64)
			case "sample_count":
				row.sampleCount = int64(vals[i].Float64)
			default:
				if vals[i].Valid {
					v := vals[i].Float64
					row.values[name] = &v
				} else {
					row.values[name] = nil
				}
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// materializeRange 计算并写入 [sinceSec, untilSec) 内与 step 对齐的完整桶，返回写入行数。
func (s *Service) materializeRange(key string, sinceSec, untilSec int64) (int, error) {
	def, ok := aggByKey[key]
	if !ok || untilSec <= sinceSec || s.db == nil {
		return 0, nil
	}
	rows, err := s.computeAggRows(def.StepSec, def.Pct, sinceSec, untilSec)
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, nil
	}
	cols := append([]string{"bucket_start"}, store.MetricKeys...)
	cols = append(cols, "sample_count", "updated_at")
	sqlText := s.db.Dialect().UpsertSQL(def.Table, cols, []string{"bucket_start"})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	now := time.Now().Unix()
	stmt, err := tx.PrepareContext(ctx, sqlText)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	for _, r := range rows {
		args := []any{r.bucket}
		for _, k := range store.MetricKeys {
			args = append(args, nullable(r.values[k]))
		}
		args = append(args, r.sampleCount, now)
		if _, err := stmt.ExecContext(ctx, args...); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			return 0, err
		}
	}
	_ = stmt.Close()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(rows), nil
}

// aggregateLastComplete 补算「最近 lookback 个完整桶」。幂等 upsert，
// 某次失败留下的空洞会在下一次回看时自动补上。
func (s *Service) aggregateLastComplete(key string, lookback int) (int, error) {
	def, ok := aggByKey[key]
	if !ok {
		return 0, nil
	}
	nowSec := time.Now().Unix()
	end := (nowSec / def.StepSec) * def.StepSec
	if lookback < 1 {
		lookback = 1
	}
	start := end - def.StepSec*int64(lookback)
	if start < 0 {
		start = 0
	}
	if end <= 0 || end <= start {
		return 0, nil
	}
	return s.materializeRange(key, start, end)
}

// cleanupExpiredAggregates 清理过期分钟桶（小时/天桶长期保留，不清理）。
func (s *Service) cleanupExpiredAggregates() error {
	if s.db == nil {
		return nil
	}
	def, ok := aggByKey["1m"]
	if !ok {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cutoff := time.Now().Unix() - store.MinuteRetentionSeconds
	_, err := s.db.SQL.ExecContext(ctx, "DELETE FROM "+def.Table+" WHERE bucket_start < ?", cutoff)
	return err
}

// backfillJob 是一次补算分片。
type backfillJob struct {
	key   string
	start int64
	end   int64
}

// backfillChunkBuckets 是单个分片包含的桶数：避免一次性锁库太久，阻塞采样与 SSE。
const backfillChunkBuckets = 512

// planBackfillJobs 只挑「raw 有数据但聚合表缺失」的桶，按连续区间分片；
// 已有的桶一律跳过，绝不在启动时全表重算。
func (s *Service) planBackfillJobs() []backfillJob {
	jobs := []backfillJob{}
	if s.db == nil {
		return jobs
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var minTS sql.NullInt64
	if err := s.db.SQL.QueryRowContext(ctx, "SELECT MIN(ts) FROM metrics").Scan(&minTS); err != nil || !minTS.Valid {
		return jobs
	}
	nowSec := time.Now().Unix()
	for _, def := range store.AggTables {
		end := (nowSec / def.StepSec) * def.StepSec
		floorStart := (minTS.Int64 / def.StepSec) * def.StepSec
		if floorStart >= end {
			continue
		}
		have := map[int64]bool{}
		if err := func() error {
			rows, err := s.db.SQL.QueryContext(ctx,
				"SELECT bucket_start FROM "+def.Table+" WHERE bucket_start >= ? AND bucket_start < ?", floorStart, end)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var b int64
				if err := rows.Scan(&b); err != nil {
					return err
				}
				have[b] = true
			}
			return rows.Err()
		}(); err != nil {
			s.log.Warn("聚合补算规划失败", "key", def.Key, "err", err)
			continue
		}

		rawBuckets := []int64{}
		if err := func() error {
			rows, err := s.db.SQL.QueryContext(ctx,
				"SELECT DISTINCT "+s.db.Dialect().CastInt("ts / ?")+" * ? AS b FROM metrics WHERE ts >= ? AND ts < ? ORDER BY b",
				def.StepSec, def.StepSec, floorStart, end)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var b int64
				if err := rows.Scan(&b); err != nil {
					return err
				}
				rawBuckets = append(rawBuckets, b)
			}
			return rows.Err()
		}(); err != nil {
			s.log.Warn("聚合补算规划失败", "key", def.Key, "err", err)
			continue
		}

		// 连续的缺失桶合成一个区间，再按 chunk 切分。
		var ranges [][2]int64
		var rangeStart int64 = -1
		var prev int64
		started := false
		for _, b := range rawBuckets {
			if have[b] {
				if started {
					ranges = append(ranges, [2]int64{rangeStart, prev})
					started = false
				}
				continue
			}
			if !started {
				rangeStart = b
				started = true
			} else if b != prev+def.StepSec {
				ranges = append(ranges, [2]int64{rangeStart, prev})
				rangeStart = b
			}
			prev = b
		}
		if started {
			ranges = append(ranges, [2]int64{rangeStart, prev})
		}

		chunk := def.StepSec * backfillChunkBuckets
		for _, rg := range ranges {
			for st := rg[0]; st <= rg[1]; st += chunk {
				e := st + chunk
				if rg[1]+def.StepSec < e {
					e = rg[1] + def.StepSec
				}
				jobs = append(jobs, backfillJob{key: def.Key, start: st, end: e})
			}
		}
	}
	return jobs
}

// runBackfill 分片执行补算。每片独立 try/warn，单片失败不影响后续。
func (s *Service) runBackfill(jobs []backfillJob) {
	for _, job := range jobs {
		if _, err := s.materializeRange(job.key, job.start, job.end); err != nil {
			s.log.Warn("聚合补算失败", "key", job.key, "err", err)
		}
	}
	if len(jobs) > 0 {
		s.log.Info("聚合启动补算完成", "jobs", len(jobs))
	}
}

// startAggregationScheduler 启动聚合调度（仅在持有采样锁的实例上调用）：
// 启动补齐缺失桶；每分钟补分钟桶、每小时补小时桶、每天补天桶（均直接读 raw 算分位）。
// 每层失败只 warn，绝不影响采样与 SSE。
func (s *Service) startAggregationScheduler() {
	s.mu.Lock()
	if s.aggScheduled {
		s.mu.Unlock()
		return
	}
	s.aggScheduled = true
	stop := s.stop
	s.mu.Unlock()

	go func() {
		select {
		case <-time.After(1500 * time.Millisecond):
			s.runBackfill(s.planBackfillJobs())
		case <-stop:
		}
	}()

	go s.scheduleLoop(stop, time.Minute, func() {
		if _, err := s.aggregateLastComplete("1m", 10); err != nil {
			s.log.Warn("分钟桶聚合失败", "err", err)
		}
		if err := s.cleanupExpiredAggregates(); err != nil {
			s.log.Warn("分钟桶清理失败", "err", err)
		}
	})
	go s.scheduleLoop(stop, time.Hour, func() {
		if _, err := s.aggregateLastComplete("1h", 3); err != nil {
			s.log.Warn("小时桶聚合失败", "err", err)
		}
	})
	go s.scheduleLoop(stop, 24*time.Hour, func() {
		if _, err := s.aggregateLastComplete("1d", 2); err != nil {
			s.log.Warn("天桶聚合失败", "err", err)
		}
	})
}

// scheduleLoop 按固定周期执行 fn，直到收到停止信号。
func (s *Service) scheduleLoop(stop <-chan struct{}, interval time.Duration, fn func()) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			fn()
		}
	}
}

// queryMetricPoints 粒度查询：1m/1h/1d 读对应物化聚合表；5m 未物化，退回对 raw 现算 P95。
// 无数据返回空 points + 完整 meta，绝不抛错、绝不返回 null。
func (s *Service) queryMetricPoints(rangeKey, stepKey string) ([]map[string]any, MetricMeta, error) {
	stepSec, stepOK := MetricsStepSeconds[stepKey]
	rangeSec, rangeOK := MetricsRangeSeconds[rangeKey]
	nowSec := time.Now().Unix()
	var since int64
	if rangeOK {
		since = nowSec - rangeSec
	}
	meta := MetricMeta{Step: stepKey, Range: rangeKey}

	if s.db == nil {
		return []map[string]any{}, meta, nil
	}

	var rows []aggRow
	if def, ok := aggByKey[stepKey]; ok {
		cols := strings.Join(store.MetricKeys, ", ")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, err := s.db.SQL.QueryContext(ctx,
			"SELECT bucket_start AS bucket, "+cols+" FROM "+def.Table+" WHERE bucket_start >= ? ORDER BY bucket_start", since)
		if err != nil {
			return nil, meta, err
		}
		names, err := r.Columns()
		if err != nil {
			_ = r.Close()
			return nil, meta, err
		}
		rows, err = scanAggRows(r, names)
		_ = r.Close()
		if err != nil {
			return nil, meta, err
		}
	} else if stepOK {
		// 未物化的档位（如 5m）现算，口径取 P95。
		r, err := s.computeAggRows(stepSec, 95, since, nowSec+1)
		if err != nil {
			return nil, meta, err
		}
		rows = r
	}

	points := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		p := map[string]any{"ts": r.bucket * 1000}
		for _, k := range store.MetricKeys {
			kv := r.values[k]
			if kv == nil {
				p[k] = nil
			} else {
				p[k] = *kv
			}
		}
		points = append(points, p)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var minTS sql.NullInt64
	if err := s.db.SQL.QueryRowContext(ctx, "SELECT MIN(ts) FROM metrics").Scan(&minTS); err == nil && minTS.Valid {
		if rec := nowSec - minTS.Int64; rec > 0 {
			meta.RecordedSeconds = rec
		}
	}
	meta.BucketCount = len(points)
	if len(points) > 0 {
		first := points[0]["ts"].(int64)
		last := points[len(points)-1]["ts"].(int64)
		meta.FirstBucket = &first
		meta.LastBucket = &last
	}
	return points, meta, nil
}
