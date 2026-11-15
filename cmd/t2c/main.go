// Command t2c 启动 Telegarm2Chat 服务：装配依赖、起 HTTP 与指标端口、优雅关闭。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/api"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/auth"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/chat"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/config"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/metrics"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/realtime"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/store"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/internal/telegram"
	"cnb.cool/asoe/TechSauce/Telegarm2Chat/web"
)

// version 由构建时 -ldflags 注入。
var version = "dev"

func main() {
	showVersion := flag.Bool("version", false, "打印版本后退出")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "配置错误:", err)
		os.Exit(1)
	}
	setupLogger(cfg.LogLevel)
	api.Version = version

	if err := run(cfg); err != nil {
		slog.Error("服务退出", "err", err)
		os.Exit(1)
	}
}

func run(cfg *config.Config) error {
	// 用 signal.NotifyContext，收到 SIGINT/SIGTERM 后所有子组件同步收敛。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	hub := realtime.NewHub()
	svc := chat.New(st, hub)
	issuer := auth.NewTokenIssuer(cfg.Secret, cfg.TokenTTL)
	reg := metrics.New()

	svc.OnMessage(func(string) {
		reg.Counter("t2c_messages_total", "落库消息总数").Add(1)
	})
	reg.Gauge("t2c_realtime_online", "在线用户数", func() int64 { return int64(hub.OnlineCount()) })
	reg.Gauge("t2c_realtime_conns", "WebSocket 连接数", func() int64 { return int64(hub.ConnCount()) })

	var bridge *telegram.Bridge
	if cfg.TelegramEnabled {
		client := telegram.NewClient(cfg.TelegramToken, cfg.TelegramAPIBase)
		bridge = telegram.NewBridge(client, st, cfg.TelegramPoll, func(ctx context.Context, chatID int64, senderName, body string) error {
			_, err := svc.PostBridgeMessage(ctx, chatID, senderName, body)
			return err
		})
		svc.SetBridge(bridge)
		bridge.Start(ctx)
		slog.Info("telegram: 桥接已启用", "poll", cfg.TelegramPoll)
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			bridge.Stop(sctx)
		}()
	}

	srv := api.New(api.Options{
		Config:  cfg,
		Service: svc,
		Issuer:  issuer,
		Hub:     hub,
		Assets:  web.Handler(),
		Metrics: reg,
	})

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("http: 开始监听", "addr", cfg.Addr, "version", version)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http: 监听失败: %w", err)
		}
	}()

	// 指标端口独立监听回环，避免把 /metrics 暴露到公网。
	metricsAddr := metricsListenAddr(cfg.Addr)
	metricsSrv := &http.Server{
		Addr:              metricsAddr,
		Handler:           reg.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		slog.Info("metrics: 开始监听", "addr", metricsAddr)
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("metrics: 监听失败: %w", err)
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("收到退出信号，开始优雅关闭")
	}

	// 先关入口再收尾：顺序反了会让新请求落到正在拆除的连接上。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("http: 关闭超时", "err", err)
	}
	if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
		slog.Warn("metrics: 关闭超时", "err", err)
	}
	hub.Shutdown(shutdownCtx)
	slog.Info("已退出")
	return nil
}

// metricsListenAddr 把 ":8080" 换算成回环上的 "127.0.0.1:9080"。
func metricsListenAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "127.0.0.1:9080"
	}
	n, convErr := strconv.Atoi(port)
	if host != "" && convErr == nil {
		return net.JoinHostPort(host, strconv.Itoa(n+1000))
	}
	if convErr != nil {
		return "127.0.0.1:9080"
	}
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(n+1000))
}

func setupLogger(level string) {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv})))
}
