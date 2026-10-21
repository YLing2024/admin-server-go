package sites

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const sampleNginxT = `# configuration file /etc/nginx/nginx.conf:
http {
    server_tokens off;
}
# configuration file /etc/nginx/conf.d/admin.conf:
server {
    listen 443 ssl http2;
    listen [::]:443 ssl http2;
    server_name admin.example.com;
    root /var/www/admin;
    ssl_certificate /etc/letsencrypt/live/admin.example.com/fullchain.pem;
    location /api/ {
        proxy_pass http://127.0.0.1:18920;
    }
    add_header X-Test "{\"brace\": true}";
}
server {
    server_name ~^.+\.example\.com$;
    listen 80;
    return 301 https://$host$request_uri;
}
`

func TestParseNginxServers(t *testing.T) {
	sites := parseNginxServers(sampleNginxT)
	if len(sites) != 2 {
		t.Fatalf("sites = %d, want 2: %+v", len(sites), sites)
	}
	first := sites[0]
	if first.ServerName != "admin.example.com" || first.IsRegex {
		t.Errorf("first server = %+v", first)
	}
	if len(first.Listen) != 2 || first.Listen[0] != "443 ssl http2" {
		t.Errorf("listen = %v", first.Listen)
	}
	if first.Root == nil || *first.Root != "/var/www/admin" {
		t.Errorf("root = %v", first.Root)
	}
	if first.ProxyPass == nil || *first.ProxyPass != "http://127.0.0.1:18920" {
		t.Errorf("proxyPass = %v", first.ProxyPass)
	}
	if first.CertPath == nil || !strings.HasSuffix(*first.CertPath, "admin.example.com/fullchain.pem") {
		t.Errorf("certPath = %v", first.CertPath)
	}
	if first.ConfigFile != "/etc/nginx/conf.d/admin.conf" {
		t.Errorf("configFile = %q", first.ConfigFile)
	}
	if first.Redirect != nil {
		t.Errorf("redirect = %v, want nil", *first.Redirect)
	}

	// 正则块必须排在最后。
	last := sites[len(sites)-1]
	if !last.IsRegex || last.ServerName != `~^.+\.example\.com$` {
		t.Errorf("regex server not last: %+v", last)
	}
	if last.Redirect == nil || *last.Redirect != "https://$host$request_uri" {
		t.Errorf("redirect = %v", last.Redirect)
	}
}

func TestNginxHandlerCaches(t *testing.T) {
	s := New(Options{NginxBin: "nginx"})
	s.now = func() time.Time { return time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC) }
	calls := 0
	s.run = func(_ context.Context, _ string, _ ...string) (string, error) {
		calls++
		return sampleNginxT, nil
	}

	call := func() NginxPayload {
		rec := httptest.NewRecorder()
		s.Nginx(rec, httptest.NewRequest(http.MethodGet, "/api/admin/sites/nginx", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var payload NginxPayload
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return payload
	}

	if got := call(); len(got.Sites) != 2 || got.Error != "" {
		t.Fatalf("first payload = %+v", got)
	}
	call()
	if calls != 1 {
		t.Fatalf("calls = %d, want 1 within TTL", calls)
	}

	s.now = func() time.Time { return time.Date(2026, 10, 21, 12, 1, 1, 0, time.UTC) }
	call()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 after TTL", calls)
	}
}

