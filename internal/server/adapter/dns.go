package adapter

import (
	"context"
	"slices"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/server/controller"
	pgstore "NYCU-SDC/caravanserai/internal/store/postgres"
)

// Compile-time interface satisfaction check.
var _ controller.DNSStore = (*DNSStoreAdapter)(nil)

// DNSStoreAdapter wraps *pgstore.Store and satisfies controller.DNSStore.
type DNSStoreAdapter struct {
	s *pgstore.Store
}

// NewDNSStoreAdapter returns a DNSStoreAdapter backed by s.
func NewDNSStoreAdapter(s *pgstore.Store) *DNSStoreAdapter {
	return &DNSStoreAdapter{s: s}
}

// ListDNSProjects satisfies controller.DNSStore.
func (a *DNSStoreAdapter) ListDNSProjects(ctx context.Context, phases []v1.ProjectPhase) ([]controller.DNSProjectSnapshot, error) {
	projects, err := a.s.ListProjectsByPhases(ctx, phases)
	if err != nil {
		return nil, err
	}
	snapshots := make([]controller.DNSProjectSnapshot, len(projects))
	for i, p := range projects {
		snapshots[i] = controller.DNSProjectSnapshot{
			Name:      p.Name,
			Phase:     p.Status.Phase,
			NodeRef:   p.Status.NodeRef,
			CreatedAt: p.CreatedAt,
			Ingress:   slices.Clone(p.Spec.Ingress),
		}
	}
	return snapshots, nil
}

// ListNodeOverlayIPs satisfies controller.DNSStore.
func (a *DNSStoreAdapter) ListNodeOverlayIPs(ctx context.Context) (map[string]string, error) {
	nodes, err := a.s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	ips := make(map[string]string, len(nodes))
	for _, n := range nodes {
		ips[n.Name] = n.Status.Network.OverlayIP
	}
	return ips, nil
}
