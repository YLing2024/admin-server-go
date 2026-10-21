package sites

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// issuerOrgRe 提取证书签发者里的 O（组织）。Let's Encrypt 的中间证书 CN 会随代际变化
// （R3 / YE2 …），组织名稳定为 "Let's Encrypt"，与需求书示例一致。
var issuerOrgRe = regexp.MustCompile(`(?:^|,\s*)O\s*=\s*([^,]+)`)

var issuerCNRe = regexp.MustCompile(`(?:^|,\s*)CN\s*=\s*([^,]+)`)

// collectCerts 扫描证书目录下每个 `*/fullchain.pem`，逐张调用 openssl 解析。
// 单张失败只给该条写 error，不影响其它证书；certbot.timer 只探测一次。
func (s *Service) collectCerts(ctx context.Context) CertsPayload {
	entries, err := os.ReadDir(s.certDir)
	if err != nil {
		s.log.Warn("证书目录读取失败", "dir", s.certDir, "err", err.Error())
		return CertsPayload{Certs: []Cert{}, Error: "读取证书目录失败: " + err.Error()}
	}

	autoRenew := s.certbotActive(ctx)
	certs := make([]Cert, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(s.certDir, entry.Name(), "fullchain.pem")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		cert := Cert{Domain: entry.Name(), AutoRenew: autoRenew}
		out, err := s.run(ctx, "openssl", "x509", "-noout", "-subject", "-enddate", "-issuer", "-in", path)
		if err != nil {
			cert.Error = err.Error()
			certs = append(certs, cert)
			continue
		}
		applyOpenSSLOutput(&cert, out, s.now())
		certs = append(certs, cert)
	}

	// 快到期（含已过期）的排在前面，解析失败的条目排最后。
	sort.SliceStable(certs, func(i, j int) bool {
		di, dj := certs[i].DaysLeft, certs[j].DaysLeft
		if di == nil || dj == nil {
			return di != nil && dj == nil
		}
		return *di < *dj
	})
	return CertsPayload{Certs: certs}
}

// certbotActive 判断证书自动续期 timer 是否 active。命令失败按未启用处理。
func (s *Service) certbotActive(ctx context.Context) bool {
	out, err := s.run(ctx, "systemctl", "is-active", "certbot.timer")
	return err == nil && strings.TrimSpace(out) == "active"
}

// applyOpenSSLOutput 解析 `openssl x509 -noout -subject -enddate -issuer` 的三行输出。
func applyOpenSSLOutput(cert *Cert, out string, now time.Time) {
	var notAfterRaw, issuerRaw string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "notAfter="):
			notAfterRaw = strings.TrimSpace(strings.TrimPrefix(line, "notAfter="))
		case strings.HasPrefix(line, "issuer="):
			issuerRaw = strings.TrimSpace(strings.TrimPrefix(line, "issuer="))
		}
	}
	cert.Issuer = issuerName(issuerRaw)

	if notAfterRaw == "" {
		cert.Error = "证书缺少 notAfter 字段"
		return
	}
	notAfter, err := time.Parse("Jan _2 15:04:05 2006 MST", notAfterRaw)
	if err != nil {
		cert.Error = "解析证书到期时间失败: " + notAfterRaw
		return
	}
	notAfter = notAfter.UTC()
	iso := notAfter.Format(time.RFC3339)
	days := int(notAfter.Sub(now).Hours() / 24)
	cert.NotAfter = &iso
	cert.DaysLeft = &days
}

// issuerName 优先返回组织名（Let's Encrypt），缺失时退回 CN，再退回原始串。
func issuerName(raw string) string {
	if m := issuerOrgRe.FindStringSubmatch(raw); m != nil {
		if v := strings.TrimSpace(m[1]); v != "" {
			return v
		}
	}
	if m := issuerCNRe.FindStringSubmatch(raw); m != nil {
		if v := strings.TrimSpace(m[1]); v != "" {
			return v
		}
	}
	return strings.TrimSpace(raw)
}
