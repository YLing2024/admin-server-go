package config

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// 口令哈希参数：PBKDF2-HMAC-SHA256（标准库 crypto/pbkdf2，无第三方依赖）。
const (
	pbkdf2Iterations = 100000
	pbkdf2KeyLen     = 32
	pbkdf2SaltLen    = 16
	hashPrefix       = "pbkdf2_sha256"
)

// FileStore 是 config.json 的进程内快照与原子写回。
//
// 兼容旧格式：旧文件里的 admin_password 是明文，读取时仍可校验（change-password 用）；
// 一旦通过 SetAdminPassword 写入，就改为只存口令哈希 admin_password_hash 并删除明文字段。
type FileStore struct {
	path string
	mu   sync.Mutex
	data map[string]any
}

// LoadFile 读取（不存在则按默认生成）config.json。
// generatedPassword 仅在本次首先生成口令时非空（供启动日志一次性提示，不落库、不回显到接口）。
// created 表示配置文件本次是首次生成；error 只在写入失败时返回，解析失败按旧实现降级为默认配置。
func LoadFile(path string) (store *FileStore, generatedPassword string, created bool, err error) {
	f := &FileStore{path: path, data: map[string]any{}}
	raw, readErr := os.ReadFile(path)
	if readErr == nil {
		if jerr := json.Unmarshal(raw, &f.data); jerr != nil || f.data == nil {
			f.data = map[string]any{}
		}
	}
	if _, ok := f.data["admin_password_hash"]; !ok {
		if _, plain := f.data["admin_password"]; !plain {
			pw, gerr := randomToken(12)
			if gerr != nil {
				return nil, "", false, gerr
			}
			h, herr := hashPassword(pw)
			if herr != nil {
				return nil, "", false, herr
			}
			f.data["admin_password_hash"] = h
			generatedPassword = pw
			created = readErr != nil
			if werr := f.persist(); werr != nil {
				return nil, "", false, werr
			}
		}
	}
	if chErr := os.Chmod(path, 0o600); chErr != nil {
		// 权限调整失败不影响启动，与旧实现一致。
		_ = chErr
	}
	return f, generatedPassword, created, nil
}

// Get 返回原始值（不存在为 nil）。
func (f *FileStore) Get(key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.data[key]
}

// Str 返回字符串值。
func (f *FileStore) Str(key string) string {
	if v, ok := f.Get(key).(string); ok {
		return v
	}
	return ""
}

// Set 写入一个键并落盘。
func (f *FileStore) Set(key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data[key] = value
	return f.persist()
}

// SetAdminPassword 写入新口令（只存哈希，同时删除可能存在的旧明文字段）。
func (f *FileStore) SetAdminPassword(password string) error {
	h, err := hashPassword(password)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.data["admin_password_hash"] = h
	delete(f.data, "admin_password")
	return f.persist()
}

// SetTotpSecret 写入 TOTP secret（与旧实现同名同形）。
func (f *FileStore) SetTotpSecret(secret string) error { return f.Set("totp_secret", secret) }

// VerifyAdminPassword 校验口令：优先哈希，其次兼容旧明文字段。两次比较都是定时安全比较。
func (f *FileStore) VerifyAdminPassword(password string) bool {
	f.mu.Lock()
	hashed, _ := f.data["admin_password_hash"].(string)
	plain, _ := f.data["admin_password"].(string)
	f.mu.Unlock()
	if hashed != "" {
		return verifyHashedPassword(password, hashed)
	}
	// 旧格式：明文存储，按旧实现 sha256 后定时安全比较。
	return subtle.ConstantTimeCompare(sha256Sum(password), sha256Sum(plain)) == 1
}

// GetOrCreateJWTSecret 返回（必要时生成并落盘）JWT 签名密钥。
func (f *FileStore) GetOrCreateJWTSecret() string {
	if s := f.Str("jwt_secret"); s != "" {
		return s
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// 随机源不可用时退化为随机哈希，仍保证进程内稳定。
		sum := sha256.Sum256([]byte(fmt.Sprintf("%d", os.Getpid())))
		secret = sum[:]
	}
	value := hex.EncodeToString(secret)
	if err := f.Set("jwt_secret", value); err != nil {
		return value
	}
	return value
}

// persist 原子写回（临时文件 → fsync → rename），权限 0600。调用方须持锁。
func (f *FileStore) persist() error {
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(f.data, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := f.path + ".tmp"
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(body); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Sync(); err != nil {
		fh.Close()
		return err
	}
	if err := fh.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return err
	}
	return os.Chmod(f.path, 0o600)
}

// hashPassword 生成 "pbkdf2_sha256$<iter>$<saltB64>$<hashB64>"。
func hashPassword(password string) (string, error) {
	salt := make([]byte, pbkdf2SaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	sum, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s$%d$%s$%s", hashPrefix, pbkdf2Iterations, base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(sum)), nil
}

// verifyHashedPassword 校验上述格式；格式非法一律拒绝。
func verifyHashedPassword(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != hashPrefix {
		return false
	}
	iter := 0
	if _, err := fmt.Sscanf(parts[1], "%d", &iter); err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return hmac.Equal(got, want)
}

func sha256Sum(s string) []byte {
	sum := sha256.Sum256([]byte(s))
	return sum[:]
}

// randomToken 生成 n 字节随机数的 base64url 文本（与旧实现 randomBytes(n).toString('base64url') 等价）。
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
