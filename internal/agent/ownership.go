package agent

import (
	"context"
	"errors"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agent/backup"
	"NYCU-SDC/caravanserai/internal/agent/docker"

	"go.uber.org/zap"
)

// routeRefresher re-points the ingress proxy at a Project's containers after
// a backup restarts them. Restarted containers get new bridge IPs, so without
// this the proxy serves 502s until the next poll happens to refresh them.
type routeRefresher struct {
	runtime docker.Runtime
	routes  RouteUpdater
	logger  *zap.Logger
}

// NewRouteRefresher returns a backup.RouteRefresher that updates routes via
// the same path the poll loop uses.
func NewRouteRefresher(runtime docker.Runtime, routes RouteUpdater, logger *zap.Logger) backup.RouteRefresher {
	return &routeRefresher{runtime: runtime, routes: routes, logger: logger}
}

func (r *routeRefresher) Refresh(ctx context.Context, project *v1.Project) {
	updateProxyRoutes(ctx, r.runtime, r.routes, project,
		r.logger.With(zap.String("project", project.Name)))
}

// ownershipResolver answers "should this node still be running the Project?"
// by re-reading it from the server. The backup flow asks after its containers
// have been stopped, because the answer can change while they are down.
type ownershipResolver struct {
	client   *Client
	nodeName string
	logger   *zap.Logger
}

// NewOwnershipResolver returns a backup.OwnershipResolver backed by the
// control-plane API.
func NewOwnershipResolver(client *Client, nodeName string, logger *zap.Logger) backup.OwnershipResolver {
	return &ownershipResolver{client: client, nodeName: nodeName, logger: logger}
}

// Resolve classifies the Project's current assignment.
//
// The distinction that matters most is between "the Project is gone" and "the
// server could not be reached". Both fail the fetch, but the first means this
// node must not restart the containers while the second means it should — see
// backup.ShouldRestartContainers.
func (r *ownershipResolver) Resolve(ctx context.Context, key backup.ResourceKey) backup.Ownership {
	project, err := r.client.GetProject(ctx, key.Name)
	if err != nil {
		if errors.Is(err, ErrProjectNotFound) {
			return backup.OwnershipLost
		}
		r.logger.Warn("Could not determine project ownership",
			zap.String("project", key.String()), zap.Error(err))
		return backup.OwnershipUnknown
	}

	switch project.Status.Phase {
	case v1.ProjectPhaseTerminating, v1.ProjectPhaseTerminated:
		// Teardown is in progress or done; restarting would resurrect
		// containers the control plane is trying to remove.
		return backup.OwnershipTerminating
	}

	switch project.Status.NodeRef {
	case r.nodeName:
		return backup.OwnershipRetained
	case "":
		// Unassigned: the Project was reset to Pending by a drain or
		// reschedule and is awaiting placement. It is no longer ours.
		return backup.OwnershipLost
	default:
		return backup.OwnershipReassigned
	}
}
