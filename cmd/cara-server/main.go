package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"NYCU-SDC/caravanserai/internal/appinit"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/overlay"
	"NYCU-SDC/caravanserai/internal/server/adapter"
	"NYCU-SDC/caravanserai/internal/server/agentdialer"
	"NYCU-SDC/caravanserai/internal/server/apiserver"
	"NYCU-SDC/caravanserai/internal/server/controller"
	"NYCU-SDC/caravanserai/internal/server/handler"
	nodehandler "NYCU-SDC/caravanserai/internal/server/handler/node"
	projecthandler "NYCU-SDC/caravanserai/internal/server/handler/project"
	secrethandler "NYCU-SDC/caravanserai/internal/server/handler/secret"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
	"NYCU-SDC/caravanserai/internal/trace"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
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
	basicMiddleware = basicMiddleware.Append(traceMiddleware.TraceMiddleware)

	// ============================================
	// API Server
	// ============================================

	apiSrv := apiserver.New(logger, basicMiddleware)

	// ── Headscale overlay join ────────────────────────────────────────────
	// Overlay networking is opt-in in 1.0 (CARA-74): cara-server joins the
	// Headscale mesh only when both HeadscaleURL and PreauthKeyFile are
	// configured.  Joining is what gives the server a route into the overlay
	// CGNAT range (100.64.0.0/10) so it can reach NAT-ed agents.  A join
	// failure is fatal — the server must not silently fall back to a transport
	// that cannot reach the overlay when overlay was requested.
	var overlayTransport http.RoundTripper
	if cfg.HeadscaleURL != "" && cfg.PreauthKeyFile != "" {
		overlayHostname := cfg.OverlayHostname
		if overlayHostname == "" {
			overlayHostname = "cara-server"
		}

		stateDir := cfg.OverlayStateDir
		if stateDir == "" {
			// Use a server-specific directory so tsnet state never collides
			// with a co-located cara-agent (whose default lives under
			// "cara-agent").
			base, err := os.UserConfigDir()
			if err != nil {
				logger.Fatal("Cannot determine overlay state directory", zap.Error(err))
			}
			stateDir = filepath.Join(base, "cara-server", "tsnet")
		}

		overlayClient, err := overlay.NewTsnetClient(overlay.TsnetConfig{
			ControlURL:     cfg.HeadscaleURL,
			PreauthKeyFile: cfg.PreauthKeyFile,
			Hostname:       overlayHostname,
			StateDir:       stateDir,
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
		overlayTransport = overlayClient.HTTPTransport()
		logger.Info("Joined Headscale overlay",
			zap.String("overlay_ip", result.OverlayIP),
			zap.String("dns_name", result.DNSName),
		)
	} else {
		logger.Info("Overlay networking disabled (headscale_url/preauth_key_file not set), using default transport")
	}

	// agentDialer is the single place cara-server resolves a Node name into a
	// dial-able HTTP endpoint for its agent.  When overlay is enabled it dials
	// through the tsnet-backed transport (reaching the agent's overlay IP);
	// otherwise Transport is nil and net/http.DefaultTransport is used.
	agentDialer := agentdialer.New(agentdialer.Config{
		Nodes:     pgStore,
		Transport: overlayTransport,
	})

	problemWriter := problem.NewWithMapping(handler.NewProblemMapping())
	apiSrv.Register(nodehandler.NewHandler(logger, pgStore, pgStore, agentDialer, problemWriter))
	apiSrv.Register(projecthandler.NewHandler(logger, pgStore, problemWriter))
	apiSrv.Register(secrethandler.NewHandler(logger, pgStore, problemWriter))

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
