package sysinfo

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"admin-server-go/internal/probe"
)

// Info 是 GET /api/admin/system/info 的响应体。
type Info struct {
	OS            string        `json:"os"`
	Kernel        string        `json:"kernel"`
	Arch          string        `json:"arch"`
	Hostname      string        `json:"hostname"`
	BootTime      int64         `json:"bootTime"`
	UptimeSeconds int64         `json:"uptimeSeconds"`
	Load          []float64     `json:"load"`
	CPUModel      string        `json:"cpuModel"`
	CPUCores      int           `json:"cpuCores"`
	Memory        Memory        `json:"memory"`
	Swap          Swap          `json:"swap"`
	Disks         []Disk        `json:"disks"`
	Ports         Ports         `json:"ports"`
	Systemd       Systemd       `json:"systemd"`
	Logins        Logins        `json:"logins"`
	Temperatures  []Temperature `json:"temperatures,omitempty"`
}

// Memory 是物理内存概况，单位字节。
type Memory struct {
	Total     int64 `json:"total"`
	Available int64 `json:"available"`
	Used      int64 `json:"used"`
	Cached    int64 `json:"cached"`
}

// Swap 是交换分区概况，单位字节。
type Swap struct {
	Total int64 `json:"total"`
	Used  int64 `json:"used"`
}

// Disk 是真实块设备的一个挂载点。百分比保留一位小数；inode 为同一挂载点的文件数口径。
type Disk struct {
	Mount        string  `json:"mount"`
	Filesystem   string  `json:"filesystem"`
	FSType       string  `json:"fstype"`
	Total        int64   `json:"total"`
	Used         int64   `json:"used"`
	Free         int64   `json:"free"`
	Percent      float64 `json:"percent"`
	InodeTotal   int64   `json:"inode_total"`
	InodeUsed    int64   `json:"inode_used"`
	InodePercent float64 `json:"inode_percent"`
}

// PortItem 是一条监听项。Protocol / State 让前端的 TCP/UDP/仅监听过滤有真实依据；
// 端口按数值升序，同端口再按协议、进程名排序。
type PortItem struct {
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	State    string `json:"state"`
	Process  string `json:"process"`
	PID      *int   `json:"pid,omitempty"`
	Address  string `json:"address"`
}

// Ports 是监听端口概况。Error 仅在 `ss` 执行失败时出现。
type Ports struct {
	Total int        `json:"total"`
	Items []PortItem `json:"items"`
	Error string     `json:"error,omitempty"`
}

// FailedUnit 是一个 systemd 失败单元。
type FailedUnit struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Systemd 是失败单元概况。Error 仅在 `systemctl` 执行失败时出现。
type Systemd struct {
	Failed []FailedUnit `json:"failed"`
	Error  string       `json:"error,omitempty"`
}

// LoginItem 是一条最近登录记录。Time 保留 `last` 的原始文本，避免受系统 locale 影响。
type LoginItem struct {
	User string `json:"user"`
	IP   string `json:"ip"`
	Time string `json:"time"`
}

// Logins 是最近登录概况。Error 仅在 `last` 执行失败时出现。
type Logins struct {
	Items []LoginItem `json:"items"`
	Error string      `json:"error,omitempty"`
}

// Temperature 是一个热区读数（摄氏度）。
type Temperature struct {
	Zone    string  `json:"zone"`
	Type    string  `json:"type"`
	Celsius float64 `json:"celsius"`
}

// collectInfo 采集完整概况。静态字段失败时以零值兜底；外部命令失败只写进对应字段的 error。
func collectInfo(ctx context.Context) Info {
	mem := probe.ReadMeminfo()
	memAvailable, memCached := int64(0), int64(0)
	if mem.Available != nil {
		memAvailable = int64(math.Round(*mem.Available))
	}
	if mem.BuffCache != nil {
		memCached = int64(math.Round(*mem.BuffCache))
	}

	var (
		wg      sync.WaitGroup
		ports   Ports
		systemd Systemd
		logins  Logins
	)
	wg.Add(3)
	go func() { defer wg.Done(); ports = readPorts(ctx) }()
	go func() { defer wg.Done(); systemd = readFailedUnits(ctx) }()
	go func() { defer wg.Done(); logins = readLogins(ctx) }()
	wg.Wait()

	bootTime, uptime := bootAndUptime()
	return Info{
		OS:            osRelease(),
		Kernel:        strings.TrimSpace(readString("/proc/sys/kernel/osrelease")),
		Arch:          displayArch(runtime.GOARCH),
		Hostname:      hostname(),
		BootTime:      bootTime,
		UptimeSeconds: uptime,
		Load:          readLoad(),
		CPUModel:      readCPUModel(),
		CPUCores:      runtime.NumCPU(),
		Memory: Memory{
			Total:     int64(math.Round(mem.Total)),
			Available: memAvailable,
			Used:      int64(math.Round(mem.Used)),
			Cached:    memCached,
		},
		Swap: Swap{
			Total: int64(math.Round(mem.SwapTotal)),
			Used:  int64(math.Round(mem.SwapUsed)),
		},
		Disks:        readDisks(),
		Ports:        ports,
		Systemd:      systemd,
		Logins:       logins,
		Temperatures: readTemperatures(),
	}
}

// osRelease 读取 /etc/os-release 的 PRETTY_NAME；缺失时退回 NAME + VERSION。
func osRelease() string {
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		kv := map[string]string{}
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
			if !ok {
				continue
			}
			kv[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
		if pretty := strings.TrimSpace(kv["PRETTY_NAME"]); pretty != "" {
			return pretty
		}
		name := strings.TrimSpace(kv["NAME"])
		version := strings.TrimSpace(kv["VERSION"])
		if joined := strings.TrimSpace(name + " " + version); joined != "" {
			return joined
		}
	}
	return "Linux"
}

