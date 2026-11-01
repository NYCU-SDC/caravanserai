package controller

import (
	"errors"
	"testing"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// errNoHostname is returned by fakeHostname for an empty host.
var errNoHostname = errors.New("host is empty")

// fakeHostname stands in for the server's real hostname function, which is
// not written yet. It appends ".dev.cara.test" to the host and rejects an
// empty host, which is enough to exercise every branch of buildDNSRecords.
func fakeHostname(_ DNSProjectSnapshot, ing v1.IngressDef) (string, error) {
	if ing.Host == "" {
		return "", errNoHostname
	}
	return ing.Host + ".dev.cara.test", nil
}

// dnsIngress returns an ingress named after host with the given scope.
func dnsIngress(host string, scope v1.IngressScope) v1.IngressDef {
	return v1.IngressDef{
		Name:   host + "-ing",
		Host:   host,
		Target: v1.IngressTarget{Service: "web", Port: 8080},
		Access: v1.IngressAccess{Scope: scope},
	}
}

// dnsProject returns a Project on node-a, created at testBaseTime plus age,
// with one Internal ingress per host.
func dnsProject(name string, phase v1.ProjectPhase, age time.Duration, hosts ...string) DNSProjectSnapshot {
	p := DNSProjectSnapshot{
		Name:      name,
		Phase:     phase,
		NodeRef:   "node-a",
		CreatedAt: testBaseTime.Add(age),
	}
	for _, h := range hosts {
		p.Ingress = append(p.Ingress, dnsIngress(h, v1.IngressScopeInternal))
	}
	return p
}

func TestBuildDNSRecords_Phases(t *testing.T) {
	tests := []struct {
		phase       v1.ProjectPhase
		wantRecords bool
	}{
		{phase: v1.ProjectPhasePending, wantRecords: false},
		{phase: v1.ProjectPhaseScheduled, wantRecords: true},
		{phase: v1.ProjectPhaseRunning, wantRecords: true},
		{phase: v1.ProjectPhaseFailed, wantRecords: true},
		{phase: v1.ProjectPhaseTerminating, wantRecords: false},
		{phase: v1.ProjectPhaseTerminated, wantRecords: false},
	}

	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			projects := []DNSProjectSnapshot{dnsProject("blog", tt.phase, 0, "www")}
			nodeIPs := map[string]string{"node-a": "100.64.0.5"}

			got := buildDNSRecords(zap.NewNop(), projects, nodeIPs, fakeHostname)

			if tt.wantRecords {
				assert.Equal(t, []DNSRecord{{Name: "www.dev.cara.test", Type: "A", Value: "100.64.0.5"}}, got)
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

func TestBuildDNSRecords_Skips(t *testing.T) {
	tests := []struct {
		name     string
		project  DNSProjectSnapshot
		nodeIPs  map[string]string
		wantWarn string // empty means the skip is silent
	}{
		{
			name: "no NodeRef",
			project: func() DNSProjectSnapshot {
				p := dnsProject("blog", v1.ProjectPhaseRunning, 0, "www")
				p.NodeRef = ""
				return p
			}(),
			nodeIPs: map[string]string{"node-a": "100.64.0.5"},
		},
		{
			name:    "no ingress",
			project: dnsProject("blog", v1.ProjectPhaseRunning, 0),
			nodeIPs: map[string]string{"node-a": "100.64.0.5"},
		},
		{
			name:     "node missing from the node list",
			project:  dnsProject("blog", v1.ProjectPhaseRunning, 0, "www"),
			nodeIPs:  map[string]string{},
			wantWarn: "DNS: node has no overlay IP, skipping project",
		},
		{
			name:     "node has an empty overlay IP",
			project:  dnsProject("blog", v1.ProjectPhaseRunning, 0, "www"),
			nodeIPs:  map[string]string{"node-a": ""},
			wantWarn: "DNS: node has no overlay IP, skipping project",
		},
		{
			name:     "overlay IP is not an address",
			project:  dnsProject("blog", v1.ProjectPhaseRunning, 0, "www"),
			nodeIPs:  map[string]string{"node-a": "not-an-ip"},
			wantWarn: "DNS: node overlay IP is not an IPv4 address, skipping project",
		},
		{
			name:     "overlay IP is IPv6",
			project:  dnsProject("blog", v1.ProjectPhaseRunning, 0, "www"),
			nodeIPs:  map[string]string{"node-a": "fd7a:115c:a1e0::1"},
			wantWarn: "DNS: node overlay IP is not an IPv4 address, skipping project",
		},
		{
			name: "scope is not Internal",
			project: func() DNSProjectSnapshot {
				p := dnsProject("blog", v1.ProjectPhaseRunning, 0)
				p.Ingress = []v1.IngressDef{dnsIngress("www", v1.IngressScope("Public"))}
				return p
			}(),
			nodeIPs: map[string]string{"node-a": "100.64.0.5"},
		},
		{
			name:     "hostname function rejects the ingress",
			project:  dnsProject("blog", v1.ProjectPhaseRunning, 0, ""),
			nodeIPs:  map[string]string{"node-a": "100.64.0.5"},
			wantWarn: "DNS: ingress has no valid hostname, skipping",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)

			got := buildDNSRecords(zap.New(core), []DNSProjectSnapshot{tt.project}, tt.nodeIPs, fakeHostname)

			assert.Empty(t, got)
			if tt.wantWarn == "" {
				assert.Zero(t, logs.Len(), "expected a silent skip")
				return
			}
			assert.Equal(t, 1, logs.FilterMessage(tt.wantWarn).Len())
		})
	}
}

func TestBuildDNSRecords_EmptyScopeMeansInternal(t *testing.T) {
	p := dnsProject("blog", v1.ProjectPhaseRunning, 0)
	p.Ingress = []v1.IngressDef{dnsIngress("www", "")}

	got := buildDNSRecords(zap.NewNop(), []DNSProjectSnapshot{p}, map[string]string{"node-a": "100.64.0.5"}, fakeHostname)

	assert.Equal(t, []DNSRecord{{Name: "www.dev.cara.test", Type: "A", Value: "100.64.0.5"}}, got)
}

func TestBuildDNSRecords_IPv4MappedIPv6IsUnmapped(t *testing.T) {
	p := dnsProject("blog", v1.ProjectPhaseRunning, 0, "www")

	got := buildDNSRecords(zap.NewNop(), []DNSProjectSnapshot{p}, map[string]string{"node-a": "::ffff:100.64.0.5"}, fakeHostname)

	require.Len(t, got, 1)
	assert.Equal(t, "100.64.0.5", got[0].Value)
}

func TestBuildDNSRecords_OneBadIngressKeepsTheOthers(t *testing.T) {
	p := dnsProject("blog", v1.ProjectPhaseRunning, 0, "www", "", "api")

	got := buildDNSRecords(zap.NewNop(), []DNSProjectSnapshot{p}, map[string]string{"node-a": "100.64.0.5"}, fakeHostname)

	assert.Equal(t, []DNSRecord{
		{Name: "api.dev.cara.test", Type: "A", Value: "100.64.0.5"},
		{Name: "www.dev.cara.test", Type: "A", Value: "100.64.0.5"},
	}, got)
}

func TestBuildDNSRecords_DuplicateHostnames(t *testing.T) {
	tests := []struct {
		name      string
		projects  []DNSProjectSnapshot
		wantValue string // the IP of the Project that keeps the name
	}{
		{
			name: "older project wins regardless of input order",
			projects: []DNSProjectSnapshot{
				onNode(dnsProject("newer", v1.ProjectPhaseRunning, time.Hour, "www"), "node-b"),
				onNode(dnsProject("older", v1.ProjectPhaseRunning, 0, "www"), "node-a"),
			},
			wantValue: "100.64.0.5",
		},
		{
			name: "same creation time falls back to project name",
			projects: []DNSProjectSnapshot{
				onNode(dnsProject("zeta", v1.ProjectPhaseRunning, 0, "www"), "node-b"),
				onNode(dnsProject("alpha", v1.ProjectPhaseRunning, 0, "www"), "node-a"),
			},
			wantValue: "100.64.0.5",
		},
		{
			name: "two ingresses of one project share a host",
			projects: []DNSProjectSnapshot{
				dnsProject("blog", v1.ProjectPhaseRunning, 0, "www", "www"),
			},
			wantValue: "100.64.0.5",
		},
	}

	nodeIPs := map[string]string{"node-a": "100.64.0.5", "node-b": "100.64.0.6"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.WarnLevel)

			got := buildDNSRecords(zap.New(core), tt.projects, nodeIPs, fakeHostname)

			assert.Equal(t, []DNSRecord{{Name: "www.dev.cara.test", Type: "A", Value: tt.wantValue}}, got)
			assert.Equal(t, 1, logs.FilterMessage("DNS: hostname already published, skipping").Len())
		})
	}
}

