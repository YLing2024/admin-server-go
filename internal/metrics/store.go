package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"admin-server-go/internal/probe"
	"admin-server-go/internal/store"
	"admin-server-go/internal/util"
)

// 历史采样参数：与旧实现 state.ts 的两个常量一致。
const (
	historyMax      = 120
	historyInterval = 5 * time.Second
)

// HistoryPoint 是内存历史环形 buffer 的一个采样点。
// 结构刻意不带内存绝对量（mem_used / mem_total），与旧 /history 接口的响应结构逐字段一致。
type HistoryPoint struct {
	TS          int64    `json:"ts"`
	CPU         float64  `json:"cpu"`
	MemPercent  float64  `json:"mem_percent"`
	SwapPercent float64  `json:"swap_percent"`
	PsiMemAvg10 *float64 `json:"psi_mem_avg10"`
	PsiCPUAvg10 *float64 `json:"psi_cpu_avg10"`
	PsiIOAvg10  *float64 `json:"psi_io_avg10"`
	NetRxRate   int64    `json:"net_rx_rate"`
	NetTxRate   int64    `json:"net_tx_rate"`
	DiskIORead  int64    `json:"disk_io_read"`
	DiskIOWrite int64    `json:"disk_io_write"`
}

// histState 是历史采样的差分基准与 EMA，单独成组便于一次性读写。
type histState struct {
	cpuPrev    *cpuBaseline
	cpuEma     float64
	cpuEmaInit bool
	netPrev    *netBaseline
	diskioPrev *diskioBaseline
}

type cpuBaseline struct {
	ts    int64
	idle  float64
	total float64
}

type netBaseline struct {
	ts int64
	rx float64
	tx float64
}

type diskioBaseline struct {
	ts    int64
	read  float64
	write float64
}

// sampleHistory 采一个历史点：CPU/内存为百分比，网速与磁盘 I/O 为两次采样差值换算的速率。
// 它直接读原始计数（不经 /api/admin/system 的独立采样状态），两条链互不干扰。
func (s *Service) sampleHistory() {
	now := time.Now().UnixMilli()
	cpuIdle, cpuTotal := probe.CPUSample()
	netRx, netTx := probe.NetSample()
	ioRead, ioWrite := probe.DiskIOSample()
	psi := probe.PsiHistPoint()
	mem := probe.ReadMeminfo()

	s.mu.Lock()
	h := &s.hist
	cpuPercent := 0.0
	if h.cpuPrev != nil && now > h.cpuPrev.ts {
		totalDiff := cpuTotal - h.cpuPrev.total
		idleDiff := cpuIdle - h.cpuPrev.idle
		raw := 0.0
		if totalDiff > 0 {
			raw = ((totalDiff - idleDiff) / totalDiff) * 100
		}
		var prev *float64
		if h.cpuEmaInit {
			v := h.cpuEma
			prev = &v
		}
		h.cpuEma = util.Ema(prev, raw, probe.CPUEMAAlpha)
		h.cpuEmaInit = true
		cpuPercent = round1(h.cpuEma)
	}

	netRxRate, netTxRate, ioReadRate, ioWriteRate := 0.0, 0.0, 0.0, 0.0
	if h.netPrev != nil {
		if dt := float64(now-h.netPrev.ts) / 1000; dt > 0 {
			netRxRate = maxf(0, (netRx-h.netPrev.rx)/dt)
			netTxRate = maxf(0, (netTx-h.netPrev.tx)/dt)
		}
	}
	if h.diskioPrev != nil {
		if dt := float64(now-h.diskioPrev.ts) / 1000; dt > 0 {
			ioReadRate = maxf(0, (ioRead-h.diskioPrev.read)/dt)
			ioWriteRate = maxf(0, (ioWrite-h.diskioPrev.write)/dt)
		}
	}
	h.cpuPrev = &cpuBaseline{ts: now, idle: cpuIdle, total: cpuTotal}
	h.netPrev = &netBaseline{ts: now, rx: netRx, tx: netTx}
	h.diskioPrev = &diskioBaseline{ts: now, read: ioRead, write: ioWrite}

	point := HistoryPoint{
		TS:          now,
		CPU:         cpuPercent,
		MemPercent:  probe.MemPercent(),
		SwapPercent: probe.SwapPercent(),
		PsiMemAvg10: psi.MemAvg10,
		PsiCPUAvg10: psi.CPUAvg10,
		PsiIOAvg10:  psi.IOAvg10,
		NetRxRate:   int64(round0(netRxRate)),
		NetTxRate:   int64(round0(netTxRate)),
		DiskIORead:  int64(round0(ioReadRate)),
		DiskIOWrite: int64(round0(ioWriteRate)),
	}

	s.history = append(s.history, point)
	if len(s.history) > historyMax {
		s.history = s.history[len(s.history)-historyMax:]
	}
	s.persistHistoryLocked()
	s.mu.Unlock()

	// 同步长期留样：写库失败只 warn，绝不影响上面的内存 buffer 与 SSE。
	s.persistMetricPoint(point, mem.Used, mem.Total)
}

