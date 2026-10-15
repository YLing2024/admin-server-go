package sqlite

import "testing"

// TestCastIntUsesInteger 守住 SQLite 的 CAST 目标类型。
func TestCastIntUsesInteger(t *testing.T) {
	if got := New().CastInt("ts / ?"); got != "CAST(ts / ? AS INTEGER)" {
		t.Fatalf("CastInt 应生成 INTEGER 形态，实际 %q", got)
	}
}

// TestUpsertSQLQuotesIdentifiers 守住 upsert 对标识符做反引号引用，
// 否则列名撞关键字（如通知类别的 key）在 MySQL 下直接语法报错。
func TestUpsertSQLQuotesIdentifiers(t *testing.T) {
	got := New().UpsertSQL("notification_types", []string{"key", "label"}, []string{"key"})
	want := "INSERT INTO `notification_types` (`key`, `label`) VALUES (?, ?) ON CONFLICT(`key`) DO UPDATE SET `label` = excluded.`label`"
	if got != want {
		t.Fatalf("UpsertSQL 未正确引用标识符：\n got %q\nwant %q", got, want)
	}
}
