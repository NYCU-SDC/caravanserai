package v1

import "github.com/invopop/jsonschema"

// SecretDataItem is a single key/value entry in a Secret. Data is modelled as
// an ordered slice rather than a map so the on-disk/JSON representation is
// stable and diff-friendly.
type SecretDataItem struct {
	Key   string `json:"key"   yaml:"key"`
	Value string `json:"value" yaml:"value"`
}

// SecretSpec is the desired state of a Secret: a flat list of key/value pairs.
type SecretSpec struct {
	// Data holds the secret's key/value pairs. Values are stored as plaintext
	// in the backend for 1.0 (a known limitation; the transport is secured by
	// Headscale and the backend moves to Infisical later).
	Data []SecretDataItem `json:"data" yaml:"data"`
}

// SecretStatus is intentionally empty: Secrets have no lifecycle phases and no
// controller-observed state. It exists only to keep the resource shape uniform
// with Node/Project.
type SecretStatus struct{}

// Secret is a namespaced resource holding sensitive configuration (database
// passwords, API keys, webhook URLs) referenced by a Project's
// EnvVar.valueFrom.secretKeyRef. It is pure CRUD — no phases, no controller.
type Secret struct {
	TypeMeta   `json:",inline"    yaml:",inline"`
	ObjectMeta `json:"metadata"   yaml:"metadata"`
	Spec       SecretSpec   `json:"spec,omitempty"   yaml:"spec,omitempty"`
	Status     SecretStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// JSONSchemaExtend sets the top-level title for the Secret schema.
func (Secret) JSONSchemaExtend(s *jsonschema.Schema) {
	s.Title = "Secret"
}

// SecretList is a collection of Secret objects returned by list operations.
type SecretList struct {
	TypeMeta `json:",inline"  yaml:",inline"`
	Items    []Secret `json:"items" yaml:"items"`
}
