package sshkeys

import (
	"strings"
	"testing"
)

// 固定样本来自 ssh-keygen 生成的真实公钥；期望指纹与 `ssh-keygen -lf` 输出一致。
const (
	testEd25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfcyMFgumwwbxSagX25R7Rx9RZn3gv8pvUO2grzVda6 test-ed25519@example.com"
	testRSA     = "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQC4swXUim0KfrpXXf8EFm5MvyoYE3kwVYZidEiRG1qCoqgHQIuwWGUiUNLibTdYspuhk0x59Xvu+Ng05Mjzig3pOyM3raX6DeGEKn5bNCIGom8BN0jDE5nmtmpbZuw+7XRz4r1ggphAClXj36CKz5RHyZif/Z6KkKLpRBirBWw+pcu/NP/SwqiHdiYz0Df6dr+8UZDzrCh1CdoeXu4JQ1bqAEN/BIG/TBGcLeJ+nGrxLiLWkhm+xq0meWmbDTeV4kaKejCiKtymN+ixQODw9Cl0Phq6Lf9GgZhQOG3+1LxqAORPJN1rBxcBPyrUdc8i7GoESd8JkncyOLWJuA7JzyJ+BECLXUIDM0GqkxQF7QLXWYpGMsd2MejM81ch4OyLNsNHP7f3Sc+PTLCjaXlxXLn0EY2H+eQJR530tSKkR9BvGc7Mouzf50wp65PE5wq7xLokRcpWYmFRkx/Zl3J5cZVtzfRCIfNw9M251Cf0jgILiUJ4PFlkZASd42MZkQNzNY0= test-rsa@example.com"
	testECDSA   = "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBAcDnAVrrZ17LRtxfS3pg8AKDOXpqiqi4tbTp/7ykq7Uyn+MHM3ZWzAC0Tl5zO6p7GLi/Y+p1uE3rZx1NqUTr7k= test-ecdsa@example.com"
)

// TestParsePublicKeyFingerprint 守住「类型 / 注释 / GitHub 风格指纹」的解析口径。
func TestParsePublicKeyFingerprint(t *testing.T) {
	cases := []struct {
		name        string
		line        string
		wantType    string
		wantComment string
		wantFp      string
	}{
		{"ed25519", testEd25519, "ssh-ed25519", "test-ed25519@example.com", "SHA256:9zCYsPfTdI1YOcwKruw2isdw+HCf6Z6w3kxtEm2lZLI"},
		{"rsa", testRSA, "ssh-rsa", "test-rsa@example.com", "SHA256:8NBcbhk1555vM8Mh0h35XHkem5p2B782z49ti6pdXvc"},
		{"ecdsa", testECDSA, "ecdsa-sha2-nistp256", "test-ecdsa@example.com", "SHA256:7IAhps8Rf4nyICSNEyGMqEkgrN6DCUWEfd9qSIiFIoI"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, err := ParsePublicKey(c.line)
			if err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if parsed.Type != c.wantType || parsed.Comment != c.wantComment {
				t.Fatalf("类型/注释 = %q/%q", parsed.Type, parsed.Comment)
			}
			if parsed.Fingerprint != c.wantFp {
				t.Fatalf("指纹 = %q, want %q", parsed.Fingerprint, c.wantFp)
			}
			if parsed.Line != c.line {
				t.Fatalf("归一化行不一致:\n got %q\nwant %q", parsed.Line, c.line)
			}
			hexValue, err := FingerprintHexFromOpenSSH(parsed.Fingerprint)
			if err != nil || hexValue != parsed.FingerprintHex || len(hexValue) != 64 {
				t.Fatalf("指纹 hex 往返失败: %q / %q / %v", hexValue, parsed.FingerprintHex, err)
			}
		})
	}
}

// TestParsePublicKeyRejectsOptions 守住「带前置选项的行一律拒绝」，防止 command= 注入。
func TestParsePublicKeyRejectsOptions(t *testing.T) {
	options := []string{
		`command="echo pwned" ` + testEd25519,
		`environment="LD_PRELOAD=/tmp/x.so" ` + testEd25519,
		`permitopen="127.0.0.1:6379" ` + testEd25519,
		`no-pty,no-port-forwarding ` + testEd25519,
	}
	for _, line := range options {
		if _, err := ParsePublicKey(line); err == nil {
			t.Fatalf("带前置选项的公钥应被拒绝: %q", line)
		}
	}
}

// TestParsePublicKeyRejectsMalformed 守住空值、多行、坏 base64、类型与 blob 不一致等分支。
func TestParsePublicKeyRejectsMalformed(t *testing.T) {
	cases := map[string]string{
		"空":         "   ",
		"多行":        testEd25519 + "\n" + testEd25519,
		"只有类型":      "ssh-ed25519",
		"坏 base64":  "ssh-ed25519 !!!!",
		"未知类型":      "ssh-dss AAAAB3NzaC1kc3MAAACB test",
		"类型与数据不一致":  "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIAfcyMFgumwwbxSagX25R7Rx9RZn3gv8pvUO2grzVda6 test",
		"base64 截断": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5",
		"注释超长":      testEd25519 + " " + strings.Repeat("注", MaxCommentRunes+1),
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePublicKey(line); err == nil {
				t.Fatalf("应拒绝: %q", line)
			}
		})
	}
}

// TestParsePublicKeyAllowsTrailingNewline 粘贴 .pub 文件常带行尾换行，应照常接受。
func TestParsePublicKeyAllowsTrailingNewline(t *testing.T) {
	if _, err := ParsePublicKey(testEd25519 + "\r\n"); err != nil {
		t.Fatalf("行尾换行不应导致失败: %v", err)
	}
}

// TestValidateTitle 守住标题非空与 100 字符上限。
func TestValidateTitle(t *testing.T) {
	if got, err := ValidateTitle("  办公笔记本  "); err != nil || got != "办公笔记本" {
		t.Fatalf("标题归一化失败: %q / %v", got, err)
	}
	if _, err := ValidateTitle("   "); err == nil {
		t.Fatal("空标题应被拒绝")
	}
	if _, err := ValidateTitle(strings.Repeat("题", MaxTitleRunes+1)); err == nil {
		t.Fatal("超长标题应被拒绝")
	}
}
