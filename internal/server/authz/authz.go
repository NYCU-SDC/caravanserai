// Package authz decides what an authenticated caller may do (CARA-140).
//
// Users are admins until roles land (ADR-0031 phase 4) and pass through. A
// Node may call only the routes in nodeRoutes, and only about its own work,
// derived from status.nodeRef the way Kubernetes Node authorization derives it
// from spec.nodeName (docs/plan_docs/authorization-model.md).
//
// Three parts enforce it:
//
//   - Wrap puts a deny-by-default allow-list in front of the whole mux, so a
//     route nobody added here stays closed to Nodes.
//   - The rules in this file check path values and read the store.
//   - Writes that carry an assignment fence are checked by RequireFenceOwner,
//     which the project handler calls where it decodes the fence; the store
//     then validates the fence atomically, as before.
package authz

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

// ErrForbidden is what a refused Node request wraps. The handler package maps
// its own ErrForbidden to 403; Wrap writes the response itself.
var ErrForbidden = errors.New("forbidden")

// ProjectReader is the part of the store the rules read. Every rule reads it
// live: the Secret rule and the Project rules must agree, and a cache on one
// side is how an Agent would see a Project but be refused its Secret.
type ProjectReader interface {
	GetProject(ctx context.Context, name string) (*v1.Project, error)
	ListProjects(ctx context.Context) ([]*v1.Project, error)
}

// AssignedTo reports whether p is assigned to node. It is the one predicate
// behind the list filter, the Project read rule and the Secret rule.
func AssignedTo(p *v1.Project, node string) bool {
	return p.Status.NodeRef == node
}

// NodeMayRead reports whether node may read p: it holds the assignment, or it
// still owes cleanup for one it gave up.
func NodeMayRead(p *v1.Project, node string) bool {
	return AssignedTo(p, node) || len(p.CleanupTasksFor(node)) > 0
}

// references reports whether any service in p takes a value from secret.
func references(p *v1.Project, secret string) bool {
	for _, svc := range p.Spec.Services {
		for _, env := range svc.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == secret {
				return true
			}
		}
	}
	return false
}

// decision is what a rule returns.
type decision int

const (
	allow decision = iota
	// deny answers 403.
	deny
	// hide answers 404: the request is about something the Node has no
	// business knowing exists. Used for Project reads, because every Agent
	// path reads a 404 as "not mine, do not run it", while any other error
	// reads as "unknown" and the backup flow then restarts the containers.
	hide
)

// rule decides one route for a Node. vars holds the route's path values. It
// may rewrite r (the list filter does).
type rule func(a *Authorizer, r *http.Request, node string, vars map[string]string) (decision, string, error)

// nodeRoutes is every route a Node may call, keyed by the pattern the route is
// registered under. A route missing here is refused to Nodes with 403.
var nodeRoutes = map[string]rule{
	// The handler checks that the registered name is the caller's (CARA-138).
	"POST /api/v1/nodes": allowRoute,

	"POST /api/v1/nodes/{name}/heartbeat":          ownNode,
	"GET /api/v1/nodes/{name}/cleanup-obligations": ownNode,

	"GET /api/v1/projects":        ownProjectList,
	"GET /api/v1/projects/{name}": readableProject,

	// Fenced writes: RequireFenceOwner checks the fence in the handler.
	"PATCH /api/v1/projects/{name}/status":                          allowRoute,
	"PUT /api/v1/projects/{name}/status/spec-applied":               allowRoute,
	"PATCH /api/v1/projects/{name}/conditions/{type}":               allowRoute,
	"DELETE /api/v1/projects/{name}/conditions/{type}":              allowRoute,
	"PUT /api/v1/projects/{name}/recovery/workload-stopped":         allowRoute,
	"PUT /api/v1/projects/{name}/recovery/resources-removed":        allowRoute,
	"PUT /api/v1/projects/{name}/recovery/committed-backup":         allowRoute,
	"PUT /api/v1/projects/{name}/recovery/managed-data-quarantined": allowRoute,
	"PUT /api/v1/projects/{name}/backup/stanzas":                    allowRoute,
	"PUT /api/v1/projects/{name}/backup/completions/{backupID}":     allowRoute,

	"GET /api/v1/secrets/{name}": usedSecret,
}

// NodeRoutes returns the patterns of the routes a Node may call, so a test can
// check each is still registered.
func NodeRoutes() []string {
	out := make([]string, 0, len(nodeRoutes))
	for p := range nodeRoutes {
		out = append(out, p)
	}
	return out
}

func allowRoute(*Authorizer, *http.Request, string, map[string]string) (decision, string, error) {
	return allow, "", nil
}

func ownNode(_ *Authorizer, _ *http.Request, node string, vars map[string]string) (decision, string, error) {
	if vars["name"] != node {
		return deny, fmt.Sprintf("node %s may act only for itself, not node %q", node, vars["name"]), nil
	}
	return allow, "", nil
}

