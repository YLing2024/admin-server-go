package probe

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// Meminfo 是 /proc/meminfo 的解析结果（单位字节）。
// Used 取真实占用（total − available，不含内核可回收的页缓存/缓冲），
// 旧口径 total − free 会把页缓存算成已用，导致面板虚高。
type Meminfo struct {
	Total     float64
	Used      float64
	Free      float64
	Available *float64
	BuffCache *float64
	SwapTotal float64
	SwapUsed  float64
	SwapFree  float64
	// SwapPercent 是 swap 使用率（百分比，一位小数）。
	SwapPercent float64
}

var meminfoLineRe = regexp.MustCompile(`^(\w+):\s+(\d+)\s*kB`)

// ReadMeminfo 解析 /proc/meminfo；字段缺失回退 sysinfo，整体不可读回退全部 sysinfo。
func ReadMeminfo() Meminfo {
	kv := map[string]float64{}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if m := meminfoLineRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.ParseFloat(m[2], 64)
				kv[m[1]] = n * 1024
			}
		}
	} else {
		kv = map[string]float64{}
	}

	total, free := sysinfoTotals()
	if v, ok := kv["MemTotal"]; ok && v > 0 {
		total = v
	}
	if v, ok := kv["MemFree"]; ok {
		free = v
	}

	var available, buffCache *float64
	if v, ok := kv["MemAvailable"]; ok {
		available = &v
	}
	if buffers, ok := kv["Buffers"]; ok {
		if cached, ok2 := kv["Cached"]; ok2 {
			sum := buffers + cached
			buffCache = &sum
		}
	}

	swapTotal := kv["SwapTotal"]
	swapFree := kv["SwapFree"]
	used := total - free
	if available != nil {
		used = total - *available
	}
	swapUsed := swapTotal - swapFree
	swapPercent := 0.0
	if swapTotal > 0 {
		swapPercent = round1((swapUsed / swapTotal) * 100)
	}
	return Meminfo{
		Total:       total,
		Used:        used,
		Free:        free,
		Available:   available,
		BuffCache:   buffCache,
		SwapTotal:   swapTotal,
		SwapUsed:    swapUsed,
		SwapFree:    swapFree,
		SwapPercent: swapPercent,
	}
}

// sysinfoTotals 是 /proc 不可读时的回退（对齐旧实现 os.totalmem / os.freemem）。
func sysinfoTotals() (total, free float64) {
	var si syscall.Sysinfo_t
	if err := syscall.Sysinfo(&si); err != nil {
		return 0, 0
	}
	unit := float64(si.Unit)
	return float64(si.Totalram) * unit, float64(si.Freeram) * unit
}

// MemPercent 返回内存使用率（百分比，一位小数），真实占用口径 total − MemAvailable。
func MemPercent() float64 {
	kv := map[string]float64{}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if m := meminfoLineRe.FindStringSubmatch(line); m != nil {
				n, _ := strconv.ParseFloat(m[2], 64)
				kv[m[1]] = n * 1024
			}
		}
		if total, ok := kv["MemTotal"]; ok && total > 0 {
			if avail, ok2 := kv["MemAvailable"]; ok2 {
				return round1(((total - avail) / total) * 100)
			}
		}
	}
	total, free := sysinfoTotals()
	if total <= 0 {
		return 0
	}
	return round1(((total - free) / total) * 100)
}

// SwapPercent 返回 swap 使用率（百分比，一位小数）；无 swap 返回 0。
func SwapPercent() float64 {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	kv := map[string]float64{}
	for _, line := range strings.Split(string(data), "\n") {
		if m := meminfoLineRe.FindStringSubmatch(line); m != nil {
			n, _ := strconv.ParseFloat(m[2], 64)
			kv[m[1]] = n * 1024
		}
	}
	total := kv["SwapTotal"]
	free := kv["SwapFree"]
	if total <= 0 {
		return 0
	}
	return round1(((total - free) / total) * 100)
}

var zramNameRe = regexp.MustCompile(`^zram\d+$`)

// getZram 扫描 /sys/block/zram*，返回首选压缩设备（zram0）的明细；无 zram 返回 nil。
func getZram() map[string]any {
	entries, err := os.ReadDir("/sys/block")
	if err != nil {
		return nil
	}
	var zramDevs []string
	for _, e := range entries {
		if zramNameRe.MatchString(e.Name()) {
			zramDevs = append(zramDevs, e.Name())
		}
	}
	if len(zramDevs) == 0 {
		return nil
	}
	sort.Strings(zramDevs)
	comp := zramDevs[0]

	readInt := func(file string) float64 {
		data, err := os.ReadFile("/sys/block/" + comp + "/" + file)
		if err != nil {
			return 0
		}
		return atof(strings.TrimSpace(string(data)))
	}

	var algorithm any
	if data, err := os.ReadFile("/sys/block/" + comp + "/comp_algorithm"); err == nil {
		raw := strings.TrimSpace(string(data))
		algorithm = extractBracketed(raw)
	}

	var orig, compr, used float64
	if data, err := os.ReadFile("/sys/block/" + comp + "/mm_stat"); err == nil {
		cols := splitFields(strings.TrimSpace(string(data)))
		if len(cols) >= 3 {
			orig = atof(cols[0])
			compr = atof(cols[1])
			used = atof(cols[2])
		}
	}
	comprSize := compr
	if comprSize == 0 {
		comprSize = readInt("mem_used_total")
	}
	if comprSize == 0 {
		comprSize = used
	}
	return map[string]any{
		"total":     readInt("disksize"),
		"used":      used,
		"origSize":  orig,
		"comprSize": comprSize,
		"algorithm": algorithm,
	}
}

// extractBracketed 取 "[lzo] lz4" 里的当前算法名；无中括号则取整串（空返回 nil）。
func extractBracketed(raw string) any {
	if i := strings.Index(raw, "["); i >= 0 {
		if j := strings.Index(raw[i:], "]"); j > 0 {
			return raw[i+1 : i+j]
		}
	}
	v := strings.TrimSpace(raw)
	if v == "" {
		return nil
	}
	return v
}
