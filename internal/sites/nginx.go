package sites

import (
	"context"
	"regexp"
	"sort"
	"strings"
)

var (
	// serverStartRe 只匹配真正开启 server 块的 `server {`；不会命中 upstream 里的 `server host:port;`。
	serverStartRe = regexp.MustCompile(`(?i)^server\s*\{`)
	serverNameRe  = regexp.MustCompile(`(?mi)^\s*server_name\s+([^;]+);`)
	listenRe      = regexp.MustCompile(`(?mi)^\s*listen\s+([^;]+);`)
	rootRe        = regexp.MustCompile(`(?mi)^\s*root\s+([^;]+);`)
	proxyPassRe   = regexp.MustCompile(`(?mi)^\s*proxy_pass\s+([^;]+);`)
	certPathRe    = regexp.MustCompile(`(?mi)^\s*ssl_certificate\s+([^;]+);`)
	redirectRe    = regexp.MustCompile(`(?mi)^\s*return\s+30[12]\s+([^;]+);`)
)

// collectNginx 执行 `nginx -T` 并解析展开后的配置。命令或解析失败时返回空列表 + error。
func (s *Service) collectNginx(ctx context.Context) NginxPayload {
	out, err := s.run(ctx, s.nginxExecutable(), "-T")
	if err != nil {
		s.log.Warn("nginx -T 采集失败", "err", err.Error())
		return NginxPayload{Sites: []NginxSite{}, Error: err.Error()}
	}
	sites := parseNginxServers(out)
	if len(sites) == 0 {
		return NginxPayload{Sites: []NginxSite{}, Error: "未能从 nginx -T 输出中解析出 server 块"}
	}
	return NginxPayload{Sites: sites}
}

// parseNginxServers 按 `# configuration file <path>:` 分节，逐行跟踪花括号，
// 每个 server 块产出一行。正则 server_name 排在最后（需求书 §1）。
func parseNginxServers(text string) []NginxSite {
	lines := strings.Split(text, "\n")
	sites := make([]NginxSite, 0, 32)
	currentFile := ""

	for i := 0; i < len(lines); i++ {
		if file, ok := nginxFileMarker(lines[i]); ok {
			currentFile = file
			continue
		}
		if !serverStartRe.MatchString(strings.TrimSpace(stripNginxComment(lines[i]))) {
			continue
		}

		configFile := currentFile
		var block strings.Builder
		depth := 0
		for ; i < len(lines); i++ {
			if file, ok := nginxFileMarker(lines[i]); ok {
				currentFile = file
				continue
			}
			line := stripNginxComment(lines[i])
			block.WriteString(line)
			block.WriteByte('\n')
			depth += braceDelta(line)
			if depth <= 0 {
				break
			}
		}
		sites = append(sites, parseServerBlock(block.String(), configFile))
	}

	// 需求只要求正则块排最后；其余保持 nginx -T 的输出顺序（即 include 顺序）。
	sort.SliceStable(sites, func(i, j int) bool {
		return !sites[i].IsRegex && sites[j].IsRegex
	})
	return sites
}

// nginxFileMarker 识别 `nginx -T` 在每个文件前插入的注释行。
func nginxFileMarker(line string) (string, bool) {
	const prefix = "# configuration file "
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, prefix) || !strings.HasSuffix(trimmed, ":") {
		return "", false
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(trimmed, prefix), ":")), true
}

// stripNginxComment 去掉行内 `#` 注释，但保留引号内的 `#`（例如 URL 片段）。
func stripNginxComment(line string) string {
	var quote byte
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case quote != 0:
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return line[:i]
		}
	}
	return line
}

// braceDelta 统计引号外的花括号净增量，避免 add_header 里的 JSON 字符串误伤块边界。
func braceDelta(line string) int {
	delta := 0
	var quote byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		if quote != 0 {
			if c == '\\' {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			quote = c
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

func parseServerBlock(block, configFile string) NginxSite {
	site := NginxSite{
		Listen:     []string{},
		ConfigFile: configFile,
	}
	if names := serverNameRe.FindStringSubmatch(block); names != nil {
		fields := strings.Fields(unquoteNginx(names[1]))
		if len(fields) > 0 {
			site.ServerName = fields[0]
			site.IsRegex = strings.HasPrefix(fields[0], "~")
		}
	}
	for _, m := range listenRe.FindAllStringSubmatch(block, -1) {
		if v := unquoteNginx(m[1]); v != "" {
			site.Listen = append(site.Listen, v)
		}
	}
	site.Root = firstMatch(rootRe, block)
	site.ProxyPass = firstMatch(proxyPassRe, block)
	site.CertPath = firstMatch(certPathRe, block)
	site.Redirect = firstMatch(redirectRe, block)
	return site
}

func firstMatch(re *regexp.Regexp, block string) *string {
	m := re.FindStringSubmatch(block)
	if m == nil {
		return nil
	}
	v := unquoteNginx(m[1])
	if v == "" {
		return nil
	}
	return &v
}

func unquoteNginx(raw string) string {
	v := strings.TrimSpace(raw)
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
	}
	return strings.TrimSpace(v)
}
