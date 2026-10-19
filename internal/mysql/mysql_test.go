package mysql

import (
	"strings"
	"testing"
)

// TestCastIntTruncates 守住 CastInt 的截断语义：MySQL 的 CAST(... AS SIGNED) 会四舍五入
// （179/60 → 3），会让分桶边界与旧 SQLite 的 CAST(... AS INTEGER) 不一致，必须用 TRUNCATE。
func TestCastIntTruncates(t *testing.T) {
	got := New().CastInt("ts / ?")
	if got != "CAST(TRUNCATE(ts / ?, 0) AS SIGNED)" {
		t.Fatalf("CastInt 应生成 TRUNCATE+CAST 形态，实际 %q", got)
	}
	if strings.Contains(got, "CAST(ts / ?") {
		t.Fatalf("不得直接 CAST 除法（会四舍五入）：%q", got)
	}
}

// TestUpsertSQLShape 守住 MySQL 的 upsert 形态（ON DUPLICATE KEY UPDATE）。
func TestUpsertSQLShape(t *testing.T) {
	got := New().UpsertSQL("t", []string{"a", "b"}, []string{"a"})
	want := "INSERT INTO `t` (`a`, `b`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `b` = VALUES(`b`)"
	if got != want {
		t.Fatalf("UpsertSQL 形态不符：\n got %q\nwant %q", got, want)
	}
}

// TestQuoteIdent 守住反引号引用与内部反引号转义。
func TestQuoteIdent(t *testing.T) {
	d := New()
	if got := d.QuoteIdent("key"); got != "`key`" {
		t.Fatalf("QuoteIdent 应加反引号，实际 %q", got)
	}
	if got := d.QuoteIdent("a`b"); got != "`a``b`" {
		t.Fatalf("内部反引号应翻倍转义，实际 %q", got)
	}
}
