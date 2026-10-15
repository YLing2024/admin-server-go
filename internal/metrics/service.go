// Package metrics 负责长期留样（raw 采样 + 分档物化聚合 + 保留期清理）、
// 120 点内存历史环形 buffer，以及系统 / 服务 / 历史 / 粒度 / SSE 的 HTTP 处理。
//
// 口径逐项对齐旧实现 src/metrics/*.ts 与 src/routes/system.ts。DB 为 nil（metrics.db 不可用）时
// 一律降级为「长期留样停用」：内存 buffer、SSE、历史接口照常工作，绝不影响进程启动。
package metrics

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sync"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/httpx"
	"admin-server-go/internal/probe"
	"admin-server-go/internal/store"
)

// Options 是构造 Service 的依赖。DB 为 nil 表示指标库不可用（降级，不报错）。
type Options struct {
	Cfg   *config.Config
	DB    *store.DB
	Log   *slog.Logger
	Probe *probe.Service
}

// Service 是指标留样与系统接口的服务实例。
type Service struct {
	cfg         *config.Config
	db          *store.DB
	log         *slog.Logger
	probe       *probe.Service
	procMaxRows int

	// mu 保护 hist / history / 采样器生命周期字段。
	mu       sync.Mutex
	hist     histState
	history  []HistoryPoint
	stop     chan struct{}
	stopOnce bool

	aggScheduled bool
	samplerHeld  bool
}

// New 构造服务。Probe 缺省时内部自建一个，保证进程排行与系统卡片共用同一份 CPU 差分状态。
func New(opts Options) *Service {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	pr := opts.Probe
	if pr == nil {
		pr = probe.New(opts.Cfg, log)
	}
	maxRows := DefaultMaxProcessRows
	if opts.Cfg != nil && opts.Cfg.ProcMaxRows > 0 {
		maxRows = opts.Cfg.ProcMaxRows
	}
	return &Service{
		cfg:         opts.Cfg,
		db:          opts.DB,
		log:         log,
		probe:       pr,
		procMaxRows: maxRows,
	}
}

/* ============ 响应组装 ============ */

// buildSystemPayload 实时采集系统信息，并同步写一条历史采样（趋势图与实时数据同源）。
func (s *Service) buildSystemPayload() (map[string]any, error) {
	data, err := s.probe.CollectSystem()
	if err != nil {
		return nil, err
	}
	// 采样失败不影响主响应：sampleHistory 内部只 warn。
	s.sampleHistory()
	return data, nil
}

// servicesPayload 是 GET /api/admin/services 的响应体。
type servicesPayload struct {
	Services      []probe.Status `json:"services"`
	Processes     []ProcNode     `json:"processes"`
	Count         int            `json:"count"`
	Truncated     bool           `json:"truncated"`
	TotalMemMB    int64          `json:"total_mem_mb"`
	TotalCPU      float64        `json:"total_cpu"`
	ParsedCount   int            `json:"parsed_count"`
	SkippedCount  int            `json:"skipped_count"`
	TotalCgroupMB *int           `json:"total_cgroup_mb"`
	TotalThreads  int            `json:"total_threads"`
}

// buildServicesPayload 组装服务状态 + 全量进程树（两者并发采集）。
func (s *Service) buildServicesPayload() (*servicesPayload, error) {
	var services []probe.Status
	var proc *procPayload
	var errServices, errProc error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); services, errServices = s.probe.CollectServices() }()
	go func() { defer wg.Done(); proc, errProc = s.collectProcesses() }()
	wg.Wait()
	if errServices != nil {
		return nil, errServices
	}
	if errProc != nil {
		return nil, errProc
	}
	return &servicesPayload{
		Services:      services,
		Processes:     proc.Processes,
		Count:         proc.Count,
		Truncated:     proc.Truncated,
		TotalMemMB:    proc.TotalMemMB,
		TotalCPU:      proc.TotalCPU,
		ParsedCount:   proc.ParsedCount,
		SkippedCount:  proc.SkippedCount,
		TotalCgroupMB: proc.TotalCgroupMB,
		TotalThreads:  proc.TotalThreads,
	}, nil
}

/* ============ HTTP 处理函数（上游已完成鉴权） ============ */

