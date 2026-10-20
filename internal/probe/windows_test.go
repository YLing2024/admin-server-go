package probe

import (
	"testing"
	"time"
)

// TestFloatRingFixedCapacity 守住环形缓冲不会无限增长：写满后覆盖最旧样本，
// 均值只按当前容量内的样本计算。
func TestFloatRingFixedCapacity(t *testing.T) {
	r := newFloatRing(3)
	r.push(1)
	r.push(2)
	if got := r.avg(); got != 1.5 {
		t.Fatalf("avg=%v，want 1.5", got)
	}
	r.push(3)
	r.push(4)
	if len(r.buf) != 3 {
		t.Fatalf("缓冲长度=%d，want 3", len(r.buf))
	}
	if got := r.avg(); got != 3 {
		t.Fatalf("覆盖后 avg=%v，want 3", got)
	}
}

// TestComputeTopMoversIncludesExited 守住窗口归因：窗口内出现又退出的短命进程
// 仍要按 Δ(utime+stime) 进入榜单并标 exited，不能因为当前 /proc 没有它而丢失。
func TestComputeTopMoversIncludesExited(t *testing.T) {
	base := time.Unix(1700000000, 0)
	hist := []procSample{
		{at: base, total: 0, procs: map[int]procCPUState{100: {ticks: 1000, name: "worker"}}},
		{at: base.Add(5 * time.Second), total: 5000, procs: map[int]procCPUState{
			100: {ticks: 1050, name: "worker"},
			200: {ticks: 100, name: "yes"},
		}},
		{at: base.Add(10 * time.Second), total: 10000, procs: map[int]procCPUState{
			100: {ticks: 1100, name: "worker"},
			200: {ticks: 400, name: "yes"},
		}},
		{at: base.Add(15 * time.Second), total: 15000, procs: map[int]procCPUState{
			100: {ticks: 1150, name: "worker"},
		}},
	}
	movers := computeTopMovers(hist, 15*time.Second, map[int]float64{100: 1.0}, 5)
	if len(movers) != 2 {
		t.Fatalf("movers=%d，want 2: %+v", len(movers), movers)
	}
	if movers[0].PID != 200 || movers[0].Name != "yes" {
		t.Fatalf("第一名应为已退出的 yes，实际 %+v", movers[0])
	}
	if !movers[0].Exited {
		t.Fatal("yes 已退出，应标 exited=true")
	}
	if movers[0].DeltaPercent != 6 {
		t.Fatalf("yes delta_percent=%v，want 6", movers[0].DeltaPercent)
	}
	if movers[1].PID != 100 || movers[1].Exited {
		t.Fatalf("第二名应为仍在运行的 worker，实际 %+v", movers[1])
	}
	if movers[1].CurrentPercent != 1.0 {
		t.Fatalf("worker current_percent=%v，want 1", movers[1].CurrentPercent)
	}
}

// TestComputeTopMoversNeedsHistory 守住只有一帧时没有可比基线，返回空榜而非乱猜。
func TestComputeTopMoversNeedsHistory(t *testing.T) {
	hist := []procSample{{at: time.Unix(1700000000, 0), total: 100, procs: map[int]procCPUState{1: {ticks: 5}}}}
	if got := computeTopMovers(hist, 15*time.Second, nil, 5); len(got) != 0 {
		t.Fatalf("单帧历史应返回空榜，实际 %+v", got)
	}
}
