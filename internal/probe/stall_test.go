package probe

import "testing"

// TestComputeStallScore 守住卡顿指数的权重与档位：空载 < 20，明显争抢 > 50。
func TestComputeStallScore(t *testing.T) {
	pressure := func(cpu, io, mem float64) map[string]any {
		entry := func(v float64) map[string]any {
			return map[string]any{"some": map[string]any{"avg60": v}}
		}
		return map[string]any{"cpu": entry(cpu), "io": entry(io), "memory": entry(mem)}
	}

	score, level := computeStallScore(pressure(0, 0, 0), 0, 8, 0)
	if score != 0 || level != "顺畅" {
		t.Fatalf("空载 score=%v level=%q，want 0/顺畅", score, level)
	}

	// 0.4*50 + 0.3*20 + 0.2*10 + 0.1*(8/8*100) = 38
	score, level = computeStallScore(pressure(50, 20, 10), 8, 8, 50)
	if score != 38 || level != "有压力" {
		t.Fatalf("有压 score=%v level=%q，want 38/有压力", score, level)
	}

	score, level = computeStallScore(pressure(90, 80, 70), 16, 8, 90)
	if score <= 50 || level != "明显卡顿" {
		t.Fatalf("满载 score=%v level=%q，want >50/明显卡顿", score, level)
	}
}

// TestStallLevelFloor 守住档位下限：分数不变，但排队或持续压满时至少「有压力」。
func TestStallLevelFloor(t *testing.T) {
	pressure := map[string]any{
		"cpu":    map[string]any{"some": map[string]any{"avg60": 0.0}},
		"io":     map[string]any{"some": map[string]any{"avg60": 0.0}},
		"memory": map[string]any{"some": map[string]any{"avg60": 0.0}},
	}
	// 8 核 run_queue=14（> 12）：分数不变，档位抬到「有压力」。
	score, level := computeStallScore(pressure, 14, 8, 0)
	if score != 10 || level != "有压力" {
		t.Fatalf("排队 score=%v level=%q，want 10/有压力", score, level)
	}

	// usage_15s 压满且 run_queue>cores：同样至少「有压力」。
	score, level = computeStallScore(pressure, 9, 8, 95)
	if score != 10 || level != "有压力" {
		t.Fatalf("持续压满 score=%v level=%q，want 10/有压力", score, level)
	}

	// 恰好压满但未排队（run_queue==cores）：不触发下限，保持「顺畅」。
	score, level = computeStallScore(pressure, 8, 8, 100)
	if score != 10 || level != "顺畅" {
		t.Fatalf("未排队 score=%v level=%q，want 10/顺畅", score, level)
	}

	// run_queue 恰好等于 cores*1.5 不触发（规则是「大于」）。
	score, level = computeStallScore(pressure, 12, 8, 0)
	if score != 10 || level != "顺畅" {
		t.Fatalf("边界排队 score=%v level=%q，want 10/顺畅", score, level)
	}
}
