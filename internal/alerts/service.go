// Package alerts 实现阈值告警：规则存储、持续判定、事件记录与 ntfy 推送。
//
// 设计要点：
//   - 判定循环吃 probe 的实时快照，不在 HTTP 请求路径里跑；
//   - 「持续 N 秒」用连续满足计数实现，中途跌回阈值以下立即清零，避免抖动误报；
//   - 同一规则命中后进入冷却期，冷却内不再重复推送，但仍记录事件结束时间；
//   - 推送失败不影响判定，只记日志（告警不能因为通知通道挂了就停摆）。
package alerts

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// Rule 是一条告警规则。
type Rule struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Metric      string    `json:"metric"`
	Op          string    `json:"op"` // gt / gte / lt / lte
	Threshold   float64   `json:"threshold"`
	DurationSec int       `json:"durationSec"`
	Severity    string    `json:"severity"` // info / warn / crit
	Enabled     bool      `json:"enabled"`
	CooldownSec int       `json:"cooldownSec"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Event 是一次触发记录。
type Event struct {
	ID        int64      `json:"id"`
	RuleID    int64      `json:"ruleId"`
	RuleName  string     `json:"ruleName"`
	Metric    string     `json:"metric"`
	Severity  string     `json:"severity"`
	Value     float64    `json:"value"`
	Threshold float64    `json:"threshold"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Notified  bool       `json:"notified"`
}

// MetricProvider 由 probe 实现：按指标名取当前值。
type MetricProvider interface {
	AlertMetric(name string) (float64, bool)
}

// Notifier 发送通知；默认实现走 ntfy。
type Notifier interface {
	Send(ctx context.Context, title, body, severity string) error
}

// Options 装配参数。
type Options struct {
	DB       *sql.DB
	Metrics  MetricProvider
	Notifier Notifier
	Log      *slog.Logger
	Interval time.Duration
}

// Service 持有规则缓存、判定状态与事件写入。
type Service struct {
	db       *sql.DB
	metrics  MetricProvider
	notifier Notifier
	log      *slog.Logger
	interval time.Duration

	mu      sync.Mutex
	rules   []Rule
	state   map[int64]*ruleState // ruleID → 连续满足状态
	lastRun time.Time
}

// ruleState 记录一条规则的连续满足计数与最近一次推送时间。
type ruleState struct {
	streak    int
	lastFired time.Time
	active    bool
	activeID  int64
}

// New 构造告警服务。
func New(opts Options) *Service {
	interval := opts.Interval
	if interval <= 0 {
		interval = time.Second
	}
	return &Service{
		db:       opts.DB,
		metrics:  opts.Metrics,
		notifier: opts.Notifier,
		log:      opts.Log,
		interval: interval,
		state:    map[int64]*ruleState{},
	}
}

// Start 启动判定循环，直到 ctx 取消。
func (s *Service) Start(ctx context.Context) {
	t := time.NewTicker(s.interval)
	defer t.Stop()
	if err := s.Reload(ctx); err != nil && s.log != nil {
		s.log.Warn("告警规则加载失败", "error", err)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.evaluate(ctx)
		}
	}
}

