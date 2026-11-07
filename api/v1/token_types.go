package v1

import "time"

// APITokenKind says what a token identifies: a Node's Agent or a person
// (ADR-0031).
type APITokenKind string

const (
	// APITokenKindNode identifies a Node's Agent. The token's name is the Node
	// name.
	APITokenKindNode APITokenKind = "node"
	// APITokenKindUser identifies a person. Every user is an admin until
	// roles land (ADR-0031).
	APITokenKindUser APITokenKind = "user"
)

// Valid reports whether k is a known token kind.
func (k APITokenKind) Valid() bool {
	return k == APITokenKindNode || k == APITokenKindUser
}

// APIToken describes an issued token. The token itself is shown once, when it
// is created, and only its hash is stored.
type APIToken struct {
	ID        int64        `json:"id"                  yaml:"id"`
	Kind      APITokenKind `json:"kind"                yaml:"kind"`
	Name      string       `json:"name"                yaml:"name"`
	CreatedBy string       `json:"createdBy"           yaml:"createdBy"`
	CreatedAt time.Time    `json:"createdAt"           yaml:"createdAt"`
	RevokedAt *time.Time   `json:"revokedAt,omitempty" yaml:"revokedAt,omitempty"`
}

// APITokenRequest is the body of POST /api/v1/tokens.
type APITokenRequest struct {
	Kind APITokenKind `json:"kind" yaml:"kind"`
	Name string       `json:"name" yaml:"name"`
}

// APITokenCreated is the response to POST /api/v1/tokens. Token is not
// retrievable again.
type APITokenCreated struct {
	APIToken `json:",inline" yaml:",inline"`
	Token    string `json:"token" yaml:"token"`
}

// NodeJoinToken describes a one-time token that enrols an Agent as one Node
// (CARA-138). The Agent trades it for a Node token; it works once, and not
// after ExpiresAt.
type NodeJoinToken struct {
	ID        int64      `json:"id"                  yaml:"id"`
	NodeName  string     `json:"nodeName"            yaml:"nodeName"`
	CreatedBy string     `json:"createdBy"           yaml:"createdBy"`
	CreatedAt time.Time  `json:"createdAt"           yaml:"createdAt"`
	ExpiresAt time.Time  `json:"expiresAt"           yaml:"expiresAt"`
	UsedAt    *time.Time `json:"usedAt,omitempty"    yaml:"usedAt,omitempty"`
	RevokedAt *time.Time `json:"revokedAt,omitempty" yaml:"revokedAt,omitempty"`
}

// NodeJoinTokenRequest is the body of POST /api/v1/nodes/{name}/join-tokens.
// TTL is a Go duration such as "1h"; empty means the server default.
type NodeJoinTokenRequest struct {
	TTL string `json:"ttl,omitempty" yaml:"ttl,omitempty"`
}

// NodeJoinTokenCreated is the response to POST /api/v1/nodes/{name}/join-tokens.
// Token is not retrievable again.
type NodeJoinTokenCreated struct {
	NodeJoinToken `json:",inline" yaml:",inline"`
	Token         string `json:"token" yaml:"token"`
}

// NodeJoined is the response to POST /api/v1/nodes/{name}/join: the Node token
// the Agent uses from then on.
type NodeJoined struct {
	APIToken `json:",inline" yaml:",inline"`
	Token    string `json:"token" yaml:"token"`
}

// NodeTokensRevoked is the response to DELETE /api/v1/nodes/{name}/tokens.
type NodeTokensRevoked struct {
	NodeTokens int `json:"nodeTokens" yaml:"nodeTokens"`
	JoinTokens int `json:"joinTokens" yaml:"joinTokens"`
}
