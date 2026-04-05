//go:build e2e

// Package integration contains integration tests that spin up real infrastructure
// (PostgreSQL via dockertest) and exercise the full HTTP request/response
// cycle through a real apiserver wired with a real pgstore.
package integration

import (
	"NYCU-SDC/caravanserai/test/integration/testhelper"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/apiserver"
	nodehandler "NYCU-SDC/caravanserai/internal/server/handler/node"
	projecthandler "NYCU-SDC/caravanserai/internal/server/handler/project"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"

	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// verbose is set via -verbose on the test binary (pass through Makefile with
// make test-integration VERBOSE=1, which appends -args -verbose to go test).
var verbose = flag.Bool("verbose", false, "enable verbose infrastructure logging")

// suite holds shared test infrastructure initialised once in TestMain.
var suite struct {
	serverURL string
	client    *http.Client
}

// TestMain starts a PostgreSQL container, runs migrations via pgstore.New,
// wires up a real apiserver, and starts it with httptest.NewServer.
// All tests in this package share the same server and database.
//
// Infrastructure teardown (container purge, store close, server close) is
// handled inside run() so that deferred calls execute before os.Exit is
// reached. Calling os.Exit directly in TestMain would skip all defers and
// leave Docker containers running after the test process exits.
func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	// flag.Parse must be called explicitly here because we read *verbose before
	// m.Run(), which is where the testing package would normally parse flags.
	flag.Parse()

	// Start a disposable PostgreSQL container.
	_, databaseURL, cleanup, err := testhelper.StartPostgres()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: start postgres: %v\n", err)
		return 1
	}
	defer cleanup()

	// Use a development logger when -verbose is passed, otherwise stay quiet.
	// The logger writes to stderr so go test's stdout buffering does not
	// swallow infrastructure log lines emitted before any test runs.
	var logger *zap.Logger
	if *verbose {
		cfg := zap.NewDevelopmentConfig()
		cfg.OutputPaths = []string{"stderr"}
		cfg.ErrorOutputPaths = []string{"stderr"}
		logger, err = cfg.Build()
		if err != nil {
			fmt.Fprintf(os.Stderr, "integration: build logger: %v\n", err)
			return 1
		}
		defer func() { _ = logger.Sync() }()
	} else {
		logger = zap.NewNop()
	}

	// pgstore.New runs embedded migrations, so the schema is always in sync.
	// Pass nil for event.Bus — integration tests don't need event publishing.
	pgStore, err := pgstore.New(context.Background(), databaseURL, logger, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: pgstore.New: %v\n", err)
		return 1
	}
	defer pgStore.Close()

	// Assemble apiserver with a minimal middleware set (no tracing overhead).
	basicMiddleware := middleware.NewSet()
	apiSrv := apiserver.New(logger, basicMiddleware)
	apiSrv.Register(nodehandler.NewHandler(logger, pgStore, pgStore))
	apiSrv.Register(projecthandler.NewHandler(logger, pgStore))

	ts := httptest.NewServer(apiSrv.Handler())
	defer ts.Close()

	suite.serverURL = ts.URL
	suite.client = ts.Client()

	return m.Run()
}

