package sysinfo

import (
	"net"
	"net/netip"
	"os"
	"sort"
	"strings"
)

// Address 是一条公网 IPv4 地址。Role 来自环境变量 ADMIN_IP_ROLES
// （形如 `24.233.30.65=高防,24.233.12.88=普通`），用于把线路用途显式标注出来；
// 未配置时为空，前端只显示地址与网卡。
type Address struct {
	Iface  string `json:"iface"`
	IP     string `json:"ip"`
	Prefix int    `json:"prefix"`
	// Default 为真表示默认路由的源地址（出站流量走这条）。
	Default bool   `json:"default"`
	Role    string `json:"role,omitempty"`
}

// skipIface 过滤虚拟网卡：容器网桥、veth、tun/tap、VPN 与回环都不算对外地址。
func skipIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{"docker", "br-", "veth", "virbr", "tun", "tap", "wg", "zt", "cni", "vxlan"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

// defaultSourceIP 用一次不发包的 UDP dial 取默认路由的本地地址；取不到返回空串。
func defaultSourceIP() string {
	conn, err := net.Dial("udp4", "1.1.1.1:53")
	if err != nil {
		return ""
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return addr.IP.String()
}

// roleMap 解析 `ip=标签` 逗号分隔的映射；格式非法的项忽略。
func roleMap(raw string) map[string]string {
	out := map[string]string{}
	for _, item := range strings.Split(raw, ",") {
		ip, role, ok := strings.Cut(strings.TrimSpace(item), "=")
		if !ok {
			continue
		}
		ip = strings.TrimSpace(ip)
		role = strings.TrimSpace(role)
		if ip != "" && role != "" {
			out[ip] = role
		}
	}
	return out
}

// readAddresses 列出本机全部公网 IPv4（回环、私网、链路本地一律跳过，网卡按名字去重），
// 默认出口排在最前，其余按地址升序。
func readAddresses() []Address {
	def := defaultSourceIP()
	roles := roleMap(os.Getenv("ADMIN_IP_ROLES"))
	ifaces, err := net.Interfaces()
	if err != nil {
		return []Address{}
	}
	seen := map[string]bool{}
	out := []Address{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || skipIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			// 只保留公网单播：回环、私网（含 10/8、172.16/12、192.168/16）、链路本地跳过。
			if ip4 == nil || !ip4.IsGlobalUnicast() || ip4.IsPrivate() || ip4.IsLinkLocalUnicast() {
				continue
			}
			text := ip4.String()
			if seen[text] {
				continue
			}
			seen[text] = true
			prefix, _ := ipNet.Mask.Size()
			out = append(out, Address{
				Iface:   iface.Name,
				IP:      text,
				Prefix:  prefix,
				Default: text == def,
				Role:    roles[text],
			})
		}
	}
	// netip 排序比字符串排序可靠（10.x 与 100.x 的相对顺序）。
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		a, errA := netip.ParseAddr(out[i].IP)
		b, errB := netip.ParseAddr(out[j].IP)
		if errA != nil || errB != nil {
			return out[i].IP < out[j].IP
		}
		return a.Less(b)
	})
	return out
}
