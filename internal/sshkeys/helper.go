package sshkeys

import (
	"context"
	"io"
	"strings"
	"time"

	"admin-server-go/internal/config"
	"admin-server-go/internal/mysql"
	"admin-server-go/internal/store"
)

// helperTimeout 限制辅助命令的整体耗时：sshd 在登录路径上同步调用它，宁可返回空，
// 也不能让数据库抖动把登录拖死。
const helperTimeout = 3 * time.Second

// RunHelper 是 ssh-keys-from-admin 的核心实现，便于脱离进程直接测试。
//
// 入参 username / fingerprint 对应 sshd 的 %u / %f。只有 username 等于 SSH_KEYS_USER
// 时才查库；任何错误都返回给调用方，由 main 静默忽略（保证 stdout 为空、退出码 0）。
func RunHelper(ctx context.Context, get func(string) string, username, fingerprint string, out io.Writer) error {
	cfg, err := config.Load(get)
	if err != nil {
		return err
	}
	if username == "" || username != cfg.SSHKeysUser {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()

	dsn := mysql.BuildDSN(cfg.SSHKeysDBHost, cfg.SSHKeysDBPort, cfg.SSHKeysDBName, cfg.SSHKeysDBUser, cfg.SSHKeysDBPassword)
	db, err := store.Open(ctx, mysql.New(), dsn, 1, nil)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	repo := NewRepository(db)
	lines, err := repo.EnabledLines(ctx)
	if err != nil {
		return err
	}
	if len(lines) > 0 {
		var builder strings.Builder
		for _, line := range lines {
			builder.WriteString(line)
			builder.WriteByte('\n')
		}
		if _, err := io.WriteString(out, builder.String()); err != nil {
			return err
		}
	}

	// last_used_at 是尽力而为：只影响管理页的「最近使用」，不能影响公钥输出。
	if fingerprint != "" {
		if hexValue, err := FingerprintHexFromOpenSSH(fingerprint); err == nil {
			touchCtx, cancelTouch := context.WithTimeout(context.Background(), time.Second)
			defer cancelTouch()
			_ = repo.TouchLastUsed(touchCtx, hexValue)
		}
	}
	return nil
}