func TestBuildDNSRecords_DoesNotReorderInput(t *testing.T) {
	projects := []DNSProjectSnapshot{
		dnsProject("newer", v1.ProjectPhaseRunning, time.Hour, "b"),
		dnsProject("older", v1.ProjectPhaseRunning, 0, "a"),
	}

	_ = buildDNSRecords(zap.NewNop(), projects, map[string]string{"node-a": "100.64.0.5"}, fakeHostname)

	assert.Equal(t, "newer", projects[0].Name, "input slice was mutated")
}

func TestBuildDNSRecords_SortedByName(t *testing.T) {
	projects := []DNSProjectSnapshot{
		dnsProject("p1", v1.ProjectPhaseRunning, 0, "zulu", "alpha"),
		onNode(dnsProject("p2", v1.ProjectPhaseRunning, time.Minute, "mike"), "node-b"),
	}
	nodeIPs := map[string]string{"node-a": "100.64.0.5", "node-b": "100.64.0.6"}

	got := buildDNSRecords(zap.NewNop(), projects, nodeIPs, fakeHostname)

	assert.Equal(t, []DNSRecord{
		{Name: "alpha.dev.cara.test", Type: "A", Value: "100.64.0.5"},
		{Name: "mike.dev.cara.test", Type: "A", Value: "100.64.0.6"},
		{Name: "zulu.dev.cara.test", Type: "A", Value: "100.64.0.5"},
	}, got)
}

func TestRenderDNSRecords(t *testing.T) {
	tests := []struct {
		name    string
		records []DNSRecord
		want    string
	}{
		{name: "nil renders an empty array", records: nil, want: "[]\n"},
		{name: "empty renders an empty array", records: []DNSRecord{}, want: "[]\n"},
		{
			name: "records render in Headscale's format",
			records: []DNSRecord{
				{Name: "wiki.dev.cara.test", Type: "A", Value: "100.64.0.7"},
				{Name: "www.dev.cara.test", Type: "A", Value: "100.64.0.5"},
			},
			want: `[
  {
    "name": "wiki.dev.cara.test",
    "type": "A",
    "value": "100.64.0.7"
  },
  {
    "name": "www.dev.cara.test",
    "type": "A",
    "value": "100.64.0.5"
  }
]
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := renderDNSRecords(tt.records)

			require.NoError(t, err)
			assert.Equal(t, tt.want, string(got))
		})
	}
}

// onNode returns p moved to node.
func onNode(p DNSProjectSnapshot, node string) DNSProjectSnapshot {
	p.NodeRef = node
	return p
}
