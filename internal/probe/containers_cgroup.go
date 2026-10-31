package probe

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

/*
容器指标的两条采集路径：

 1. 首选 cgroup v2 + 容器清单 API：只读 /sys/fs/cgroup 下的小文件，外加一次
    GET /containers/json（unix socket），单轮耗时几毫秒，不再为每 5 秒的刷新
    fork 一个 `docker stats` 进程（那才是常态 8–14% CPU 的来源）。
 2. 兜底 docker CLI：cgroup 或 socket 不可用（非 cgroup v2、非 systemd driver、
    docker 未装）时退回原来的 `docker ps -a` + `docker stats --no-stream`。

两条路径产出的字段语义一致：CPU% 是窗口内的增量，内存/PIDs 是当前值，
NetIO / BlockIO 与 docker stats 一样给「容器启动至今的累计值」。
*/

// dockerSocketPath / cgroupRoot / procRoot 抽成变量，单测可替换成临时目录。
var (
	dockerSocketPath = "/var/run/docker.sock"
	cgroupRoot       = "/sys/fs/cgroup"
	procRoot         = "/proc"
)

// ctPrevSample 是算容器 CPU% 用的上一次 cgroup 记账值。
type ctPrevSample struct {
	usageUsec int64
	at        time.Time
}

// ctPrevCache 保存每个容器的上一份 CPU 记账；与 containerCache 共用一把锁。
type ctPrevCache map[string]ctPrevSample

