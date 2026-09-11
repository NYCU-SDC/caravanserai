package main

import (
	"fmt"
	"net"
)

// overlayListener is the part of overlay.TsnetClient that agentListeners needs.
// Keeping it to one method lets the tests supply a stub without standing up a
// real tsnet node.
type overlayListener interface {
	Listen(network, addr string) (net.Listener, error)
}

// agentListeners returns every listener the agent API must be served on.
//
// The host socket is always present: localhost tooling, health checks and the
// underlay fallback depend on it. When overlay networking is enabled a second
// listener is added on the node's overlay IP, because a plain socket on the
// host stack is not reachable there — tsnet is a userspace network stack and
// hands traffic addressed to 100.64.0.0/10 only to listeners it created
// itself, dropping the rest rather than forwarding to loopback. Without it
// cara-server's agentdialer dials http://<overlayIP>:<port> and nothing
// answers, which is what made node probe, logs and port-forward fail.
//
// Both listeners use the same port. agentdialer builds the overlay address
// from the port the Node reported, so a different one here would fail exactly
// like the missing listener did: reachable, but nothing responding.
//
// Callers serve every returned listener on a single http.Server, which keeps
// the handler set and the shutdown path single-sourced. On error the caller
// owns the decision to abort; returning it rather than exiting here is what
// makes the failure path testable.
func agentListeners(port string, ov overlayListener) ([]net.Listener, error) {
	hostLn, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
	if err != nil {
		return nil, fmt.Errorf("listen on host socket: %w", err)
	}

	if ov == nil {
		return []net.Listener{hostLn}, nil
	}

	overlayLn, err := ov.Listen("tcp", net.JoinHostPort("", port))
	if err != nil {
		// The host listener is already open; close it so a failed startup does
		// not leave the port bound.
		_ = hostLn.Close()
		return nil, fmt.Errorf("listen on overlay network: %w", err)
	}

	return []net.Listener{hostLn, overlayLn}, nil
}
