package main

import (
	"NYCU-SDC/caravanserai/internal/agent"
	"NYCU-SDC/caravanserai/internal/agent/docker"
	"NYCU-SDC/caravanserai/internal/appinit"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/trace"
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

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

	go agent.Run(ctx, agentClient, dockerRuntime, cfg.HeartbeatInterval, logger)

	logger.Info("Agent running, waiting for shutdown signal...")

	<-ctx.Done()
	logger.Info("Shutting down cara-agent...")

	otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otelCancel()
	if err := shutdownOtel(otelCtx); err != nil {
		logger.Error("OpenTelemetry forced to shutdown", zap.Error(err))
	}

	logger.Info("cara-agent stopped")
}
