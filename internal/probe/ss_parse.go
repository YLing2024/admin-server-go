package probe

import (
	"strconv"
	"strings"
)

/*
服务状态探活的旧做法是「每个端口 fork 一次 ss -ltnp，每个 systemd 单元 fork 一次
systemctl is-active + 一次 pgrep」，而 SSE 每帧都会调一次 → 每秒十来个子进程，
常态吃掉接近一个核。这里把两件事合成「一次调用 + 5 秒缓存」：

  - parseSSListen：一次 ss -ltnp 的输出 → 「监听端口 → pid」全表；
  - parseSystemctlShow：一次 systemctl show 的输出 → ActiveState + MainPID。

解析逻辑抽成纯函数放到这里，方便单测（不 spawn 任何进程）。
*/

// parseSSListen 解析 `ss -ltnp` 输出，返回「本地端口 → 监听进程 pid」。
// 只收 pid 已知的监听项；同一端口被多个进程监听时保留第一个。
func parseSSListen(out string) map[int]int {
	ports := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "LISTEN" {
			continue
		}
		port, ok := portFromLocal(fields[3])
		if !ok {
			continue
		}
		pid, ok := pidFromUsers(fields)
		if !ok {
			continue
		}
		if _, exists := ports[port]; !exists {
			ports[port] = pid
		}
	}
	return ports
}

// portFromLocal 从 "127.0.0.1:6379" / "*:80" / "[::]:443" 里取端口号。
func portFromLocal(local string) (int, bool) {
	idx := strings.LastIndex(local, ":")
	if idx < 0 || idx == len(local)-1 {
		return 0, false
	}
	port, err := strconv.Atoi(local[idx+1:])
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}

// pidFromUsers 从 `users:(("redis-server",pid=1234,fd=6))` 里取 pid；
// 该字段可能被空格拆开，所以按整行拼接后再找。
func pidFromUsers(fields []string) (int, bool) {
	joined := strings.Join(fields, " ")
	const marker = "pid="
	idx := strings.Index(joined, marker)
	if idx < 0 {
		return 0, false
	}
	rest := joined[idx+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(rest[:end])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// parseSystemctlShow 解析 `systemctl show -p ActiveState -p MainPID <unit>` 的输出。
// 必须按 key=value 解析：systemctl 的输出顺序不按请求顺序（实测 MainPID 会排在
// ActiveState 前面），靠行号取值会把 pid 当成状态。
func parseSystemctlShow(out string) (active bool, pid int, ok bool) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ActiveState":
			active = strings.TrimSpace(value) == "active"
			ok = true
		case "MainPID":
			if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
				pid = n
			}
			ok = true
		}
	}
	return active, pid, ok
}
