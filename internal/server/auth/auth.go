// Package auth resolves every cara-server API request to the principal that
// made it (ADR-0031, CARA-137).
//
// A caller presents a bearer token. The server issues tokens itself and keeps
// only their SHA-256 hashes; a break-glass admin token can also be configured
// from a file, for bootstrapping and for when nothing else works. This package
// answers "who is calling"; what each principal may do is decided elsewhere.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/store"

	"go.uber.org/zap"
)

// BreakGlassName is the principal name of the break-glass token.
const BreakGlassName = "break-glass"

// tokenPrefix marks a Cara token, so one pasted into the wrong place is
// recognisable.
const tokenPrefix = "cara_"

// Principal is an authenticated caller.
type Principal struct {
	Kind v1.APITokenKind
	Name string
}

// String renders the principal as "kind:name", the form audit records use.
func (p Principal) String() string {
	return string(p.Kind) + ":" + p.Name
}

// IsAdmin reports whether p may do anything. Every user is an admin until roles
// land (ADR-0031 phase 4); a Node never is.
func (p Principal) IsAdmin() bool {
	return p.Kind == v1.APITokenKindUser
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal the middleware attached, if any.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// NewToken returns a new random token. Show it to the caller once and store
// only HashToken of it.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the form a token is stored and looked up in.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// ErrUnauthenticated means the request carried no valid token.
var ErrUnauthenticated = errors.New("a valid bearer token is required")

// TokenLookup is the part of the store the Authenticator reads.
type TokenLookup interface {
	LookupAPIToken(ctx context.Context, tokenHash string) (v1.APIToken, error)
}

// Authenticator turns bearer tokens into principals.
type Authenticator struct {
	tokens TokenLookup
	// breakGlassHash is the hash of the break-glass token, or nil when none is
	// configured. Comparing hashes keeps the comparison fixed-length.
	breakGlassHash []byte
	logger         *zap.Logger
}

// NewAuthenticator returns an Authenticator reading tokens from tokens. An
// empty breakGlassToken configures none.
func NewAuthenticator(logger *zap.Logger, tokens TokenLookup, breakGlassToken string) *Authenticator {
	a := &Authenticator{tokens: tokens, logger: logger}
	if breakGlassToken != "" {
		sum := sha256.Sum256([]byte(breakGlassToken))
		a.breakGlassHash = sum[:]
	}
	return a
}

// Authenticate returns the principal a token identifies. It returns
// ErrUnauthenticated for an empty, unknown or revoked token.
func (a *Authenticator) Authenticate(ctx context.Context, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	if a.breakGlassHash != nil {
		sum := sha256.Sum256([]byte(token))
		if subtle.ConstantTimeCompare(sum[:], a.breakGlassHash) == 1 {
			return Principal{Kind: v1.APITokenKindUser, Name: BreakGlassName}, nil
		}
	}
	t, err := a.tokens.LookupAPIToken(ctx, HashToken(token))
	if errors.Is(err, store.ErrNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, err
	}
	return Principal{Kind: t.Kind, Name: t.Name}, nil
}

// publicPaths are served without a token. The health probe is polled by
// supervisors that hold no credential and reveals nothing.
var publicPaths = map[string]bool{
	"/api/healthz": true,
}

// Middleware attaches the caller's principal to every request and answers 401
// when there is none.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if publicPaths[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			token = ""
		}
		p, err := a.Authenticate(r.Context(), strings.TrimSpace(token))
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				a.logger.Error("Failed to authenticate a request", zap.String("path", r.URL.Path), zap.Error(err))
				writeProblem(w, http.StatusInternalServerError, "Internal Server Error", "authentication failed")
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="cara"`)
			writeProblem(w, http.StatusUnauthorized, "Unauthorized", ErrUnauthenticated.Error())
			return
		}
		next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), p)))
	})
}

// writeProblem writes an RFC 9457 problem in the shape the handlers' problem
// writer produces, so clients parse one error format.
func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":   "https://developer.mozilla.org/en-US/docs/Web/HTTP/Status/" + strconv.Itoa(status),
		"title":  title,
		"status": status,
		"detail": detail,
	})
}
