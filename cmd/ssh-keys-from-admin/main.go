// Command ssh-keys-from-admin 是 sshd 的 AuthorizedKeysCommand 辅助程序。
//
// 调用形态与 sshd 配置一致：`ssh-keys-from-admin <username> <fingerprint>`，其中
// %u 为登录用户名、%f 为该公钥指纹（仅在 key 验证阶段有值）。程序只允许
// SSH_KEYS_USER（默认 root）登录，输出启用中的公钥，每行 `type base64 comment`。
//
// 纪律：任何异常都必须保持 stdout 为空并以 0 退出——sshd 会把它当作「没有可用公钥」，
// 宁可登录失败，也不能因为程序报错阻塞登录或误放行。
package main

import (
	"context"
	"os"
	"strings"

	"admin-server-go/internal/config"
	"admin-server-go/internal/sshkeys"
)

func main() {
	defer func() {
		// 兜底吞掉 panic：绝不向 stdout 打栈，退出码保持 0。
		if recover() != nil {
			os.Exit(0)
		}
		os.Exit(0)
	}()

	args := os.Args[1:]
	username := ""
	fingerprint := ""
	if len(args) > 0 {
		username = strings.TrimSpace(args[0])
	}
	if len(args) > 1 {
		fingerprint = strings.TrimSpace(args[1])
	}

	envFile := strings.TrimSpace(os.Getenv("SSH_KEYS_ENV_FILE"))
	if envFile == "" {
		envFile = config.DefaultSSHKeysEnvFile
	}
	get := config.EnvFileLoader(os.Getenv, envFile)
	_ = sshkeys.RunHelper(context.Background(), get, username, fingerprint, os.Stdout)
}