// readHistory 读取历史：优先从文件（多实例共享），文件不可用则退回本进程内存 buffer。
func (s *Service) readHistory() []HistoryPoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	if data, err := os.ReadFile(s.cfg.HistoryFile); err == nil {
		var arr []HistoryPoint
		if err := json.Unmarshal(data, &arr); err == nil && arr != nil {
			if out := tail(arr, historyMax); out != nil {
				return out
			}
			return []HistoryPoint{}
		}
	}
	if out := tail(append([]HistoryPoint(nil), s.history...), historyMax); out != nil {
		return out
	}
	return []HistoryPoint{}
}

// persistHistoryLocked 原子写历史文件（先写临时文件再 rename，读端不会读到半截内容）。
// 调用方必须已持有 s.mu。
func (s *Service) persistHistoryLocked() {
	buf, err := json.Marshal(s.history)
	if err != nil {
		return
	}
	tmp := s.cfg.HistoryFile + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.cfg.HistoryFile)
}

// persistMetricPoint 写入一个 raw 采样点（ts 用 epoch 秒），并顺手按保留期清理过期 raw 点。
// 任何异常只 warn：写库失败不能拖累 SSE 与内存采样。
func (s *Service) persistMetricPoint(point HistoryPoint, memUsed, memTotal float64) {
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cols := append([]string{"ts"}, store.MetricKeys...)
	sqlText := s.db.Dialect().UpsertSQL("metrics", cols, []string{"ts"})
	args := []any{
		point.TS / 1000,
		point.CPU,
		point.MemPercent,
		memUsed,
		memTotal,
		point.SwapPercent,
		point.PsiMemAvg10,
		point.PsiCPUAvg10,
		point.PsiIOAvg10,
		point.NetRxRate,
		point.NetTxRate,
		point.DiskIORead,
		point.DiskIOWrite,
	}
	if _, err := s.db.SQL.ExecContext(ctx, sqlText, args...); err != nil {
		s.log.Warn("metrics.db 写入失败", "err", err)
		return
	}
	cutoff := time.Now().Unix() - store.RawRetentionSeconds
	if _, err := s.db.SQL.ExecContext(ctx, "DELETE FROM metrics WHERE ts < ?", cutoff); err != nil {
		s.log.Warn("metrics.db 过期清理失败", "err", err)
	}
}

// StartSampler 启动后台采样（每 5 秒一次）。用文件锁（ADMIN_SAMPLER_LOCK）保证多实例下
// 只有一个实例真正采样；未抢到锁的实例直接返回，不产生任何副作用。
func (s *Service) StartSampler() {
	if !s.acquireSamplerLock() {
		return
	}
	s.log.Info("历史采样器已启动", "interval", historyInterval.String(), "max", historyMax)
	s.sampleHistory()

	s.stop = make(chan struct{})
	s.stopOnce = false
	ticker := time.NewTicker(historyInterval)
	go func() {
		for {
			select {
			case <-s.stop:
				ticker.Stop()
				return
			case <-ticker.C:
				s.sampleHistory()
			}
		}
	}()

	// 聚合调度与采样器同锁：仅主实例补算/物化聚合表。
	s.startAggregationScheduler()
}

// Stop 停止采样并释放采样锁（测试与优雅退出用）。
func (s *Service) Stop() {
	s.mu.Lock()
	if s.stop != nil && !s.stopOnce {
		close(s.stop)
		s.stopOnce = true
	}
	s.mu.Unlock()
	if s.samplerHeld {
		_ = os.Remove(s.cfg.SamplerLock)
	}
}

// acquireSamplerLock 尝试获取采样器独占锁。锁文件记录持有者 pid，
// 若持有者进程已失效（进程退出/崩溃遗留的陈旧锁）则视为可抢占。
func (s *Service) acquireSamplerLock() bool {
	data, err := os.ReadFile(s.cfg.SamplerLock)
	if err == nil {
		holder, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr == nil && holder > 0 && holder != os.Getpid() && isPidAlive(holder) {
			return false
		}
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.SamplerLock), 0o755); err != nil {
		return false
	}
	if err := os.WriteFile(s.cfg.SamplerLock, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return false
	}
	s.samplerHeld = true
	return true
}

// isPidAlive 探测进程是否存在；无权限探测（EPERM）同样视为存活。
func isPidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

/* ============ 小工具 ============ */

func tail(points []HistoryPoint, n int) []HistoryPoint {
	if len(points) <= n {
		return points
	}
	return points[len(points)-n:]
}
