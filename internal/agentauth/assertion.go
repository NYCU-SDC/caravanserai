// Package agentauth lets cara-agent tell cara-server's requests apart from
// everyone else's (CARA-139, ADR-0031).
//
// cara-server holds one Ed25519 key. When it calls an Agent it adds a short
// assertion to the request, signed with that key and bound to the Node it is
// meant for, the method and the request URI. The Agent fetches the public key
// from cara-server with its Node token and refuses every request on its API
// that does not carry a valid assertion for it, except the health probe.
//
// The assertion authorizes opening a request, not how long it lasts: a log
// stream or a port-forward tunnel opened with a valid assertion keeps running
// after the assertion expires.
package agentauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Header carries the assertion on a request from cara-server to an Agent.
const Header = "X-Cara-Server-Assertion"

// version prefixes every assertion and is part of what is signed, so a
// signature made for something else with the same key never verifies here.
const version = "cara-agent-assertion-v1"

// DefaultTTL is how long an assertion is valid after it is made. It covers the
// dial to the Agent and nothing more: cara-server makes a new one per request.
const DefaultTTL = 30 * time.Second

// DefaultSkew is how far apart the two clocks may be. An assertion is accepted
// until ExpiresAt plus this, and refused when made more than this in the
// future.
const DefaultSkew = 30 * time.Second

// Claims is what an assertion says.
type Claims struct {
	// Node is the Agent the request is meant for.
	Node string `json:"node"`
	// Method and URI are the request's method and request URI (path and
	// query) as sent.
	Method string `json:"method"`
	URI    string `json:"uri"`
	// IssuedAt and ExpiresAt are Unix seconds.
	IssuedAt  int64 `json:"iat"`
	ExpiresAt int64 `json:"exp"`
	// ID is random, so an Agent can refuse an assertion it has already seen.
	ID string `json:"jti"`
}

// Signer makes assertions with cara-server's private key.
type Signer struct {
	key ed25519.PrivateKey
	ttl time.Duration
	now func() time.Time
}

// NewSigner returns a Signer for key.
func NewSigner(key ed25519.PrivateKey) *Signer {
	return &Signer{key: key, ttl: DefaultTTL, now: time.Now}
}

// GenerateKey returns a new private key in the form SignerFromKey takes.
func GenerateKey() ([]byte, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// SignerFromKey returns a Signer for a private key GenerateKey made.
func SignerFromKey(raw []byte) (*Signer, error) {
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("agentauth: private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return NewSigner(ed25519.PrivateKey(raw)), nil
}

// PublicKey returns the key Agents verify with.
func (s *Signer) PublicKey() ed25519.PublicKey {
	return s.key.Public().(ed25519.PublicKey)
}

// Sign returns an assertion for a request with method and uri to node.
func (s *Signer) Sign(node, method, uri string) (string, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", fmt.Errorf("agentauth: random id: %w", err)
	}
	now := s.now()
	payload, err := json.Marshal(Claims{
		Node:      node,
		Method:    method,
		URI:       uri,
		IssuedAt:  now.Unix(),
		ExpiresAt: now.Add(s.ttl).Unix(),
		ID:        hex.EncodeToString(id),
	})
	if err != nil {
		return "", fmt.Errorf("agentauth: encode claims: %w", err)
	}
	body := base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(s.key, signedBytes(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func signedBytes(body string) []byte {
	return []byte(version + "." + body)
}

// Reasons a request is refused. Verify wraps one of them.
var (
	ErrMissing    = errors.New("no server assertion")
	ErrMalformed  = errors.New("malformed server assertion")
	ErrSignature  = errors.New("server assertion signature does not verify")
	ErrWrongNode  = errors.New("server assertion is for another node")
	ErrWrongRoute = errors.New("server assertion is for another request")
	ErrExpired    = errors.New("server assertion has expired")
	ErrNotYet     = errors.New("server assertion is from the future")
	ErrReplayed   = errors.New("server assertion was already used")
	ErrNoKey      = errors.New("the server's public key is not known yet")
)

// Verifier checks assertions on one Agent.
type Verifier struct {
	node string
	keys *KeyHolder
	skew time.Duration
	now  func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time // assertion ID -> when it can be forgotten
}

// NewVerifier returns a Verifier for the Agent of node, checking signatures
// with whatever key keys holds at the time.
func NewVerifier(node string, keys *KeyHolder) *Verifier {
	return &Verifier{node: node, keys: keys, skew: DefaultSkew, now: time.Now, seen: map[string]time.Time{}}
}

// Verify checks assertion for a request with method and uri. An assertion is
// accepted once: a second request with the same one is refused.
func (v *Verifier) Verify(assertion, method, uri string) (Claims, error) {
	if assertion == "" {
		return Claims{}, ErrMissing
	}
	key := v.keys.Get()
	if key == nil {
		return Claims{}, ErrNoKey
	}
	body, sigText, ok := strings.Cut(assertion, ".")
	if !ok {
		return Claims{}, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigText)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	if !ed25519.Verify(key, signedBytes(body), sig) {
		return Claims{}, ErrSignature
	}
	payload, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return Claims{}, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil || c.ID == "" {
		return Claims{}, ErrMalformed
	}
	if c.Node != v.node {
		return c, fmt.Errorf("%w: made for node %q, this is %q", ErrWrongNode, c.Node, v.node)
	}
	if c.Method != method || c.URI != uri {
		return c, fmt.Errorf("%w: made for %s %s", ErrWrongRoute, c.Method, c.URI)
	}
	now := v.now()
	forgetAt := time.Unix(c.ExpiresAt, 0).Add(v.skew)
	if !now.Before(forgetAt) {
		return c, ErrExpired
	}
	if time.Unix(c.IssuedAt, 0).After(now.Add(v.skew)) {
		return c, ErrNotYet
	}
	if err := v.remember(c.ID, forgetAt, now); err != nil {
		return c, err
	}
	return c, nil
}

// remember records id until forgetAt, and refuses one it already holds.
func (v *Verifier) remember(id string, forgetAt, now time.Time) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	for seenID, until := range v.seen {
		if !now.Before(until) {
			delete(v.seen, seenID)
		}
	}
	if _, ok := v.seen[id]; ok {
		return ErrReplayed
	}
	v.seen[id] = forgetAt
	return nil
}

// KeyHolder holds the server's public key once the Agent has fetched it. It
// is safe for concurrent use.
type KeyHolder struct {
	mu  sync.RWMutex
	key ed25519.PublicKey
}

// Get returns the key, or nil before one was set.
func (h *KeyHolder) Get() ed25519.PublicKey {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.key
}

// Set replaces the key. It reports whether the key changed.
func (h *KeyHolder) Set(key ed25519.PublicKey) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	changed := !h.key.Equal(key)
	h.key = key
	return changed
}

// EncodePublicKey and DecodePublicKey are how the key travels in the API.
func EncodePublicKey(key ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(key)
}

// DecodePublicKey parses what EncodePublicKey produced.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("agentauth: decode public key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("agentauth: public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}
