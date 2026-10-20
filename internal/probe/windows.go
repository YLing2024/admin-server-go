package probe

import (
	"sort"
	"strconv"
	"time"
)

// floatRing 是固定长度的浮点环形缓冲。窗口均值只用已写入的样本，
// 启动初期返回现有样本均值，避免把 0 当成真实采样。
type floatRing struct {
	buf   []float64
	next  int
	count int
}

// newFloatRing 创建容量为 capacity 的环形缓冲；容量必须为正。
func newFloatRing(capacity int) *floatRing {
	if capacity < 1 {
		capacity = 1
	}
	return &floatRing{buf: make([]float64, capacity)}
}

// push 写入一个样本，写满后覆盖最旧的样本。
func (r *floatRing) push(v float64) {
	r.buf[r.next] = v
	r.next = (r.next + 1) % len(r.buf)
	if r.count < len(r.buf) {
		r.count++
	}
}

// avg 返回已写入样本的算术均值；没有样本时返回 0。
func (r *floatRing) avg() float64 {
	if r.count == 0 {
		return 0
	}
	sum := 0.0
	for i := 0; i < r.count; i++ {
		sum += r.buf[i]
	}
	return sum / float64(r.count)
}

// procSample 是一个 tick 的全部进程累计滴答快照，用于 15 秒窗口归因。
// 保留整份快照（而非只保留总量）是为了让窗口内退出的进程仍能算出 Δ 与名字。
type procSample struct {
	at    time.Time
	total float64
	procs map[int]procCPUState
}

// cpuMover 是窗口内 CPU 增量榜的一行。
type cpuMover struct {
	PID            int     `json:"pid"`
	Name           string  `json:"name"`
	DeltaPercent   float64 `json:"delta_percent"`
	CurrentPercent float64 `json:"current_percent"`
	Exited         bool    `json:"exited,omitempty"`
}

// pushProcSample 追加一份进程快照并按窗口上限裁剪历史。
// capacity 是窗口内 tick 数 + 1：额外一帧用来覆盖恰好 15 秒前的基线。
func (s *Service) pushProcSample(sample procSample) {
	capacity := s.windowSamples(15) + 1
	s.procHist = append(s.procHist, sample)
	if len(s.procHist) > capacity {
		s.procHist = s.procHist[len(s.procHist)-capacity:]
	}
}

// computeTopMovers 计算最近 window 内 Δ(utime+stime) 最大的进程。
//
// 基线取窗口内最旧的一帧：窗口开始后才出现的进程，用第一次出现的帧做基线；
// 窗口内退出的进程，用最后一次出现（即退出前）的累计滴答做终点。这样
// 「先出现后消失」的短命进程也能归因，而不是因为没有当前 /proc 条目被丢掉。
func computeTopMovers(hist []procSample, window time.Duration, currentPercent map[int]float64, limit int) []cpuMover {
	if len(hist) < 2 || limit <= 0 {
		return []cpuMover{}
	}
	last := hist[len(hist)-1]
	baseIdx := len(hist) - 1
	for i := 0; i < len(hist); i++ {
		if last.at.Sub(hist[i].at) <= window {
			baseIdx = i
			break
		}
	}
	if baseIdx >= len(hist)-1 {
		return []cpuMover{}
	}
	base := hist[baseIdx]

	type span struct {
		ticks float64
		total float64
	}
	starts := make(map[int]span, len(base.procs))
	for pid, info := range base.procs {
		starts[pid] = span{ticks: info.ticks, total: base.total}
	}
	// 窗口开始后才出现的进程：从第一次出现的累计值算起。
	for i := baseIdx; i < len(hist); i++ {
		for pid, info := range hist[i].procs {
			if _, ok := starts[pid]; !ok {
				starts[pid] = span{ticks: info.ticks, total: hist[i].total}
			}
		}
	}

	lastSeen := make(map[int]procCPUState, len(last.procs))
	lastTotal := make(map[int]float64, len(last.procs))
	for i := baseIdx; i < len(hist); i++ {
		for pid, info := range hist[i].procs {
			lastSeen[pid] = info
			lastTotal[pid] = hist[i].total
		}
	}

	type candidate struct {
		mover cpuMover
		delta float64
	}
	candidates := make([]candidate, 0, len(lastSeen))
	for pid, start := range starts {
		end, ok := lastSeen[pid]
		if !ok {
			continue
		}
		delta := end.ticks - start.ticks
		denom := lastTotal[pid] - start.total
		if delta <= 0 || denom <= 0 {
			continue
		}
		pct := round1(clampPercent(delta / denom * 100))
		if pct < 0.1 {
			continue
		}
		name := end.name
		if name == "" {
			name = "pid " + strconv.Itoa(pid)
		}
		_, alive := last.procs[pid]
		candidates = append(candidates, candidate{
			delta: delta,
			mover: cpuMover{
				PID:            pid,
				Name:           name,
				DeltaPercent:   pct,
				CurrentPercent: currentPercent[pid],
				Exited:         !alive,
			},
		})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].delta != candidates[j].delta {
			return candidates[i].delta > candidates[j].delta
		}
		return candidates[i].mover.PID < candidates[j].mover.PID
	})
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out := make([]cpuMover, 0, len(candidates))
	for _, c := range candidates {
		out = append(out, c.mover)
	}
	return out
}
