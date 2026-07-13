package v1

import (
	"fmt"
	"regexp"
)

// nameRegexp matches Kubernetes-style DNS subdomain names:
//   - max 253 characters
//   - lowercase alphanumeric, hyphens (-), and dots (.)
//   - must start and end with a lowercase alphanumeric character
//
// Single-character names (e.g. "a") are valid.
var nameRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ValidateName checks that name conforms to DNS subdomain naming rules.
// It returns nil when the name is valid, or a descriptive error otherwise.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("name must not be empty")
	}
	if len(name) > 253 {
		return fmt.Errorf(
			"name %q is %d characters long: must be no more than 253 characters",
			truncate(name, 20), len(name),
		)
	}
	if !nameRegexp.MatchString(name) {
		return fmt.Errorf(
			"name %q is invalid: must consist of lowercase alphanumeric characters, hyphens (-) or dots (.), and must start and end with an alphanumeric character",
			truncate(name, 40),
		)
	}
	return nil
}

// DefaultNamespace is the only namespace value accepted for 1.0.
const DefaultNamespace = "default"

// ValidateNamespace rejects any metadata.namespace other than "" or
// "default". 1.0 locks all resources to DefaultNamespace; remove this check
// together with namespace-qualified routes and work-queue keys once
// namespace support lands post-1.0.
func ValidateNamespace(namespace string) error {
	if namespace != "" && namespace != DefaultNamespace {
		return fmt.Errorf("namespace support lands post-1.0; omit the field or use %q", DefaultNamespace)
	}
	return nil
}

// truncate shortens s to at most maxLen characters, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
