package probe

import (
	"math"
)

// netStat 是一组网络字节数（rx / tx），既用于累计值也用于速率（字节/秒）。
type netStat struct {
	rx float64
	tx float64
}

// rateSnapshot 是唯一后台采样循环产出的「最近一次速率快照」。
//
// diskBytes / diskRate 按设备名索引（整盘），diskTotalBytes / diskTotalRate 是全部整盘合计，
// netBytes / netRate 是全部接口合计。ts=0 表示循环尚未产出第一个点，读取方一律按 0 处理。
type rateSnapshot struct {
	ts             int64
	diskBytes      map[string]DiskIOStat
	diskRate       map[string]DiskIOStat
	diskTotalBytes DiskIOStat
	diskTotalRate  DiskIOStat
	netBytes       netStat
	netRate        netStat
}

// rateCounters 是上一次采样的绝对计数（时刻 + 按设备磁盘累计字节 + 网络累计字节），
// 仅由采样循环持有，不对外暴露。
type rateCounters struct {
	ts   int64
	disk map[string]DiskIOStat
	rx   float64
	tx   float64
}

// sampleRates 采一次样：读绝对计数、与上一次做差换算速率，整体发布为新快照。
// 首次采样（无基线）速率全为 0；新出现的设备本次只登记绝对计数，速率从下一次起有值。
func (s *Service) sampleRates() {
	now := nowMillis()
	diskAll := DiskIOSampleAll()
	rx, tx := NetSample()

	s.rateMu.Lock()
	defer s.rateMu.Unlock()

	prev := s.ratePrev
	snap := rateSnapshot{
		ts:        now,
		diskBytes: diskAll,
		diskRate:  map[string]DiskIOStat{},
		netBytes:  netStat{rx: rx, tx: tx},
	}
	if prev.ts != 0 {
		if dt := float64(now-prev.ts) / 1000; dt > 0 {
			snap.diskRate = computeDiskRates(prev.disk, diskAll, dt)
			snap.netRate = netStat{
				rx: math.Max(0, (rx-prev.rx)/dt),
				tx: math.Max(0, (tx-prev.tx)/dt),
			}
		}
	}
	snap.diskTotalBytes = sumDiskIO(diskAll)
	snap.diskTotalRate = sumDiskIO(snap.diskRate)

	s.ratePrev = rateCounters{ts: now, disk: diskAll, rx: rx, tx: tx}
	s.rateSnap = snap
}

// currentRateSnapshot 返回最近一次快照的副本。快照发布后不再被原地修改，故读方无需持锁。
func (s *Service) currentRateSnapshot() rateSnapshot {
	s.rateMu.RLock()
	defer s.rateMu.RUnlock()
	return s.rateSnap
}

// computeDiskRates 按设备计算 (本次 − 上次)/dt 的读写速率（字节/秒）。
// dt<=0 或设备在上次采样中不存在时，该设备速率按 0 处理。
func computeDiskRates(prev, cur map[string]DiskIOStat, dt float64) map[string]DiskIOStat {
	out := make(map[string]DiskIOStat, len(cur))
	if dt <= 0 {
		return out
	}
	for dev, c := range cur {
		p, ok := prev[dev]
		if !ok {
			continue
		}
		out[dev] = DiskIOStat{
			ReadBytes:  math.Max(0, (c.ReadBytes-p.ReadBytes)/dt),
			WriteBytes: math.Max(0, (c.WriteBytes-p.WriteBytes)/dt),
		}
	}
	return out
}

// sumDiskIO 累加一份按设备的读写字节（累计值或速率均可）。
func sumDiskIO(m map[string]DiskIOStat) DiskIOStat {
	var total DiskIOStat
	for _, v := range m {
		total.ReadBytes += v.ReadBytes
		total.WriteBytes += v.WriteBytes
	}
	return total
}
