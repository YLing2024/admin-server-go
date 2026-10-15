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

	// mu 保护下面全部可变采样状态。旧实现靠 Node 单线程天然串行；Go 下 HTTP 与 SSE
	// 会并发调用，必须显式串行化，避免差分基准被并发请求撕裂成乱值。
	mu sync.Mutex

	netState    netState
	diskioState diskioState

	// cpuPerCorePrev 是上一次每核 (total, idle)，用于算每核占用。nil 表示尚无基线。
	cpuPerCorePrev map[int]coreTicks

	// procCpuSample / procCpuEma 是进程瞬时 CPU 的差分样本与 EMA 平滑缓存，
	// 由系统卡片与进程排行共用（旧实现同样共享 state.procCpuSample / procCpuEma）。
	procCpuSample map[int]procSample
	procCpuEma    map[int]float64
	totalEmaPrev  float64
	totalEmaInit  bool
}

// netState 记录上一次网络累计字节与采样时刻，用于算速率。
type netState struct {
	ts int64
	rx float64
	tx float64
}

// diskioState 记录上一次磁盘读写累计字节与采样时刻。
type diskioState struct {
	ts    int64
	read  float64
	write float64
}

// coreTicks 是单个核的累计时钟滴答（内核口径）。
type coreTicks struct {
	total float64
	idle  float64
}

// procSample 是单个进程上一次的累计 CPU 滴答与系统总滴答。
type procSample struct {
	cpu   float64
	total float64
}

// New 构造采集器。
func New(cfg *config.Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		cfg:           cfg,
		log:           log,
		procCpuSample: map[int]procSample{},
		procCpuEma:    map[int]float64{},
	}
}

// round1 保留一位小数，等价旧实现的 Math.round(v*10)/10。
// 参与运算的都是占用率 / 速率（非负），故 Go 的「四舍五入远离零」与 JS 一致。
func round1(v float64) float64 { return math.Round(v*10) / 10 }

// nowMillis 取当前毫秒时间戳，集中一处便于测试时理解时间来源。
func nowMillis() int64 { return time.Now().UnixMilli() }
