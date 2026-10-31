package probe

import "testing"

// TestParseSSListen 覆盖 ss -ltnp 的三种地址写法与 pid 提取。
func TestParseSSListen(t *testing.T) {
	out := `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
LISTEN 0      511        127.0.0.1:6379       0.0.0.0:*    users:(("redis-server",pid=2981404,fd=6))
LISTEN 0      511             *:80               *:*    users:(("nginx",pid=702346,fd=8),("nginx",pid=702340,fd=8))
LISTEN 0      4096          [::]:443             [::]:*    users:(("nginx",pid=702346,fd=9))
ESTAB  0      0        127.0.0.1:3100    127.0.0.1:51000
`
	ports := parseSSListen(out)
	if ports[6379] != 2981404 {
		t.Errorf("6379 → %d，应为 2981404", ports[6379])
	}
	if ports[80] != 702346 {
		t.Errorf("80 → %d，应为 702346（取第一个监听进程）", ports[80])
	}
	if ports[443] != 702346 {
		t.Errorf("[::]:443 → %d，应为 702346", ports[443])
	}
	if _, ok := ports[3100]; ok {
		t.Error("非 LISTEN 行不该进端口表")
	}
	if len(ports) != 3 {
		t.Errorf("端口表大小 = %d，应为 3", len(ports))
	}
}

// TestParseSystemctlShow 覆盖 key=value 解析（顺序与请求无关）与 unknown 状态。
func TestParseSystemctlShow(t *testing.T) {
	// 实测 systemctl 会把 MainPID 排在 ActiveState 前
	active, pid, ok := parseSystemctlShow("MainPID=1074577\nActiveState=active\n")
	if !ok || !active || pid != 1074577 {
		t.Errorf("解析失败: active=%v pid=%d ok=%v", active, pid, ok)
	}

	active, pid, ok = parseSystemctlShow("ActiveState=inactive\nMainPID=0\n")
	if !ok || active || pid != 0 {
		t.Errorf("inactive 解析错误: active=%v pid=%d ok=%v", active, pid, ok)
	}

	if _, _, ok := parseSystemctlShow("garbage\n"); ok {
		t.Error("无 key=value 行时应当失败")
	}
}
