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

	score, level := computeStallScore(pressure(0, 0, 0), 0, 8)
	if score != 0 || level != "顺畅" {
		t.Fatalf("空载 score=%v level=%q，want 0/顺畅", score, level)
	}

	// 0.4*50 + 0.3*20 + 0.2*10 + 0.1*(8/8*100) = 38
	score, level = computeStallScore(pressure(50, 20, 10), 8, 8)
	if score != 38 || level != "有压力" {
		t.Fatalf("有压 score=%v level=%q，want 38/有压力", score, level)
	}

	score, level = computeStallScore(pressure(90, 80, 70), 16, 8)
	if score <= 50 || level != "明显卡顿" {
		t.Fatalf("满载 score=%v level=%q，want >50/明显卡顿", score, level)
	}
}

// TestParseProcQueue 守住运行队列解析：只认 procs_running / procs_blocked 两行。
func TestParseProcQueue(t *testing.T) {
	fixture := "cpu  1 2 3 4 5 6 7 8 9 10\n" +
		"intr 12345 0 0\n" +
		"procs_running 4\n" +
		"procs_blocked 2\n"
	running, blocked := parseProcQueue(fixture)
	if running != 4 || blocked != 2 {
		t.Fatalf("running=%d blocked=%d，want 4/2", running, blocked)
	}
}

// TestParseCgroupMemMB 守住 cgroup 内存解析：正常字节数四舍五入到 MB，非法值返回 nil。
func TestParseCgroupMemMB(t *testing.T) {
	if got := parseCgroupMemMB("576716800\n"); got == nil || *got != 550 {
		t.Fatalf("mem=%v，want 550", got)
	}
	for _, raw := range []string{"", "[not set]", "abc", "-1", "infinity"} {
		if got := parseCgroupMemMB(raw); got != nil {
			t.Errorf("parseCgroupMemMB(%q)=%v，want nil", raw, *got)
		}
	}
}