// ownProjectList forces the nodeRef filter to the caller, whatever was asked.
// The handler filters with AssignedTo's comparison.
func ownProjectList(_ *Authorizer, r *http.Request, node string, _ map[string]string) (decision, string, error) {
	q := r.URL.Query()
	q.Set("nodeRef", node)
	r.URL.RawQuery = q.Encode()
	return allow, "", nil
}

func readableProject(a *Authorizer, r *http.Request, node string, vars map[string]string) (decision, string, error) {
	p, err := a.projects.GetProject(r.Context(), vars["name"])
	if errors.Is(err, store.ErrNotFound) {
		// Let the handler answer its own 404.
		return allow, "", nil
	}
	if err != nil {
		return deny, "", err
	}
	if !NodeMayRead(p, node) {
		return hide, fmt.Sprintf("project %q is neither assigned to node %s nor owes it cleanup", p.Name, node), nil
	}
	return allow, "", nil
}

func usedSecret(a *Authorizer, r *http.Request, node string, vars map[string]string) (decision, string, error) {
	projects, err := a.projects.ListProjects(r.Context())
	if err != nil {
		return deny, "", err
	}
	for _, p := range projects {
		if AssignedTo(p, node) && references(p, vars["name"]) {
			return allow, "", nil
		}
	}
	return deny, fmt.Sprintf("secret %q is not referenced by any project assigned to node %s", vars["name"], node), nil
}

// RequireFenceOwner returns nil unless the caller is a Node and the fence it
// presented names another Node. The store still validates the fence against
// the current assignment or cleanup obligation; this only stops a Node from
// presenting someone else's.
func RequireFenceOwner(ctx context.Context, fenceNode string) error {
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind != v1.APITokenKindNode || p.Name == fenceNode {
		return nil
	}
	return fmt.Errorf("%w: node %s presented an assignment fence for node %q", ErrForbidden, p.Name, fenceNode)
}

// Authorizer applies the rules.
type Authorizer struct {
	projects ProjectReader
	logger   *zap.Logger
}

// New returns an Authorizer reading Projects from projects.
func New(logger *zap.Logger, projects ProjectReader) *Authorizer {
	return &Authorizer{projects: projects, logger: logger}
}

// Wrap returns mux behind the rules. It must sit inside the authentication
// middleware, which attaches the principal.
func (a *Authorizer) Wrap(mux *http.ServeMux) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := auth.FromContext(r.Context())
		// No principal: a public path or a join, which authentication let
		// through on purpose. Users are admins.
		if !ok || p.Kind != v1.APITokenKindNode {
			mux.ServeHTTP(w, r)
			return
		}
		_, pattern := mux.Handler(r)
		if pattern == "" {
			// No route: let the mux answer 404 or 405.
			mux.ServeHTTP(w, r)
			return
		}
		check, listed := nodeRoutes[pattern]
		if !listed {
			a.refuse(w, r, p, http.StatusForbidden, "this route is not available to nodes")
			return
		}
		d, why, err := check(a, r, p.Name, pathValues(pattern, r.URL.Path))
		if err != nil {
			a.logger.Error("Authorization check failed", zap.String("principal", p.String()),
				zap.String("method", r.Method), zap.String("path", r.URL.Path), zap.Error(err))
			writeProblem(w, http.StatusInternalServerError, "authorization check failed")
			return
		}
		switch d {
		case deny:
			a.refuse(w, r, p, http.StatusForbidden, why)
		case hide:
			a.refuse(w, r, p, http.StatusNotFound, why)
		default:
			mux.ServeHTTP(w, r)
		}
	})
}

func (a *Authorizer) refuse(w http.ResponseWriter, r *http.Request, p auth.Principal, status int, why string) {
	a.logger.Warn("Refused a node request", zap.String("principal", p.String()),
		zap.String("method", r.Method), zap.String("path", r.URL.Path),
		zap.Int("status", status), zap.String("reason", why))
	if status == http.StatusNotFound {
		// Says no more than a missing resource would.
		writeProblem(w, status, "not found")
		return
	}
	writeProblem(w, status, why)
}

// pathValues extracts the {name} segments of pattern ("METHOD /a/{x}/b") from
// path. The mux has already matched the two, so they have the same number of
// segments; patterns here use no {x...} wildcards.
func pathValues(pattern, path string) map[string]string {
	if _, p, ok := strings.Cut(pattern, " "); ok {
		pattern = p
	}
	ps := strings.Split(strings.Trim(pattern, "/"), "/")
	rs := strings.Split(strings.Trim(path, "/"), "/")
	vars := map[string]string{}
	for i, seg := range ps {
		if i < len(rs) && strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			vars[strings.Trim(seg, "{}")] = rs[i]
		}
	}
	return vars
}

// writeProblem writes an RFC 9457 problem in the shape the handlers write.
func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/" + strconv.Itoa(status),
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}
