package probe

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// minSampleWindow 是 CPU 占用可上报的最小窗口。Jiffies 的时间粒度通常为 10ms，
// 窗口越短量化误差越大；窗口不足 1 秒的样本直接丢弃，等下一个 tick 再算。
const minSampleWindow = time.Second

// coreUsage 是单个逻辑核在一个采样窗口内的占用率。
type coreUsage struct {
	id      int
	percent float64
}

// procCPUState 是单个进程的累计 CPU 滴答与进程名。保留进程名是为了让已退出进程
// 仍能在窗口归因里被认出（/proc 已读不到，只能依赖采样历史）。
type procCPUState struct {
	ticks float64
	name  string
}

// cpuState 是某一时刻的 /proc/stat 与全部进程累计滴答快照。
// at 取读取 /proc/stat 之前的时间，保证相邻两帧的窗口与滴答增量对应。
type cpuState struct {
	at       time.Time
	agg      coreTicks
	cores    map[int]coreTicks
	proc     map[int]procCPUState
	runQueue int
	blocked  int
}

// cpuWindow 是同一对快照算出的系统、每核与进程占用。
type cpuWindow struct {
	window   time.Duration
	total    float64
	perCore  []coreUsage
	proc     map[int]float64
	runQueue int
	blocked  int
}

// hostSnapshot 是后台采样器发布的只读快照：CPU / 内存 / 负载 / PSI / 窗口归因
// 与 cgroup 服务占用都同属一个 tick。
type hostSnapshot struct {
	at          time.Time
	window      time.Duration
	cpuPercent  float64
	usage15s    float64
	usage60s    float64
	runQueue    int
	blocked     int
	perCore     []coreUsage
	procCPU     map[int]float64
	movers      []cpuMover
	services    []serviceCPU
	stallScore  float64
	stallLevel  string
	psi         map[string]any
	mem         Meminfo
	loadavg     [3]float64
	loadPercent float64
}

// StartSampler 启动唯一的后台采样循环（幂等）。CPU、进程、内存与网络/磁盘速率
// 都由这一个 ticker 推进；HTTP 请求只读快照，绝不修改采样基准。
func (s *Service) StartSampler() {
	s.sampleMu.Lock()
	if s.sampleStarted {
		s.sampleMu.Unlock()
		return
	}
	s.sampleStarted = true
	s.cpu15 = newFloatRing(s.windowSamples(15))
	s.cpu60 = newFloatRing(s.windowSamples(60))
	s.procHist = nil
	stop := make(chan struct{})
	done := make(chan struct{})
	s.sampleStop = stop
	s.sampleDone = done
	s.sampleMu.Unlock()

	go func() {
		defer close(done)
		// 立即采一次建立基线：接口在第一个完整窗口产出前返回零值，不是陈旧值。
		s.sampleAll()
		ticker := time.NewTicker(s.sampleInterval())
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.sampleAll()
			}
		}
	}()
}

// StopSampler 停止采样循环并等 goroutine 退出；未启动时为无操作，可重复调用。
func (s *Service) StopSampler() {
	s.sampleMu.Lock()
	if !s.sampleStarted {
		s.sampleMu.Unlock()
		return
	}
	stop, done := s.sampleStop, s.sampleDone
	s.sampleStop, s.sampleDone = nil, nil
	s.sampleStarted = false
	s.sampleMu.Unlock()

	close(stop)
	if done != nil {
		<-done
	}
}

// sampleInterval 返回配置的 tick 间隔；配置缺失或非法时按 1 秒处理。
func (s *Service) sampleInterval() time.Duration {
	sec := 1
	if s.cfg != nil && s.cfg.ProbeIntervalSec >= 1 {
		sec = s.cfg.ProbeIntervalSec
	}
	return time.Duration(sec) * time.Second
}

// windowSamples 把秒窗口换算成 tick 数，保证 usage_15s / usage_60s 在
// PROBE_INTERVAL_SEC 变化时仍是真实的 15 秒 / 60 秒语义。
func (s *Service) windowSamples(seconds int) int {
	step := int(s.sampleInterval() / time.Second)
	if step < 1 {
		step = 1
	}
	n := seconds / step
	if n < 1 {
		n = 1
	}
	return n
}

// sampleAll 执行一个完整 tick。速率、主机指标与 cgroup 服务占用共用同一时钟节奏；
// 单次采集超过 200ms 说明遍历开销异常，记 warn 但不影响本轮结果。
func (s *Service) sampleAll() {
	start := time.Now()
	s.sampleRates()
	s.sampleHost()
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		s.log.Warn("系统采样单次耗时超过 200ms", "elapsed", elapsed.String())
	}
}

