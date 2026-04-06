// Package proxy implements an HTTP reverse proxy for Caravanserai projects.
//
// The proxy routes incoming HTTP requests to containers based on the Host
// header, using ingress rules defined in each project's spec. Routes are
// maintained by a thread-safe RouteTable that maps hostnames to backend URLs.
package proxy

import (
	"fmt"
	"strings"
	"sync"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"go.uber.org/zap"
)

// RouteTable is a thread-safe mapping of hostnames to backend URLs.
// It tracks which routes belong to which project so they can be removed
// atomically when a project is terminated.
type RouteTable struct {
	mu sync.RWMutex

	// routes maps resolved hostname → backend URL (e.g. "http://172.18.0.3:8080").
	routes map[string]string

	// projectRoutes maps projectName → list of hostnames owned by that project.
	// Used by Remove to clean up all routes for a project.
	projectRoutes map[string][]string

	logger *zap.Logger
}

// NewRouteTable creates an empty RouteTable.
func NewRouteTable(logger *zap.Logger) *RouteTable {
	return &RouteTable{
		routes:        make(map[string]string),
		projectRoutes: make(map[string][]string),
		logger:        logger,
	}
}

// Update builds routes from a project's ingress definitions and the discovered
// container IPs. Any existing routes for the project are replaced atomically.
//
// containerIPs maps service name → IP address on the project bridge network.
func (rt *RouteTable) Update(project *v1.Project, containerIPs map[string]string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	// Remove old routes for this project first.
	rt.removeLockedUnsafe(project.Name)

	var hosts []string
	for _, ing := range project.Spec.Ingress {
		ip, ok := containerIPs[ing.Target.Service]
		if !ok {
			rt.logger.Warn("Ingress target service has no container IP, skipping route",
				zap.String("project", project.Name),
				zap.String("ingress", ing.Name),
				zap.String("service", ing.Target.Service),
			)
			continue
		}

		host := ResolveHost(ing.Host, project.Name)
		backend := fmt.Sprintf("http://%s:%d", ip, ing.Target.Port)

		rt.routes[host] = backend
		hosts = append(hosts, host)

		rt.logger.Info("Route added",
			zap.String("host", host),
			zap.String("backend", backend),
			zap.String("project", project.Name),
		)
	}

	if len(hosts) > 0 {
		rt.projectRoutes[project.Name] = hosts
	}
}

// Remove deletes all routes belonging to the named project.
func (rt *RouteTable) Remove(projectName string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	rt.removeLockedUnsafe(projectName)
}

// removeLockedUnsafe removes all routes for a project.
// Caller must hold rt.mu in write mode.
func (rt *RouteTable) removeLockedUnsafe(projectName string) {
	hosts, ok := rt.projectRoutes[projectName]
	if !ok {
		return
	}

	for _, h := range hosts {
		delete(rt.routes, h)
		rt.logger.Info("Route removed",
			zap.String("host", h),
			zap.String("project", projectName),
		)
	}
	delete(rt.projectRoutes, projectName)
}

// Lookup returns the backend URL for the given hostname.
func (rt *RouteTable) Lookup(host string) (backendURL string, found bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	// Strip port from Host header if present (e.g. "example.com:8081" → "example.com").
	if idx := strings.LastIndex(host, ":"); idx != -1 {
		host = host[:idx]
	}

	url, ok := rt.routes[host]
	return url, ok
}

// Routes returns a copy of all current routes for debugging/logging.
func (rt *RouteTable) Routes() map[string]string {
	rt.mu.RLock()
	defer rt.mu.RUnlock()

	copy := make(map[string]string, len(rt.routes))
	for k, v := range rt.routes {
		copy[k] = v
	}
	return copy
}

// ResolveHost applies the Phase 1 host resolution rules:
//   - If host contains a dot → use as-is (e.g. "blog.example.com")
//   - If host does not contain a dot → assemble as "{host}.{projectName}.local"
//
// A future ticket (CARA-17) will implement the full PRD rule:
// {host}.{environment}.{baseDomain}.
func ResolveHost(host, projectName string) string {
	if strings.Contains(host, ".") {
		return host
	}
	return fmt.Sprintf("%s.%s.local", host, projectName)
}
