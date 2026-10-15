package probe

import (
	"os/exec"
	"strings"
)

// getDisks 解析 `df -kP`，返回所有真实挂载的设备盘（filesystem 以 /dev/ 开头），
// 排除 tmpfs/udev/overlay 等伪文件系统，跳过 /boot 与 /boot/efi；单位字节。df 失败返回空数组。
func getDisks() []map[string]any {
	disks := []map[string]any{}
	out, err := exec.Command("df", "-kP").Output()
	if err != nil {
		return disks
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for _, line := range lines[1:] {
		cols := splitFields(line)
		if len(cols) < 6 {
			continue
		}
		filesystem := cols[0]
		// 挂载点可能含空格：df 的挂载点始终是最后一个字段之后的部分，这里取第 6 列起再拼回。
		mount := strings.Join(cols[5:], " ")
		if !strings.HasPrefix(filesystem, "/dev/") {
			continue
		}
		if mount == "/boot" || mount == "/boot/efi" {
			continue
		}
		disks = append(disks, map[string]any{
			"filesystem": filesystem,
			"mount":      mount,
			"total":      atoi(cols[1]) * 1024,
			"used":       atoi(cols[2]) * 1024,
			"free":       atoi(cols[3]) * 1024,
			"percent":    int(atoi(strings.TrimSuffix(cols[4], "%"))),
		})
	}
	return disks
}

// getDisk 是旧字段兼容：只取挂载点为 "/" 的盘，找不到时保持全 0。
func getDisk() map[string]any {
	disk := map[string]any{"total": 0, "used": 0, "free": 0, "percent": 0}
	for _, d := range getDisks() {
		if d["mount"] == "/" {
			disk["total"] = d["total"]
			disk["used"] = d["used"]
			disk["free"] = d["free"]
			disk["percent"] = d["percent"]
			break
		}
	}
	return disk
}