// System 处理 GET /api/admin/system。
func (s *Service) System(w http.ResponseWriter, r *http.Request) {
	payload, err := s.buildSystemPayload()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取系统信息失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}

// History 处理 GET /api/admin/system/history：返回环形 buffer 中的采样点（最多 120 点）。
func (s *Service) History(w http.ResponseWriter, r *http.Request) {
	httpx.WriteJSON(w, http.StatusOK, s.readHistory())
}

// metricsPayload 固定字段顺序，便于与旧接口逐字段比对。
type metricsPayload struct {
	Step   string           `json:"step"`
	Range  string           `json:"range"`
	Points []map[string]any `json:"points"`
	Meta   MetricMeta       `json:"meta"`
}

// Metrics 处理 GET /api/admin/system/metrics：未知 range/step 回退 1d/1m 并返回 200；
// 查询失败降级为空数组 + 完整 meta，绝不 500（前端保持上一帧）。
func (s *Service) Metrics(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rangeKey := q.Get("range")
	if _, ok := MetricsRangeSeconds[rangeKey]; !ok {
		rangeKey = "1d"
	}
	stepKey := q.Get("step")
	if _, ok := MetricsStepSeconds[stepKey]; !ok {
		stepKey = "1m"
	}
	points := []map[string]any{}
	meta := MetricMeta{Step: stepKey, Range: rangeKey}
	if p, m, err := s.queryMetricPoints(rangeKey, stepKey); err != nil {
		s.log.Warn("metrics 查询失败", "err", err)
	} else {
		points = p
		meta = m
	}
	httpx.WriteJSON(w, http.StatusOK, metricsPayload{Step: stepKey, Range: rangeKey, Points: points, Meta: meta})
}

// streamServices 是 SSE 帧里的服务块（不含进程明细：进程树由 REST /services 提供）。
type streamServices struct {
	Services []probe.Status `json:"services"`
	TotalCPU float64        `json:"total_cpu"`
}

// streamFrame 是 SSE snapshot 帧的结构。
type streamFrame struct {
	System   map[string]any `json:"system"`
	Services streamServices `json:"services"`
	History  []HistoryPoint `json:"history"`
}

// Stream 处理 GET /api/admin/system/stream（SSE）：建连后立即推第一帧，之后每 1s 一帧；
// 每 10 帧补一条注释行防代理超时；请求关闭即停表。
func (s *Service) Stream(w http.ResponseWriter, r *http.Request) {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if flusher != nil {
		flusher.Flush()
	}

	tick := 0
	push := func() error {
		// 先读历史再采集，与旧实现的调用时序一致（新点下一帧才进入 history）。
		history := s.readHistory()
		var system map[string]any
		var services []probe.Status
		var errSystem, errServices error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); system, errSystem = s.buildSystemPayload() }()
		go func() { defer wg.Done(); services, errServices = s.probe.CollectServices() }()
		wg.Wait()
		if errSystem != nil || errServices != nil {
			return fmt.Errorf("采集失败")
		}
		totalCPU := 0.0
		if cpu, ok := system["cpu"].(map[string]any); ok {
			if v, ok := cpu["usage_percent"].(float64); ok {
				totalCPU = v
			}
		}
		frame := streamFrame{
			System:   system,
			Services: streamServices{Services: services, TotalCPU: totalCPU},
			History:  history,
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(frame); err != nil {
			return err
		}
		payload := bytes.TrimRight(buf.Bytes(), "\n")
		if _, err := fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", payload); err != nil {
			return err
		}
		tick++
		if tick%10 == 0 {
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return err
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	if err := push(); err != nil {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if err := push(); err != nil {
				return
			}
		}
	}
}

// Services 处理 GET /api/admin/services。
func (s *Service) Services(w http.ResponseWriter, r *http.Request) {
	payload, err := s.buildServicesPayload()
	if err != nil {
		httpx.Error(w, http.StatusInternalServerError, "获取服务状态失败: "+err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, payload)
}

/* ============ 小工具 ============ */

func round0(v float64) float64 { return math.Round(v) }

func round1(v float64) float64 { return math.Round(v*10) / 10 }

func maxf(a, b float64) float64 { return math.Max(a, b) }

// nullable 把可空浮点转成数据库参数：nil 传 NULL，否则传值。
func nullable(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
