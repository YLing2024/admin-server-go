package migrate

import (
	"testing"

	"admin-server-go/internal/sqlite"
)

// TestLogicalDBsFilter 守住 --db 过滤：空串两库都返，非空只返该库。
func TestLogicalDBsFilter(t *testing.T) {
	d := sqlite.New()

	all := logicalDBs(d, "")
	if len(all) != 2 || all[0].name != "metrics" || all[1].name != "notifications" {
		t.Fatalf("不过滤应返回两库（metrics、notifications），实际 %v", names(all))
	}

	only := logicalDBs(d, "notifications")
	if len(only) != 1 || only[0].name != "notifications" {
		t.Fatalf("--db notifications 应只返回通知库，实际 %v", names(only))
	}

	if got := logicalDBs(d, "nope"); len(got) != 0 {
		t.Fatalf("未知库名应返回空，实际 %v", names(got))
	}
}

func names(dbs []logicalDB) []string {
	out := make([]string, len(dbs))
	for i, d := range dbs {
		out[i] = d.name
	}
	return out
}
