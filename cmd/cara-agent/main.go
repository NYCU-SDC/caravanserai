package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"NYCU-SDC/caravanserai/internal/agent"
	agentapiserver "NYCU-SDC/caravanserai/internal/agent/apiserver"
	forwardhandler "NYCU-SDC/caravanserai/internal/agent/apiserver/handler/forward"
	logshandler "NYCU-SDC/caravanserai/internal/agent/apiserver/handler/logs"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/agent/proxy"
	"NYCU-SDC/caravanserai/internal/appinit"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/overlay"
	"NYCU-SDC/caravanserai/internal/trace"

	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.uber.org/zap"
)

// Build-time variables injected by the Makefile via -ldflags.
var (
	AppName    = "cara-agent"
	Version    = "dev"
	BuildTime  = "unknown"
	CommitHash = "unknown"
	Env        = "development"
)

func main() {
	if v := os.Getenv("APP_NAME"); v != "" {
		AppName = v
	}

	if BuildTime == "unknown" {
		BuildTime = "not provided (now: " + time.Now().Format(time.RFC3339) + ")"
	}

	if v := os.Getenv("ENV"); v != "" {
		Env = v
	}

	appMetadata := []zap.Field{
		zap.String("app_name", AppName),
		zap.String("version", Version),
		zap.String("build_time", BuildTime),
		zap.String("commit_hash", CommitHash),
		zap.String("environment", Env),
	}

	cfg, cfgLog := config.LoadAgent()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	logger, err := appinit.InitLogger(cfg.Debug, appMetadata)
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}

	cfgLog.FlushToZap(logger)
	logger.Info("Starting cara-agent...", zap.String("server", cfg.ServerURL))

	shutdownOtel, err := appinit.InitOpenTelemetry(AppName, Version, BuildTime, CommitHash, Env, cfg.OtelCollectorUrl)
	if err != nil {
		logger.Fatal("Failed to init OpenTelemetry", zap.Error(err))
	}

	// trace middleware is available for future HTTP callbacks from the server.
	_ = trace.NewMiddleware(logger, cfg.Debug)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	agentClient := agent.NewClient(logger, cfg.ServerURL, cfg.NodeName)

	// ── Headscale overlay join ────────────────────────────────────────────
	// Overlay networking is opt-in in 1.0 (CARA-55): the agent joins the
	// Headscale mesh only when both HeadscaleURL and PreauthKeyFile are
	// configured.  When they are unset the agent runs on the underlay as
	// before.  This is expected to become mandatory once the Headscale epic
	// (CARA-47) is complete.  A join failure is fatal — the agent must not
	// silently fall back to the underlay when overlay was requested.
	if cfg.HeadscaleURL != "" && cfg.PreauthKeyFile != "" {
		overlayHostname := cfg.OverlayHostname
		if overlayHostname == "" {
			overlayHostname = cfg.NodeName
		}

		overlayClient, err := overlay.NewTsnetClient(overlay.TsnetConfig{
			ControlURL:     cfg.HeadscaleURL,
			PreauthKeyFile: cfg.PreauthKeyFile,
			Hostname:       overlayHostname,
		}, logger)
		if err != nil {
			logger.Fatal("Invalid overlay configuration", zap.Error(err))
		}
		defer func() {
			if closeErr := overlayClient.Close(); closeErr != nil {
				logger.Warn("Failed to leave overlay network", zap.Error(closeErr))
			}
		}()

		logger.Info("Joining Headscale overlay...", zap.String("control_url", cfg.HeadscaleURL))
		result, err := overlay.JoinWithRetry(ctx, overlayClient, logger)
		if err != nil {
			logger.Fatal("Failed to join Headscale overlay", zap.Error(err))
		}
		agentClient.SetOverlayIP(result.OverlayIP)
		logger.Info("Joined Headscale overlay",
			zap.String("overlay_ip", result.OverlayIP),
			zap.String("dns_name", result.DNSName),
		)
	} else {
		logger.Info("Overlay networking disabled (headscale_url/preauth_key_file not set), running on underlay")
	}

	dockerRuntime, err := docker.NewDockerRuntime(cfg.DockerHost, logger)
	if err != nil {
		logger.Fatal("Failed to create Docker runtime", zap.Error(err))
	}
	defer func() {
		if closeErr := dockerRuntime.Close(); closeErr != nil {
			logger.Warn("Failed to close Docker runtime", zap.Error(closeErr))
		}
	}()

	// Parse agent port for heartbeat reporting.
	agentPort, err := strconv.Atoi(cfg.ListenPort)
	if err != nil {
		logger.Fatal("Invalid agent port", zap.String("port", cfg.ListenPort), zap.Error(err))
	}

	// ── Ingress reverse proxy ───────────────────────────────────────────
	routeTable := proxy.NewRouteTable(logger)
	proxyServer := proxy.NewServer(logger, cfg.ProxyListenAddr, routeTable)

	go func() {
		if proxyErr := proxyServer.ListenAndServe(); proxyErr != nil {
			logger.Fatal("Proxy server failed", zap.Error(proxyErr))
		}
	}()

	go agent.Run(ctx, agentClient, dockerRuntime, cfg.HeartbeatInterval, agentPort, cfg.AdvertiseIP, routeTable, logger)

	// ── Agent HTTP server ────────────────────────────────────────────────
	apiSrv := agentapiserver.New(logger)

	forwardPW := problem.NewWithMapping(forwardhandler.NewProblemMapping())
	apiSrv.Register(forwardhandler.NewHandler(logger, dockerRuntime, forwardPW))

	logsPW := problem.NewWithMapping(logshandler.NewProblemMapping())
	apiSrv.Register(logshandler.NewHandler(logger, dockerRuntime, logsPW))

	httpServer := &http.Server{
		Addr:    net.JoinHostPort("0.0.0.0", cfg.ListenPort),
		Handler: apiSrv.Handler(),
	}

	go func() {
		logger.Info("Agent HTTP server listening", zap.String("addr", httpServer.Addr))
		if srvErr := httpServer.ListenAndServe(); srvErr != nil && srvErr != http.ErrServerClosed {
			logger.Fatal("Agent HTTP server failed", zap.Error(srvErr))
		}
	}()

	logger.Info("Agent running, waiting for shutdown signal...")

	<-ctx.Done()
	logger.Info("Shutting down cara-agent...")

	// Gracefully shut down the proxy server.
	proxyShutdownCtx, proxyShutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer proxyShutdownCancel()
	if err := proxyServer.Shutdown(proxyShutdownCtx); err != nil {
		logger.Error("Proxy server forced to shutdown", zap.Error(err))
	}

	// Gracefully shut down the HTTP server.
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Agent HTTP server forced to shutdown", zap.Error(err))
	}

	otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otelCancel()
	if err := shutdownOtel(otelCtx); err != nil {
		logger.Error("OpenTelemetry forced to shutdown", zap.Error(err))
	}

	logger.Info("cara-agent stopped")
}
