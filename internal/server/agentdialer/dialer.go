// Package agentdialer resolves a Cara Node name to the HTTP endpoint of its
// running Agent and returns an *http.Client that can reach it.
//
// It is the single place in cara-server that turns a node identifier into the
// currently stored agent address. Every server→agent HTTP call site MUST go
// through a Dialer instead of building URLs from raw Node status fields.
//
// The abstraction supports two transports so that:
//
//   - Headscale/tailscale deployments can inject an in-process `tsnet.Server`
//     transport once overlay join/configuration lands (see CARA-48/CARA-55);
//   - development, tests, and pre-overlay deployments use the standard
//     net/http transport against the address stored in Node.Status.Network.IP.
//
// Timeouts live on the returned *http.Client so that every call site inherits
// identical, tunable transport behaviour.
package agentdialer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
)

// DefaultAgentPort is used when a Node's Status.Network.AgentPort is 0.
// Kept in sync with the default in cmd/cara-agent/main.go.
const DefaultAgentPort = 9090

// DefaultTimeout is applied to the returned *http.Client when the caller does
// not override it. It bounds the total request time (dial + write + read).
const DefaultTimeout = 10 * time.Second

// Errors returned by Dialer implementations. Callers can use errors.Is to
// distinguish between "we don't have an address for this node" (transient,
// retryable) and "the node does not exist" (permanent).
var (
	// ErrNodeUnreachable indicates the store has no agent address for the
	// named node yet. This is typically transient — the agent has not
	// heartbeated with its routable IP, or has not joined the overlay.
	ErrNodeUnreachable = errors.New("agentdialer: node has no reachable address")

	// ErrNodeLookup wraps any error returned by the underlying store lookup.
	// Callers that need to distinguish "not found" from other lookup failures
	// should errors.Is against store.ErrNotFound on the wrapped chain.
	ErrNodeLookup = errors.New("agentdialer: node lookup failed")
)

// NodeAddressLookup is the narrow store contract the dialer depends on.
// Keeping it minimal makes the dialer trivial to fake in tests.
type NodeAddressLookup interface {
	// GetNode returns the Node resource for the given name. Implementations
	// SHOULD return an error wrapping store.ErrNotFound when the node does
	// not exist.
	GetNode(ctx context.Context, name string) (*v1.Node, error)
}

// Dialer resolves an agent by node name and returns an HTTP client + base URL
// suitable for making requests to that agent.
type Dialer interface {
	// Client returns an HTTP client and the base URL ("http://host:port") for
	// calling the named node's agent. The address is resolved on every call
	// (no caching) so that a Node whose overlay IP changes between calls is
	// observed on the next request without a controller restart.
	Client(ctx context.Context, nodeName string) (*http.Client, string, error)
}

// httpDialer is the concrete Dialer used in production and in tests. It reads
// the node's network address from the store on every call and constructs a
// base URL using its Transport. Today the default transport dials the stored
// address directly; future Headscale wiring can inject a tsnet-backed
// RoundTripper without changing call sites.
type httpDialer struct {
	nodes     NodeAddressLookup
	transport http.RoundTripper
	timeout   time.Duration
}

// Config configures New.
type Config struct {
	// Nodes is the store used to resolve node addresses. Required.
	Nodes NodeAddressLookup

	// Transport is the http.RoundTripper used by every returned *http.Client.
	// When nil, http.DefaultTransport is used, which dials the underlay IP
	// stored in Node.Status.Network.IP.
	//
	// To route traffic over the Headscale overlay, callers will pass a
	// tsnet.Server-backed RoundTripper after CARA-48/CARA-55 wire the server
	// into the overlay. The dialer itself has no compile-time dependency on
	// tsnet so tests do not need to pull in the WireGuard stack.
	Transport http.RoundTripper

	// Timeout bounds the total HTTP request lifetime. When zero,
	// DefaultTimeout applies.
	Timeout time.Duration
}

// New returns a Dialer configured with cfg.
func New(cfg Config) Dialer {
	if cfg.Nodes == nil {
		panic("agentdialer.New: Nodes is required")
	}

	transport := cfg.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	return &httpDialer{
		nodes:     cfg.Nodes,
		transport: transport,
		timeout:   timeout,
	}
}

// Client implements Dialer.
func (d *httpDialer) Client(ctx context.Context, nodeName string) (*http.Client, string, error) {
	node, err := d.nodes.GetNode(ctx, nodeName)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %w", ErrNodeLookup, err)
	}

	ip := node.Status.Network.IP
	if ip == "" {
		return nil, "", fmt.Errorf("%w: node %q has no Status.Network.IP", ErrNodeUnreachable, nodeName)
	}

	port := node.Status.Network.AgentPort
	if port == 0 {
		port = DefaultAgentPort
	}

	baseURL := "http://" + net.JoinHostPort(ip, strconv.Itoa(port))

	return &http.Client{
		Transport: d.transport,
		Timeout:   d.timeout,
	}, baseURL, nil
}
