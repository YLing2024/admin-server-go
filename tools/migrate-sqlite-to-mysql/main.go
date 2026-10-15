// Command migrate-sqlite-to-mysql 独立运行，把旧 SQLite 库搬进 MySQL：
//
//	go run ./tools/migrate-sqlite-to-mysql \
//	  --db all \
//	  --src-metrics ./data/metrics.db --src-notifications ./data/notifications.db \
//	  --dst-host 127.0.0.1 --dst-port 3306 --dst-user root --dst-password '***' \
//	  --verify
//
// 源库只读打开（只发 SELECT），目标库只做增量迁移 + 批量 upsert，可重复执行。
// 该工具不被服务主程序 import（服务运行期只连 MySQL，不含任何 SQLite 自建库路径）。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"admin-server-go/internal/mysql"
	"admin-server-go/internal/store"

	// 注册 modernc sqlite 驱动：本工具只读外部 SQLite 源库。
	_ "modernc.org/sqlite"
)

// sqliteDriverName 是 modernc.org/sqlite 注册的驱动名。
const sqliteDriverName = "sqlite"

// mysqlDialect 返回 MySQL 方言（唯一目标方言）。
func mysqlDialect() store.Dialect { return mysql.New() }

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "迁移失败：%v\n", err)
		os.Exit(1)
	}
}

// dstFlags 是 MySQL 目标的连接参数。
type dstFlags struct {
	host        string
	port        int
	user        string
	password    string
	metricsName string
	notifName   string
	metricsDSN  string
	notifDSN    string
}

// run 解析参数、组装逻辑库并按需执行搬运。
func run(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("migrate-sqlite-to-mysql", flag.ContinueOnError)
	fs.SetOutput(out)
	onlyDB := fs.String("db", "all", "要搬运的逻辑库：metrics | notifications | all")
	srcMetrics := fs.String("src-metrics", "", "源 SQLite 指标库文件（只读）")
	srcNotifications := fs.String("src-notifications", "", "源 SQLite 通知库文件（只读）")
	dst := dstFlags{}
	fs.StringVar(&dst.host, "dst-host", "127.0.0.1", "目标 MySQL 主机")
	fs.IntVar(&dst.port, "dst-port", 3306, "目标 MySQL 端口")
	fs.StringVar(&dst.user, "dst-user", "", "目标 MySQL 账号")
	fs.StringVar(&dst.password, "dst-password", "", "目标 MySQL 口令")
	fs.StringVar(&dst.metricsName, "dst-metrics-name", "admin_metrics", "目标指标库名")
	fs.StringVar(&dst.notifName, "dst-notifications-name", "admin_notifications", "目标通知库名")
	fs.StringVar(&dst.metricsDSN, "dst-metrics-dsn", "", "目标指标库 DSN（给出则覆盖 host/port/user/库名）")
	fs.StringVar(&dst.notifDSN, "dst-notifications-dsn", "", "目标通知库 DSN（给出则覆盖 host/port/user/库名）")
	dryRun := fs.Bool("dry-run", false, "只统计与校验，不写目标库")
	verify := fs.Bool("verify", false, "搬运后比对条数与抽样行，不一致即非 0 退出")
	sample := fs.Int("sample", 5, "每表首尾抽样条数")
	batch := fs.Int("batch", 500, "单条多行 INSERT 的行数")
	concurrency := fs.Int("concurrency", 2, "并发写事务数上限")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if dst.port < 1 || dst.port > 65535 {
		return fmt.Errorf("--dst-port 非法：%d", dst.port)
	}
	if *onlyDB != "all" && *onlyDB != "metrics" && *onlyDB != "notifications" {
		return fmt.Errorf("--db 只能是 metrics / notifications / all，当前为 %q", *onlyDB)
	}
	if *batch < 1 || *concurrency < 1 {
		return errors.New("--batch 与 --concurrency 必须为正整数")
	}

	dbs, err := buildLogicalDBs(*onlyDB, *srcMetrics, *srcNotifications, dst)
	if err != nil {
		return err
	}
	report, err := Run(context.Background(), dbs, Options{
		DryRun:      *dryRun,
		Verify:      *verify,
		Sample:      *sample,
		Batch:       *batch,
		Concurrency: *concurrency,
		Logf:        func(format string, a ...any) { fmt.Fprintf(out, format+"\n", a...) },
	})
	if err != nil {
		return err
	}
	fmt.Fprint(out, report.Summary())
	return nil
}

// buildLogicalDBs 按 --db 选择逻辑库并校验其源路径 / 目标 DSN 必填项。
func buildLogicalDBs(only, srcMetrics, srcNotifications string, dst dstFlags) ([]logicalDB, error) {
	type want struct {
		name    string
		srcPath string
		dstName string
		dstDSN  string
		tables  []tableSpec
		migrate func(context.Context, *sql.DB) error
	}
	all := []want{
		{
			name: "metrics", srcPath: strings.TrimSpace(srcMetrics),
			dstName: dst.metricsName, dstDSN: dst.metricsDSN,
			tables: metricsTables, migrate: mysqlDialect().MigrateMetrics,
		},
		{
			name: "notifications", srcPath: strings.TrimSpace(srcNotifications),
			dstName: dst.notifName, dstDSN: dst.notifDSN,
			tables: notificationTables, migrate: mysqlDialect().MigrateNotifications,
		},
	}
	var out []logicalDB
	for _, w := range all {
		if only != "all" && only != w.name {
			continue
		}
		if w.srcPath == "" {
			return nil, fmt.Errorf("--db %s 需要给出源库路径（--src-%s）", w.name, w.name)
		}
		dsn := w.dstDSN
		if dsn == "" {
			if strings.TrimSpace(dst.user) == "" {
				return nil, fmt.Errorf("缺少 --dst-user（或为 %s 给出 --dst-%s-dsn）", w.name, w.name)
			}
			dsn = mysql.BuildDSN(dst.host, dst.port, w.dstName, dst.user, dst.password)
		}
		out = append(out, logicalDB{
			name:    w.name,
			srcPath: w.srcPath,
			dstDSN:  dsn,
			tables:  w.tables,
			migrate: w.migrate,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--db %s 未匹配任何逻辑库", only)
	}
	return out, nil
}
