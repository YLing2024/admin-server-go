package sqlite

import "testing"

// TestCastIntUsesInteger 守住 SQLite 的 CAST 目标类型。
func TestCastIntUsesInteger(t *testing.T) {
	if got := New().CastInt("ts / ?"); got != "CAST(ts / ? AS INTEGER)" {
		t.Fatalf("CastInt 应生成 INTEGER 形态，实际 %q", got)
	}
}
