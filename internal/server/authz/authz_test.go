package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

type fakeProjects map[string]*v1.Project

func (f fakeProjects) GetProject(_ context.Context, name string) (*v1.Project, error) {
	p, ok := f[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	return p, nil
}

func (f fakeProjects) ListProjects(context.Context) ([]*v1.Project, error) {
	var out []*v1.Project
	for _, p := range f {
		out = append(out, p)
	}
	return out, nil
}

func project(name, node string, phase v1.ProjectPhase, secrets ...string) *v1.Project {
	p := &v1.Project{ObjectMeta: v1.ObjectMeta{Name: name, UID: "uid-" + name}}
	p.Status.NodeRef = node
	p.Status.Phase = phase
	svc := v1.ServiceDef{Name: "app"}
	for _, s := range secrets {
		svc.Env = append(svc.Env, v1.EnvVar{Name: "X", ValueFrom: &v1.EnvVarSource{SecretKeyRef: &v1.SecretKeySelector{Name: s, Key: "k"}}})
	}
	p.Spec.Services = []v1.ServiceDef{svc}
	return p
}

// owing adds an unresolved cleanup obligation for node to p.
func owing(p *v1.Project, node string) *v1.Project {
	p.Status.Recovery = &v1.ProjectRecoveryStatus{CleanupObligations: []v1.CleanupObligation{{
		Assignment: v1.AssignmentIdentity{ProjectUID: p.UID, NodeName: node, AssignmentGeneration: 1},
	}}}
	return p
}

// fixture: node-a runs "mine" (secret s-a, s-shared) and is cleaning up
// "moved"; node-b runs "theirs" (secret s-b, s-shared) and "moved".
func fixture() fakeProjects {
	return fakeProjects{
		"mine":    project("mine", "node-a", v1.ProjectPhaseRunning, "s-a", "s-shared"),
		"leaving": project("leaving", "node-a", v1.ProjectPhaseTerminating, "s-leaving"),
		"theirs":  project("theirs", "node-b", v1.ProjectPhaseRunning, "s-b", "s-shared"),
		"moved":   owing(project("moved", "node-b", v1.ProjectPhaseScheduled, "s-moved"), "node-a"),
		"pending": owing(project("pending", "", v1.ProjectPhasePending), "node-a"),
	}
}

// serve sends a request as principal p (nil: none) through Wrap around a mux
// with the routes nodes use plus one admin route. It returns the status and,
// when the request reached the mux's handler, the query it saw.
func serve(t *testing.T, projects ProjectReader, p *auth.Principal, method, path string) (int, string, bool) {
	t.Helper()
	mux := http.NewServeMux()
	var reached bool
	var query string
	h := func(w http.ResponseWriter, r *http.Request) {
		reached = true
		query = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}
	for _, pattern := range NodeRoutes() {
		mux.HandleFunc(pattern, h)
	}
	mux.HandleFunc("POST /api/v1/projects", h)
	mux.HandleFunc("DELETE /api/v1/projects/{name}", h)

	req := httptest.NewRequest(method, path, nil)
	if p != nil {
		req = req.WithContext(auth.WithPrincipal(req.Context(), *p))
	}
	rec := httptest.NewRecorder()
	New(zap.NewNop(), projects).Wrap(mux).ServeHTTP(rec, req)
	return rec.Code, query, reached
}

var (
	nodeA = &auth.Principal{Kind: v1.APITokenKindNode, Name: "node-a"}
	admin = &auth.Principal{Kind: v1.APITokenKindUser, Name: auth.BreakGlassName}
)

func TestNodeRules(t *testing.T) {
	cases := []struct {
		name         string
		method, path string
		want         int
	}{
		{"unlisted route: create project", http.MethodPost, "/api/v1/projects", http.StatusForbidden},
		{"unlisted route: delete project", http.MethodDelete, "/api/v1/projects/mine", http.StatusForbidden},
		{"own heartbeat", http.MethodPost, "/api/v1/nodes/node-a/heartbeat", http.StatusOK},
		{"another node's heartbeat", http.MethodPost, "/api/v1/nodes/node-b/heartbeat", http.StatusForbidden},
		{"own cleanup obligations", http.MethodGet, "/api/v1/nodes/node-a/cleanup-obligations", http.StatusOK},
		{"another node's cleanup obligations", http.MethodGet, "/api/v1/nodes/node-b/cleanup-obligations", http.StatusForbidden},
		{"register", http.MethodPost, "/api/v1/nodes", http.StatusOK},
		{"read own project", http.MethodGet, "/api/v1/projects/mine", http.StatusOK},
		{"read own terminating project", http.MethodGet, "/api/v1/projects/leaving", http.StatusOK},
		{"read moved project it owes cleanup for", http.MethodGet, "/api/v1/projects/moved", http.StatusOK},
		{"read released, unassigned project it owes cleanup for", http.MethodGet, "/api/v1/projects/pending", http.StatusOK},
		{"read another node's project", http.MethodGet, "/api/v1/projects/theirs", http.StatusNotFound},
		{"read a missing project reaches the handler", http.MethodGet, "/api/v1/projects/nope", http.StatusOK},
		{"secret of own project", http.MethodGet, "/api/v1/secrets/s-a", http.StatusOK},
		{"secret shared with another node", http.MethodGet, "/api/v1/secrets/s-shared", http.StatusOK},
		{"secret of own terminating project", http.MethodGet, "/api/v1/secrets/s-leaving", http.StatusOK},
		{"secret only another node uses", http.MethodGet, "/api/v1/secrets/s-b", http.StatusForbidden},
		{"secret of a project it only owes cleanup for", http.MethodGet, "/api/v1/secrets/s-moved", http.StatusForbidden},
		{"secret nobody uses", http.MethodGet, "/api/v1/secrets/unused", http.StatusForbidden},
		{"fenced status write reaches the handler", http.MethodPatch, "/api/v1/projects/theirs/status", http.StatusOK},
		{"fenced evidence write reaches the handler", http.MethodPut, "/api/v1/projects/moved/recovery/resources-removed", http.StatusOK},
		{"no such route", http.MethodGet, "/api/v1/nothing", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, _ := serve(t, fixture(), nodeA, tc.method, tc.path)
			if got != tc.want {
				t.Fatalf("%s %s as node-a = %d, want %d", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestNodeProjectListIsForcedToTheCaller(t *testing.T) {
	for _, path := range []string{"/api/v1/projects", "/api/v1/projects?nodeRef=node-b", "/api/v1/projects?phase=Scheduled&nodeRef=node-b"} {
		code, query, reached := serve(t, fixture(), nodeA, http.MethodGet, path)
		if code != http.StatusOK || !reached {
			t.Fatalf("%s: code %d reached %v", path, code, reached)
		}
		if got := mustQuery(t, query).Get("nodeRef"); got != "node-a" {
			t.Fatalf("%s: handler saw nodeRef=%q, want node-a", path, got)
		}
	}
	_, query, _ := serve(t, fixture(), nodeA, http.MethodGet, "/api/v1/projects?phase=Scheduled&nodeRef=node-b")
	if got := mustQuery(t, query).Get("phase"); got != "Scheduled" {
		t.Fatalf("phase filter dropped: %q", got)
	}
}

func TestAdminsAndAnonymousRequestsPassThrough(t *testing.T) {
	for _, p := range []*auth.Principal{admin, nil} {
		code, query, reached := serve(t, fixture(), p, http.MethodGet, "/api/v1/projects?nodeRef=node-b")
		if code != http.StatusOK || !reached || mustQuery(t, query).Get("nodeRef") != "node-b" {
			t.Fatalf("principal %v: code %d reached %v query %q", p, code, reached, query)
		}
		if code, _, _ := serve(t, fixture(), p, http.MethodPost, "/api/v1/projects"); code != http.StatusOK {
			t.Fatalf("principal %v: create project = %d", p, code)
		}
	}
}

func TestStoreErrorIsNotAnAllow(t *testing.T) {
	code, _, reached := serve(t, fakeProjectsErr{}, nodeA, http.MethodGet, "/api/v1/projects/mine")
	if code != http.StatusInternalServerError || reached {
		t.Fatalf("code %d reached %v, want 500 without reaching the handler", code, reached)
	}
}

type fakeProjectsErr struct{}

func (fakeProjectsErr) GetProject(context.Context, string) (*v1.Project, error) {
	return nil, errors.New("db down")
}
func (fakeProjectsErr) ListProjects(context.Context) ([]*v1.Project, error) {
	return nil, errors.New("db down")
}

func TestRequireFenceOwner(t *testing.T) {
	ctx := context.Background()
	if err := RequireFenceOwner(auth.WithPrincipal(ctx, *nodeA), "node-a"); err != nil {
		t.Fatalf("own fence refused: %v", err)
	}
	if err := RequireFenceOwner(auth.WithPrincipal(ctx, *nodeA), "node-b"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("another node's fence: err %v, want ErrForbidden", err)
	}
	if err := RequireFenceOwner(auth.WithPrincipal(ctx, *admin), "node-b"); err != nil {
		t.Fatalf("admin refused: %v", err)
	}
	if err := RequireFenceOwner(ctx, "node-b"); err != nil {
		t.Fatalf("no principal refused: %v", err)
	}
}

func TestPathValues(t *testing.T) {
	got := pathValues("PUT /api/v1/projects/{name}/backup/completions/{backupID}", "/api/v1/projects/demo/backup/completions/20261106T000000Z-0a1b2c3d")
	if got["name"] != "demo" || got["backupID"] != "20261106T000000Z-0a1b2c3d" || len(got) != 2 {
		t.Fatalf("pathValues = %v", got)
	}
}

func mustQuery(t *testing.T, raw string) url.Values {
	t.Helper()
	q, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
