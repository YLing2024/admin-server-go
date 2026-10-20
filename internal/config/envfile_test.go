package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadEnvFile 守住 sshd 场景下的凭证文件解析：注释/空行、export 前缀、成对引号。
func TestLoadEnvFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh-keys.env")
	content := "# 只读账号\nexport SSH_KEYS_DB_USER=readonly\n\nSSH_KEYS_DB_PASSWORD=\"p@ss word\"\nSSH_KEYS_DB_NAME='admin_metrics'\nBROKEN_LINE\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	values := LoadEnvFile(path)
	if values["SSH_KEYS_DB_USER"] != "readonly" {
		t.Fatalf("export 前缀解析失败: %+v", values)
	}
	if values["SSH_KEYS_DB_PASSWORD"] != "p@ss word" {
		t.Fatalf("双引号解析失败: %q", values["SSH_KEYS_DB_PASSWORD"])
	}
	if values["SSH_KEYS_DB_NAME"] != "admin_metrics" {
		t.Fatalf("单引号解析失败: %q", values["SSH_KEYS_DB_NAME"])
	}
	if _, ok := values["BROKEN_LINE"]; ok {
		t.Fatal("缺等号的行应被忽略")
	}
}

// TestEnvFileLoaderPrecedence 守住「进程环境优先、文件兜底」：手工排查时可临时覆盖。
func TestEnvFileLoaderPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh-keys.env")
	if err := os.WriteFile(path, []byte("SSH_KEYS_DB_USER=from-file\nSSH_KEYS_DB_NAME=from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"SSH_KEYS_DB_NAME": "from-process"}
	get := EnvFileLoader(func(key string) string { return env[key] }, path)
	if got := get("SSH_KEYS_DB_USER"); got != "from-file" {
		t.Fatalf("文件值 = %q", got)
	}
	if got := get("SSH_KEYS_DB_NAME"); got != "from-process" {
		t.Fatalf("进程环境应优先，得到 %q", got)
	}
}

// TestLoadEnvFileMissing 守住「文件不存在返回空映射」——辅助命令不能因此报错退出。
func TestLoadEnvFileMissing(t *testing.T) {
	if values := LoadEnvFile(filepath.Join(t.TempDir(), "nope.env")); len(values) != 0 {
		t.Fatalf("缺失文件应返回空映射: %+v", values)
	}
}
