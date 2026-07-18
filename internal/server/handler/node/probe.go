package node

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"NYCU-SDC/caravanserai/internal/server/agentdialer"

	handlerutil "github.com/NYCU-SDC/summer/pkg/handler"
	logutil "github.com/NYCU-SDC/summer/pkg/log"
	"go.uber.org/zap"
)

// probeResult is the response body of POST /api/v1/nodes/{name}/probe.

// A successful probe means cara-server was able to reach the agent's /healthz
// endpoint through the configured Dialer transport
type probeResult struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode"`
	LatencyMs  int64  `json:"latencyMs"`
	Address    string `json:"address"`
	Error      string `json:"error,omitempty"`
}

// probe dials the named node's agent via the injected Dialer and reports
// reachability.
func (h *Handler) probe(w http.ResponseWriter, r *http.Request) {
	traceCtx, span := h.tracer.Start(r.Context(), "probe")
	defer span.End()
	logger := logutil.WithContext(traceCtx, h.logger)

	name := r.PathValue("name")

	if h.dialer == nil {
		h.problemWriter.WriteError(traceCtx, w,
			fmt.Errorf("probe not available: dialer is not configured"), logger)
		return
	}

	client, baseURL, err := h.dialer.Client(traceCtx, name)
	if err != nil {
		if errors.Is(err, agentdialer.ErrNodeUnreachable) {
			handlerutil.WriteJSONResponse(w, http.StatusConflict, probeResult{
				OK:      false,
				Address: "",
				Error:   err.Error(),
			})
			return
		}
		// Anything else (store errors, missing node) is reported through the
		// standard problem writer, which maps store.ErrNotFound → 404.
		h.problemWriter.WriteError(traceCtx, w, err, logger)
		return
	}

	statusCode, latency, probeErr := doProbe(traceCtx, client, baseURL)
	result := probeResult{
		OK:         probeErr == nil && statusCode == http.StatusOK,
		StatusCode: statusCode,
		LatencyMs:  latency.Milliseconds(),
		Address:    baseURL,
	}
	if probeErr != nil {
		result.Error = probeErr.Error()
	}

	logger.Debug("Probe complete",
		zap.String("node", name),
		zap.String("address", baseURL),
		zap.Int("status", statusCode),
		zap.Duration("latency", latency),
		zap.Bool("ok", result.OK),
	)
	handlerutil.WriteJSONResponse(w, http.StatusOK, result)
}

// doProbe issues a single GET /healthz against baseURL using client and
// returns the observed status code, wall-clock latency, and any transport
// error.
func doProbe(ctx context.Context, client *http.Client, baseURL string) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return 0, 0, fmt.Errorf("build request: %w", err)
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := time.Since(start)
	if err != nil {
		return 0, latency, fmt.Errorf("dial agent: %w", err)
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()

	// Drain a bounded amount of the body so the connection can be reused.
	const bodyReadLimit = 4 << 10
	_, _ = io.CopyN(io.Discard, resp.Body, bodyReadLimit)

	// Wrap non-2xx responses with a descriptive error for the response body.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, latency, fmt.Errorf("agent returned status %d", resp.StatusCode)
	}
	return resp.StatusCode, latency, nil
}
