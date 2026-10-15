package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// TOTP 参数与旧实现 lib/totp.js 一致：30s 步长、6 位、SHA1、base32（无 padding）、±1 步容差。
const (
	totpStep   = 30
	totpDigits = 6
	// totpIssuer 是 otpauth URI 与 JWT sub 使用的发行方标签。
	totpIssuer = "HomeAdmin"
	// totpJWTExpiresIn 是模块 /login 签发的 JWT 有效期文本（旧实现 "12h"）。
	totpJWTExpiresIn = "12h"
)

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// GenerateTotpSecret 生成新的 base32 secret（20 随机字节，与旧实现一致）。
func GenerateTotpSecret() (string, error) {
	raw := make([]byte, 20)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base32NoPad.EncodeToString(raw), nil
}

// OtpauthURI 构造 otpauth URI（逐字对齐旧实现 totp-auth 的 otpauthUri）。
func OtpauthURI(secret string) string {
	label := urlEncodeComponent(totpIssuer)
	return fmt.Sprintf("otpauth://totp/%s:%s?secret=%s&issuer=%s&period=30&digits=6&algorithm=SHA1", label, label, secret, label)
}

// GenerateTotp 计算给定时刻的 TOTP 码。
func GenerateTotp(secret string, unixSeconds int64) (string, error) {
	key, err := base32NoPad.DecodeString(strings.ToUpper(strings.ReplaceAll(strings.TrimRight(secret, "="), " ", "")))
	if err != nil {
		return "", err
	}
	counter := uint64(unixSeconds / totpStep)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < totpDigits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", totpDigits, bin%mod), nil
}

// VerifyTotp 校验 TOTP 码（±1 步窗口，与旧实现 verifyCode 一致）。
func VerifyTotp(secret, code string) bool {
	expected := strings.ReplaceAll(code, " ", "")
	if expected == "" {
		return false
	}
	now := time.Now().Unix()
	for step := int64(-1); step <= 1; step++ {
		got, err := GenerateTotp(secret, now+step*totpStep)
		if err != nil {
			return false
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

// GetTotpSecret 读取 TOTP secret 文件；不存在/损坏返回空串。
func (a *Authenticator) GetTotpSecret() string {
	raw, err := os.ReadFile(a.Cfg.TotpSecretFile)
	if err != nil {
		return ""
	}
	var data struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return ""
	}
	return data.Secret
}

// SaveTotpSecret 原子写回 TOTP secret 文件（临时文件 → rename）。
func (a *Authenticator) SaveTotpSecret(secret string) error {
	if err := os.MkdirAll(filepath.Dir(a.Cfg.TotpSecretFile), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(map[string]string{"secret": secret}, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", a.Cfg.TotpSecretFile, os.Getpid())
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.Cfg.TotpSecretFile)
}

// ---- JWT（HS256，纯标准库） ----

// SignJWT 按旧实现 jwt.sign 的形态签发 HS256 JWT（iat/exp 以秒计）。
func SignJWT(payload map[string]any, secret string, expiresInSeconds int64) (string, error) {
	now := time.Now().Unix()
	claims := map[string]any{}
	for k, v := range payload {
		claims[k] = v
	}
	claims["iat"] = now
	claims["exp"] = now + expiresInSeconds
	return signJWT(claims, secret)
}

// SignJWTExpiresIn 用 "12h" 形式的有效期签发（供模块 /login 使用）。
func SignJWTExpiresIn(payload map[string]any, secret, expiresIn string) (string, error) {
	secs, err := parseDurationSeconds(expiresIn)
	if err != nil {
		return "", err
	}
	return SignJWT(payload, secret, secs)
}

func signJWT(claims map[string]any, secret string) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	h := base64.RawURLEncoding.EncodeToString(header)
	c := base64.RawURLEncoding.EncodeToString(body)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(h + "." + c))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return h + "." + c + "." + sig, nil
}

// parseDurationSeconds 解析 "12h"/"30m"/"60s"/"7d"/毫秒数，返回秒数。
func parseDurationSeconds(input string) (int64, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return 0, fmt.Errorf("invalid expiresIn")
	}
	unit := ""
	num := s
	switch {
	case strings.HasSuffix(s, "ms"):
		unit, num = "ms", strings.TrimSuffix(s, "ms")
	case strings.HasSuffix(s, "s"):
		unit, num = "s", strings.TrimSuffix(s, "s")
	case strings.HasSuffix(s, "m"):
		unit, num = "m", strings.TrimSuffix(s, "m")
	case strings.HasSuffix(s, "h"):
		unit, num = "h", strings.TrimSuffix(s, "h")
	case strings.HasSuffix(s, "d"):
		unit, num = "d", strings.TrimSuffix(s, "d")
	default:
		unit = "ms"
	}
	var n int64
	if _, err := fmt.Sscanf(num, "%d", &n); err != nil {
		return 0, fmt.Errorf("invalid expiresIn")
	}
	switch unit {
	case "ms":
		return n / 1000, nil
	case "s":
		return n, nil
	case "m":
		return n * 60, nil
	case "h":
		return n * 3600, nil
	case "d":
		return n * 86400, nil
	}
	return 0, fmt.Errorf("invalid expiresIn")
}

