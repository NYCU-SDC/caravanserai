package objectstore

import (
	"fmt"
	"strings"
)

// ParseEndpoint splits a scheme-qualified endpoint URL (as configured by
// AgentConfig.S3.Endpoint, e.g. "https://minio.internal:9000") into the
// bare host[:port] MinioConfig.Endpoint expects and the Secure flag TLS is
// derived from. There is no separate "use TLS" setting anywhere in cara's S3
// configuration — the scheme is the single source of truth for it, so this
// is the one place that decoding happens.
func ParseEndpoint(rawEndpoint string) (host string, secure bool, err error) {
	scheme, rest, found := strings.Cut(rawEndpoint, "://")
	if !found {
		return "", false, fmt.Errorf("objectstore: endpoint %q must start with http:// or https://", rawEndpoint)
	}
	switch scheme {
	case "https":
		secure = true
	case "http":
		secure = false
	default:
		return "", false, fmt.Errorf("objectstore: endpoint %q must start with http:// or https://", rawEndpoint)
	}
	if rest == "" {
		return "", false, fmt.Errorf("objectstore: endpoint %q has no host", rawEndpoint)
	}
	return rest, secure, nil
}
