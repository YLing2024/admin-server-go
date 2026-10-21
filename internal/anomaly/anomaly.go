// Package anomaly 做轻量基线异常检测：不是"超过阈值"，而是"偏离自己的历史"。
//
// 借鉴 Netdata 的 ML 思路，但用可在 10 行内解释的统计量：
//   - 每个指标维护 EWMA 均值与方差（指数加权，对近期更敏感）
//   - 当前值偏离均值超过 N 个标准差 → 记一次异常
//   - 绝对阈值（如 CPU>90%）不算异常，那是用户自己配的告警，两者互补
//
// 事件写入 MySQL（低频：异常才写），供面板与日志按时间对齐查询。
package anomaly

import (
	"context"
	"database/sql"
	"math"
	"strings"
	"sync"
	"time"
)

// Keys 是被监测的指标（与 CollectSystem 的字段对应）。
var Keys = []string{
	"cpu_percent",  // cpu.usage_percent
	"mem_percent",  // memory.percent
	"disk_percent", // disk.percent（根分区）
	"stall_score",  // 卡顿指数
	"run_queue",    // cpu.run_queue
	"psi_cpu",      // psi.cpu.some.avg10
	"psi_io",       // psi.io.some.avg10
}

// Config 是判定参数。
type Config struct {
	Alpha    float64 // EWMA 平滑系数，越大越敏感
	Warmup   int     // 前 N 个样本只学习不判定
	Sigma    float64 // 触发阈值（标准差倍数）
	MinSigma float64 // 方差下限，避免平稳指标被微小波动触发
	Cooldown time.Duration
}

// DefaultConfig 给出保守但够用的默认值。
func DefaultConfig() Config {
	return Config{Alpha: 0.15, Warmup: 40, Sigma: 4.0, MinSigma: 0.5, Cooldown: 10 * time.Minute}
}

// baseline 是单指标的 EWMA 统计量。
type baseline struct {
	mean, variance float64
	n              int
	lastFire       time.Time
}

// Service 维护全部指标的基线并在触发时落库。
type Service struct {
	db     *sql.DB
	cfg    Config
	mu     sync.Mutex
	states map[string]*baseline
}

// New 构造检测器；db 可为 nil（只算不落库，用于测试）。
func New(db *sql.DB, cfg Config) *Service {
	return &Service{db: db, cfg: cfg, states: map[string]*baseline{}}
}

// Outcome 是一次判定的结果。
type Outcome struct {
	Key    string
	Value  float64
	Mean   float64
	Stddev float64
	Z      float64
	Fired  bool
}

// Observe 喂一个样本，返回判定结果。key 不存在于 Keys 时忽略。
func (s *Service) Observe(key string, value float64) Outcome {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return Outcome{Key: key, Value: value}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	b := s.states[key]
	if b == nil {
		b = &baseline{mean: value, variance: 0}
		s.states[key] = b
	}
	// EWMA：均值与方差同时更新（方差用平方差近似，够用且稳定）
	diff := value - b.mean
	b.mean += s.cfg.Alpha * diff
	b.variance = (1 - s.cfg.Alpha) * (b.variance + s.cfg.Alpha*diff*diff)
	b.n++

	std := math.Sqrt(b.variance)
	out := Outcome{Key: key, Value: value, Mean: b.mean, Stddev: std, Z: 0}
	if std < s.cfg.MinSigma {
		std = s.cfg.MinSigma
	}
	out.Z = (value - b.mean) / std
	if b.n <= s.cfg.Warmup {
		return out
	}
	if math.Abs(out.Z) >= s.cfg.Sigma && time.Since(b.lastFire) >= s.cfg.Cooldown {
		b.lastFire = time.Now()
		out.Fired = true
	}
	return out
}

// Record 把触发写库（表不存在时自动建）。
func (s *Service) Record(ctx context.Context, o Outcome) error {
	if s.db == nil || !o.Fired {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO anomaly_events
		(ts, metric, value, mean, stddev, zscore) VALUES (?,?,?,?,?,?)`,
		time.Now().UTC(), o.Key, round2(o.Value), round2(o.Mean), round2(o.Stddev), round2(o.Z))
	return err
}

// List 返回最近事件。
func (s *Service) List(ctx context.Context, limit int) ([]Event, error) {
	if s.db == nil {
		return nil, nil
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, metric, value, mean, stddev, zscore FROM anomaly_events ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.TS, &e.Metric, &e.Value, &e.Mean, &e.Stddev, &e.Z); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Event 是一条异常记录。
type Event struct {
	TS     time.Time `json:"ts"`
	Metric string    `json:"metric"`
	Value  float64   `json:"value"`
	Mean   float64   `json:"mean"`
	Stddev float64   `json:"stddev"`
	Z      float64   `json:"z"`
}

// DDL 建表语句（幂等）。
const DDL = `CREATE TABLE IF NOT EXISTS anomaly_events (
  id BIGINT AUTO_INCREMENT PRIMARY KEY,
  ts DATETIME(3) NOT NULL,
  metric VARCHAR(64) NOT NULL,
  value DOUBLE NOT NULL,
  mean DOUBLE NOT NULL,
  stddev DOUBLE NOT NULL,
  zscore DOUBLE NOT NULL,
  KEY idx_ts (ts),
  KEY idx_metric (metric)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// NormalizeKey 把 CollectSystem 的路径式字段名转成监测键。
func NormalizeKey(path string) (string, bool) {
	k := strings.TrimSpace(path)
	for _, allowed := range Keys {
		if k == allowed {
			return k, true
		}
	}
	return "", false
}

// Extract 从 CollectSystem 的快照里取出被监测的 7 个指标。
// 只取数值型；缺失的键直接跳过，让检测器自行预热，不做 0 填充。
func Extract(snap map[string]any) map[string]float64 {
	out := map[string]float64{}
	cpu, _ := snap["cpu"].(map[string]any)
	mem, _ := snap["memory"].(map[string]any)
	disk, _ := snap["disk"].(map[string]any)
	psi, _ := snap["psi"].(map[string]any)

	set := func(k string, v any) {
		if f, ok := v.(float64); ok {
			out[k] = f
		}
	}
	if cpu != nil {
		set("cpu_percent", cpu["usage_percent"])
		if rq, ok := cpu["run_queue"].(int); ok {
			out["run_queue"] = float64(rq)
		}
	}
	if mem != nil {
		set("mem_percent", mem["percent"])
	}
	if disk != nil {
		set("disk_percent", disk["percent"])
	}
	set("stall_score", snap["stall_score"])
	if psi != nil {
		if c, ok := psi["cpu"].(map[string]any); ok {
			if some, ok := c["some"].(map[string]any); ok {
				set("psi_cpu", some["avg10"])
			}
		}
		if i, ok := psi["io"].(map[string]any); ok {
			if some, ok := i["some"].(map[string]any); ok {
				set("psi_io", some["avg10"])
			}
		}
	}
	return out
}
