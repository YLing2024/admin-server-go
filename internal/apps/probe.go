package apps

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

// tcpChecker 探测本地端口是否可连；由 probe.Service.CheckTCPPort 注入。
type tcpChecker func(port int) bool

// probeApp 对单条登记表应用执行探活；无可用探活方式记 unknown。
func probeApp(app map[string]any, check tcpChecker) ProbeResult {
	probe := NormalizeProbe(app)
	if probe == nil {
		return ProbeResult{Status: StatusUnknown}
	}

	var r ProbeResult
	switch probe.Type {
	case "http":
		r = probeHTTP(probe.Target, probe.Expect)
	case "systemd":
		r = probeSystemd(probe.Unit)
	case "docker":
		r = probeDocker(probe.Container)
	default:
		r = probeTCP(probe.Port, check)
	}
	// 按需唤醒：平时停着、访问时才起，探活不通属正常 → 归为休眠。
	if app["onDemand"] == true && r.Status == StatusDown {
		return ProbeResult{Status: StatusIdle}
	}
	return r
}

// newProbeClient 用不跟随重定向的客户端，等价旧实现的 redirect: 'manual'。
func newProbeClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeoutMs * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// probeHTTP 做一次 HTTP GET 探活（1500ms 超时）。
func probeHTTP(target string, expect []int) ProbeResult {
	start := time.Now()
	resp, err := newProbeClient().Get(target)
	latency := time.Since(start).Milliseconds()
	if err == nil {
		defer resp.Body.Close()
		return ProbeResult{Status: HTTPStatusFromCode(resp.StatusCode, expect), LatencyMs: ptrI64(latency)}
	}
	if isTimeout(err) {
		return ProbeResult{Status: StatusDegraded, LatencyMs: ptrI64(latency)}
	}
	return ProbeResult{Status: StatusDown, LatencyMs: ptrI64(latency)}
}

// probeSystemd 执行 systemctl is-active；非 active 单元退出码非 0，但 stdout 仍可能是 inactive/failed。
func probeSystemd(unit string) ProbeResult {
	start := time.Now()
	out, runErr := execStdout(probeTimeoutMs, "systemctl", "is-active", unit)
	latency := time.Since(start).Milliseconds()
	trimmed := strings.TrimSpace(out)
	if runErr == nil {
		return ProbeResult{Status: SystemdStatusFromOutput(trimmed), LatencyMs: ptrI64(latency)}
	}
	status := StatusDown
	if trimmed == "activating" || trimmed == "reloading" {
		status = StatusDegraded
	}
	return ProbeResult{Status: status, LatencyMs: ptrI64(latency)}
}

// probeDocker 执行 docker inspect -f '{{.State.Status}}'；容器不存在 / docker 不可用 → down。
func probeDocker(container string) ProbeResult {
	start := time.Now()
	out, runErr := execStdout(probeTimeoutMs, "docker", "inspect", "-f", "{{.State.Status}}", container)
	latency := time.Since(start).Milliseconds()
	if runErr != nil {
		return ProbeResult{Status: StatusDown, LatencyMs: ptrI64(latency)}
	}
	return ProbeResult{Status: DockerStatusFromOutput(strings.TrimSpace(out)), LatencyMs: ptrI64(latency)}
}

// probeTCP 探测本地端口；latency 为探活耗时。
func probeTCP(port int, check tcpChecker) ProbeResult {
	start := time.Now()
	up := check(port)
	latency := time.Since(start).Milliseconds()
	status := StatusDown
	if up {
		status = StatusUp
	}
	return ProbeResult{Status: status, LatencyMs: ptrI64(latency)}
}

// isTimeout 判定 HTTP 探活是否超时（客户端超时或请求上下文超时）。
func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne interface{ Timeout() bool }
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return os.IsTimeout(err)
}

// execStdout 执行外部命令并返回 stdout（超时用 context 截断），保证非零退出时仍能拿到 stdout。
func execStdout(timeoutMs int, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := cmd.Run()
	return stdout.String(), err
}
