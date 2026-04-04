package main

import (
	"NYCU-SDC/caravanserai/internal/appinit"
	"NYCU-SDC/caravanserai/internal/config"
	"NYCU-SDC/caravanserai/internal/event"
	"NYCU-SDC/caravanserai/internal/server/apiserver"
	"NYCU-SDC/caravanserai/internal/server/controller"
	nodehandler "NYCU-SDC/caravanserai/internal/server/handler/node"
	projecthandler "NYCU-SDC/caravanserai/internal/server/handler/project"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
	"NYCU-SDC/caravanserai/internal/trace"
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

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

// ============================================================
// Store adapters
//
// The controller narrow interfaces (NodeStore, SchedulerProjectStore, etc.)
// use controller-local types (controller.NodeState, controller.ProjectPhase).
// The postgres.Store uses api/v1 types. The adapters below bridge the two
// without introducing a circular import: main.go can import both packages
// freely.
// ============================================================

// nodeStoreAdapter wraps *pgstore.Store and satisfies controller.NodeStore and
// controller.ReschedulerNodeStore.
type nodeStoreAdapter struct {
	s *pgstore.Store
}

func (a *nodeStoreAdapter) ListNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(nodes))
	for i, n := range nodes {
		names[i] = n.Name
	}
	return names, nil
}

func (a *nodeStoreAdapter) GetNodeStatus(ctx context.Context, name string) (controller.NodeStatusSnapshot, error) {
	node, err := a.s.GetNode(ctx, name)
	if err != nil {
		return controller.NodeStatusSnapshot{}, err
	}
	return controller.NodeStatusSnapshot{
		LastHeartbeat: node.Status.LastHeartbeat,
		State:         controller.NodeState(node.Status.State),
	}, nil
}

func (a *nodeStoreAdapter) SetNodeState(ctx context.Context, name string, state controller.NodeState, reason, message string) error {
	node, err := a.s.GetNode(ctx, name)
	if err != nil {
		return err
	}
	node.Status.State = v1.NodeState(state)
	// Update or append the Ready condition.
	condType := "Ready"
	condStatus := v1.ConditionTrue
	if state != controller.NodeStateReady {
		condStatus = v1.ConditionFalse
	}
	now := time.Now().UTC()
	updated := false
	for i, c := range node.Status.Conditions {
		if c.Type == condType {
			node.Status.Conditions[i] = v1.Condition{
				Type:               condType,
				Status:             condStatus,
				Reason:             reason,
				Message:            message,
				LastTransitionTime: now,
			}
			updated = true
			break
		}
	}
	if !updated {
		node.Status.Conditions = append(node.Status.Conditions, v1.Condition{
			Type:               condType,
			Status:             condStatus,
			Reason:             reason,
			Message:            message,
			LastTransitionTime: now,
		})
	}
	return a.s.UpdateNodeStatus(ctx, name, node.Status)
}

