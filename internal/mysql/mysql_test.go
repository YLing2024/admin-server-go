package mysql

import "testing"

// TestCastIntUsesSigned 守住 MySQL 的 CAST 目标类型：MySQL 只认 SIGNED/UNSIGNED，
// 写成 INTEGER 会在服务端直接语法报错（ERROR 1064），业务 SQL 不得再写死 INTEGER。
func TestCastIntUsesSigned(t *testing.T) {
	if got := New().CastInt("ts / ?"); got != "CAST(ts / ? AS SIGNED)" {
		t.Fatalf("CastInt 应生成 SIGNED 形态，实际 %q", got)
	}
}

// TestUpsertSQLShape 守住 MySQL 的 upsert 形态（ON DUPLICATE KEY UPDATE）。
func TestUpsertSQLShape(t *testing.T) {
	got := New().UpsertSQL("t", []string{"a", "b"}, []string{"a"})
	want := "INSERT INTO t (a, b) VALUES (?, ?) ON DUPLICATE KEY UPDATE b = VALUES(b)"
	if got != want {
		t.Fatalf("UpsertSQL 形态不符：\n got %q\nwant %q", got, want)
	}
}
