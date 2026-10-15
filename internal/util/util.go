// Package util 提供最底层通用工具：审计日志、客户端 IP、哈希比对与 EMA 平滑。
// 与旧实现 src/util.ts 一一对应；不依赖项目内其它模块。
package util

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Audit 追加一行 JSON 审计记录。写入失败只告警，不阻断业务。
func Audit(path, action, ip string, ok bool, detail string) {
	if ip == "" {
		ip = "unknown"
	}
	line, err := json.Marshal(map[string]any{
		"ts":     time.Now().UTC().Format("2006-01-02T15:04:05.000Z"),
		"action": action,
		"ip":     ip,
		"ok":     ok,
		"detail": detail,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// ClientIP 取真实客户端 IP：nginx 统一注入 X-Real-IP，退化到 X-Forwarded-For 首个，
// 最后才用连接对端地址（去掉端口）。与旧实现 clientIp 一致。
func ClientIP(r *http.Request) string {
	if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
		return xr
	}
	if xf := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-For"), ",")[0]); xf != "" {
		return xf
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host != "" {
		return host
	}
	return "unknown"
}

// Sha256 返回 sha256 摘要（旧实现 sha256 的等价物）。
func Sha256(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// VerifySHA256 比较 sha256(input) 与 sha256(stored)，定时安全。旧实现 verifyPassword 用。
func VerifySHA256(input, stored string) bool {
	return subtle.ConstantTimeCompare(Sha256(input), Sha256(stored)) == 1
}

// Sha256Hex 返回十六进制 sha256（接口令牌 key / 终端票据 key 用）。
func Sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// Ema 通用指数移动平均平滑；prev 为 nil（首次）时直接取 current。
func Ema(prev *float64, current, alpha float64) float64 {
	if prev == nil {
		return current
	}
	return *prev*(1-alpha) + current*alpha
}
