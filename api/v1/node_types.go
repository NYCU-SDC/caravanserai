package v1

import "time"

// NetworkMode describes the connectivity mode reported by the Tailscale/Headscale
// overlay network.
type NetworkMode string

const (
	NetworkModeDirect NetworkMode = "Direct"
	NetworkModeDERP   NetworkMode = "DERP"
)

// NodeNetworkStatus reports the overlay-network state of a Node.
type NodeNetworkStatus struct {
	// IP is the Headscale-assigned overlay IP (e.g. "100.64.0.5").
	IP string `json:"ip,omitempty" yaml:"ip,omitempty"`

	// DNSName is the MagicDNS FQDN for service discovery.
	DNSName string `json:"dnsName,omitempty" yaml:"dnsName,omitempty"`

	// Mode is Direct when a peer-to-peer path exists, DERP otherwise.
	Mode NetworkMode `json:"mode,omitempty" yaml:"mode,omitempty"`

	// AgentPort is the TCP port the Agent's HTTP server listens on.
	// Used by caractl to construct the Agent address for port-forward tunnels.
	AgentPort int `json:"agentPort,omitempty" yaml:"agentPort,omitempty"`

	// Throughput is measured by the Agent at startup and periodically.
	Throughput NodeThroughput `json:"throughput,omitempty" yaml:"throughput,omitempty"`
}

// NodeThroughput holds the last measured upload/download speeds of a Node.
// The Scheduler uses download speed to estimate "time to pull a backup" and
// upload speed to estimate "RPO feasibility".
type NodeThroughput struct {
	Download     string    `json:"download,omitempty"     yaml:"download,omitempty"`
	Upload       string    `json:"upload,omitempty"       yaml:"upload,omitempty"`
	LastTestTime time.Time `json:"lastTestTime,omitempty" yaml:"lastTestTime,omitempty"`
}

// NodeState is the top-level health summary computed by the Controller Manager.
type NodeState string

const (
	NodeStateReady    NodeState = "Ready"
	NodeStateNotReady NodeState = "NotReady"
	NodeStateDraining NodeState = "Draining"
)

// IsValid reports whether s is one of the recognised NodeState constants.
func (s NodeState) IsValid() bool {
	switch s {
	case NodeStateReady,
		NodeStateNotReady,
		NodeStateDraining:
		return true
	default:
		return false
	}
}

// NodeSpec contains the administrator-declared configuration of a Node.
type NodeSpec struct {
	// Hostname is the OS-level hostname of the machine.
	Hostname string `json:"hostname,omitempty" yaml:"hostname,omitempty"`

	// Unschedulable, when true, prevents new Projects from being scheduled
	// onto this Node. The TaintsController will automatically add a
	// NoSchedule Taint when this field is true.
	Unschedulable bool `json:"unschedulable,omitempty" yaml:"unschedulable,omitempty"`
}

// NodeStatus is written by the Agent (heartbeat fields) and the Controller
// Manager (aggregated state, injected taints).
type NodeStatus struct {
	// State is the high-level summary: Ready | NotReady | Draining.
	State NodeState `json:"state,omitempty" yaml:"state,omitempty"`

	// Network describes the overlay-network connectivity.
	Network NodeNetworkStatus `json:"network,omitempty" yaml:"network,omitempty"`

	// Capacity is the raw physical resource total reported by the Agent.
	Capacity ResourceList `json:"capacity,omitempty" yaml:"capacity,omitempty"`

	// Allocatable is Capacity minus system-reserved amounts, also reported
	// by the Agent. The Scheduler subtracts running-Project usage to derive
	// the effective available headroom.
	Allocatable ResourceList `json:"allocatable,omitempty" yaml:"allocatable,omitempty"`

	// LastHeartbeat is the timestamp of the most recent heartbeat received
	// from the Agent. The NodeController uses this to detect timeouts.
	LastHeartbeat time.Time `json:"lastHeartbeat,omitempty" yaml:"lastHeartbeat,omitempty"`

	// Conditions is a list of observable Node conditions.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Node represents a physical or virtual machine managed by a Caravanserai
// Agent. Nodes are either self-registered by the Agent on startup or manually
// approved by an administrator.
//
// YAML example:
//
//	apiVersion: caravanserai/v1
//	kind: Node
//	metadata:
//	  name: pve1-server-03
//	  labels:
//	    caravanserai.io/zone: ed312
//	spec:
//	  hostname: pve-03
//	  unschedulable: false
type Node struct {
	TypeMeta   `json:",inline"    yaml:",inline"`
	ObjectMeta `json:"metadata"   yaml:"metadata"`
	Spec       NodeSpec   `json:"spec,omitempty"   yaml:"spec,omitempty"`
	Status     NodeStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// NodeList is a collection of Node objects returned by list operations.
type NodeList struct {
	TypeMeta `json:",inline"  yaml:",inline"`
	Items    []Node `json:"items" yaml:"items"`
}
