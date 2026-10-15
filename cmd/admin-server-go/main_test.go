package main

import "testing"

// TestRunRejectsBadArgs 守住子命令分派：未知参数报错、serve 不接受额外参数，
// 二者都在读环境变量/建连接前返回，便于无副作用断言。
func TestRunRejectsBadArgs(t *testing.T) {
	get := func(string) string { return "" }

	if err := run([]string{"bogus"}, get); err == nil {
		t.Fatal("未知参数应报错")
	}
	if err := run([]string{"serve", "extra"}, get); err == nil {
		t.Fatal("serve 带额外参数应报错")
	}
}