// displayArch 把 Go 的 GOARCH 映射为 `uname -m` 风格的展示值（前端概要行按该口径显示）。
func displayArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x86_64"
	case "386":
		return "i386"
	case "arm64":
		return "aarch64"
	default:
		return goarch
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// bootAndUptime 从 /proc/stat 的 btime 与 /proc/uptime 取启动时间（epoch 毫秒）与运行秒数。
// btime 缺失时用「当前时间 − 运行时长」补齐，保证 bootTime 与 uptimeSeconds 自洽。
func bootAndUptime() (int64, int64) {
	var uptime float64
	if cols := strings.Fields(readString("/proc/uptime")); len(cols) > 0 {
		uptime = parseFloat(cols[0])
	}
	uptimeSeconds := int64(math.Round(uptime))

	var btime int64
	for _, line := range strings.Split(readString("/proc/stat"), "\n") {
		cols := strings.Fields(line)
		if len(cols) == 2 && cols[0] == "btime" {
			btime = parseInt64(cols[1])
			break
		}
	}
	if btime > 0 {
		return btime * 1000, uptimeSeconds
	}
	return time.Now().UnixMilli() - uptimeSeconds*1000, uptimeSeconds
}

// readLoad 读取 /proc/loadavg 的前三列（1/5/15 分钟）。
func readLoad() []float64 {
	load := []float64{0, 0, 0}
	cols := strings.Fields(readString("/proc/loadavg"))
	for i := 0; i < len(load) && i < len(cols); i++ {
		load[i] = parseFloat(cols[i])
	}
	return load
}

// readCPUModel 读取 /proc/cpuinfo 的型号；读不到返回空串（前端显示 —）。
func readCPUModel() string {
	for _, line := range strings.Split(readString("/proc/cpuinfo"), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "model name", "Model", "Hardware":
			if v := strings.TrimSpace(value); v != "" {
				return v
			}
		}
	}
	return ""
}

// readDisks 从 /proc/self/mounts 取真实块设备挂载点，再用 Statfs 读容量与 inode。
// 伪文件系统（device 不以 /dev/ 开头）、overlay、tmpfs、squashfs 与 loop 设备一律跳过。
func readDisks() []Disk {
	disks := []Disk{}
	seen := map[string]bool{}
	for _, line := range strings.Split(readString("/proc/self/mounts"), "\n") {
		cols := strings.Fields(line)
		if len(cols) < 3 {
			continue
		}
		device := decodeMountField(cols[0])
		mount := decodeMountField(cols[1])
		fstype := cols[2]
		if !strings.HasPrefix(device, "/dev/") || seen[mount] || skipFSType(fstype, device) {
			continue
		}
		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		seen[mount] = true
		disks = append(disks, diskFromStatfs(mount, device, fstype, &st))
	}
	sort.SliceStable(disks, func(i, j int) bool {
		if disks[i].Mount == "/" {
			return true
		}
		if disks[j].Mount == "/" {
			return false
		}
		return disks[i].Mount < disks[j].Mount
	})
	return disks
}

func skipFSType(fstype, device string) bool {
	switch fstype {
	case "tmpfs", "overlay", "devtmpfs", "squashfs", "iso9660":
		return true
	}
	return strings.HasPrefix(device, "/dev/loop")
}

// diskFromStatfs 把 Statfs 计数换算成字节与人眼百分比。used 按 (blocks − bfree) 计，
// 与 `df` 的 Used 同口径；free 按非特权用户可用块（bavail）计。
func diskFromStatfs(mount, device, fstype string, st *syscall.Statfs_t) Disk {
	bsize := float64(st.Bsize)
	total := float64(st.Blocks) * bsize
	free := float64(st.Bavail) * bsize
	used := (float64(st.Blocks) - float64(st.Bfree)) * bsize
	percent := 0.0
	if total > 0 {
		percent = round1(used / total * 100)
	}
	inodePercent := 0.0
	inodeTotal := int64(st.Files)
	inodeUsed := int64(st.Files - st.Ffree)
	if inodeUsed < 0 {
		inodeUsed = 0
	}
	if st.Files > 0 {
		inodePercent = round1(float64(st.Files-st.Ffree) / float64(st.Files) * 100)
	}
	return Disk{
		Mount:        mount,
		Filesystem:   device,
		FSType:       fstype,
		Total:        int64(math.Round(total)),
		Used:         int64(math.Round(used)),
		Free:         int64(math.Round(free)),
		Percent:      percent,
		InodeTotal:   inodeTotal,
		InodeUsed:    inodeUsed,
		InodePercent: inodePercent,
	}
}

// decodeMountField 还原 /proc/mounts 的八进制转义（空格 / 制表 / 换行 / 反斜杠）。
func decodeMountField(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	replacer := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return replacer.Replace(s)
}

// readTemperatures 读取 sysfs 热区；没有任何热区时返回 nil（JSON 省略该字段）。
func readTemperatures() []Temperature {
	paths, err := filepath.Glob("/sys/class/thermal/thermal_zone*/temp")
	if err != nil || len(paths) == 0 {
		return nil
	}
	out := make([]Temperature, 0, len(paths))
	for _, path := range paths {
		raw := strings.TrimSpace(readString(path))
		if raw == "" {
			continue
		}
		milli, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		zone := filepath.Base(filepath.Dir(path))
		out = append(out, Temperature{
			Zone:    zone,
			Type:    strings.TrimSpace(readString(filepath.Join(filepath.Dir(path), "type"))),
			Celsius: round1(milli / 1000),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Zone < out[j].Zone })
	return out
}

/* ============ 小工具 ============ */

func readString(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

func parseFloat(s string) float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return v
}

func parseInt64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
