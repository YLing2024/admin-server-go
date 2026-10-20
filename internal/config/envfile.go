package config

import (
	"bufio"
	"os"
	"strings"
)

// LoadEnvFile 读取 systemd 风格的 KEY=VALUE 文件，返回键值映射。
//
// 只支持注释、空行、可选的 `export ` 前缀与成对引号；不做变量展开。文件不存在或不可读时
// 返回空映射而不是错误：辅助命令必须「任何异常都输出空并退出 0」。
func LoadEnvFile(path string) map[string]string {
	values := map[string]string{}
	if strings.TrimSpace(path) == "" {
		return values
	}
	file, err := os.Open(path)
	if err != nil {
		return values
	}
	defer func() { _ = file.Close() }()

	scanner := bufio.NewScanner(file)
	// systemd 环境文件允许很长的值（如 DSN），放宽单行上限。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		idx := strings.IndexByte(line, '=')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		value := strings.TrimSpace(line[idx+1:])
		value = trimQuotes(value)
		values[key] = value
	}
	return values
}

// EnvFileLoader 返回一个 get：进程环境优先，其次环境文件。
//
// 进程环境优先是为了让手工排查（临时 export 一组只读账号）能覆盖文件配置；
// sshd 下没有这些变量，自然全部走文件。
func EnvFileLoader(get func(string) string, path string) func(string) string {
	values := LoadEnvFile(path)
	return func(key string) string {
		if value := get(key); value != "" {
			return value
		}
		return values[key]
	}
}

// trimQuotes 去掉成对的单/双引号；不处理转义，凭证里出现引号时建议用文件权限保护而非转义。
func trimQuotes(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}
