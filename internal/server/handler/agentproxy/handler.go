// Package agentproxy serves the routes through which callers reach an Agent
// (CARA-139). Agents accept only requests cara-server signed, so caractl logs
// and caractl port-forward go through here instead of dialing the Agent: the
// server checks the caller's token like on any route, then calls the Agent
// with a signed request and relays the answer.
//
// Routes:
//
//	GET /api/v1/agent-signing-key                                — the public key Agents verify with
//	GET /api/v1/projects/{name}/services/{service}/logs          — the Agent's log stream
//	GET /api/v1/projects/{name}/services/{service}/forward/{port} — the Agent's port-forward WebSocket
//
// The two proxy routes go to the Node in the Project's status.nodeRef, or to
// the one named by ?node=. They are registered without the trace middleware:
// its ResponseWriter cannot flush or hijack, so a log stream would arrive all
// at once at the end and a WebSocket could not be upgraded.
package agentproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agentauth"
	"NYCU-SDC/caravanserai/internal/server/agentdialer"
	"NYCU-SDC/caravanserai/internal/server/auth"
	"NYCU-SDC/caravanserai/internal/store"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	"github.com/NYCU-SDC/summer/pkg/middleware"
	"github.com/NYCU-SDC/summer/pkg/problem"
	"go.uber.org/zap"
)

// DefaultHeaderTimeout bounds how long the proxy waits for the Agent to
// answer. A stream that has started is not bounded; one that never starts
// fails with 502 instead of leaving the caller hanging.
const DefaultHeaderTimeout = 15 * time.Second

// ProjectReader is the part of the store the proxy reads.
type ProjectReader interface {
	GetProject(ctx context.Context, name string) (*v1.Project, error)
}

// Handler serves the routes.
type Handler struct {
	logger        *zap.Logger
	projects      ProjectReader
	agents        agentdialer.EndpointResolver
	publicKey     string
	problemWriter *problem.HttpWriter
	headerTimeout time.Duration
}

// NewHandler returns a Handler that reaches Agents through agents and
// publishes signer's public key.
func NewHandler(logger *zap.Logger, projects ProjectReader, agents agentdialer.EndpointResolver, signer *agentauth.Signer, pw *problem.HttpWriter) *Handler {
	return &Handler{
		logger:        logger,
		projects:      projects,
		agents:        agents,
		publicKey:     agentauth.EncodePublicKey(signer.PublicKey()),
		problemWriter: pw,
		headerTimeout: DefaultHeaderTimeout,
	}
}

// RegisterRoutes satisfies apiserver.RouteRegistrar.
func (h *Handler) RegisterRoutes(mux *http.ServeMux, mid *middleware.Set) {
	mux.HandleFunc("GET /api/v1/agent-signing-key", mid.HandlerFunc(h.signingKey))
	// No middleware set on purpose; see the package comment.
	mux.HandleFunc("GET /api/v1/projects/{name}/services/{service}/logs", h.logs)
	mux.HandleFunc("GET /api/v1/projects/{name}/services/{service}/forward/{port}", h.forward)
}

func (h *Handler) signingKey(w http.ResponseWriter, _ *http.Request) {
	handlerutil.WriteJSONResponse(w, http.StatusOK, v1.AgentSigningKey{PublicKey: h.publicKey})
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request) {
	project, service := r.PathValue("name"), r.PathValue("service")
	// Pass on only what the Agent's logs route reads.
	in, out := r.URL.Query(), url.Values{}
	for _, k := range []string{"follow", "tail", "timestamps"} {
		if v := in.Get(k); v != "" {
			out.Set(k, v)
		}
	}
	h.proxy(w, r, project, "/api/v1/logs/"+url.PathEscape(project)+"/"+url.PathEscape(service), out.Encode())
}

func (h *Handler) forward(w http.ResponseWriter, r *http.Request) {
	project, service, portText := r.PathValue("name"), r.PathValue("service"), r.PathValue("port")
	if port, err := strconv.Atoi(portText); err != nil || port < 1 || port > 65535 {
		h.problemWriter.WriteError(r.Context(), w,
			handlerutil.NewValidationError("port", portText, "port must be a number from 1 to 65535"), h.logger)
		return
	}
	h.proxy(w, r, project,
		"/api/v1/forward/"+url.PathEscape(project)+"/"+url.PathEscape(service)+"/"+portText, "")
}

