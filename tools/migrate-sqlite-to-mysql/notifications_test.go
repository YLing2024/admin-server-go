package main

import (
	"strings"
	"testing"
)

// TestNotificationTypesDDLQuotesKey 守住通知类别建表语句里 key 列被引用：
// key 是 MySQL 保留字，裸写会让建表在服务端报 ERROR 1064。
// 该 DDL 于 2026-10-19 随通知中心下线从 internal/mysql 迁入本工具，断言一并搬来。
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
