// Package sshkeys 管理允许登录服务器的 SSH 公钥白名单。
//
// 公钥的唯一事实源是 MySQL 的 ssh_key 表；sshd 通过独立命令 cmd/ssh-keys-from-admin
// 实时查询，因此停用 / 删除在下一次登录时立即生效，不需要定时渲染 authorized_keys 文件。
package sshkeys

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// 限额与旧实现/需求书一致：整行最多 8KB，标题与注释分别 100/200 字符。
const (
	// MaxLineBytes 是公钥整行（含类型、base64、注释）的字节上限。
	MaxLineBytes = 8 * 1024
	// MaxTitleRunes 是标题的字符上限。
	MaxTitleRunes = 100
	// MaxCommentRunes 是公钥注释的字符上限。
	MaxCommentRunes = 200
)

// 只接受纯公钥的算法白名单。证书（*-cert-v01@openssh.com）与未知算法一律拒绝，
// 避免把 sshd 的证书校验路径与旧客户端兼容性引入这套管理面。
var allowedKeyTypes = map[string]bool{
	"ssh-ed25519":                        true,
	"ssh-rsa":                            true,
	"ecdsa-sha2-nistp256":                true,
	"ecdsa-sha2-nistp384":                true,
	"ecdsa-sha2-nistp521":                true,
	"sk-ssh-ed25519@openssh.com":         true,
	"sk-ecdsa-sha2-nistp256@openssh.com": true,
}

var ecdsaCurves = map[string]string{
	"ecdsa-sha2-nistp256": "nistp256",
	"ecdsa-sha2-nistp384": "nistp384",
	"ecdsa-sha2-nistp521": "nistp521",
}

// ParsedKey 是严格解析后的公钥。FingerprintHex 入库用于去重，Fingerprint 是对外的
// GitHub 风格展示值（SHA256: + 无填充 base64）。
type ParsedKey struct {
	Type           string
	Base64         string
	Comment        string
	Line           string
	Fingerprint    string
	FingerprintHex string
}

// ParsePublicKey 解析一行 OpenSSH 公钥并做服务端校验。
//
// 只接受 `type base64 [comment]`，base64 必须能解码且解码后的 blob 必须是结构合法的
// OpenSSH 公钥；带 command= / environment= / permitopen= 等前置选项的行一律拒绝，
// 防止把强制命令塞进白名单。
func ParsePublicKey(raw string) (*ParsedKey, error) {
	line := strings.TrimSpace(raw)
	if line == "" {
		return nil, errors.New("公钥不能为空")
	}
	if len(line) > MaxLineBytes {
		return nil, fmt.Errorf("公钥整行不能超过 %d 字节", MaxLineBytes)
	}
	if strings.ContainsAny(line, "\r\n") {
		return nil, errors.New("只接受一行公钥")
	}

	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, errors.New("公钥格式应为 type base64 [comment]")
	}
	keyType := fields[0]
	if !allowedKeyTypes[keyType] {
		if strings.Contains(keyType, "=") {
			return nil, errors.New("只接受纯公钥，不支持 command= 等前置选项")
		}
		return nil, fmt.Errorf("不支持的公钥类型：%s", keyType)
	}
	encoded := fields[1]

	blob, err := decodeBase64(encoded)
	if err != nil {
		return nil, errors.New("公钥 base64 无法解码")
	}
	if err := validateBlob(keyType, blob); err != nil {
		return nil, err
	}

	comment := strings.Join(fields[2:], " ")
	if utf8.RuneCountInString(comment) > MaxCommentRunes {
		return nil, fmt.Errorf("公钥注释不能超过 %d 个字符", MaxCommentRunes)
	}

	sum := sha256.Sum256(blob)
	parsed := &ParsedKey{
		Type:           keyType,
		Base64:         encoded,
		Comment:        comment,
		Fingerprint:    "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]),
		FingerprintHex: hex.EncodeToString(sum[:]),
	}
	parsed.Line = keyType + " " + encoded
	if comment != "" {
		parsed.Line += " " + comment
	}
	return parsed, nil
}

// ValidateTitle 校验并归一化标题（去首尾空白、非空、长度上限）。
func ValidateTitle(raw string) (string, error) {
	title := strings.TrimSpace(raw)
	if title == "" {
		return "", errors.New("标题不能为空")
	}
	if utf8.RuneCountInString(title) > MaxTitleRunes {
		return "", fmt.Errorf("标题不能超过 %d 个字符", MaxTitleRunes)
	}
	return title, nil
}

