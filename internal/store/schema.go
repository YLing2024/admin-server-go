package store

import "sort"

// 指标库的列口径（与旧实现 src/db.ts 的 METRIC_KEYS / AGG_TABLES / METRIC_ROUND 一致）。
// 固定 12 列之后按启动时探测到的整盘设备追加 disk_io_<dev>_read / _write 速率列。

// baseMetricKeys 是固定 12 列（合计口径），永远排在最前。
var baseMetricKeys = []string{
	"cpu",
	"mem_percent",
	"mem_used",
	"mem_total",
	"swap_percent",
	"psi_mem_avg10",
	"psi_cpu_avg10",
	"psi_io_avg10",
	"net_rx_rate",
	"net_tx_rate",
	"disk_io_read",
	"disk_io_write",
}

// baseMetricRound 是固定列的小数位（与旧 AVG 展示口径一致）。
var baseMetricRound = map[string]int{
	"cpu":           1,
	"mem_percent":   1,
	"mem_used":      0,
	"mem_total":     0,
	"swap_percent":  1,
	"psi_mem_avg10": 2,
	"psi_cpu_avg10": 2,
	"psi_io_avg10":  2,
	"net_rx_rate":   0,
	"net_tx_rate":   0,
	"disk_io_read":  0,
	"disk_io_write": 0,
}

// MetricKeys 是 raw 与物化桶共用的指标列（顺序即建表顺序）：
// 固定 12 列 + 每块整盘的按盘速率列。由 SetDiskDevices 在启动时重建。
var MetricKeys = append([]string(nil), baseMetricKeys...)

// MetricRound 是各指标物化值的小数位；按盘速率列一律 0。
var MetricRound = cloneMetricRound(baseMetricRound)

// diskDevices 是启动时登记的整盘设备名（按字典序）。
var diskDevices []string

// SetDiskDevices 登记整盘设备集合并据此重建 MetricKeys / MetricRound：在固定 12 列后按设备名
// 字典序追加 `disk_io_<dev>_read` / `disk_io_<dev>_write`（合计列保持不变）。必须在建表 / 迁移
// 之前调用；重复调用幂等，以最后一次传入的全量集合为准。
func SetDiskDevices(devs []string) {
	set := make(map[string]bool, len(devs))
	for _, d := range devs {
		if d != "" {
			set[d] = true
		}
	}
	diskDevices = diskDevices[:0]
	for d := range set {
		diskDevices = append(diskDevices, d)
	}
	sort.Strings(diskDevices)

	MetricKeys = append([]string(nil), baseMetricKeys...)
	MetricRound = cloneMetricRound(baseMetricRound)
	for _, dev := range diskDevices {
		read := "disk_io_" + dev + "_read"
		write := "disk_io_" + dev + "_write"
		MetricKeys = append(MetricKeys, read, write)
		MetricRound[read] = 0
		MetricRound[write] = 0
	}
}

// DiskDevices 返回已登记的整盘设备名副本（按字典序）。
func DiskDevices() []string {
	return append([]string(nil), diskDevices...)
}

// cloneMetricRound 复制一份小数位映射，避免调用方改动内部基准表。
func cloneMetricRound(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// AggDef 描述一个物化聚合档位。
type AggDef struct {
	// Key 是档位键（1m / 1h / 1d）。
	Key string
	// Table 是物化表名。
	Table string
	// StepSec 是桶宽（秒）。
	StepSec int64
	// Pct 是分位口径：100 表示桶内最大值（等价 Max），其余为最近秩分位 ceil(pct/100*N)。
	Pct int
}

// AggTables 是全部物化档位；三者都直接从 raw metrics 计算，1h / 1d 严禁由 1m 递归聚合。
var AggTables = []AggDef{
	{Key: "1m", Table: "metrics_1m", StepSec: 60, Pct: 100},
	{Key: "1h", Table: "metrics_1h", StepSec: 3600, Pct: 90},
	{Key: "1d", Table: "metrics_1d", StepSec: 86400, Pct: 99},
}

// RawRetentionSeconds 是 raw 5s 点保留窗口（30 天）。
const RawRetentionSeconds = 30 * 24 * 3600

// MinuteRetentionSeconds 是分钟桶保留窗口（90 天）。
const MinuteRetentionSeconds = 90 * 24 * 3600
