package agentauth

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

type fixture struct {
	signer   *Signer
	verifier *Verifier
	clock    time.Time
}

func newFixture(t *testing.T, node string) *fixture {
	t.Helper()
	f := &fixture{clock: time.Unix(1_800_000_000, 0)}
	f.signer = NewSigner(newKey(t))
	f.signer.now = func() time.Time { return f.clock }
	keys := &KeyHolder{}
	keys.Set(f.signer.PublicKey())
	f.verifier = NewVerifier(node, keys)
	f.verifier.now = func() time.Time { return f.clock }
	return f
}

func (f *fixture) sign(t *testing.T, node, method, uri string) string {
	t.Helper()
	a, err := f.signer.Sign(node, method, uri)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

const logsURI = "/api/v1/logs/web/app?follow=true"

func TestVerify_AcceptsAssertionForThisRequest(t *testing.T) {
	f := newFixture(t, "node-a")
	c, err := f.verifier.Verify(f.sign(t, "node-a", "GET", logsURI), "GET", logsURI)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Node != "node-a" || c.URI != logsURI {
		t.Errorf("claims = %+v", c)
	}
}

func TestVerify_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *fixture) (assertion, method, uri string)
		want   error
	}{
		{"missing", func(*testing.T, *fixture) (string, string, string) { return "", "GET", logsURI }, ErrMissing},
		{"another node", func(t *testing.T, f *fixture) (string, string, string) {
			return f.sign(t, "node-b", "GET", logsURI), "GET", logsURI
		}, ErrWrongNode},
		{"another path", func(t *testing.T, f *fixture) (string, string, string) {
			return f.sign(t, "node-a", "GET", logsURI), "GET", "/api/v1/forward/web/db/5432"
		}, ErrWrongRoute},
		{"another query", func(t *testing.T, f *fixture) (string, string, string) {
			return f.sign(t, "node-a", "GET", logsURI), "GET", "/api/v1/logs/web/app"
		}, ErrWrongRoute},
		{"another method", func(t *testing.T, f *fixture) (string, string, string) {
			return f.sign(t, "node-a", "GET", logsURI), "POST", logsURI
		}, ErrWrongRoute},
		{"expired", func(t *testing.T, f *fixture) (string, string, string) {
			a := f.sign(t, "node-a", "GET", logsURI)
			f.clock = f.clock.Add(DefaultTTL + DefaultSkew)
			return a, "GET", logsURI
		}, ErrExpired},
		{"from the future", func(t *testing.T, f *fixture) (string, string, string) {
			f.clock = f.clock.Add(DefaultSkew + time.Second)
			a := f.sign(t, "node-a", "GET", logsURI)
			f.clock = f.clock.Add(-(DefaultSkew + time.Second))
			return a, "GET", logsURI
		}, ErrNotYet},
		{"another key", func(t *testing.T, f *fixture) (string, string, string) {
			other := NewSigner(newKey(t))
			other.now = f.signer.now
			a, err := other.Sign("node-a", "GET", logsURI)
			if err != nil {
				t.Fatal(err)
			}
			return a, "GET", logsURI
		}, ErrSignature},
		{"tampered claims", func(t *testing.T, f *fixture) (string, string, string) {
			// node-a's claims under the signature made for node-b's.
			_, sig, _ := strings.Cut(f.sign(t, "node-b", "GET", logsURI), ".")
			body, _, _ := strings.Cut(f.sign(t, "node-a", "GET", logsURI), ".")
			return body + "." + sig, "GET", logsURI
		}, ErrSignature},
		{"garbage", func(*testing.T, *fixture) (string, string, string) { return "not-an-assertion", "GET", logsURI }, ErrMalformed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, "node-a")
			a, method, uri := tc.mutate(t, f)
			if _, err := f.verifier.Verify(a, method, uri); !errors.Is(err, tc.want) {
				t.Fatalf("Verify err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestVerify_AcceptsWithinSkew(t *testing.T) {
	f := newFixture(t, "node-a")
	a := f.sign(t, "node-a", "GET", logsURI)
	f.clock = f.clock.Add(DefaultTTL + DefaultSkew - time.Second)
	if _, err := f.verifier.Verify(a, "GET", logsURI); err != nil {
		t.Fatalf("an assertion %v past its expiry should still verify: %v", DefaultSkew-time.Second, err)
	}

	f = newFixture(t, "node-a")
	f.clock = f.clock.Add(DefaultSkew)
	a = f.sign(t, "node-a", "GET", logsURI)
	f.clock = f.clock.Add(-DefaultSkew)
	if _, err := f.verifier.Verify(a, "GET", logsURI); err != nil {
		t.Fatalf("an assertion made %v ahead should verify: %v", DefaultSkew, err)
	}
}

func TestVerify_RefusesReplay(t *testing.T) {
	f := newFixture(t, "node-a")
	a := f.sign(t, "node-a", "GET", logsURI)
	if _, err := f.verifier.Verify(a, "GET", logsURI); err != nil {
		t.Fatal(err)
	}
	if _, err := f.verifier.Verify(a, "GET", logsURI); !errors.Is(err, ErrReplayed) {
		t.Fatalf("second use err = %v, want ErrReplayed", err)
	}
}

func TestVerify_ForgetsExpiredIDs(t *testing.T) {
	f := newFixture(t, "node-a")
	if _, err := f.verifier.Verify(f.sign(t, "node-a", "GET", logsURI), "GET", logsURI); err != nil {
		t.Fatal(err)
	}
	f.clock = f.clock.Add(DefaultTTL + DefaultSkew)
	if _, err := f.verifier.Verify(f.sign(t, "node-a", "GET", logsURI), "GET", logsURI); err != nil {
		t.Fatal(err)
	}
	if n := len(f.verifier.seen); n != 1 {
		t.Errorf("verifier remembers %d assertion IDs, want only the live one", n)
	}
}

func TestVerify_NoKeyYet(t *testing.T) {
	f := newFixture(t, "node-a")
	v := NewVerifier("node-a", &KeyHolder{})
	if _, err := v.Verify(f.sign(t, "node-a", "GET", logsURI), "GET", logsURI); !errors.Is(err, ErrNoKey) {
		t.Fatalf("err = %v, want ErrNoKey", err)
	}
}

func TestPublicKeyRoundTrip(t *testing.T) {
	pub := NewSigner(newKey(t)).PublicKey()
	got, err := DecodePublicKey(EncodePublicKey(pub))
	if err != nil || !got.Equal(pub) {
		t.Fatalf("round trip = %v, %v", got, err)
	}
	if _, err := DecodePublicKey(EncodePublicKey(pub[:10])); err == nil {
		t.Fatal("a short key decoded")
	}
}

// TestMiddlewareAndTransport runs a signed request through the real HTTP
// stack, so the URI the Transport signs is the one the Agent sees.
func TestMiddlewareAndTransport(t *testing.T) {
	signer := NewSigner(newKey(t))
	keys := &KeyHolder{}
	keys.Set(signer.PublicKey())
	reached := 0
	agent := httptest.NewServer(Middleware(NewVerifier("node-a", keys), zap.NewNop(),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached++ })))
	defer agent.Close()

	get := func(rt http.RoundTripper, path string) int {
		t.Helper()
		resp, err := (&http.Client{Transport: rt}).Get(agent.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	path := "/api/v1/logs/web/my%20app?tail=10&follow=true"
	if got := get(&Transport{Signer: signer, Node: "node-a"}, path); got != http.StatusOK {
		t.Fatalf("signed request: status %d", got)
	}
	if got := get(http.DefaultTransport, path); got != http.StatusUnauthorized {
		t.Fatalf("unsigned request: status %d, want 401", got)
	}
	if got := get(&Transport{Signer: signer, Node: "node-b"}, path); got != http.StatusUnauthorized {
		t.Fatalf("request signed for node-b: status %d, want 401", got)
	}
	if got := get(http.DefaultTransport, HealthPath); got != http.StatusOK {
		t.Fatalf("unsigned /healthz: status %d, want 200", got)
	}
	if reached != 2 {
		t.Errorf("handler reached %d times, want 2 (the signed request and /healthz)", reached)
	}

	keys2 := &KeyHolder{}
	waiting := httptest.NewServer(Middleware(NewVerifier("node-a", keys2), zap.NewNop(), http.NotFoundHandler()))
	defer waiting.Close()
	resp, err := (&http.Client{Transport: &Transport{Signer: signer, Node: "node-a"}}).Get(waiting.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("before the key is fetched: status %d, want 503", resp.StatusCode)
	}
}
