// Package keyref derives non-secret references from a Headscale pre-auth key.
//
// A pre-auth key is secret material: it must never be logged or persisted in
// full. Both cara-server (when it issues a key) and cara-agent (when it joins
// the overlay) derive the same reference from the raw key so the server can
// map a heartbeat back to the Cara Node the key was issued for (CARA-68).
//
// The package deliberately has no other dependencies so it can be imported by
// both the agent and the server without pulling in tsnet/tailscale.
package keyref

import (
	"crypto/sha256"
	"encoding/hex"
)

// PrefixLen is how many leading characters of the raw pre-auth key are retained
// as a non-secret prefix for operator-facing audit and log correlation. It is
// short enough that it cannot be used to reconstruct or brute-force the key.
const PrefixLen = 8

// Hash derives a stable reference for a raw pre-auth key. hash is the hex
// SHA-256 of the full key and is used as the durable lookup key; prefix is the
// first PrefixLen characters of the raw key, kept only for audit. The full key
// is never returned. Both server and agent call this so their references match.
func Hash(key string) (hash, prefix string) {
	sum := sha256.Sum256([]byte(key))
	hash = hex.EncodeToString(sum[:])
	prefix = key
	if len(prefix) > PrefixLen {
		prefix = prefix[:PrefixLen]
	}
	return hash, prefix
}
