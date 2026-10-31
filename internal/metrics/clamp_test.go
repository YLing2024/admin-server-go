package metrics

import "testing"

// TestClampPercent 确认脏百分比被夹进 0–100，正常值与 nil 指针不被改写。
func TestClampPercent(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0, 0}, {55.9, 55.9}, {100, 100}, {4144569, 100}, {-3, 0},
	}
	for _, c := range cases {
		if got := clampPercent(c.in); got != c.want {
			t.Errorf("clampPercent(%v) = %v，应为 %v", c.in, got, c.want)
		}
	}
	v := 596822.0
	if got, ok := clampPercentPtr(&v).(float64); !ok || got != 100 {
		t.Errorf("clampPercentPtr(596822) = %v，应为 100", got)
	}
	if clampPercentPtr(nil) != nil {
		t.Errorf("nil 指针应保持 nil（写库为 NULL）")
	}
}

// TestMetricPointValuesClamps 确认落库前的摊平结果里没有越界百分比。
func TestMetricPointValuesClamps(t *testing.T) {
	p := HistoryPoint{CPU: 12.5, MemPercent: 55, DiskPercent: 4144569, SwapPercent: 38}
	vals := metricPointValues(p, 1e9, 1.6e10)
	if vals["disk_percent"] != 100.0 {
		t.Errorf("disk_percent = %v，应为 100", vals["disk_percent"])
	}
	if vals["cpu"] != 12.5 || vals["swap_percent"] != 38.0 {
		t.Errorf("正常值被改写: %+v", vals)
	}
}
