package agentauth

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// HealthPath is the one Agent route served without an assertion, so that
// local tooling and load balancers can still check the process is up.
const HealthPath = "/healthz"

// RefusalHeader is set on the Agent's answer when it refuses a request for
// want of a valid assertion, so cara-server can tell that apart from an
// answer it should relay.
const RefusalHeader = "X-Cara-Assertion-Refused"

// Middleware refuses every request to next that does not carry a valid
// assertion for this Agent, except GET /healthz. A refused request gets 401,
// or 503 while the Agent has not fetched the server's key yet.
func Middleware(v *Verifier, logger *zap.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == HealthPath {
			next.ServeHTTP(w, r)
			return
		}
		claims, err := v.Verify(r.Header.Get(Header), r.Method, r.RequestURI)
		if err != nil {
			status := http.StatusUnauthorized
			if errors.Is(err, ErrNoKey) {
				status = http.StatusServiceUnavailable
			}
			logger.Warn("Refused a request without a valid server assertion",
				zap.String("method", r.Method), zap.String("uri", r.RequestURI),
				zap.String("remote", r.RemoteAddr), zap.String("assertion_id", claims.ID),
				zap.Error(err))
			w.Header().Set(RefusalHeader, "true")
			writeProblem(w, status, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeProblem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"title":  http.StatusText(status),
		"status": status,
		"detail": detail,
	})
}

// Transport signs every request it sends for node, then hands it to base.
type Transport struct {
	Signer *Signer
	Node   string
	Base   http.RoundTripper
}

// RoundTrip implements http.RoundTripper. It does not modify req.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	assertion, err := t.Signer.Sign(t.Node, req.Method, req.URL.RequestURI())
	if err != nil {
		return nil, err
	}
	out := req.Clone(req.Context())
	out.Header.Set(Header, assertion)
	base := t.Base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(out)
}

// KeyFetcher fetches the server's public key.
type KeyFetcher func(ctx context.Context) (ed25519.PublicKey, error)

// KeepFetched fetches the server's key into keys, retrying every retry until
// it succeeds, then fetches it again every refresh so an Agent follows a
// server whose key changed. It returns when ctx ends.
func KeepFetched(ctx context.Context, fetch KeyFetcher, keys *KeyHolder, retry, refresh time.Duration, logger *zap.Logger) {
	for {
		wait := refresh
		key, err := fetch(ctx)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return
			}
			wait = retry
			if keys.Get() == nil {
				logger.Warn("Cannot fetch the server's public key yet; the agent API refuses everything but /healthz until it can",
					zap.Error(err), zap.Duration("retry", retry))
			} else {
				logger.Warn("Cannot refresh the server's public key; keeping the one already fetched",
					zap.Error(err), zap.Duration("retry", retry))
			}
		case keys.Set(key):
			logger.Info("Fetched the server's public key; the agent API now accepts requests signed with it")
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
