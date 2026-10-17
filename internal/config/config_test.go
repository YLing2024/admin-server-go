package config

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// loadWith 用最小可用环境跑 Load（DB_USER 必填），供终端开关用例复用。
func loadWith(t *testing.T, env map[string]string) *Config {
	t.Helper()
	env["DB_USER"] = "test"
	cfg, err := Load(func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	return cfg
}

// TestParseTermUnlock 覆盖 ADMIN_TERM_UNLOCK 的取值口径：缺省与 password 保留口令门，
// off 关闭，大小写与首尾空白不敏感，非法值回落 password。
func TestParseTermUnlock(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want TermUnlockMode
	}{
		{"缺省", "", TermUnlockPassword},
		{"password", "password", TermUnlockPassword},
		{"off", "off", TermUnlockOff},
		{"大小写不敏感", "OFF", TermUnlockOff},
		{"去首尾空白", " off ", TermUnlockOff},
		{"非法值回落", "disabled", TermUnlockPassword},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := parseTermUnlock(c.raw); got != c.want {
				t.Fatalf("parseTermUnlock(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

// TestLoadTermUnlockField 校验 Config 字段接线：未设置 → password；off → off。
func TestLoadTermUnlockField(t *testing.T) {
	if cfg := loadWith(t, map[string]string{}); cfg.TermUnlock != TermUnlockPassword {
		t.Fatalf("未设置时 TermUnlock = %q, want %q", cfg.TermUnlock, TermUnlockPassword)
	}
	if cfg := loadWith(t, map[string]string{"ADMIN_TERM_UNLOCK": "off"}); cfg.TermUnlock != TermUnlockOff {
		t.Fatalf("off 时 TermUnlock = %q, want %q", cfg.TermUnlock, TermUnlockOff)
	}
}

// TestLoadTermUnlockWarnsOnIllegal 校验非法值除回落外还告警一次（用于运维排错）。
// 合法值与缺省不得产生该告警。
func TestLoadTermUnlockWarnsOnIllegal(t *testing.T) {
	orig := slog.Default()
	defer slog.SetDefault(orig)

	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))

	if cfg := loadWith(t, map[string]string{"ADMIN_TERM_UNLOCK": "bogus"}); cfg.TermUnlock != TermUnlockPassword {
		t.Fatalf("非法值应回落 password，得到 %q", cfg.TermUnlock)
	}
	if !strings.Contains(buf.String(), "ADMIN_TERM_UNLOCK") {
		t.Fatalf("非法值应告警，实际日志=%q", buf.String())
	}

	buf.Reset()
	loadWith(t, map[string]string{"ADMIN_TERM_UNLOCK": "off"})
	if buf.Len() != 0 {
		t.Fatalf("合法值不应告警，实际日志=%q", buf.String())
	}
}
