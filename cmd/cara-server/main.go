package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"NYCU-SDC/caravanserai/internal/appinit"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/apiserver"
	"NYCU-SDC/caravanserai/internal/server/controller"
	nodehandler "NYCU-SDC/caravanserai/internal/server/handler/node"
	projecthandler "NYCU-SDC/caravanserai/internal/server/handler/project"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
	"NYCU-SDC/caravanserai/internal/trace"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"go.uber.org/zap"
)

// Build-time variables injected by the Makefile via -ldflags.
var (
	AppName    = "cara-server"
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

	cfg, cfgLog := config.Load()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	logger, err := appinit.InitLogger(cfg.Debug, appMetadata)
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}

	cfgLog.FlushToZap(logger)
	logger.Info("Starting cara-server...")

	shutdownOtel, err := appinit.InitOpenTelemetry(AppName, Version, BuildTime, CommitHash, Env, cfg.OtelCollectorUrl)
	if err != nil {
		logger.Fatal("Failed to init OpenTelemetry", zap.Error(err))
	}

	// ============================================
	// Event Bus
	// ============================================

	eventBus := event.New(logger, 256)

	// ============================================
	// Store
	// ============================================

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pgStore, err := pgstore.New(ctx, cfg.DatabaseURL, logger, eventBus)
	if err != nil {
		logger.Fatal("Failed to connect to database", zap.Error(err))
	}
	defer pgStore.Close()

	// ============================================
	// Middleware
	// ============================================

	traceMiddleware := trace.NewMiddleware(logger, cfg.Debug)

	basicMiddleware := middleware.NewSet(traceMiddleware.RecoverMiddleware)
	//basicMiddleware = basicMiddleware.Append(traceMiddleware.TraceMiddleware)

	// ============================================
	// API Server
	// ============================================

	apiSrv := apiserver.New(logger, basicMiddleware)

	apiSrv.Register(nodehandler.NewHandler(logger, pgStore, pgStore))
	apiSrv.Register(projecthandler.NewHandler(logger, pgStore))

	// ============================================
	// Controller Manager
	// ============================================

	nodeAdapter := adapter.NewNodeStoreAdapter(pgStore)
	projectAdapter := adapter.NewProjectStoreAdapter(pgStore)

	ctrlManager := controller.NewManager(logger)

	ctrlManager.Add(controller.NewNodeHealthController(logger, nodeAdapter, eventBus))
	ctrlManager.Add(controller.NewProjectSchedulerController(logger,
		projectAdapter,
		adapter.NewNodeReadyAdapter(pgStore),
		eventBus,
	))
	ctrlManager.Add(controller.NewProjectTerminationController(logger,
		projectAdapter,
		eventBus,
	))
	ctrlManager.Add(controller.NewProjectReschedulerController(logger,
		projectAdapter,
		nodeAdapter,
		eventBus,
	))
	// TODO: ProjectGCController — handle spec.expireAt (post-MVP)
	// TODO: ProjectTimeoutController — reschedule Scheduled projects whose Agent goes silent (post-MVP)

	// ============================================
	// Run
	// ============================================

	// Start the Controller Manager in the background.
	go func() {
		if err := ctrlManager.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("Controller Manager stopped with error", zap.Error(err))
		}
	}()

	srv := &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: apiSrv.Handler(),
	}

	go func() {
		logger.Info("Listening", zap.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal("Server error", zap.Error(err))
		}
	}()

	<-ctx.Done()
	logger.Info("Shutting down gracefully...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("HTTP server forced to shutdown", zap.Error(err))
	}

	otelCtx, otelCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer otelCancel()
	if err := shutdownOtel(otelCtx); err != nil {
		logger.Error("OpenTelemetry forced to shutdown", zap.Error(err))
	}

	logger.Info("cara-server stopped")
}