// TestNodeCRUD exercises the full Node lifecycle:
//
//	POST   /api/v1/nodes               → 201, state == NotReady
//	POST   /api/v1/nodes (duplicate)   → 409 Conflict
//	GET    /api/v1/nodes               → 200, one item
//	GET    /api/v1/nodes/{name}        → 200, correct name
//	POST   /api/v1/nodes/{name}/heartbeat → 204
//	GET    /api/v1/nodes/{name}        → 200, state == Ready after heartbeat
//	DELETE /api/v1/nodes/{name}        → 204
//	GET    /api/v1/nodes/{name}        → 404 after deletion
func TestNodeCRUD(t *testing.T) {
	const nodeName = "e2e-test-node-01"

	// ── 1. Create ──────────────────────────────────────────────────────────────

	createBody := mustMarshal(t, v1.Node{
		ObjectMeta: v1.ObjectMeta{Name: nodeName},
		Spec:       v1.NodeSpec{Hostname: "e2e-host-01"},
	})

	resp := doRequest(t, http.MethodPost, "/api/v1/nodes", createBody)
	require.Equal(t, http.StatusCreated, resp.StatusCode, "create node: expected 201")

	var created v1.Node
	mustDecodeBody(t, resp, &created)
	assert.Equal(t, nodeName, created.Name)
	assert.Equal(t, v1.NodeStateNotReady, created.Status.State, "initial state must be NotReady")

	// ── 2. Create duplicate → 409 ──────────────────────────────────────────────

	resp = doRequest(t, http.MethodPost, "/api/v1/nodes", createBody)
	assert.Equal(t, http.StatusConflict, resp.StatusCode, "duplicate create: expected 409")
	drainBody(resp)

	// ── 3. List → one item ─────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodGet, "/api/v1/nodes", nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "list nodes: expected 200")

	var list v1.NodeList
	mustDecodeBody(t, resp, &list)
	require.Len(t, list.Items, 1, "list must contain exactly one node")
	assert.Equal(t, nodeName, list.Items[0].Name)

	// ── 4. Get by name ─────────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodGet, "/api/v1/nodes/"+nodeName, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "get node: expected 200")

	var fetched v1.Node
	mustDecodeBody(t, resp, &fetched)
	assert.Equal(t, nodeName, fetched.Name)

	// ── 5. Heartbeat ───────────────────────────────────────────────────────────

	heartbeatBody := mustMarshal(t, map[string]string{"state": string(v1.NodeStateReady)})
	resp = doRequest(t, http.MethodPost, "/api/v1/nodes/"+nodeName+"/heartbeat", heartbeatBody)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "heartbeat: expected 204")
	drainBody(resp)

	// ── 6. State is Ready after heartbeat ─────────────────────────────────────

	resp = doRequest(t, http.MethodGet, "/api/v1/nodes/"+nodeName, nil)
	require.Equal(t, http.StatusOK, resp.StatusCode, "get after heartbeat: expected 200")

	var afterHB v1.Node
	mustDecodeBody(t, resp, &afterHB)
	assert.Equal(t, v1.NodeStateReady, afterHB.Status.State, "state must be Ready after heartbeat")

	// ── 7. Delete ──────────────────────────────────────────────────────────────

	resp = doRequest(t, http.MethodDelete, "/api/v1/nodes/"+nodeName, nil)
	assert.Equal(t, http.StatusNoContent, resp.StatusCode, "delete node: expected 204")
	drainBody(resp)

	// ── 8. Get after delete → 404 ─────────────────────────────────────────────

	resp = doRequest(t, http.MethodGet, "/api/v1/nodes/"+nodeName, nil)
	assert.Equal(t, http.StatusNotFound, resp.StatusCode, "get after delete: expected 404")
	drainBody(resp)
}

// ── helpers ───────────────────────────────────────────────────────────────────

// doRequest sends an HTTP request to the shared test server and returns the
// response. The caller is responsible for consuming / draining the body.
func doRequest(t *testing.T, method, path string, body []byte) *http.Response {
	t.Helper()
	var bodyReader *bytes.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	} else {
		bodyReader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, suite.serverURL+path, bodyReader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := suite.client.Do(req)
	require.NoError(t, err)
	return resp
}

// mustMarshal serialises v to JSON, failing the test on error.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}

// mustDecodeBody decodes the JSON response body into v, then closes the body.
func mustDecodeBody(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	require.NoError(t, json.NewDecoder(resp.Body).Decode(v))
}

// drainBody discards and closes the response body to allow connection reuse.
func drainBody(resp *http.Response) {
	defer resp.Body.Close()
	_, _ = bytes.NewBuffer(nil).ReadFrom(resp.Body)
}
