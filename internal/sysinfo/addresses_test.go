package sysinfo

import (
	"net/netip"
	"testing"
)

// TestReadAddressesPublicOnly 确认只返回公网 IPv4：回环与私网绝不出现在结果里。
func TestReadAddressesPublicOnly(t *testing.T) {
	for _, addr := range readAddresses() {
		ip, err := netip.ParseAddr(addr.IP)
		if err != nil {
			t.Fatalf("地址无法解析: %q", addr.IP)
		}
		if !ip.Is4() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() {
			t.Errorf("泄露了非公网地址: %+v", addr)
		}
		if addr.Iface == "" || addr.Prefix <= 0 || addr.Prefix > 32 {
			t.Errorf("网卡或前缀异常: %+v", addr)
		}
	}
}

// TestReadAddressesRoles 确认 ADMIN_IP_ROLES 按 `ip=标签` 生效，且无匹配时不加字段。
func TestReadAddressesRoles(t *testing.T) {
	addrs := readAddresses()
	if len(addrs) == 0 {
		t.Skip("本机没有公网 IPv4，跳过角色映射校验")
	}
	target := addrs[0].IP
	t.Setenv("ADMIN_IP_ROLES", "198.51.100.7=备用,"+target+"=高防")
	got := readAddresses()
	for _, addr := range got {
		if addr.IP == target && addr.Role != "高防" {
			t.Errorf("%s 的角色 = %q，应为 高防", target, addr.Role)
		}
		if addr.IP != target && addr.Role != "" {
			t.Errorf("%s 不该有角色: %q", addr.IP, addr.Role)
		}
	}
}

// TestSkipIface 确认虚拟网卡被过滤、真实网卡不被误杀。
func TestSkipIface(t *testing.T) {
	for _, name := range []string{"lo", "docker0", "br-1234", "veth9a", "tun0", "wg0"} {
		if !skipIface(name) {
			t.Errorf("%s 应被过滤", name)
		}
	}
	for _, name := range []string{"ens17", "eth0", "enp3s0"} {
		if skipIface(name) {
			t.Errorf("%s 不应被过滤", name)
		}
	}
}