// ---- 分档限速器（与旧实现 lib/rate-limit.js 一致） ----

// RateLimiter 是内存按 IP 分档锁定的限速器：连续 maxFailures 次失败触发下一档锁定期，
// 成功即清除。默认 5 次 → 60s → 再 5 次 → 300s → 再 5 次 → 900s。
type RateLimiter struct {
	mu          sync.Mutex
	maxFailures int
	lockout     []int
	entries     map[string]*rateEntry
	now         func() int64
}

type rateEntry struct {
	failures  int
	tier      int
	lockUntil int64
}

// NewRateLimiter 构造限速器；maxFailures<=0 时用 5，lockout 为空时用 [60,300,900]。
func NewRateLimiter(maxFailures int, lockout []int) *RateLimiter {
	if maxFailures <= 0 {
		maxFailures = 5
	}
	if len(lockout) == 0 {
		lockout = []int{60, 300, 900}
	}
	return &RateLimiter{maxFailures: maxFailures, lockout: lockout, entries: map[string]*rateEntry{}, now: func() int64 { return time.Now().UnixMilli() }}
}

// Status 返回锁定状态；retryAfter 为剩余秒数（未锁定为 0）。
func (l *RateLimiter) Status(ip string) (locked bool, retryAfter int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ip)
	if e.lockUntil > l.now() {
		return true, int((e.lockUntil - l.now() + 999) / 1000)
	}
	return false, 0
}

// RecordFailure 记录一次失败，必要时进入锁定。
func (l *RateLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entry(ip)
	e.failures++
	if e.failures >= l.maxFailures {
		tier := e.tier
		if tier > len(l.lockout)-1 {
			tier = len(l.lockout) - 1
		}
		e.lockUntil = l.now() + int64(l.lockout[tier])*1000
		e.failures = 0
		e.tier = e.tier + 1
		if e.tier > len(l.lockout) {
			e.tier = len(l.lockout)
		}
	}
}

// RecordSuccess 清除该 IP 的记录。
func (l *RateLimiter) RecordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, ip)
}

func (l *RateLimiter) entry(ip string) *rateEntry {
	e := l.entries[ip]
	if e == nil {
		e = &rateEntry{}
		l.entries[ip] = e
	}
	if e.lockUntil != 0 && e.lockUntil <= l.now() {
		e.lockUntil = 0
		e.failures = 0
	}
	return e
}

// urlEncodeComponent 模拟 JS encodeURIComponent（用于 issuer 标签）。
func urlEncodeComponent(s string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if strings.IndexByte(unreserved, c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
