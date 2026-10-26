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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"docker-sync/internal/api"
	"docker-sync/internal/cas"
	"docker-sync/internal/scheduler"
	"docker-sync/internal/store"
	"docker-sync/internal/tlscert"
)

// version and commit are overridable at build time with
// -ldflags "-X main.version=0.1.2 -X main.commit=<sha>". When commit is empty it
// is taken from the VCS stamp that go build embeds automatically, so every
// binary reports the exact source revision it was built from instead of a
// version number that only changes when a release is cut.
var (
	version = "0.1.0"
	commit  = ""
)

// buildVersion renders the version string shown by the API and the UI, e.g.
// "0.1.0+ef8b472" or "0.1.2+1cdc758-dirty".
func buildVersion() string {
	c, dirty := commit, false
	if c == "" {
		if info, ok := debug.ReadBuildInfo(); ok {
			for _, setting := range info.Settings {
				switch setting.Key {
				case "vcs.revision":
					c = setting.Value
				case "vcs.modified":
					dirty = setting.Value == "true"
				}
			}
		}
	}
	if len(c) > 7 {
		c = c[:7]
	}
	out := version
	if c != "" {
		out += "+" + c
		if dirty {
			out += "-dirty"
		}
	}
	return out
}

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
	tlsMode := flags.String("tls", envOr("TLS_MODE", "auto"), "HTTPS 模式：auto/self-signed/files/off（auto = 有证书用证书，没有就自动生成自签；off 为明文 HTTP），也可用环境变量 TLS_MODE")
	tlsCert := flags.String("tls-cert", envOr("TLS_CERT", ""), "TLS 证书文件（与 -tls-key 成对提供即启用外部证书，默认模式 auto 下自动生效），也可用环境变量 TLS_CERT")
	tlsKey := flags.String("tls-key", envOr("TLS_KEY", ""), "TLS 私钥文件（与 -tls-cert 成对提供即启用外部证书，默认模式 auto 下自动生效），也可用环境变量 TLS_KEY")
	tlsHosts := flags.String("tls-host", envOr("TLS_HOSTS", ""), "追加到自签证书里的域名/IP，逗号分隔，也可用环境变量 TLS_HOSTS")
	tlsDir := flags.String("tls-dir", envOr("TLS_DIR", ""), "自签证书存放目录，默认 <data>/tls，也可用环境变量 TLS_DIR")
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
	certDir := strings.TrimSpace(*tlsDir)
	if certDir == "" {
		certDir = filepath.Join(absData, "tls")
	} else if certDir, err = filepath.Abs(certDir); err != nil {
		return fmt.Errorf("解析证书目录失败: %w", err)
	}
	tlsMaterial, err := buildTLS(*tlsMode, *tlsCert, *tlsKey, *tlsHosts, certDir, logger)
	if err != nil {
		return err
	}
	ver := buildVersion()
	scheme := "http"
	if tlsMaterial != nil {
		scheme = "https"
	}
	fmt.Fprintf(os.Stdout, "docker-sync v%s\n  listen: %s://%s\n  data:   %s\n", ver, scheme, *addr, absData)
	logger.Info("镜像同步服务启动",
		"version", ver,
		"addr", *addr,
		"dataDir", absData,
		"logLevel", level.String(),
		"scheme", scheme,
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
		TLS:       tlsMaterial,
		Version:   ver,
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
		if tlsMaterial != nil {
			serveErr <- httpServer.ListenAndServeTLS(tlsMaterial.CertFile, tlsMaterial.KeyFile)
			return
		}
		serveErr <- httpServer.ListenAndServe()
	}()
	logger.Info("HTTP 服务已监听", "addr", *addr, "scheme", scheme, "web", "/", "api", "/api")
	if tlsMaterial != nil && len(tlsMaterial.CAPEM) > 0 {
		logger.Info("内置自签证书已就绪：客户端下载 /api/mirror/ca.crt 并放到 /etc/docker/certs.d/<host:port>/ca.crt",
			"sans", strings.Join(tlsMaterial.SANs, ","),
			"notAfter", tlsMaterial.NotAfter.Format(time.RFC3339),
		)
	}

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

// buildTLS turns the -tls flags into the material the HTTP server listens with.
//
// 默认模式 auto：给了证书文件就按外部证书加载，没给就用内置自签证书；
// 想跑明文 HTTP 需要显式传 -tls off。返回 nil 表示继续用明文 HTTP。
func buildTLS(mode, certFile, keyFile, hosts, dir string, logger *slog.Logger) (*tlscert.Material, error) {
	certFile = strings.TrimSpace(certFile)
	keyFile = strings.TrimSpace(keyFile)

	switch mode = strings.ToLower(strings.TrimSpace(mode)); mode {
	case "off", "none", "http", "disabled":
		return nil, nil
	case "", "auto", "default":
		switch {
		case certFile == "" && keyFile == "":
			mode = tlsModeSelfSigned
		case certFile == "" || keyFile == "":
			return nil, fmt.Errorf("-tls-cert 与 -tls-key 需要成对提供（当前只给了其中一个）")
		default:
			mode = tlsModeFiles
		}
	case "self-signed", "selfsigned":
		mode = tlsModeSelfSigned
	case "files", "file", "custom":
		mode = tlsModeFiles
	default:
		return nil, fmt.Errorf("非法 TLS 模式 %q（可选 auto/off/self-signed/files）", mode)
	}

	switch mode {
	case tlsModeSelfSigned:
		m, err := tlscert.SelfSigned(tlscert.Options{Dir: dir, Hosts: splitList(hosts)})
		if err != nil {
			return nil, fmt.Errorf("初始化自签证书失败: %w", err)
		}
		logger.Info("已启用 HTTPS（内置自签证书）",
			"dir", dir,
			"sans", strings.Join(m.SANs, ","),
			"notAfter", m.NotAfter.Format(time.RFC3339),
			"ca", "/api/mirror/ca.crt",
		)
		return m, nil
	case tlsModeFiles:
		m, err := tlscert.FromFiles(certFile, keyFile)
		if err != nil {
			return nil, fmt.Errorf("加载 TLS 证书失败: %w", err)
		}
		logger.Info("已启用 HTTPS（外部证书）",
			"cert", certFile,
			"sans", strings.Join(m.SANs, ","),
			"notAfter", m.NotAfter.Format(time.RFC3339),
		)
		return m, nil
	}
	return nil, fmt.Errorf("非法 TLS 模式 %q", mode)
}

// TLS 模式取值（见 -tls flag）。
const (
	tlsModeSelfSigned = "self-signed"
	tlsModeFiles      = "files"
)

// splitList splits a comma separated flag value, dropping empty entries.
func splitList(raw string) []string {
	out := make([]string, 0, 4)
	for _, part := range strings.Split(raw, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
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
