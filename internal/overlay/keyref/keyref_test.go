package keyref

import (
	"strings"
	"testing"
)

func TestHash_StableAndNonReversible(t *testing.T) {
	const key = "tskey-auth-abcdef0123456789"

	hash, prefix := Hash(key)

	// Deterministic: the agent and server must derive the same reference.
	hash2, prefix2 := Hash(key)
	if hash != hash2 || prefix != prefix2 {
		t.Fatalf("Hash is not deterministic: (%s,%s) != (%s,%s)", hash, prefix, hash2, prefix2)
	}

	// hex SHA-256 is 64 characters.
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}

	// The full key must never appear in the derived reference.
	if strings.Contains(hash, key) {
		t.Error("hash must not contain the raw key")
	}
	if hash == key {
		t.Error("hash must not equal the raw key")
	}

	// Prefix is bounded and is a genuine prefix of the key.
	if len(prefix) > PrefixLen {
		t.Errorf("prefix length = %d, want <= %d", len(prefix), PrefixLen)
	}
	if !strings.HasPrefix(key, prefix) {
		t.Errorf("prefix %q is not a prefix of the key", prefix)
	}
}

func TestHash_DifferentKeysDifferentHashes(t *testing.T) {
	a, _ := Hash("key-one")
	b, _ := Hash("key-two")
	if a == b {
		t.Error("distinct keys must hash to distinct references")
	}
}

func TestHash_ShortKey(t *testing.T) {
	hash, prefix := Hash("abc")
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64", len(hash))
	}
	if prefix != "abc" {
		t.Errorf("prefix = %q, want the whole short key", prefix)
	}
}
