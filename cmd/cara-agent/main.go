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
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/appinit"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/trace"

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

	go agent.Run(ctx, agentClient, dockerRuntime, cfg.HeartbeatInterval, agentPort, logger)

	// ── Agent HTTP server ────────────────────────────────────────────────
	apiSrv := agentapiserver.New(logger)
	inspector := forwardhandler.NewDockerInspector(dockerRuntime)
	apiSrv.Register(forwardhandler.NewHandler(logger, inspector))

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
