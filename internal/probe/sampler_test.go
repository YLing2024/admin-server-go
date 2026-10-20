package probe

import (
	"math"
	"testing"
	"time"
)

// cpuStateAt 构造测试用快照；聚合行必须等于各核之和，才能验证自洽性。
func cpuStateAt(at time.Time, agg coreTicks, cores map[int]coreTicks, proc map[int]float64) cpuState {
	return cpuState{at: at, agg: agg, cores: cores, proc: proc}
}

// TestDiffCPUStateSelfConsistent 守住同一对快照下 total 与 per_core 均值一致：
// cpu0 忙碌 20%、cpu1 空闲，聚合总占用也必须是 10%，进程占比用同一 Δtotal。
func TestDiffCPUStateSelfConsistent(t *testing.T) {
	base := time.Unix(1700000000, 0)
	prev := cpuStateAt(base,
		coreTicks{total: 10000, idle: 8000},
		map[int]coreTicks{
			0: {total: 5000, idle: 4000},
			1: {total: 5000, idle: 4000},
		},
		map[int]float64{123: 100, 456: 0},
	)
	cur := cpuStateAt(base.Add(2*time.Second),
		coreTicks{total: 11000, idle: 8900},
		map[int]coreTicks{
			0: {total: 5500, idle: 4400}, // Δtotal=500, Δidle=400 → 20%
			1: {total: 5500, idle: 4500}, // Δtotal=500, Δidle=500 → 0%
		},
		map[int]float64{123: 150, 457: 999}, // 123 用 50/1000 → 5%；457 新进程无基线
	)

	win, ok := diffCPUState(prev, cur)
	if !ok {
		t.Fatal("2 秒窗口应产生样本")
	}
	if win.total != 10 {
		t.Fatalf("total=%.1f，want 10", win.total)
	}
	if len(win.perCore) != 2 {
		t.Fatalf("per_core=%d 项，want 2", len(win.perCore))
	}
	sum := 0.0
	for _, c := range win.perCore {
		sum += c.percent
	}
	if mean := sum / float64(len(win.perCore)); math.Abs(mean-win.total) > 0.01 {
		t.Fatalf("per_core 均值=%.2f 与 total=%.2f 不自洽", mean, win.total)
	}
	if got := win.proc[123]; got != 5 {
		t.Fatalf("pid 123 cpu=%.1f，want 5", got)
	}
	if _, ok := win.proc[457]; ok {
		t.Fatal("新进程没有上一帧基线，不应出现在结果里")
	}
}

// TestDiffCPUStateDiscardsShortWindow 守住 <1 秒窗口直接丢弃，不产出任何值。
func TestDiffCPUStateDiscardsShortWindow(t *testing.T) {
	base := time.Unix(1700000000, 0)
	prev := cpuStateAt(base, coreTicks{total: 1000, idle: 800},
		map[int]coreTicks{0: {total: 1000, idle: 800}}, map[int]float64{1: 10})
	cur := cpuStateAt(base.Add(500*time.Millisecond), coreTicks{total: 1010, idle: 805},
		map[int]coreTicks{0: {total: 1010, idle: 805}}, map[int]float64{1: 12})

	if _, ok := diffCPUState(prev, cur); ok {
		t.Fatal("500ms 窗口应被丢弃")
	}
}

// TestCollectSystemReadsCachedCPUSnapshot 守住请求路径只读快照：连续调用多次后，
// CPU 值与基线都不变（旧实现的问题正是每次请求都偷走采样基线）。
func TestCollectSystemReadsCachedCPUSnapshot(t *testing.T) {
	s := New(nil, nil)
	s.sampleSnap = hostSnapshot{
		cpuPercent:  12.3,
		perCore:     []coreUsage{{id: 0, percent: 12.3}},
		procCPU:     map[int]float64{42: 6.5},
		mem:         Meminfo{Total: 1000, Used: 400},
		loadavg:     [3]float64{1.2, 1.0, 0.8},
		loadPercent: 15.0,
	}

	for i := 0; i < 10; i++ {
		sys, err := s.CollectSystem()
		if err != nil {
			t.Fatalf("CollectSystem: %v", err)
		}
		cpu, _ := sys["cpu"].(map[string]any)
		if got := cpu["usage_percent"]; got != 12.3 {
			t.Fatalf("第 %d 次 usage_percent=%v，应恒为 12.3", i+1, got)
		}
		if got := cpu["load_percent"]; got != 15.0 {
			t.Fatalf("第 %d 次 load_percent=%v，应恒为 15.0", i+1, got)
		}
	}
	if s.samplePrev != nil {
		t.Fatal("HTTP 请求不应写入 CPU 采样基线")
	}
}
