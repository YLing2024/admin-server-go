package probe

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// Status 是服务状态卡片的一行。
type Status struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	PID    *int   `json:"pid,omitempty"`
}

// serviceCheck 是待检测服务：有端口的按 TCP 连通判定 up/down；
// self 恒 up；systemd 无固定端口，用 systemctl is-active 判定。
type serviceCheck struct {
	name    string
	port    int
	hasPort bool
	self    bool
	systemd bool
}

// SystemdServices 是进程排行用的 systemd 正规化清单。
// 进程识别以 systemctl MainPID 为准，避免命令行参数含服务名造成误判。
var SystemdServices = []string{
	"admin-server",
	"admin-server-test",
	"blog-server",
	"auth-server",
	"deeptutor",
	"nginx",
	"redis-server",
	"hermes-gateway",
	"hermes-serve",
	"aionui-web",
}

func (s *Service) serviceChecks() []serviceCheck {
	return []serviceCheck{
		{name: "admin-server", port: s.cfg.Port, hasPort: true, self: true},
		{name: "hermes-gateway", systemd: true},
		{name: "hermes-serve", port: 9119, hasPort: true},
		{name: "nginx", port: 80, hasPort: true},
		{name: "redis", port: 6379, hasPort: true},
		{name: "blog", port: 4000, hasPort: true},
		{name: "admin-test", port: 3101, hasPort: true},
		{name: "aionui", port: 3010, hasPort: true},
	}
}

// CollectServices 采集服务状态列表（并发探活，顺序与清单一致）。
func (s *Service) CollectServices() ([]Status, error) {
	checks := s.serviceChecks()
	results := make([]Status, len(checks))
	var wg sync.WaitGroup
	for i, svc := range checks {
		wg.Add(1)
		go func(i int, svc serviceCheck) {
			defer wg.Done()
			results[i] = s.checkService(svc)
		}(i, svc)
	}
	wg.Wait()
	return results, nil
}

func (s *Service) checkService(svc serviceCheck) Status {
	if svc.self {
		pid := os.Getpid()
		return Status{Name: svc.name, Status: "up", PID: &pid}
	}
	if svc.systemd {
		st := s.systemdState(svc.name)
		if st.active {
			status := Status{Name: svc.name, Status: "up"}
			if st.pid > 0 {
				pid := st.pid
				status.PID = &pid
			}
			return status
		}
		return Status{Name: svc.name, Status: "down"}
	}
	if s.CheckTCPPort(svc.port, 1500) {
		return Status{Name: svc.name, Status: "up", PID: s.pidByPort(svc.port)}
	}
	return Status{Name: svc.name, Status: "down"}
}

// CheckTCPPort 探测 127.0.0.1:port 是否可连（timeoutMs 毫秒）；connect 成功即视为 up。
func (s *Service) CheckTCPPort(port int, timeoutMs int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		time.Duration(timeoutMs)*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// svcSnapTTL 是服务探活快照的有效期。SSE 每秒一帧，5 秒足够新鲜，
// 却把「每帧 5 次 ss + 2 次 systemctl + 1 次 pgrep」压成「每 5 秒 1 次 ss」。
const svcSnapTTL = 5 * time.Second

// sysdEntry 是某个 systemd 单元的探活结果。
type sysdEntry struct {
	active bool
	pid    int
}

// svcSnapshot 在锁内返回端口表与 systemd 状态；过期时只 fork 一次 `ss -ltnp`。
func (s *Service) svcSnapshot() (map[int]int, map[string]sysdEntry) {
	s.svcMu.Lock()
	defer s.svcMu.Unlock()
	if s.svcPort == nil {
		s.svcPort = map[int]int{}
	}
	if s.svcSysd == nil {
		s.svcSysd = map[string]sysdEntry{}
	}
	if time.Since(s.svcAt) < svcSnapTTL {
		return s.svcPort, s.svcSysd
	}
	out, err := runShell("ss -ltnp 2>/dev/null", 2*time.Second)
	if err == nil {
		s.svcPort = parseSSListen(out)
		// 端口变了（服务重启后 pid 变化）就丢掉 systemd 缓存，下一轮重新问。
		s.svcSysd = map[string]sysdEntry{}
	}
	s.svcAt = time.Now()
	return s.svcPort, s.svcSysd
}

// pidByPort 从 5 秒快照里查端口对应的 pid；快照里没有就是 nil。
func (s *Service) pidByPort(port int) *int {
	ports, _ := s.svcSnapshot()
	if pid, ok := ports[port]; ok && pid > 0 {
		return &pid
	}
	return nil
}

// systemdState 读单元状态：每个单元 5 秒内最多 fork 一次 systemctl show。
func (s *Service) systemdState(name string) sysdEntry {
	_, sysd := s.svcSnapshot()
	s.svcMu.Lock()
	entry, ok := sysd[name]
	s.svcMu.Unlock()
	if ok {
		return entry
	}

	out, err := runShell(fmt.Sprintf("systemctl show -p ActiveState -p MainPID %s.service", name), 3*time.Second)
	entry = sysdEntry{}
	if err == nil {
		if active, pid, ok := parseSystemctlShow(out); ok {
			entry = sysdEntry{active: active, pid: pid}
		}
	}
	s.svcMu.Lock()
	s.svcSysd[name] = entry
	s.svcMu.Unlock()
	return entry
}

// runShell 用 sh -c 执行与旧实现完全相同的命令行文本，保证 `ss | awk | head` 这类管道的语义不变。
func runShell(script string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", script).Output()
	return string(out), err
}
