package v1

import "fmt"

// LabelNodeTier is the Node label that places a Node in a scheduling tier.
// The scheduler prefers primary Nodes and falls back to backup Nodes
// (docs/scheduler-strategy.md §3.2).
const LabelNodeTier = "cara.io/tier"

// NodeTier is the value of the LabelNodeTier label.
type NodeTier string

const (
	// NodeTierPrimary marks a Node as the normal home for Projects.
	NodeTierPrimary NodeTier = "primary"
	// NodeTierBackup marks a Node as a failover home for Projects.
	NodeTierBackup NodeTier = "backup"
)

// DefaultNodeTier is the effective tier of a Node without a LabelNodeTier
// label. Defaulting to backup keeps an unlabeled Node from attracting
// Projects ahead of the Nodes an operator has marked primary. Interim value
// pending product sign-off (docs/scheduler-strategy.md §3.2).
const DefaultNodeTier = NodeTierBackup

// IsValid reports whether t is one of the defined tiers.
func (t NodeTier) IsValid() bool {
	return t == NodeTierPrimary || t == NodeTierBackup
}

// ValidateNodeLabels checks the labels that cara interprets on a Node. Only
// LabelNodeTier is checked; other labels are free-form. It returns nil when
// the labels are valid, or a descriptive error otherwise.
func ValidateNodeLabels(labels map[string]string) error {
	tier, ok := labels[LabelNodeTier]
	if !ok {
		return nil
	}
	if !NodeTier(tier).IsValid() {
		return fmt.Errorf("label %s must be %q or %q, got %q",
			LabelNodeTier, NodeTierPrimary, NodeTierBackup, tier)
	}
	return nil
}

// NodeTierFromLabels returns the tier in labels' LabelNodeTier, or
// DefaultNodeTier when the label is unset. Labels are validated on write, so an
// invalid value is not expected here; it also resolves to DefaultNodeTier.
func NodeTierFromLabels(labels map[string]string) NodeTier {
	if tier := NodeTier(labels[LabelNodeTier]); tier.IsValid() {
		return tier
	}
	return DefaultNodeTier
}

// EffectiveTier returns the Node's tier; see NodeTierFromLabels.
func (n Node) EffectiveTier() NodeTier {
	return NodeTierFromLabels(n.Labels)
}
