package sites

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runCommand 执行只读命令并收集 stdout。命令失败时把 stderr 附进错误，
// 便于前端/日志定位（例如 nginx 配置语法错误）。
func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		detail = truncate(detail, 400)
		if detail != "" {
			return stdout.String(), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, detail)
		}
		return stdout.String(), fmt.Errorf("%s %s: %v", name, strings.Join(args, " "), err)
	}
	return stdout.String(), nil
}

// truncate 限制错误文案长度，避免把整份 nginx 配置塞进 JSON。
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// nginxExecutable 优先使用配置值；PATH 里找不到时回退到 Debian 系的 /usr/sbin/nginx。
// systemd 服务的 PATH 不一定包含 sbin，显式回退比启动后才报「找不到命令」可靠。
func (s *Service) nginxExecutable() string {
	if strings.ContainsRune(s.nginxBin, os.PathSeparator) {
		return s.nginxBin
	}
	if path, err := exec.LookPath(s.nginxBin); err == nil {
		return path
	}
	if path, err := exec.LookPath("/usr/sbin/nginx"); err == nil {
		return path
	}
	return s.nginxBin
}
