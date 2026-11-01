package controller

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"

	"go.uber.org/zap"
)

// dnsRecordTypeA is the only record type the DNS controller publishes. Node
// overlay addresses come from Headscale's IPv4 prefix (100.64.0.0/10).
const dnsRecordTypeA = "A"

// dnsRecordPhases are the Project phases that publish DNS records. A Project
// in one of them has a NodeRef, and the Node may still serve its containers:
//
//   - Scheduled: containers are starting; publishing early means the name
//     already resolves when the Project reaches Running.
//   - Running: the normal case.
//   - Failed: Failed does not mean every container stopped, and the record
//     follows NodeRef until a move clears it.
//
// Pending has no Node yet. Terminating and Terminated are being deleted.
var dnsRecordPhases = []v1.ProjectPhase{
	v1.ProjectPhaseScheduled,
	v1.ProjectPhaseRunning,
	v1.ProjectPhaseFailed,
}

// DNSRecord is one entry of the Headscale extra records file. The JSON shape
// matches what Headscale reads from dns.extra_records_path.
type DNSRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// DNSProjectSnapshot is the minimal view of a Project the DNS controller needs.
type DNSProjectSnapshot struct {
	Name      string
	Phase     v1.ProjectPhase
	NodeRef   string
	CreatedAt time.Time
	Ingress   []v1.IngressDef
}

// HostnameFunc returns the full hostname for one ingress of a Project. It
// returns an error when the ingress has no valid hostname (for example an
// empty host, or a name outside the configured base domain). The DNS
// controller skips such an ingress instead of failing the whole file.
//
// cara-server computes the hostname in exactly one function, so that the DNS
// record name and the Traefik Host rule can never disagree.
type HostnameFunc func(project DNSProjectSnapshot, ingress v1.IngressDef) (string, error)

// buildDNSRecords turns Projects and Node overlay IPs into the sorted list of
// records to publish. nodeIPs maps a Node name to its overlay IP.
//
// It never fails. Anything it cannot publish is skipped with a warning, so
// one bad Project cannot remove every other Project's name:
//
//   - a Project outside dnsRecordPhases, or with no NodeRef
//   - a Node with no overlay IP, or one that is not a valid IPv4 address
//   - an ingress whose scope is not Internal (an empty scope means Internal)
//   - an ingress whose hostname cannot be computed
//   - a hostname already published, by an older Project (the oldest one
//     wins) or by an earlier ingress of the same Project
func buildDNSRecords(
	logger *zap.Logger,
	projects []DNSProjectSnapshot,
	nodeIPs map[string]string,
	hostname HostnameFunc,
) []DNSRecord {
	ordered := sortByAge(projects)
	owners := make(map[string]string) // hostname -> Project that claimed it
	records := make([]DNSRecord, 0)

	for _, p := range ordered {
		log := logger.With(zap.String("project", p.Name))

		ip, ok := projectNodeIP(log, p, nodeIPs)
		if !ok {
			continue
		}

		for _, ing := range p.Ingress {
			name, ok := ingressHostname(log, p, ing, hostname)
			if !ok {
				continue
			}
			if owner, taken := owners[name]; taken {
				log.Warn("DNS: hostname already published, skipping",
					zap.String("ingress", ing.Name),
					zap.String("hostname", name),
					zap.String("owner", owner),
				)
				continue
			}
			owners[name] = p.Name
			records = append(records, DNSRecord{Name: name, Type: dnsRecordTypeA, Value: ip})
		}
	}

	slices.SortFunc(records, func(a, b DNSRecord) int {
		return strings.Compare(a.Name, b.Name)
	})
	return records
}

// sortByAge returns a copy of projects ordered oldest first, with the name as
// a tie-breaker so the order is stable across runs.
func sortByAge(projects []DNSProjectSnapshot) []DNSProjectSnapshot {
	ordered := slices.Clone(projects)
	slices.SortFunc(ordered, func(a, b DNSProjectSnapshot) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return ordered
}

// projectNodeIP returns the overlay IP that p's records should point to, or
// false when p publishes no records.
func projectNodeIP(log *zap.Logger, p DNSProjectSnapshot, nodeIPs map[string]string) (string, bool) {
	if !slices.Contains(dnsRecordPhases, p.Phase) || p.NodeRef == "" {
		return "", false
	}
	if len(p.Ingress) == 0 {
		return "", false
	}

	raw := nodeIPs[p.NodeRef]
	if raw == "" {
		log.Warn("DNS: node has no overlay IP, skipping project", zap.String("node", p.NodeRef))
		return "", false
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil || !addr.Unmap().Is4() {
		log.Warn("DNS: node overlay IP is not an IPv4 address, skipping project",
			zap.String("node", p.NodeRef),
			zap.String("overlay_ip", raw),
		)
		return "", false
	}
	return addr.Unmap().String(), true
}

// ingressHostname returns the hostname to publish for ing, or false when ing
// publishes no record.
func ingressHostname(log *zap.Logger, p DNSProjectSnapshot, ing v1.IngressDef, hostname HostnameFunc) (string, bool) {
	if ing.Access.Scope != "" && ing.Access.Scope != v1.IngressScopeInternal {
		return "", false
	}
	name, err := hostname(p, ing)
	if err != nil {
		log.Warn("DNS: ingress has no valid hostname, skipping",
			zap.String("ingress", ing.Name),
			zap.Error(err),
		)
		return "", false
	}
	return name, true
}

// renderDNSRecords encodes records as the Headscale extra records file. The
// output is byte-for-byte stable for the same input, so an unchanged record
// set never rewrites the file. An empty set renders as "[]", never "null".
func renderDNSRecords(records []DNSRecord) ([]byte, error) {
	if records == nil {
		records = []DNSRecord{}
	}
	body, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("dns: marshal records: %w", err)
	}
	return append(body, '\n'), nil
}
