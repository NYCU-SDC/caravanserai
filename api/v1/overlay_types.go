package v1

import "time"

// Overlay administration DTOs. These are request/response shapes for the
// /api/v1/overlay endpoints (CARA-49) shared between cara-server and caractl.
// They are not managed resources like Node/Project/Secret, so they are not part
// of the JSON Schema generation pipeline.

// CreatePreAuthKeyRequest is the body of POST /api/v1/overlay/preauth-keys.
type CreatePreAuthKeyRequest struct {
	// Node is the Cara node name the key is intended for. It is recorded for
	// operator context; the durable key→node mapping is CARA-68.
	Node string `json:"node,omitempty"`
	// TTL is how long the key stays valid, as a Go duration string (e.g. "24h").
	// Empty means the server default.
	TTL string `json:"ttl,omitempty"`
}

// PreAuthKeyResponse is returned by POST /api/v1/overlay/preauth-keys. The key
// is shown once and is never logged.
type PreAuthKeyResponse struct {
	Key        string    `json:"key"`
	Expiration time.Time `json:"expiration,omitempty"`
}

// OverlayNode is a single node as seen by Headscale.
type OverlayNode struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	IPAddresses []string `json:"ipAddresses,omitempty"`
	Online      bool     `json:"online"`
}

// OverlayNodeList is returned by GET /api/v1/overlay/nodes.
type OverlayNodeList struct {
	Nodes []OverlayNode `json:"nodes"`
}

// RevokeNodeResponse reports the outcome of DELETE /api/v1/overlay/nodes/{name}.
// Both booleans are surfaced so a partial failure (drift) is never silent.
type RevokeNodeResponse struct {
	// HeadscaleRemoved is true when the node is gone from Headscale (including
	// the case where it was already absent).
	HeadscaleRemoved bool `json:"headscaleRemoved"`
	// StoreRemoved is true when the node is gone from the Cara node store.
	StoreRemoved bool `json:"storeRemoved"`
	// Drift is true when the two views disagree after the operation, i.e. one
	// side was removed but the other was not.
	Drift bool `json:"drift,omitempty"`
	// Message explains the outcome, especially any drift, in operator terms.
	Message string `json:"message,omitempty"`
}