// Reload 从库里重新加载规则（增删改后调用）。
func (s *Service) Reload(ctx context.Context) error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, metric, op, threshold, duration_sec, severity, enabled, cooldown_sec, created_at FROM alert_rules ORDER BY id`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()

	var list []Rule
	for rows.Next() {
		var r Rule
		var createdAt []byte
		if err := rows.Scan(&r.ID, &r.Name, &r.Metric, &r.Op, &r.Threshold, &r.DurationSec, &r.Severity, &r.Enabled, &r.CooldownSec, &createdAt); err != nil {
			return err
		}
		// 驱动未开 parseTime，DATETIME 以 []byte 返回；解析失败按零值处理，不影响规则生效
		if t, ok := parseMySQLTime(string(createdAt)); ok {
			r.CreatedAt = t
		}
		list = append(list, r)
	}
	s.mu.Lock()
	s.rules = list
	s.mu.Unlock()
	return rows.Err()
}

// evaluate 跑一轮判定。每轮对每条规则按当前值累加/清零连续计数。
func (s *Service) evaluate(ctx context.Context) {
	s.mu.Lock()
	rules := append([]Rule(nil), s.rules...)
	s.mu.Unlock()

	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		v, ok := s.metrics.AlertMetric(r.Metric)
		if !ok {
			continue
		}
		hit := compare(v, r.Op, r.Threshold)

		s.mu.Lock()
		st := s.state[r.ID]
		if st == nil {
			st = &ruleState{}
			s.state[r.ID] = st
		}
		if hit {
			st.streak++
		} else {
			st.streak = 0
		}
		need := r.DurationSec
		if need <= 0 {
			need = 1
		}
		ready := st.streak >= need
		cooling := !st.lastFired.IsZero() && time.Since(st.lastFired) < time.Duration(r.CooldownSec)*time.Second
		fire := ready && !cooling
		if fire {
			st.lastFired = time.Now()
			st.active = true
		}
		cleared := !hit && st.active && st.streak == 0
		if cleared {
			st.active = false
		}
		activeID := st.activeID
		s.mu.Unlock()

		if fire {
			ev, err := s.insertEvent(ctx, r, v)
			if err != nil {
				if s.log != nil {
					s.log.Warn("告警事件写入失败", "rule", r.Name, "error", err)
				}
				continue
			}
			s.mu.Lock()
			s.state[r.ID].activeID = ev.ID
			s.mu.Unlock()
			s.notify(ctx, r, v)
		}
		if cleared && activeID > 0 {
			_, _ = s.db.ExecContext(ctx, `UPDATE alert_events SET ended_at = ? WHERE id = ?`, time.Now(), activeID)
		}
	}
}

// insertEvent 写一条触发记录。
func (s *Service) insertEvent(ctx context.Context, r Rule, v float64) (Event, error) {
	now := time.Now()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_events (rule_id, rule_name, metric, severity, value, threshold, started_at, notified) VALUES (?,?,?,?,?,?,?,0)`,
		r.ID, r.Name, r.Metric, r.Severity, v, r.Threshold, now)
	if err != nil {
		return Event{}, err
	}
	id, _ := res.LastInsertId()
	return Event{ID: id, RuleID: r.ID, RuleName: r.Name, Metric: r.Metric, Severity: r.Severity,
		Value: v, Threshold: r.Threshold, StartedAt: now}, nil
}

// notify 推送并标记已通知。
func (s *Service) notify(ctx context.Context, r Rule, v float64) {
	if s.notifier == nil {
		return
	}
	title := fmt.Sprintf("[%s] %s", strings.ToUpper(r.Severity), r.Name)
	body := fmt.Sprintf("%s %s %.2f（阈值 %.2f），持续 %d 秒",
		metricLabel(r.Metric), opLabel(r.Op), v, r.Threshold, r.DurationSec)
	if err := s.notifier.Send(ctx, title, body, r.Severity); err != nil {
		if s.log != nil {
			s.log.Warn("告警推送失败", "rule", r.Name, "error", err)
		}
		return
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE alert_events SET notified = 1 WHERE rule_id = ? AND notified = 0`, r.ID)
}

// ListRules 返回全部规则。
func (s *Service) ListRules(ctx context.Context) ([]Rule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Rule(nil), s.rules...), nil
}

// ListEvents 返回最近事件，limit 默认 100。
func (s *Service) ListEvents(ctx context.Context, limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, rule_id, rule_name, metric, severity, value, threshold, started_at, ended_at, notified
		 FROM alert_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []Event{}
	for rows.Next() {
		var e Event
		var ended sql.NullTime
		if err := rows.Scan(&e.ID, &e.RuleID, &e.RuleName, &e.Metric, &e.Severity, &e.Value, &e.Threshold, &e.StartedAt, &ended, &e.Notified); err != nil {
			return nil, err
		}
		if ended.Valid {
			t := ended.Time
			e.EndedAt = &t
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CreateRule 新增规则并刷新缓存。
func (s *Service) CreateRule(ctx context.Context, r Rule) (Rule, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO alert_rules (name, metric, op, threshold, duration_sec, severity, enabled, cooldown_sec) VALUES (?,?,?,?,?,?,?,?)`,
		r.Name, r.Metric, r.Op, r.Threshold, r.DurationSec, r.Severity, r.Enabled, r.CooldownSec)
	if err != nil {
		return Rule{}, err
	}
	id, _ := res.LastInsertId()
	if err := s.Reload(ctx); err != nil {
		return Rule{}, err
	}
	r.ID = id
	return r, nil
}

