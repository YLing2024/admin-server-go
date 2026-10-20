package store

import (
	"reflect"
	"testing"
)

// TestSetDiskDevicesAppendsSortedColumns 守住 MetricKeys 的动态扩展：固定列在前，
// 按盘速率列按设备名字典序稳定追加；MetricRound 新列为 0；DiskDevices 去重且有序。
func TestSetDiskDevicesAppendsSortedColumns(t *testing.T) {
	defer SetDiskDevices(nil)

	SetDiskDevices([]string{"vdb", "vda", "vda"})
	want := append(append([]string(nil), baseMetricKeys...),
		"disk_io_vda_read", "disk_io_vda_write", "disk_io_vdb_read", "disk_io_vdb_write")
	if !reflect.DeepEqual(MetricKeys, want) {
		t.Fatalf("MetricKeys 列序不符:\n got %v\nwant %v", MetricKeys, want)
	}
	for _, k := range []string{"disk_io_vda_read", "disk_io_vdb_write"} {
		if got, ok := MetricRound[k]; !ok || got != 0 {
			t.Errorf("MetricRound[%s]=%d(存在=%v)，应为 0", k, got, ok)
		}
	}
	if got, want := DiskDevices(), []string{"vda", "vdb"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("DiskDevices()=%v want %v", got, want)
	}

	// 幂等：重置后不残留上一批设备列。
	SetDiskDevices([]string{"vda"})
	if got, want := MetricKeys[len(MetricKeys)-2:], []string{"disk_io_vda_read", "disk_io_vda_write"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("重置后列尾不符: %v", got)
	}
	if len(MetricKeys) != len(baseMetricKeys)+2 {
		t.Fatalf("重置后列数应为 %d，实际 %d", len(baseMetricKeys)+2, len(MetricKeys))
	}
}
