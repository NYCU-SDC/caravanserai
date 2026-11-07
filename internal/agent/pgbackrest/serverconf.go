package pgbackrest

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strings"
)

// Authorization lets one Project lifetime use its stanzas through the repo
// host, reaching it over the Project's own network.
type Authorization struct {
	Stanzas []string `json:"stanzas"`
	Network string   `json:"network"`
}

// authorizations is the durable desired state of the repo host, keyed by
// Project UID. It is the only input the server configuration is built from.
type authorizations map[string]Authorization

// S3 is the repository the repo host stores stanzas in.
type S3 struct {
	// Endpoint must be https://host[:port]: pgBackRest has no plain-HTTP
	// transport for S3.
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
	// CAFile, when set, is a CA certificate the endpoint's certificate is
	// signed by, for an object store with a private CA.
	CAFile string
}

// ErrRepositoryNotHTTPS reports an object store pgBackRest cannot reach.
var ErrRepositoryNotHTTPS = errors.New("pgbackrest: the object store must be reached over https")

// RepositoryPath is where stanzas live in the bucket.
const RepositoryPath = "/cara/v2/pgbackrest"

// noClient keeps tls-server-auth non-empty when no Project is authorized: the
// server refuses to start without the option, and no certificate is ever
// issued for this CN, which is never a Project UID.
const noClient = "cara-no-client=cara-no-stanza"

// Paths inside the repo host container.
const (
	serverTLSDir = "/etc/cara/pgbackrest/tls"
	s3CAName     = "s3-ca.crt"
)

// renderServerConfig builds the repo host configuration from the complete set
// of authorizations. Output is deterministic: the same state always renders
// the same bytes, so an unchanged state never causes a reload.
func renderServerConfig(auth authorizations, s3 S3) ([]byte, error) {
	endpoint, err := url.Parse(s3.Endpoint)
	if err != nil || endpoint.Host == "" {
		return nil, fmt.Errorf("pgbackrest: object store endpoint %q: %w", s3.Endpoint, ErrRepositoryNotHTTPS)
	}
	if endpoint.Scheme != "https" {
		return nil, fmt.Errorf("pgbackrest: object store endpoint %q: %w", s3.Endpoint, ErrRepositoryNotHTTPS)
	}

	var b strings.Builder
	line := func(k, v string) { fmt.Fprintf(&b, "%s=%s\n", k, v) }
	b.WriteString("[global]\n")
	line("tls-server-address", "*")
	line("tls-server-ca-file", serverTLSDir+"/ca.crt")
	line("tls-server-cert-file", serverTLSDir+"/server.crt")
	line("tls-server-key-file", serverTLSDir+"/server.key")

	uids := make([]string, 0, len(auth))
	for uid := range auth {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	written := 0
	for _, uid := range uids {
		stanzas := slices.Clone(auth[uid].Stanzas)
		sort.Strings(stanzas)
		stanzas = slices.Compact(stanzas)
		if len(stanzas) == 0 {
			continue
		}
		line("tls-server-auth", uid+"="+strings.Join(stanzas, ","))
		written++
	}
	if written == 0 {
		line("tls-server-auth", noClient)
	}

	line("repo1-type", "s3")
	line("repo1-path", RepositoryPath)
	line("repo1-s3-bucket", s3.Bucket)
	line("repo1-s3-endpoint", endpoint.Hostname())
	if port := endpoint.Port(); port != "" {
		line("repo1-storage-port", port)
	}
	region := s3.Region
	if region == "" {
		region = "us-east-1"
	}
	line("repo1-s3-region", region)
	line("repo1-s3-key", s3.AccessKey)
	line("repo1-s3-key-secret", s3.SecretKey)
	line("repo1-s3-uri-style", "path")
	if s3.CAFile != "" {
		line("repo1-storage-ca-file", serverTLSDir+"/"+s3CAName)
	}
	line("log-level-console", "info")
	line("log-level-file", "off")
	return []byte(b.String()), nil
}