// dockerAPIContainer 是 GET /containers/json?all=1 的元素（只取用到的字段）。
type dockerAPIContainer struct {
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`
	Status string   `json:"Status"`
}

// newDockerClient 造一个走 unix socket 的 HTTP 客户端；超时压得很短，
// 因为这条路只是读本地 daemon 的内存结构。
func newDockerClient() *http.Client {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	return &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialer.DialContext(ctx, "unix", dockerSocketPath)
			},
			MaxIdleConns:    2,
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

// listContainersAPI 等价于 `docker ps -a --format '{{json .}}'`，但走 API。
func listContainersAPI(ctx context.Context) ([]dockerAPIContainer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/json?all=1", nil)
	if err != nil {
		return nil, err
	}
	resp, err := newDockerClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker API /containers/json: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var list []dockerAPIContainer
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	return list, nil
}

// containerCgroupDir 找容器 cgroup 目录：systemd driver 放在 system.slice 下，
// cgroupfs driver 直接挂在根上，两种都试。
func containerCgroupDir(id string) (string, bool) {
	for _, candidate := range []string{
		filepath.Join(cgroupRoot, "system.slice", "docker-"+id+".scope"),
		filepath.Join(cgroupRoot, "docker-"+id+".scope"),
	} {
		if _, err := os.Stat(filepath.Join(candidate, "cpu.stat")); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// readKVFile 读 "key value" 每行一对的小文件（cpu.stat / io.stat 同族格式）。
func readKVFile(path string) (map[string]int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 2 {
			continue
		}
		if v, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			out[fields[0]] = v
		}
	}
	return out, sc.Err()
}

// readSingleInt 读只含一个整数的文件（memory.current / pids.current）；"max" 之类返回 false。
func readSingleInt(path string) (int64, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// netTotals 读容器 init 进程所在网络的累计收发字节（/proc/<pid>/net/dev 天然是本命名空间的）。
func netTotals(pid int) (rx, tx int64) {
	f, err := os.Open(filepath.Join(procRoot, strconv.Itoa(pid), "net", "dev"))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.TrimSpace(name) == "lo" {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			continue
		}
		r, _ := strconv.ParseInt(fields[0], 10, 64)
		t, _ := strconv.ParseInt(fields[8], 10, 64)
		rx += r
		tx += t
	}
	return rx, tx
}

// cgroupProcsFirstPID 取 cgroup 里的第一个进程号（容器 init）。
func cgroupProcsFirstPID(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid > 0 {
			return pid
		}
	}
	return 0
}

// fillFromCgroup 用 cgroup 记账填满一个容器的指标；读不到关键文件时返回 false 交给兜底路径。
func fillFromCgroup(dir, id string, stat *ContainerStat, prev ctPrevCache, now time.Time, hostMemBytes int64) (ctPrevSample, bool) {
	cpu, err := readKVFile(filepath.Join(dir, "cpu.stat"))
	if err != nil {
		return ctPrevSample{}, false
	}
	usageUsec, ok := cpu["usage_usec"]
	if !ok {
		return ctPrevSample{}, false
	}
	sample := ctPrevSample{usageUsec: usageUsec, at: now}
	// CPU% 与 docker stats 同口径：窗口内 CPU 时间增量 / 墙钟增量，1 核 = 100%。
	if p, ok := prev[id]; ok {
		if dt := now.Sub(p.at).Seconds(); dt > 0 && usageUsec >= p.usageUsec {
			stat.CPUPct = round1(float64(usageUsec-p.usageUsec) / 1e6 / dt * 100)
		}
	}

	if cur, ok := readSingleInt(filepath.Join(dir, "memory.current")); ok {
		stat.MemMB = round1(float64(cur) / (1024 * 1024))
	}
	limit := int64(0)
	if v, ok := readSingleInt(filepath.Join(dir, "memory.max")); ok && v > 0 {
		limit = v
	} else {
		// memory.max 为 "max"（不限）时，docker stats 也按宿主机总量展示。
		limit = hostMemBytes
	}
	if limit > 0 {
		stat.MemLimitMB = round1(float64(limit) / (1024 * 1024))
		if stat.MemMB > 0 {
			stat.MemPct = round1(stat.MemMB / stat.MemLimitMB * 100)
		}
	}

	// io.stat 每行形如 "8:0 rbytes=... wbytes=... rios=... wios=..."，按设备累加。
	if f, err := os.Open(filepath.Join(dir, "io.stat")); err == nil {
		var rbytes, wbytes int64
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			for _, field := range strings.Fields(sc.Text()) {
				if v, ok := strings.CutPrefix(field, "rbytes="); ok {
					n, _ := strconv.ParseInt(v, 10, 64)
					rbytes += n
				} else if v, ok := strings.CutPrefix(field, "wbytes="); ok {
					n, _ := strconv.ParseInt(v, 10, 64)
					wbytes += n
				}
			}
		}
		f.Close()
		stat.BlockReadMB = round1(float64(rbytes) / (1024 * 1024))
		stat.BlockWriteMB = round1(float64(wbytes) / (1024 * 1024))
	}

	if pids, ok := readSingleInt(filepath.Join(dir, "pids.current")); ok {
		stat.PIDs = int(pids)
	}

	if pid := cgroupProcsFirstPID(dir); pid > 0 {
		rx, tx := netTotals(pid)
		stat.NetRxMB = round1(float64(rx) / (1024 * 1024))
		stat.NetTxMB = round1(float64(tx) / (1024 * 1024))
	}
	return sample, true
}

// collectContainersCgroup 是 collectContainers 的 cgroup 版本；任何一步不成立就返回错误，
// 由调用方退回 docker CLI 路径（面板宁可慢一点，也不能没有数据）。
func (s *Service) collectContainersCgroup(ctx context.Context, hostMemBytes int64) ([]ContainerStat, error) {
	list, err := listContainersAPI(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	items := make([]ContainerStat, 0, len(list))
	next := ctPrevCache{}
	filled := 0
	for _, c := range list {
		name := strings.TrimPrefix(strings.Join(c.Names, ","), "/")
		if name == "" {
			name = c.ID[:min(12, len(c.ID))]
		}
		stat := ContainerStat{Name: name, Image: c.Image, State: c.State, Status: c.Status}
		if c.State == "running" {
			if dir, ok := containerCgroupDir(c.ID); ok {
				if sample, ok := fillFromCgroup(dir, c.ID, &stat, s.containers.prev, now, hostMemBytes); ok {
					next[c.ID] = sample
					filled++
				}
			}
		}
		items = append(items, stat)
	}
	if filled == 0 && len(list) > 0 {
		return nil, fmt.Errorf("cgroup 路径没有采到任何运行中容器的指标")
	}
	s.containers.prev = next
	return items, nil
}

// hostMemBytes 读 /proc/meminfo 的 MemTotal（字节）；读不到返回 0，此时不给内存上限与百分比。
func hostMemBytes() int64 {
	raw, err := os.ReadFile(filepath.Join(procRoot, "meminfo"))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "MemTotal:" {
			continue
		}
		if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			return kb * 1024
		}
	}
	return 0
}

// ensureContainerPrev 兜底路径也要清掉 cgroup 记账，避免长时间不用后算出一个跨小时的 CPU%。
func (c *containerCache) ensureContainerPrev() ctPrevCache {
	if c.prev == nil {
		c.prev = ctPrevCache{}
	}
	return c.prev
}