// proxy relays r to agentPath on the Agent running project.
func (h *Handler) proxy(w http.ResponseWriter, r *http.Request, project, agentPath, rawQuery string) {
	ctx := r.Context()
	log := h.logger.With(zap.String("project", project), zap.String("agent_path", agentPath))
	if p, ok := auth.FromContext(ctx); ok {
		log = log.With(zap.String("principal", p.String()))
	}

	node, err := h.targetNode(ctx, project, r.URL.Query().Get("node"))
	if err != nil {
		h.problemWriter.WriteError(ctx, w, err, log)
		return
	}
	log = log.With(zap.String("node", node))

	transport, base, err := h.agents.Endpoint(ctx, node)
	if err != nil {
		switch {
		case errors.Is(err, agentdialer.ErrNodeUnreachable):
			err = fmt.Errorf("%w: %w", store.ErrConflictState, err)
		case errors.Is(err, store.ErrNotFound):
			err = fmt.Errorf("node %q: %w", node, store.ErrNotFound)
		}
		h.problemWriter.WriteError(ctx, w, err, log)
		return
	}

	started := time.Now()
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = base.Scheme
			pr.Out.URL.Host = base.Host
			pr.Out.URL.Path = agentPath
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = rawQuery
			pr.Out.Host = ""
			// The caller's credentials are for cara-server, not the Agent.
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
		},
		Transport:     &headerTimeout{base: transport, timeout: h.headerTimeout},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				// The caller went away.
				return
			}
			log.Warn("Cannot reach the agent", zap.Error(err))
			writeProblem(w, http.StatusBadGateway, fmt.Sprintf("cannot reach the agent on node %q: %v", node, err))
		},
		ModifyResponse: func(resp *http.Response) error {
			log.Info("Proxying to the agent", zap.Int("agent_status", resp.StatusCode))
			if resp.Header.Get(agentauth.RefusalHeader) != "" {
				// The Agent refused the server's signature. Relayed as is,
				// the caller would read it as its own token being refused.
				defer func() { _ = resp.Body.Close() }()
				detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				return fmt.Errorf("the agent refused cara-server's signature (%d): %s", resp.StatusCode, detail)
			}
			return nil
		},
	}
	proxy.ServeHTTP(w, r)
	log.Info("Agent proxy closed", zap.Duration("duration", time.Since(started)))
}

// targetNode returns the Node to reach for project: override when set, the
// Project's assignment otherwise.
func (h *Handler) targetNode(ctx context.Context, project, override string) (string, error) {
	if override != "" {
		if err := v1.ValidateName(override); err != nil {
			return "", handlerutil.NewValidationError("node", override, err.Error())
		}
		return override, nil
	}
	p, err := h.projects.GetProject(ctx, project)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("project %q: %w", project, store.ErrNotFound)
		}
		return "", err
	}
	if p.Status.NodeRef == "" {
		return "", fmt.Errorf("%w: project %q is not assigned to a node; pass ?node= to reach one", store.ErrConflictState, project)
	}
	return p.Status.NodeRef, nil
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	handlerutil.WriteJSONResponse(w, status, problem.Problem{
		Title:  http.StatusText(status),
		Status: status,
		Detail: detail,
	})
}

// headerTimeout cancels a request whose response headers do not arrive within
// timeout. Once they arrive the body runs until the caller or the Agent ends
// it.
type headerTimeout struct {
	base    http.RoundTripper
	timeout time.Duration
}

func (t *headerTimeout) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	timer := time.AfterFunc(t.timeout, cancel)
	resp, err := t.base.RoundTrip(req.WithContext(ctx))
	if !timer.Stop() && err == nil {
		// The timer fired as the headers arrived; the body is already dead.
		_ = resp.Body.Close()
		err = context.DeadlineExceeded
	}
	if err != nil {
		cancel()
		if ctx.Err() != nil && req.Context().Err() == nil {
			err = fmt.Errorf("no answer within %s: %w", t.timeout, err)
		}
		return nil, err
	}
	// Release the context with the body. An upgraded connection's body is
	// also its write side, which ReverseProxy needs to keep.
	if rwc, ok := resp.Body.(io.ReadWriteCloser); ok {
		resp.Body = &cancelReadWriteCloser{ReadWriteCloser: rwc, cancel: cancel}
	} else {
		resp.Body = &cancelReadCloser{ReadCloser: resp.Body, cancel: cancel}
	}
	return resp, nil
}

type cancelReadCloser struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (c *cancelReadCloser) Close() error {
	err := c.ReadCloser.Close()
	c.cancel()
	return err
}

type cancelReadWriteCloser struct {
	io.ReadWriteCloser
	cancel context.CancelFunc
}

func (c *cancelReadWriteCloser) Close() error {
	err := c.ReadWriteCloser.Close()
	c.cancel()
	return err
}
