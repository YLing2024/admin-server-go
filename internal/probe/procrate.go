package probe

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
按进程的吞吐（网速 / 磁盘速率）：

  - 网速：Linux 没有「每进程网卡字节」这种计数器，能做的是按套接字求和。
    `ss -tinp` 会带 tcp_info 的 bytes_sent / bytes_received，把同一 pid 的所有
    套接字加起来，两次采样做差即得速率。纯回环（127.0.0.1 ↔ 127.0.0.1）不算，
    否则 nginx↔后端 这类内网往返会被重复计一遍。
  - 磁盘：直接读 /proc/<pid>/io 的 read_bytes / write_bytes（真实块设备 IO，
    不含页缓存命中），同样两次采样做差。

两者都不是「实时精确」的：采样窗口内的短连接（比如一次 HTTP 请求建完就断）
它的字节会丢；页缓存命中的读也不进 read_bytes。这一点在前端文案里要写清楚。
*/

// ProcRate 是一个进程在本采样窗口内的平均速率，单位字节/秒。
type ProcRate struct {
	Rx    float64 // 网络接收
	Tx    float64 // 网络发送
	Read  float64 // 磁盘读
	Write float64 // 磁盘写
}

// procTotals 是一次采样的累计计数（做差用）。
type procTotals struct {
	rx, tx      int64
	read, write int64
}

// procRateState 保存上一次采样的累计值。
type procRateState struct {
	mu     sync.Mutex
	at     time.Time
	prev   map[int]procTotals
	cached map[int]ProcRate
}

const procRateTTL = 5 * time.Second

// parseSSSocketBytes 解析 `ss -tinp`：返回 pid → (接收字节, 发送字节)。
// 输出是「套接字行 + 缩进的 tcp_info 行」成对出现，套接字行里带 pid，信息行里带字节数。
// 两端都是回环地址的套接字跳过。
func parseSSSocketBytes(out string) map[int][2]int64 {
	res := map[int][2]int64{}
	curPID := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if line[0] != '\t' && line[0] != ' ' {
			// 套接字行：State Recv-Q Send-Q Local Peer Process
			fields := strings.Fields(line)
			curPID = 0
			if len(fields) < 5 || fields[0] == "State" {
				continue
			}
			if isLoopbackAddr(fields[3]) && isLoopbackAddr(fields[4]) {
				continue
			}
			if pid, ok := pidFromUsers(fields); ok {
				curPID = pid
			}
			continue
		}
		if curPID == 0 {
			continue
		}
		sent, recv := int64(-1), int64(-1)
		for _, field := range strings.Fields(line) {
			if v, ok := strings.CutPrefix(field, "bytes_sent:"); ok {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					sent = n
				}
			} else if v, ok := strings.CutPrefix(field, "bytes_received:"); ok {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					recv = n
				}
			}
		}
		if sent < 0 && recv < 0 {
			continue
		}
		cur := res[curPID]
		if recv > 0 {
			cur[0] += recv
		}
		if sent > 0 {
			cur[1] += sent
		}
		res[curPID] = cur
	}
	return res
}

// isLoopbackAddr 判断 "127.0.0.1:6379" / "[::1]:80" 这类地址是否为回环。
func isLoopbackAddr(addr string) bool {
	host := addr
	if idx := strings.LastIndex(addr, ":"); idx > 0 {
		host = addr[:idx]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.")
}

// networkTotals 跑一次 ss -tinp 并按 pid 汇总累计字节。
func networkTotals() map[int][2]int64 {
	out, err := runShell("ss -tinp 2>/dev/null", 2*time.Second)
	if err != nil {
		return map[int][2]int64{}
	}
	return parseSSSocketBytes(out)
}

// ioTotalOfPID 读 /proc/<pid>/io 的 read_bytes / write_bytes；读不到返回 ok=false。
func ioTotalOfPID(pid int) (read, write int64, ok bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "io"))
	if err != nil {
		return 0, 0, false
	}
	var got bool
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "read_bytes":
			read = n
			got = true
		case "write_bytes":
			write = n
			got = true
		}
	}
	return read, write, got
}

// ProcessRates 返回本次窗口内每个进程的四项速率。缓存 5 秒：
// 进程表本身 5 秒刷新一次，更密的采样只是白烧 CPU。
func (s *Service) ProcessRates() map[int]ProcRate {
	st := &s.rates
	st.mu.Lock()
	defer st.mu.Unlock()

	now := time.Now()
	if st.prev != nil && now.Sub(st.at) < procRateTTL {
		return st.cached
	}

	net := networkTotals()
	cur := map[int]procTotals{}
	for pid, v := range net {
		cur[pid] = procTotals{rx: v[0], tx: v[1]}
	}

	entries, err := os.ReadDir(procRoot)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			pid, err := strconv.Atoi(entry.Name())
			if err != nil {
				continue
			}
			read, write, ok := ioTotalOfPID(pid)
			if !ok {
				continue
			}
			item := cur[pid]
			item.read, item.write = read, write
			cur[pid] = item
		}
	}

	rates := map[int]ProcRate{}
	if st.prev != nil {
		dt := now.Sub(st.at).Seconds()
		if dt <= 0 {
			dt = 1
		}
		for pid, c := range cur {
			p, ok := st.prev[pid]
			if !ok {
				continue
			}
			rate := ProcRate{}
			if c.rx >= p.rx {
				rate.Rx = float64(c.rx-p.rx) / dt
			}
			if c.tx >= p.tx {
				rate.Tx = float64(c.tx-p.tx) / dt
			}
			if c.read >= p.read {
				rate.Read = float64(c.read-p.read) / dt
			}
			if c.write >= p.write {
				rate.Write = float64(c.write-p.write) / dt
			}
			rates[pid] = rate
		}
	}

	st.prev = cur
	st.at = now
	st.cached = rates
	return rates
}
