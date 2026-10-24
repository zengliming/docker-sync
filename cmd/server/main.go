// Command server runs the docker-sync mirror synchronisation service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"docker-sync/internal/api"
	"docker-sync/internal/cas"
	"docker-sync/internal/scheduler"
	"docker-sync/internal/store"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "0.1.0"

const (
	defaultAddr     = ":8080"
	defaultDataDir  = "./data"
	defaultLogLevel = "info"
	shutdownWait    = 30 * time.Second
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "docker-sync: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	flags := flag.NewFlagSet("docker-sync", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	addr := flags.String("addr", envOr("ADDR", defaultAddr), "HTTP 监听地址，也可用环境变量 ADDR")
	dataDir := flags.String("data", envOr("DATA_DIR", defaultDataDir), "数据目录（状态、tar、blob 缓存），也可用环境变量 DATA_DIR")
	logLevel := flags.String("log-level", envOr("LOG_LEVEL", defaultLogLevel), "日志级别：debug/info/warn/error，也可用环境变量 LOG_LEVEL")
	if err := flags.Parse(args); err != nil {
		return err
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	startedAt := time.Now()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	log.SetOutput(os.Stdout)
	log.SetFlags(log.LstdFlags)

	absData, err := filepath.Abs(*dataDir)
	if err != nil {
		return fmt.Errorf("解析数据目录 %q 失败: %w", *dataDir, err)
	}
	fmt.Fprintf(os.Stdout, "docker-sync v%s\n  listen: %s\n  data:   %s\n", version, *addr, absData)
	logger.Info("镜像同步服务启动",
		"version", version,
		"addr", *addr,
		"dataDir", absData,
		"logLevel", level.String(),
		"go", runtime.Version(),
		"pid", os.Getpid(),
	)

	st, err := store.Open(store.Options{Dir: absData, Logger: logger})
	if err != nil {
		return fmt.Errorf("初始化状态存储失败: %w", err)
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			logger.Error("关闭状态存储失败", "error", cerr)
		}
	}()

	blobs, err := cas.New(filepath.Join(absData, "blobs"))
	if err != nil {
		return fmt.Errorf("初始化 blob 缓存失败: %w", err)
	}

	sched, err := scheduler.New(scheduler.Options{Store: st, Blobs: blobs, Logger: logger})
	if err != nil {
		return fmt.Errorf("初始化调度器失败: %w", err)
	}

	apiServer, err := api.New(api.Options{
		Store:     st,
		Scheduler: sched,
		Blobs:     blobs,
		Version:   version,
		StartedAt: startedAt,
		Logger:    logger,
	})
	if err != nil {
		return fmt.Errorf("初始化 HTTP 服务失败: %w", err)
	}
	defer apiServer.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := sched.Start(ctx); err != nil {
		return fmt.Errorf("启动调度器失败: %w", err)
	}

	httpServer := &http.Server{
		Addr:              *addr,
		Handler:           apiServer.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
	}()
	logger.Info("HTTP 服务已监听", "addr", *addr, "web", "/", "api", "/api")

	var runErr error
	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("HTTP 服务异常退出: %w", err)
		}
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
	}

	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownWait)
	defer cancel()

	// Closing the SSE hub first lets streaming handlers return so that the HTTP
	// shutdown below does not block on open event streams.
	apiServer.Close()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Warn("HTTP 服务关闭未在期限内完成", "error", err)
	}
	if err := sched.Shutdown(shutdownCtx); err != nil {
		logger.Warn("调度器关闭未在期限内完成", "error", err)
	}
	if runErr != nil {
		return runErr
	}
	logger.Info("服务已退出")
	return nil
}

// parseLevel maps the -log-level flag to a slog level.
func parseLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("非法日志级别 %q（可选 debug/info/warn/error）", raw)
	}
}

// envOr returns the environment variable value or def when unset/empty.
func envOr(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}