func TestNginxHandlerDegrades(t *testing.T) {
	s := New(Options{NginxBin: "nginx"})
	s.run = func(_ context.Context, _ string, _ ...string) (string, error) {
		return "", errors.New("nginx: configuration file test failed")
	}
	rec := httptest.NewRecorder()
	s.Nginx(rec, httptest.NewRequest(http.MethodGet, "/api/admin/sites/nginx", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var payload NginxPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Error == "" || payload.Sites == nil || len(payload.Sites) != 0 {
		t.Errorf("未降级: %+v", payload)
	}
}

func TestCollectCerts(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "admin.example.com")
	broken := filepath.Join(dir, "broken.example.com")
	for _, d := range []string{good, broken} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "fullchain.pem"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s := New(Options{CertDir: dir})
	s.now = func() time.Time { return now }
	s.run = func(_ context.Context, name string, args ...string) (string, error) {
		if name == "systemctl" {
			return "active\n", nil
		}
		if strings.Contains(strings.Join(args, " "), "broken.example.com") {
			return "", errors.New("openssl: unable to load certificate")
		}
		return "subject=CN = admin.example.com\nnotAfter=Jan 19 12:00:00 2027 GMT\nissuer=C = US, O = Let's Encrypt, CN = YE2\n", nil
	}

	payload := s.collectCerts(context.Background())
	if len(payload.Certs) != 2 {
		t.Fatalf("certs = %d: %+v", len(payload.Certs), payload.Certs)
	}
	first := payload.Certs[0]
	if first.Domain != "admin.example.com" {
		t.Errorf("有效证书应排在最前: %+v", payload.Certs)
	}
	if first.DaysLeft == nil || *first.DaysLeft != 90 {
		t.Errorf("daysLeft = %v, want 90", first.DaysLeft)
	}
	if first.NotAfter == nil || *first.NotAfter != "2027-01-19T12:00:00Z" {
		t.Errorf("notAfter = %v", first.NotAfter)
	}
	if first.Issuer != "Let's Encrypt" || !first.AutoRenew {
		t.Errorf("issuer/autoRenew = %q/%v", first.Issuer, first.AutoRenew)
	}
	if payload.Certs[1].Error == "" {
		t.Errorf("破损证书缺少 error: %+v", payload.Certs[1])
	}
}

func TestIssuerName(t *testing.T) {
	cases := map[string]string{
		"C = US, O = Let's Encrypt, CN = YE2": "Let's Encrypt",
		"C = US, O = Example CA, CN = Root":   "Example CA",
		"C = US, CN = Root":                   "Root",
		"":                                    "",
	}
	for raw, want := range cases {
		if got := issuerName(raw); got != want {
			t.Errorf("issuerName(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestCollectBackups(t *testing.T) {
	dir := t.TempDir()
	older := filepath.Join(dir, "backup-old")
	newer := filepath.Join(dir, "backup-new")
	for _, d := range []string{older, newer} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	oldTime := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	newTime := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newer, newTime, newTime); err != nil {
		t.Fatal(err)
	}

	s := New(Options{BackupDir: dir})
	s.run = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "du":
			return "100\t" + older + "\n250\t" + newer + "\n", nil
		case "systemctl":
			if args[0] == "list-units" {
				return "daily-full-backup.timer loaded active waiting Daily\ncertbot.timer loaded active waiting Certbot\n", nil
			}
			return "NextElapseUSecRealtime=Thu 2026-10-22 04:02:53 HKT\n" +
				"LastTriggerUSec=Wed 2026-10-21 04:03:31 HKT\n" +
				"Id=daily-full-backup.timer\n\n", nil
		default:
			return "", errors.New("unexpected command " + name)
		}
	}

	payload := s.collectBackups(context.Background())
	if payload.Error != "" {
		t.Fatalf("error = %q", payload.Error)
	}
	if payload.Total != 2 || payload.TotalSize != 350 {
		t.Errorf("total/totalSize = %d/%d", payload.Total, payload.TotalSize)
	}
	if payload.LatestModifiedAt != newTime.UnixMilli() {
		t.Errorf("latest = %d", payload.LatestModifiedAt)
	}
	if len(payload.Backups) != 2 || payload.Backups[0].Name != "backup-new" || payload.Backups[0].Size != 250 {
		t.Errorf("backups = %+v", payload.Backups)
	}
	if len(payload.Timers) != 1 || payload.Timers[0].Name != "daily-full-backup.timer" {
		t.Fatalf("timers = %+v", payload.Timers)
	}
	if payload.Timers[0].NextElapseUSecRealtime == "" || payload.Timers[0].LastTriggerUSec == "" {
		t.Errorf("timer 属性为空: %+v", payload.Timers[0])
	}
}

func TestParseDuSizes(t *testing.T) {
	sizes := parseDuSizes("12\t/a\n34\t/b\nbadline\n56\t/c\r\n")
	if sizes["/a"] != 12 || sizes["/b"] != 34 || sizes["/c"] != 56 {
		t.Errorf("sizes = %+v", sizes)
	}
}
