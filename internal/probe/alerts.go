package probe

// AlertMetric 实现 alerts.MetricProvider：把告警规则里的指标名映射到当前快照值。
// 只读快照，不触发采样，也不会因为某指标缺失而阻塞其它规则。
func (s *Service) AlertMetric(name string) (float64, bool) {
	snap := s.currentHostSnapshot()
	switch name {
	case "cpu_percent":
		return snap.cpuPercent, true
	case "mem_percent":
		if snap.mem.Total > 0 {
			return round1(snap.mem.Used / snap.mem.Total * 100), true
		}
		return 0, false
	case "disk_percent":
		if d := getDisk(); d != nil {
			if v, ok := d["percent"].(float64); ok {
				return v, true
			}
		}
		return 0, false
	case "stall_score":
		return snap.stallScore, true
	case "run_queue":
		return float64(snap.runQueue), true
	case "psi_cpu", "psi_io", "psi_mem":
		kind := name[len("psi_"):]
		entry, ok := snap.psi[kind].(map[string]any)
		if !ok {
			return 0, false
		}
		some, ok := entry["some"].(map[string]any)
		if !ok {
			return 0, false
		}
		v, ok := some["avg60"].(float64)
		return v, ok
	}
	return 0, false
}