// FingerprintHexFromOpenSSH 把 sshd 的 %f（SHA256:base64）或已入库的 hex 统一成 hex。
func FingerprintHexFromOpenSSH(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", errors.New("指纹为空")
	}
	if !strings.HasPrefix(value, "SHA256:") {
		if len(value) == 64 {
			if _, err := hex.DecodeString(value); err == nil {
				return strings.ToLower(value), nil
			}
		}
		return "", errors.New("指纹格式非法")
	}
	sum, err := decodeBase64(strings.TrimPrefix(value, "SHA256:"))
	if err != nil || len(sum) != sha256.Size {
		return "", errors.New("指纹格式非法")
	}
	return hex.EncodeToString(sum), nil
}

// decodeBase64 同时接受带填充与不带填充的标准 base64（OpenSSH 用带填充，%f 不带）。
func decodeBase64(value string) ([]byte, error) {
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(value, "="))
}

// fingerprintFromHex 把库里的 sha256 hex 转成 `SHA256:` + 无填充 base64（GitHub 风格）。
func fingerprintFromHex(value string) (string, error) {
	sum, err := hex.DecodeString(strings.TrimSpace(value))
	if err != nil || len(sum) != sha256.Size {
		return "", errors.New("指纹格式非法")
	}
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum), nil
}

// validateBlob 校验 OpenSSH 公钥 blob 的结构：首个字符串是算法名，之后是该算法要求的字段。
// 这里不复刻完整密码学解析，只做「类型与长度自洽」检查，足以拦住把任意文本塞进 base64 的情况。
func validateBlob(keyType string, blob []byte) error {
	r := &blobReader{data: blob}
	alg, err := r.readString()
	if err != nil {
		return errors.New("公钥数据不完整")
	}
	if alg != keyType {
		return errors.New("公钥类型与数据不一致")
	}
	switch keyType {
	case "ssh-ed25519":
		key, err := r.readBytes()
		if err != nil || len(key) != 32 || r.remaining() != 0 {
			return errors.New("ed25519 公钥数据非法")
		}
	case "ssh-rsa":
		e, err := r.readMPInt()
		if err != nil || len(e) == 0 || isZero(e) {
			return errors.New("RSA 公钥数据非法")
		}
		n, err := r.readMPInt()
		if err != nil || len(n) == 0 || isZero(n) || r.remaining() != 0 {
			return errors.New("RSA 公钥数据非法")
		}
	case "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		curve, err := r.readString()
		if err != nil || curve != ecdsaCurves[keyType] {
			return errors.New("ECDSA 公钥曲线非法")
		}
		point, err := r.readBytes()
		if err != nil || len(point) == 0 || r.remaining() != 0 {
			return errors.New("ECDSA 公钥数据非法")
		}
	case "sk-ssh-ed25519@openssh.com":
		key, err := r.readBytes()
		if err != nil || len(key) != 32 {
			return errors.New("安全密钥公钥数据非法")
		}
		app, err := r.readString()
		if err != nil || app == "" {
			return errors.New("安全密钥公钥数据非法")
		}
		if _, err := r.readByte(); err != nil || r.remaining() != 0 {
			return errors.New("安全密钥公钥数据非法")
		}
	case "sk-ecdsa-sha2-nistp256@openssh.com":
		curve, err := r.readString()
		if err != nil || curve != "nistp256" {
			return errors.New("安全密钥公钥曲线非法")
		}
		point, err := r.readBytes()
		if err != nil || len(point) == 0 {
			return errors.New("安全密钥公钥数据非法")
		}
		app, err := r.readString()
		if err != nil || app == "" {
			return errors.New("安全密钥公钥数据非法")
		}
		if _, err := r.readByte(); err != nil || r.remaining() != 0 {
			return errors.New("安全密钥公钥数据非法")
		}
	}
	return nil
}

// blobReader 按 RFC 4251 的 uint32 长度前缀读取字段。
type blobReader struct {
	data []byte
	pos  int
}

func (r *blobReader) readString() (string, error) {
	b, err := r.readBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *blobReader) readMPInt() ([]byte, error) { return r.readBytes() }

func (r *blobReader) readBytes() ([]byte, error) {
	if r.pos+4 > len(r.data) {
		return nil, errors.New("长度前缀缺失")
	}
	n := int(binary.BigEndian.Uint32(r.data[r.pos : r.pos+4]))
	r.pos += 4
	if n < 0 || r.pos+n > len(r.data) {
		return nil, errors.New("字段长度越界")
	}
	out := r.data[r.pos : r.pos+n]
	r.pos += n
	return out, nil
}

func (r *blobReader) readByte() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, errors.New("字节缺失")
	}
	b := r.data[r.pos]
	r.pos++
	return b, nil
}

func (r *blobReader) remaining() int { return len(r.data) - r.pos }

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
