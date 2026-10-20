package probe

import (
	"math"
	"sync"
	"testing"
)

// collectRates 调一次 CollectSystem 并取出网络 / 磁盘合计速率，供断言复用。
func collectRates(t *testing.T, s *Service) (net, dio map[string]any) {
	t.Helper()
	sys, err := s.CollectSystem()
	if err != nil {
		t.Fatalf("CollectSystem: %v", err)
	}
	n, _ := sys["network"].(map[string]any)
	d, _ := sys["disk_io"].(map[string]any)
	if n == nil || d == nil {
		t.Fatalf("响应缺少 network / disk_io: %v", sys)
	}
	return n, d
}

// TestCollectSystemRatesZeroWhenSamplerNotStarted 守住契约：采样循环未产出时，
// 速率一律为 0 且不是 NaN。
func TestCollectSystemRatesZeroWhenSamplerNotStarted(t *testing.T) {
	s := New(nil, nil)
	net, dio := collectRates(t, s)
	for _, k := range []string{"rx_rate", "tx_rate"} {
		if v, ok := net[k].(float64); !ok || math.IsNaN(v) || v != 0 {
			t.Errorf("network.%s=%v，应为 0 且非 NaN", k, net[k])
		}
	}
	for _, k := range []string{"read_rate", "write_rate"} {
		if v, ok := dio[k].(float64); !ok || math.IsNaN(v) || v != 0 {
			t.Errorf("disk_io.%s=%v，应为 0 且非 NaN", k, dio[k])
		}
	}
}

// TestCollectSystemReadsSingleSnapshot 守住速率来源唯一：注入一份快照后并发多次调用
// CollectSystem，返回值始终等于注入值、快照 ts 不被任何读方重置（不再「谁后调用谁偷基线」）。
func TestCollectSystemReadsSingleSnapshot(t *testing.T) {
	s := New(nil, nil)
	s.rateMu.Lock()
	s.rateSnap = rateSnapshot{
		ts:             111,
		diskBytes:      map[string]DiskIOStat{"vda": {ReadBytes: 1, WriteBytes: 2}},
		diskRate:       map[string]DiskIOStat{"vda": {ReadBytes: 1234, WriteBytes: 5678}},
		diskTotalBytes: DiskIOStat{ReadBytes: 1, WriteBytes: 2},
		diskTotalRate:  DiskIOStat{ReadBytes: 1234, WriteBytes: 5678},
		netBytes:       netStat{rx: 10, tx: 20},
		netRate:        netStat{rx: 111, tx: 222},
	}
	s.rateMu.Unlock()

	const workers = 8
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			net, dio := collectRates(t, s)
			if got := dio["read_rate"]; got != float64(1234) {
				t.Errorf("disk_io.read_rate=%v，应为 1234", got)
			}
			if got := dio["write_rate"]; got != float64(5678) {
				t.Errorf("disk_io.write_rate=%v，应为 5678", got)
			}
			if got := net["rx_rate"]; got != float64(111) {
				t.Errorf("network.rx_rate=%v，应为 111", got)
			}
			if got := net["tx_rate"]; got != float64(222) {
				t.Errorf("network.tx_rate=%v，应为 222", got)
			}
		}()
	}
	wg.Wait()

	if got := s.currentRateSnapshot().ts; got != 111 {
		t.Fatalf("并发读取后快照 ts=%d，被读方重置（应为 111）", got)
	}
}

// TestSamplerStopIsIdempotent 守住采样循环可重复启停且不 panic。
func TestSamplerStopIsIdempotent(t *testing.T) {
	s := New(nil, nil)
	s.StartSampler()
	s.StartSampler() // 幂等：不应起第二个循环
	s.StopSampler()
	s.StopSampler()
}