// ListNotReadyNodeNames satisfies controller.ReschedulerNodeStore.
func (a *nodeStoreAdapter) ListNotReadyNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range nodes {
		if n.Status.State == v1.NodeStateNotReady {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

// projectStoreAdapter wraps *pgstore.Store and satisfies
// controller.SchedulerProjectStore, controller.TerminationProjectStore, and
// controller.ReschedulerProjectStore.
type projectStoreAdapter struct {
	s *pgstore.Store
}

func (a *projectStoreAdapter) ListProjectNamesByPhase(ctx context.Context, phase controller.ProjectPhase) ([]string, error) {
	projects, err := a.s.ListProjectsByPhase(ctx, v1.ProjectPhase(phase))
	if err != nil {
		return nil, err
	}
	names := make([]string, len(projects))
	for i, p := range projects {
		names[i] = p.Name
	}
	return names, nil
}

func (a *projectStoreAdapter) GetProjectPhase(ctx context.Context, name string) (controller.ProjectPhase, string, error) {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return "", "", err
	}
	return controller.ProjectPhase(project.Status.Phase), project.Status.NodeRef, nil
}

func (a *projectStoreAdapter) SetProjectScheduled(ctx context.Context, name, nodeRef string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhaseScheduled
	project.Status.NodeRef = nodeRef
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

func (a *projectStoreAdapter) SetProjectPhase(ctx context.Context, name string, phase controller.ProjectPhase, reason, message string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhase(phase)
	// Update or append a phase condition.
	condType := "Phase"
	now := time.Now().UTC()
	cond := v1.Condition{
		Type:               condType,
		Status:             "True",
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == condType {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

func (a *projectStoreAdapter) DeleteProject(ctx context.Context, name string) error {
	return a.s.DeleteProject(ctx, name)
}

// ListProjectsByNodeRef satisfies controller.ReschedulerProjectStore.
// It converts api/v1 Projects into controller.ProjectSnapshot values.
func (a *projectStoreAdapter) ListProjectsByNodeRef(ctx context.Context, nodeRef string, phases []controller.ProjectPhase) ([]*controller.ProjectSnapshot, error) {
	v1Phases := make([]v1.ProjectPhase, len(phases))
	for i, p := range phases {
		v1Phases[i] = v1.ProjectPhase(p)
	}
	projects, err := a.s.ListProjectsByNodeRef(ctx, nodeRef, v1Phases)
	if err != nil {
		return nil, err
	}
	snapshots := make([]*controller.ProjectSnapshot, len(projects))
	for i, p := range projects {
		conditions := make([]controller.ConditionSnapshot, len(p.Status.Conditions))
		for j, c := range p.Status.Conditions {
			conditions[j] = controller.ConditionSnapshot{
				Type:               c.Type,
				LastTransitionTime: c.LastTransitionTime,
			}
		}
		snapshots[i] = &controller.ProjectSnapshot{
			Name:       p.Name,
			Phase:      controller.ProjectPhase(p.Status.Phase),
			NodeRef:    p.Status.NodeRef,
			Conditions: conditions,
		}
	}
	return snapshots, nil
}

// SetProjectPending satisfies controller.ReschedulerProjectStore.
// Clears nodeRef, sets phase=Pending, and records a Phase condition with
// reason=NodeNotReady.
func (a *projectStoreAdapter) SetProjectPending(ctx context.Context, name string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhasePending
	project.Status.NodeRef = ""
	now := time.Now().UTC()
	cond := v1.Condition{
		Type:               "Phase",
		Status:             v1.ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady; project reset to Pending for rescheduling",
		LastTransitionTime: now,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == "Phase" {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

// SetTerminatingAt satisfies controller.ReschedulerProjectStore.
// Writes (or replaces) the TerminatingAt condition to record the time at which
// the rescheduler first observed this project as stranded on a NotReady node.
func (a *projectStoreAdapter) SetTerminatingAt(ctx context.Context, name string, at time.Time) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	cond := v1.Condition{
		Type:               "TerminatingAt",
		Status:             v1.ConditionTrue,
		Reason:             "NodeNotReady",
		Message:            "Node went NotReady while project was Terminating; force-termination timeout clock started",
		LastTransitionTime: at,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == "TerminatingAt" {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

// ForceTerminated satisfies controller.ReschedulerProjectStore.
// Transitions the project to Terminated and records a Phase condition with
// reason=TerminationTimeout.
func (a *projectStoreAdapter) ForceTerminated(ctx context.Context, name string) error {
	project, err := a.s.GetProject(ctx, name)
	if err != nil {
		return err
	}
	project.Status.Phase = v1.ProjectPhaseTerminated
	now := time.Now().UTC()
	cond := v1.Condition{
		Type:               "Phase",
		Status:             v1.ConditionTrue,
		Reason:             "TerminationTimeout",
		Message:            "Node was NotReady for too long; project force-terminated. Docker resources on the node may need manual cleanup.",
		LastTransitionTime: now,
	}
	updated := false
	for i, c := range project.Status.Conditions {
		if c.Type == "Phase" {
			project.Status.Conditions[i] = cond
			updated = true
			break
		}
	}
	if !updated {
		project.Status.Conditions = append(project.Status.Conditions, cond)
	}
	return a.s.UpdateProjectStatus(ctx, name, project.Status)
}

// nodeReadyAdapter wraps *pgstore.Store and satisfies controller.SchedulerNodeStore.
type nodeReadyAdapter struct {
	s *pgstore.Store
}

func (a *nodeReadyAdapter) ListReadyNodeNames(ctx context.Context) ([]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range nodes {
		if n.Status.State == v1.NodeStateReady && !n.Spec.Unschedulable {
			names = append(names, n.Name)
		}
	}
	return names, nil
}

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

	nodeAdapter := &nodeStoreAdapter{pgStore}
	projectAdapter := &projectStoreAdapter{pgStore}

	ctrlManager := controller.NewManager(logger)

	ctrlManager.Add(controller.NewNodeHealthController(logger, nodeAdapter, eventBus))
	ctrlManager.Add(controller.NewProjectSchedulerController(logger,
		projectAdapter,
		&nodeReadyAdapter{pgStore},
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
