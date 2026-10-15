package probe

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
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
		if systemdIsActive(svc.name) {
			return Status{Name: svc.name, Status: "up", PID: pidByProcessName(svc.name)}
		}
		return Status{Name: svc.name, Status: "down"}
	}
	if s.CheckTCPPort(svc.port, 1500) {
		return Status{Name: svc.name, Status: "up", PID: pidByPort(svc.port)}
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

// systemdIsActive 执行 systemctl is-active；仅当 stdout 恰为 active 才算 up（非零退出码一律 down）。
func systemdIsActive(name string) bool {
	cmd := exec.Command("systemctl", "is-active", name+".service")
	timer := time.AfterFunc(3*time.Second, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	var stdout strings.Builder
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false
	}
	return strings.TrimSpace(stdout.String()) == "active"
}

// pidByPort 通过 ss -ltnp 反查监听端口的进程 pid（不可用/失败返回 nil）。
func pidByPort(port int) *int {
	script := fmt.Sprintf("ss -ltnp 2>/dev/null | awk '$4 ~ /:%d$/ {print $6}' | head -1", port)
	out, err := runShell(script, 3*time.Second)
	if err != nil {
		return nil
	}
	m := pidRe.FindStringSubmatch(out)
	if m == nil {
		return nil
	}
	pid := int(atoi(m[1]))
	return &pid
}

// pidByProcessName 按进程名反查 pid（systemd 服务用）。
func pidByProcessName(name string) *int {
	out, err := runShell(fmt.Sprintf(`pgrep -f "%s" | head -1`, name), 3*time.Second)
	if err != nil {
		return nil
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil
	}
	n, err := strconv.Atoi(strings.SplitN(out, "\n", 2)[0])
	if err != nil {
		return nil
	}
	return &n
}

var pidRe = regexp.MustCompile(`pid=(\d+)`)

// runShell 用 sh -c 执行与旧实现完全相同的命令行文本，保证 `ss | awk | head` 这类管道的语义不变。
func runShell(script string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "sh", "-c", script).Output()
	return string(out), err
}
