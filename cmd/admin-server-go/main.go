// Command admin-server-go 是个人网站 Admin 后端的 Go 重写版。
//
// 运行形态：admin-server-go [serve]，起 HTTP 服务（默认，监听 127.0.0.1:19265）。
// 存储一律 MySQL（无方言开关）；旧 SQLite 数据由独立工具 tools/migrate-sqlite-to-mysql 一次性搬运。
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"admin-server-go/internal/apps"
	"admin-server-go/internal/config"
	"admin-server-go/internal/files"
	"admin-server-go/internal/httpapi"
	"admin-server-go/internal/metrics"
	"admin-server-go/internal/mysql"
	"admin-server-go/internal/notifications"
	"admin-server-go/internal/probe"
	"admin-server-go/internal/shares"
	"admin-server-go/internal/state"
	"admin-server-go/internal/store"
)

func main() {
	if err := run(os.Args[1:], os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "启动失败：%v\n", err)
		os.Exit(1)
	}
}

// run 解析子命令并分派；get 便于测试注入环境变量。
func run(args []string, get func(string) string) error {
	logger := newLogger()
	if len(args) > 0 {
		switch args[0] {
		case "serve":
			if len(args) > 1 {
				return fmt.Errorf("serve 不接受额外参数：%v", args[1:])
			}
		default:
			return fmt.Errorf("未知参数 %q（可用：serve）", args[0])
		}
	}
	cfg, err := config.Load(get)
	if err != nil {
		return err
	}
	return serve(cfg, logger)
}

func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// serve 开库 → 装配服务 → listen → 起后台任务 → 等退出信号。
func serve(cfg *config.Config, logger *slog.Logger) error {
	ctx := context.Background()

	files_, generatedPassword, _, err := config.LoadFile(cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("加载 config.json 失败: %w", err)
	}
	if generatedPassword != "" {
		// 只在此处提示一次，不落日志文件、不进审计、接口不回显。
		logger.Info("已生成初始管理口令（仅本次显示）", "admin_password", generatedPassword)
	}

	redis, err := state.New(ctx, cfg.RedisURL)
	if err != nil {
		return err
	}
	defer func() { _ = redis.Close() }()

	probeSvc := probe.New(cfg, logger)

	metricsDB, err := openDB(ctx, cfg, "metrics")
	if err != nil {
		// 旧实现：metrics.db 打不开时降级为「长期留样停用」，不影响进程启动。
		logger.Warn("指标库不可用，长期留样停用", "err", err.Error())
		metricsDB = nil
	}
	if metricsDB != nil {
		defer func() { _ = metricsDB.Close() }()
	}
	notifDB, err := openDB(ctx, cfg, "notifications")
	if err != nil {
		logger.Warn("通知库不可用，通知接口降级为 503", "err", err.Error())
		notifDB = nil
	}
	if notifDB != nil {
		defer func() { _ = notifDB.Close() }()
	}

	metricsSvc := metrics.New(metrics.Options{Cfg: cfg, DB: metricsDB, Log: logger, Probe: probeSvc})
	appsSvc := apps.New(apps.Options{Cfg: cfg, Log: logger, Probe: probeSvc})
	notifSvc := notifications.New(notifications.Options{Cfg: cfg, DB: notifDB, Log: logger})
	fileSvc := files.New(files.Options{Cfg: cfg, Log: logger})
	shareSvc := shares.New(shares.Options{Cfg: cfg, Files: fileSvc, Log: logger})

	handler := httpapi.New(httpapi.Options{
		Cfg:     cfg,
		Files:   files_,
		Redis:   redis,
		Log:     logger,
		Metrics: metricsSvc,
		Apps:    appsSvc,
		Notifs:  notifSvc,
		FileSvc: fileSvc,
		Shares:  shareSvc,
	})

	server := &http.Server{
		Addr:              net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	logger.Info("admin-server-go 已启动: http://" + server.Addr + " | " + cfg.Describe())

	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP 服务错误", "err", err.Error())
		}
	}()

	// 后台任务在 listen 成功后启动（对齐旧实现 index.ts 的 listen 回调）。
	metricsSvc.StartSampler()
	notifSvc.StartMaintenance()

	return waitForShutdown(server, metricsSvc, logger)
}

// openDB 打开一个逻辑库（metrics / notifications）并跑增量迁移。
// 库名由 logical 选定：metrics → DB_METRICS_NAME，其余 → DB_NOTIFICATIONS_NAME。
func openDB(ctx context.Context, cfg *config.Config, logical string) (*store.DB, error) {
	dialect := mysql.New()
	name := cfg.DBMetricsName
	migrateFn := dialect.MigrateMetrics
	if logical == "notifications" {
		name = cfg.DBNotificationsName
		migrateFn = dialect.MigrateNotifications
	}
	dsn := mysql.BuildDSN(cfg.DBHost, cfg.DBPort, name, cfg.DBUser, cfg.DBPassword)
	return store.Open(ctx, dialect, dsn, 16, migrateFn)
}

// waitForShutdown 优雅退出：停采样器 → 关 HTTP。
func waitForShutdown(server *http.Server, metricsSvc *metrics.Service, logger *slog.Logger) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	sig := <-signals
	logger.Info("收到退出信号，正在停止", "signal", sig.String())

	if metricsSvc != nil {
		metricsSvc.Stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP 关闭超时", "err", err.Error())
	}
	return nil
}
