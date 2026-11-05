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
