package probe

import (
	"math"
	"os/exec"
	"strings"
	"syscall"
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
		d := map[string]any{
			"filesystem": filesystem,
			"mount":      mount,
			"device":     baseDevice(filesystem),
			"total":      atoi(cols[1]) * 1024,
			"used":       atoi(cols[2]) * 1024,
			"free":       atoi(cols[3]) * 1024,
			"percent":    int(atoi(strings.TrimSuffix(cols[4], "%"))),
		}
		// inode 计数只能从 statfs 拿；伪文件系统已在上面的 device 过滤里排除。
		if inodePercent, inodeTotal, ok := statfsInodes(mount); ok {
			d["inode_percent"] = inodePercent
			d["inode_total"] = inodeTotal
		}
		disks = append(disks, d)
	}
	return disks
}

// statfsInodes 返回挂载点的 inode 使用率（%）与总数；statfs 失败或文件系统不报 inode 时 ok=false。
func statfsInodes(mount string) (percent float64, total int64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(mount, &st); err != nil || st.Files == 0 {
		return 0, 0, false
	}
	used := int64(st.Files - st.Ffree)
	if used < 0 {
		used = 0
	}
	return round1(float64(used) / float64(st.Files) * 100), int64(st.Files), true
}

// baseDevice 把挂载点的 filesystem 归一到基础设备名（与 /proc/diskstats 的设备名对齐）：
// `/dev/vda1` → `vda`、`/dev/nvme0n1p1` → `nvme0n1`、`/dev/mapper/x` → `x`。无法映射时返回空串。
func baseDevice(filesystem string) string {
	name := strings.TrimPrefix(filesystem, "/dev/")
	if mapper := strings.TrimPrefix(name, "mapper/"); mapper != name {
		return mapper
	}
	if name == filesystem {
		// 不以 /dev/ 开头（理论上不会到这里）：不猜。
		return ""
	}
	return stripPartition(name)
}

// stripPartition 去掉设备名的分区后缀：`vda1` → `vda`、`nvme0n1p1` → `nvme0n1`、
// `mmcblk0p1` → `mmcblk0`。名字本身就是整盘或无法归一时原样返回（调用方按空串处理未命中）。
func stripPartition(name string) string {
	if isWholeDisk(name) {
		return name
	}
	// nvme / mmcblk 的分区形如 <整盘>p<数字>。
	if i := strings.LastIndexByte(name, 'p'); i > 0 {
		if base := name[:i]; isWholeDisk(base) && isDigits(name[i+1:]) {
			return base
		}
	}
	// sd/vd/hd/md/dm 的分区直接在整盘名后接数字。
	j := len(name)
	for j > 0 && name[j-1] >= '0' && name[j-1] <= '9' {
		j--
	}
	if j < len(name) {
		if base := name[:j]; isWholeDisk(base) {
			return base
		}
	}
	return ""
}

// RootDiskPercent 返回根分区使用率（0–100，一位小数）；Statfs 失败返回 0。
// 历史趋势的「磁盘」列用它，与 /api/admin/system/info 的 disks[].percent 同口径（used/blocks）。
func RootDiskPercent() float64 {
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err != nil || st.Blocks == 0 {
		return 0
	}
	used := float64(st.Blocks - st.Bfree)
	percent := used / float64(st.Blocks) * 100
	return math.Round(percent*10) / 10
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