// sampleHost 采一次 CPU / 进程 / 内存 / 负载 / PSI / cgroup 服务占用。
// CPU 读失败时保留上一次快照并告警；窗口不足 1 秒时不发布新值，也不前移基线，
// 让窗口自然累积到足够长。
func (s *Service) sampleHost() {
	mem := ReadMeminfo()
	load, loadErr := readLoadAvg()
	psi := readPressure()

	s.sampleMu.Lock()
	s.sampleSnap.mem = mem
	s.sampleSnap.psi = psi
	if loadErr == nil {
		s.sampleSnap.loadavg = load
		cores := runtime.NumCPU()
		if cores > 0 {
			s.sampleSnap.loadPercent = round1(load[0] / float64(cores) * 100)
		}
	}
	s.sampleMu.Unlock()
	if loadErr != nil {
		s.log.Warn("读取 /proc/loadavg 失败，保留上一次负载值", "err", loadErr)
	}

	cur, err := readCPUState()
	if err != nil {
		s.log.Warn("读取 /proc/stat 失败，保留上一次 CPU 采样", "err", err)
		return
	}
	// cgroup CPU 与进程 CPU 都取本轮 tick 的时间点做差，互不依赖，读文件失败只让该块为空。
	services := s.sampleCgroupCPU(cur.at)

	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	s.sampleSnap.services = services

	prev := s.samplePrev
	if prev == nil {
		s.samplePrev = &cur
		return
	}
	win, ok := diffCPUState(*prev, cur)
	if !ok {
		return
	}
	s.samplePrev = &cur
	s.sampleSnap.at = cur.at
	s.sampleSnap.window = win.window
	s.sampleSnap.cpuPercent = win.total
	s.sampleSnap.runQueue = win.runQueue
	s.sampleSnap.blocked = win.blocked
	s.sampleSnap.perCore = win.perCore
	s.sampleSnap.procCPU = win.proc

	if s.cpu15 == nil || s.cpu60 == nil {
		s.cpu15 = newFloatRing(s.windowSamples(15))
		s.cpu60 = newFloatRing(s.windowSamples(60))
	}
	s.cpu15.push(win.total)
	s.cpu60.push(win.total)
	s.sampleSnap.usage15s = round1(s.cpu15.avg())
	s.sampleSnap.usage60s = round1(s.cpu60.avg())

	s.pushProcSample(procSample{at: cur.at, total: cur.agg.total, procs: cur.proc})
	moverWindow := time.Duration(s.windowSamples(15)) * s.sampleInterval()
	s.sampleSnap.movers = computeTopMovers(s.procHist, moverWindow, win.proc, 5)

	cores := runtime.NumCPU()
	s.sampleSnap.stallScore, s.sampleSnap.stallLevel = computeStallScore(psi, win.runQueue, cores, s.sampleSnap.usage15s)
}

// currentHostSnapshot 返回最近一次快照的副本。快照发布后不再被原地修改，
// CPU 列表与进程表按值拷贝，读取方无法反向影响采样基准。
func (s *Service) currentHostSnapshot() hostSnapshot {
	s.sampleMu.RLock()
	defer s.sampleMu.RUnlock()
	snap := s.sampleSnap
	snap.perCore = append([]coreUsage(nil), snap.perCore...)
	snap.procCPU = cloneProcPct(snap.procCPU)
	snap.movers = append([]cpuMover(nil), snap.movers...)
	snap.services = append([]serviceCPU(nil), snap.services...)
	return snap
}

// readCPUState 读一次 /proc/stat 与全部 /proc/<pid>/stat。
// 进程扫描发生在 /proc/stat 之后，相邻两帧的扫描偏移相同，故进程窗口与系统窗口一致。
func readCPUState() (cpuState, error) {
	at := time.Now()
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return cpuState{}, err
	}
	agg, cores, err := parseCPUStat(string(data))
	if err != nil {
		return cpuState{}, err
	}
	runQueue, blocked := parseProcQueue(string(data))
	proc, err := readProcTicks()
	if err != nil {
		return cpuState{}, fmt.Errorf("读取 /proc 进程列表失败: %w", err)
	}
	return cpuState{at: at, agg: agg, cores: cores, proc: proc, runQueue: runQueue, blocked: blocked}, nil
}

// parseProcQueue 解析 /proc/stat 的 procs_running / procs_blocked。
// 运行队列比 1 分钟负载更及时：running > 核数说明已经排队，blocked > 0 说明有进程卡在 IO。
func parseProcQueue(content string) (running, blocked int) {
	for _, line := range strings.Split(content, "\n") {
		cols := splitFields(line)
		if len(cols) < 2 {
			continue
		}
		switch cols[0] {
		case "procs_running":
			running = int(atoi(cols[1]))
		case "procs_blocked":
			blocked = int(atoi(cols[1]))
		}
	}
	if running < 0 {
		running = 0
	}
	if blocked < 0 {
		blocked = 0
	}
	return running, blocked
}

// readCPUTicks 解析 /proc/stat 的 cpu 聚合行与 cpuN 行。
// 两处使用同一列口径：total 累加 user..steal（guest 已包含在 user/nice 中，不重复累加），
// idle 取 idle+iowait，因此 total 与 per_core 必然自洽。
func readCPUTicks() (coreTicks, map[int]coreTicks, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return coreTicks{}, nil, err
	}
	return parseCPUStat(string(data))
}

