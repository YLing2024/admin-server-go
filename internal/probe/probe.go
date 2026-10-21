// Package probe 采集主机实时状态（CPU / 内存 / 磁盘 / 网络 / 磁盘 I/O / PSI / 进程瞬时 CPU）
// 与本地服务探活（TCP 端口、systemd 单元、进程 pid）。
//
// 口径逐项对齐旧实现 src/probe/system.ts 与 src/probe/services.ts。旧实现把采样差分状态放在
// 模块级单例（state.ts）里；本包把它收敛到 Service 实例，语义等价：速率类指标都是
// 「本次采样 − 上次采样」，跨请求必须共享同一份状态，否则每次都会退回 0。
package probe

import (
	"log/slog"
	"math"
	"sync"
	"time"

	"admin-server-go/internal/config"
)

// Service 是主机与服务探活的采集器，持有跨请求的采样差分状态。
type Service struct {
	cfg *config.Config
	log *slog.Logger

	// containers 缓存 docker 采集结果：`docker stats` 单次数百毫秒，不能挂在每帧上。
	containers containerCache

	// rateMu 保护「最近一次速率快照」与采样循环生命周期（见 rates.go）。快照是 REST 与 SSE
	// 唯一的速率来源，由 StartSampler 起的唯一后台循环写入，读取方只读、不各自做差。
	rateMu   sync.RWMutex
	rateSnap rateSnapshot
	ratePrev rateCounters

	// sampleMu 保护 CPU / 内存 / 负载快照与上一份 CPU 基线。HTTP 请求只读快照，
	// 绝不更新 samplePrev；采样窗口固定由后台 ticker 推进。
	sampleMu      sync.RWMutex
	sampleSnap    hostSnapshot
	samplePrev    *cpuState
	sampleStop    chan struct{}
	sampleDone    chan struct{}
	sampleStarted bool

	// 以下字段只在 sampleMu 内推进：三档 CPU 窗口的环形缓冲与进程窗口历史。
	// 环形缓冲长度由 tick 间隔换算，保证 15s / 60s 的语义不随配置漂移。
	cpu15    *floatRing
	cpu60    *floatRing
	procHist []procSample

	// cgMu 保护 cgroup 单元级 CPU 的上一次 usage_usec 基线。与进程维度分开加锁，
	// 避免逐个单元读文件时阻塞 HTTP 读取 CPU 快照。
	cgMu     sync.Mutex
	cgPrev   map[string]cgroupUsage
	cgPrevAt time.Time

	// extraMu 保护监控补全指标的最近快照与磁盘差分基线。与 CPU / 速率分开加锁，
	// 让 HTTP 读方不会因为 /proc/net/tcp 这类文件读取而阻塞。
	extraMu     sync.RWMutex
	extraSnap   extraSnapshot
	extraPrev   map[string]diskRaw
	extraPrevAt time.Time
}

// coreTicks 是单个核的累计时钟滴答（内核口径）。
type coreTicks struct {
	total float64
	idle  float64
}

// New 构造采集器。
func New(cfg *config.Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg: cfg,
		log: log,
	}
}

// round1 保留一位小数，等价旧实现的 Math.round(v*10)/10。
// 参与运算的都是占用率 / 速率（非负），故 Go 的「四舍五入远离零」与 JS 一致。
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// nowMillis 取当前毫秒时间戳，集中一处便于测试时理解时间来源。
func nowMillis() int64 { return time.Now().UnixMilli() }