// DeleteRule 删除规则。
func (s *Service) DeleteRule(ctx context.Context, id int64) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM alert_rules WHERE id = ?`, id); err != nil {
		return err
	}
	return s.Reload(ctx)
}

// ToggleRule 启停规则。
func (s *Service) ToggleRule(ctx context.Context, id int64, enabled bool) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE alert_rules SET enabled = ? WHERE id = ?`, enabled, id); err != nil {
		return err
	}
	return s.Reload(ctx)
}

// compare 按运算符比较。
func compare(v float64, op string, threshold float64) bool {
	switch op {
	case "gt":
		return v > threshold
	case "gte":
		return v >= threshold
	case "lt":
		return v < threshold
	case "lte":
		return v <= threshold
	}
	return false
}

func metricLabel(m string) string {
	switch m {
	case "cpu_percent":
		return "CPU 占用"
	case "mem_percent":
		return "内存占用"
	case "disk_percent":
		return "磁盘占用"
	case "stall_score":
		return "卡顿指数"
	case "run_queue":
		return "运行队列"
	case "psi_cpu":
		return "PSI CPU"
	case "psi_io":
		return "PSI IO"
	case "psi_mem":
		return "PSI 内存"
	}
	return m
}

func opLabel(op string) string {
	switch op {
	case "gt":
		return ">"
	case "gte":
		return "≥"
	case "lt":
		return "<"
	case "lte":
		return "≤"
	}
	return op
}

/* ---------------- ntfy 通知实现 ---------------- */

// NtfyNotifier 直接 POST 到 ntfy 主题；token 从文件读，避免进环境变量与日志。
type NtfyNotifier struct {
	BaseURL string
	Topic   string
	Token   string
	Client  *http.Client
}

// NewNtfyFromFile 从 token 文件构造通知器；文件缺失时返回 nil（不阻断告警）。
func NewNtfyFromFile(baseURL, topic, tokenPath string) *NtfyNotifier {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" {
		return nil
	}
	return &NtfyNotifier{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Topic:   topic,
		Token:   tok,
		Client:  &http.Client{Timeout: 8 * time.Second},
	}
}

// Send 发一条通知。severity 映射到 ntfy 的 priority/tags。
func (n *NtfyNotifier) Send(ctx context.Context, title, body, severity string) error {
	pri := "3"
	tag := "information_source"
	switch severity {
	case "crit":
		pri, tag = "5", "rotating_light"
	case "warn":
		pri, tag = "4", "warning"
	}
	payload := map[string]any{
		"topic":    n.Topic,
		"title":    title,
		"message":  body,
		"priority": atoiSafe(pri),
		"tags":     []string{tag},
	}
	raw, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.BaseURL+"/", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+n.Token)
	resp, err := n.Client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("ntfy 返回 %d", resp.StatusCode)
	}
	return nil
}

func atoiSafe(s string) int {
	switch s {
	case "5":
		return 5
	case "4":
		return 4
	case "2":
		return 2
	case "1":
		return 1
	}
	return 3
}

// parseMySQLTime 解析 MySQL DATETIME(3) 文本（UTC 语义）。
func parseMySQLTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(strings.Trim(raw, "\x00"))
	if raw == "" {
		return time.Time{}, false
	}
	for _, l := range []string{"2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(l, raw, time.UTC); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
