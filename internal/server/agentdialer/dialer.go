// Package agentdialer resolves a Cara Node name to the HTTP endpoint of its
// running Agent and returns an *http.Client that can reach it.
package agentdialer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	v1 "NYCU-SDC/caravanserai/api/v1"
	"NYCU-SDC/caravanserai/internal/agentauth"
)

// DefaultAgentPort is used when a Node's Status.Network.AgentPort is 0.
const DefaultAgentPort = 9090

// DefaultTimeout is applied to the returned *http.Client when the caller does
// not override it. It bounds the total request time (dial + write + read).
const DefaultTimeout = 10 * time.Second

var (
	// ErrNodeUnreachable indicates that the node exists but does not have a reachable IP.
	ErrNodeUnreachable = errors.New("agentdialer: node has no reachable address")

	// ErrNodeLookup indicates that the node could not be found in the store.
	ErrNodeLookup = errors.New("agentdialer: node lookup failed")
)

// NodeAddressLookup is the narrow store contract the dialer depends on.
type NodeAddressLookup interface {
	// GetNode returns the Node resource for the given name.
	GetNode(ctx context.Context, name string) (*v1.Node, error)
}

// Dialer resolves an agent by node name and returns an HTTP client + base URL
type Dialer interface {
	// Client returns an HTTP client and the base URL ("http://host:port") for
	// calling the named node's agent. The address is resolved on every call
	// (no caching) so that a Node whose overlay IP changes between calls is
	// observed on the next request without a controller restart.
	Client(ctx context.Context, nodeName string) (*http.Client, string, error)
}

// EndpointResolver is what a proxy to an Agent needs: the transport and base
// URL without the client timeout, which would cut a log stream or a
// port-forward tunnel short.
type EndpointResolver interface {
	// Endpoint returns a RoundTripper that reaches the named node's agent
	// and signs every request for it, and the agent's base URL.
	Endpoint(ctx context.Context, nodeName string) (http.RoundTripper, *url.URL, error)
}

// httpDialer is the concrete Dialer used in production and in tests. It reads
// the node's network address from the store on every call and constructs a
// base URL using its Transport. Today the default transport dials the stored
// address directly; future Headscale wiring can inject a tsnet-backed
// RoundTripper without changing call sites.
type httpDialer struct {
	nodes     NodeAddressLookup
	transport http.RoundTripper
	signer    *agentauth.Signer
	timeout   time.Duration
}

// Config configures New.
type Config struct {
	// Nodes is the store used to resolve node addresses. Required.
	Nodes NodeAddressLookup

	// Transport is the http.RoundTripper used by every returned *http.Client.
	// When nil, http.DefaultTransport is used, which dials the underlay IP
	// stored in Node.Status.Network.OverlayIP.
	Transport http.RoundTripper

	// Signer signs every request to an agent (CARA-139); agents refuse
	// requests without a signature for them. Nil sends requests unsigned,
	// which only tests do.
	Signer *agentauth.Signer

	Timeout time.Duration
}

// HTTPDialer is a Dialer and an EndpointResolver.
type HTTPDialer interface {
	Dialer
	EndpointResolver
}

// New returns a dialer configured with cfg.
func New(cfg Config) HTTPDialer {
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
		signer:    cfg.Signer,
		timeout:   timeout,
	}
}

// Client implements Dialer.
func (d *httpDialer) Client(ctx context.Context, nodeName string) (*http.Client, string, error) {
	transport, base, err := d.Endpoint(ctx, nodeName)
	if err != nil {
		return nil, "", err
	}
	return &http.Client{
		Transport: transport,
		Timeout:   d.timeout,
	}, base.String(), nil
}

// Endpoint implements EndpointResolver.
func (d *httpDialer) Endpoint(ctx context.Context, nodeName string) (http.RoundTripper, *url.URL, error) {
	node, err := d.nodes.GetNode(ctx, nodeName)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrNodeLookup, err)
	}

	ip := node.Status.Network.OverlayIP
	if ip == "" {
		return nil, nil, fmt.Errorf("%w: node %q has no Status.Network.OverlayIP", ErrNodeUnreachable, nodeName)
	}

	port := node.Status.Network.AgentPort
	if port == 0 {
		port = DefaultAgentPort
	}

	base := &url.URL{Scheme: "http", Host: net.JoinHostPort(ip, strconv.Itoa(port))}

	transport := d.transport
	if d.signer != nil {
		transport = &agentauth.Transport{Signer: d.signer, Node: nodeName, Base: transport}
	}
	return transport, base, nil
}
