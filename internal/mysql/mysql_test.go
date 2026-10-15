package mysql

import (
	"strings"
	"testing"
)

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

// TestNotificationTypesDDLQuotesKey 守住通知类别建表语句里 key 列被引用：
// key 是 MySQL 保留字，裸写会让建表在服务端报 ERROR 1064。
func TestNotificationTypesDDLQuotesKey(t *testing.T) {
	var ddl string
	for _, s := range notificationsDDL {
		if strings.Contains(s, "notification_types") {
			ddl = s
			break
		}
	}
	if ddl == "" {
		t.Fatal("未找到 notification_types 建表语句")
	}
	if !strings.Contains(ddl, "`key`") {
		t.Fatalf("key 列未被反引号引用：\n%s", ddl)
	}
	if strings.Contains(ddl, "\t\tkey ") || strings.Contains(ddl, "\n\t\tkey ") {
		t.Fatalf("存在裸写的 key 列：\n%s", ddl)
	}
	if !strings.Contains(ddl, "PRIMARY KEY (`key`)") {
		t.Fatalf("主键未引用 key 列：\n%s", ddl)
	}
}
