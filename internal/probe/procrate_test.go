package probe

import "testing"

// TestParseSSSocketBytes 覆盖：同一 pid 多个套接字求和、回环对跳过、缺字节字段的行跳过。
func TestParseSSSocketBytes(t *testing.T) {
	out := `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port Process
ESTAB      0      0      10.0.0.5:50000    93.184.216.34:443 users:(("nginx",pid=100,fd=3))
	 cubic bytes_sent:1000 bytes_acked:1000 bytes_received:2000 segs_out:10
ESTAB      0      0      10.0.0.5:50001    93.184.216.34:443 users:(("nginx",pid=100,fd=4))
	 cubic bytes_sent:500 bytes_received:700
ESTAB      0      0      127.0.0.1:4000    127.0.0.1:55555 users:(("blog",pid=200,fd=7))
	 cubic bytes_sent:99999 bytes_received:99999
ESTAB      0      0      10.0.0.5:50002    93.184.216.34:443 users:(("empty",pid=300,fd=8))
	 cubic segs_out:1
`
	res := parseSSSocketBytes(out)
	if got := res[100]; got[0] != 2700 || got[1] != 1500 {
		t.Errorf("pid 100 = rx%v tx%v，应为 rx2700 tx1500", got[0], got[1])
	}
	if _, ok := res[200]; ok {
		t.Error("纯回环套接字不该计入")
	}
	if _, ok := res[300]; ok {
		t.Error("没有字节字段的套接字不该计入")
	}
}

// TestParseSSSocketBytesHeader 确认表头行不会被当成套接字。
func TestParseSSSocketBytesHeader(t *testing.T) {
	out := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"
	if len(parseSSSocketBytes(out)) != 0 {
		t.Error("表头行不该产生条目")
	}
}

// TestIsLoopbackAddr 覆盖常见回环写法。
func TestIsLoopbackAddr(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:6379", "[::1]:80", "127.1.2.3:5"} {
		if !isLoopbackAddr(addr) {
			t.Errorf("%s 应判定为回环", addr)
		}
	}
	for _, addr := range []string{"10.0.0.5:50000", "93.184.216.34:443", "[::]:80"} {
		if isLoopbackAddr(addr) {
			t.Errorf("%s 不该判定为回环", addr)
		}
	}
}
