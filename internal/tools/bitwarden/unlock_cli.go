package bitwarden

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	// cliUnlockTimeout 覆盖一次 bw unlock：PBKDF2 60 万次在本机约 100ms 级，30 秒足够宽松。
	cliUnlockTimeout = 30 * time.Second
	// cliMinTokenLen 是会话令牌的最小长度；bw 的 raw 输出远长于此。
	cliMinTokenLen = 40
)

// cliEchoGrace 是写入密码前的等待：让 PTY 里的 stty -echo 先生效，
// 否则密码会被终端回显进我们捕获的输出里。声明为变量以便测试缩短。
var cliEchoGrace = 700 * time.Millisecond

// unlockViaCLI 让官方 bw CLI 自己完成 KDF 派生与密钥解密。
//
// 为什么不自己算：Bitwarden 的 /identity/connect/token 收的是客户端派生出的
// master password hash（PBKDF2/Argon2 由账号的 KDF 参数决定），不是明文主密码。
// 自行实现等于重写一套密码学，KDF 参数一变就静默失效；交给 CLI 则天然跟随账号设置。
//
// 密码只经 stdin 进入子进程：不进 argv（ps 看不到）、不进环境变量、不进日志。
// 子进程输出在返回前会做一次「若含密码则抹除」的兜底，双保险。
func (s *Service) unlockViaCLI(ctx context.Context, password string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cliUnlockTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "script", "-qec", "stty -echo; bw unlock --raw", "/dev/null")
	// 不继承外部 BW_SESSION，避免上一轮会话干扰本次解锁语义。
	// 同时确保 HOME 存在：systemd 服务默认没有 HOME，bw 会去相对路径找配置，
	// 结果永远是「You are not logged in」，而不是真正的解密失败。
	env := make([]string, 0, len(os.Environ())+2)
	hasHome := false
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "BW_SESSION=") {
			continue
		}
		if strings.HasPrefix(kv, "HOME=") && len(kv) > len("HOME=") {
			hasHome = true
		}
		env = append(env, kv)
	}
	if !hasHome {
		env = append(env, "HOME=/root")
	}
	cmd.Env = env

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", errAuthFailed
	}
	if err := cmd.Start(); err != nil {
		return "", errAuthFailed
	}

	go func() {
		defer func() { _ = stdin.Close() }()
		select {
		case <-time.After(cliEchoGrace):
		case <-ctx.Done():
			return
		}
		_, _ = stdin.Write([]byte(password + "\n"))
	}()

	waitErr := cmd.Wait()
	text := out.String()
	if password != "" && strings.Contains(text, password) {
		text = strings.ReplaceAll(text, password, "")
	}
	// 环境问题优先于密码问题判定：CLI 没找到登录态时，任何密码都会失败。
	if strings.Contains(text, "You are not logged in") {
		return "", errCLINotLoggedIn
	}
	if waitErr != nil {
		return "", errAuthFailed
	}
	token := extractSessionToken(text)
	if token == "" {
		return "", errAuthFailed
	}
	return token, nil
}

// extractSessionToken 从 PTY 输出里取出 bw unlock --raw 打印的裸令牌。
// 输出混有提示符与 ANSI 控制序列，因此按行扫描：只接受长度足够、
// 且字符集与 base64 令牌相符的行。
func extractSessionToken(text string) string {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(stripANSI(line))
		if len(line) < cliMinTokenLen {
			continue
		}
		if isTokenCharset(line) {
			return line
		}
	}
	return ""
}

// isTokenCharset 判断整行是否只由 base64 字符与 URL-safe 变体组成。
func isTokenCharset(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case c == '+' || c == '/' || c == '=' || c == '_' || c == '-' || c == '.':
		default:
			return false
		}
	}
	return true
}

// stripANSI 去掉常见的 CSI / OSC 转义序列，避免它们混进令牌行。
func stripANSI(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && !(s[j] >= 0x40 && s[j] <= 0x7e) {
					j++
				}
				i = j
				continue
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