// parseCPUStat 是 readCPUTicks 的纯解析部分，便于用固定夹具做单测。
func parseCPUStat(content string) (coreTicks, map[int]coreTicks, error) {
	var agg coreTicks
	hasAgg := false
	cores := map[int]coreTicks{}
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "cpu") {
			continue
		}
		cols := splitFields(line)
		if len(cols) == 0 {
			continue
		}
		if cols[0] == "cpu" {
			ticks, ok := parseCPULine(cols[1:])
			if !ok {
				return coreTicks{}, nil, fmt.Errorf("/proc/stat 的 cpu 聚合行格式非法")
			}
			agg, hasAgg = ticks, true
			continue
		}
		suffix := strings.TrimPrefix(cols[0], "cpu")
		if !isDigits(suffix) {
			continue
		}
		id, err := strconv.Atoi(suffix)
		if err != nil {
			continue
		}
		if ticks, ok := parseCPULine(cols[1:]); ok {
			cores[id] = ticks
		}
	}
	if !hasAgg {
		return coreTicks{}, nil, fmt.Errorf("/proc/stat 缺少 cpu 聚合行")
	}
	return agg, cores, nil
}

// parseCPULine 把一行 CPU 字段转成累计滴答。至少要有 user/nice/system/idle 四列；
// iowait 存在时并入 idle（与内核「空闲含 IO 等待」口径一致）。
func parseCPULine(fields []string) (coreTicks, bool) {
	if len(fields) < 4 {
		return coreTicks{}, false
	}
	nums := parseFields(fields)
	total := 0.0
	for i := 0; i < len(nums) && i < 8; i++ {
		total += nums[i]
	}
	idle := nums[3]
	if len(nums) > 4 {
		idle += nums[4]
	}
	return coreTicks{total: total, idle: idle}, true
}

// readProcTicks 单次遍历 /proc，读出全部进程的累计 (utime+stime) 滴答与进程名。
func readProcTicks() (map[int]procCPUState, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make(map[int]procCPUState, len(entries))
	for _, e := range entries {
		name := e.Name()
		if !isDigits(name) {
			continue
		}
		pid, err := strconv.Atoi(name)
		if err != nil {
			continue
		}
		if info, ok := procCPUInfo(pid); ok {
			out[pid] = info
		}
	}
	return out, nil
}

// diffCPUState 用同一对快照计算总占用、每核占用与进程占用。
// 窗口不足 1 秒或总滴答未推进时返回 ok=false，调用方不得发布该样本。
func diffCPUState(prev, cur cpuState) (cpuWindow, bool) {
	window := cur.at.Sub(prev.at)
	if window < minSampleWindow {
		return cpuWindow{}, false
	}
	dTotal := cur.agg.total - prev.agg.total
	if dTotal <= 0 {
		return cpuWindow{}, false
	}
	dIdle := cur.agg.idle - prev.agg.idle
	total := round1(clampPercent((dTotal - dIdle) / dTotal * 100))

	ids := make([]int, 0, len(cur.cores))
	for id := range cur.cores {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	perCore := make([]coreUsage, 0, len(ids))
	for _, id := range ids {
		p, ok := prev.cores[id]
		if !ok {
			continue
		}
		c := cur.cores[id]
		dt := c.total - p.total
		if dt <= 0 {
			continue
		}
		usage := round1(clampPercent((dt - (c.idle - p.idle)) / dt * 100))
		perCore = append(perCore, coreUsage{id: id, percent: usage})
		if len(perCore) == maxCores {
			break
		}
	}

	proc := make(map[int]float64, len(cur.proc))
	for pid, c := range cur.proc {
		p, ok := prev.proc[pid]
		if !ok {
			continue
		}
		d := c.ticks - p.ticks
		if d < 0 {
			continue
		}
		proc[pid] = round1(clampPercent(d / dTotal * 100))
	}

	return cpuWindow{
		window: window, total: total, perCore: perCore, proc: proc,
		runQueue: cur.runQueue, blocked: cur.blocked,
	}, true
}

// readLoadAvg 读取 /proc/loadavg 前三列。
func readLoadAvg() ([3]float64, error) {
	var out [3]float64
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return out, err
	}
	cols := splitFields(strings.TrimSpace(string(data)))
	if len(cols) < 3 {
		return out, fmt.Errorf("/proc/loadavg 字段不足")
	}
	for i := 0; i < 3; i++ {
		out[i] = atof(cols[i])
	}
	return out, nil
}

// cloneProcPct 复制一张 pid → 占用率表，避免调用方与快照共享可变 map。
func cloneProcPct(in map[int]float64) map[int]float64 {
	if in == nil {
		return nil
	}
	out := make(map[int]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// clampPercent 把占用率限制在 0–100（计数器回绕 / 异常内核数据时兜底）。
func clampPercent(v float64) float64 {
	if math.IsNaN(v) || v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}
