// Package overlay implements the cara-agent side of the Headscale overlay
// network join flow (CARA-55).
//
// On startup the agent joins a Headscale-managed WireGuard mesh so that the
// control plane can reach it by a stable overlay IP even when the agent lives
// behind NAT.  The concrete implementation embeds a userspace Tailscale node
// via tsnet (see tsnet.go); the Client interface keeps that dependency behind
// a seam so the join flow can be unit-tested with a fake.
//
// Overlay networking is opt-in in 1.0.  Callers decide whether to invoke Join
// at all based on configuration; this package only concerns itself with
// performing the join once asked.
package overlay

import (
	"context"
	"errors"
)

// ErrConfig marks a join failure that stems from configuration or input the
// caller controls (missing/empty pre-auth key file, malformed key, invalid
// hostname).  Retrying such a failure cannot succeed, so JoinWithRetry returns
// immediately when a join error wraps ErrConfig.  Failures that do not wrap
// ErrConfig are treated as transient (e.g. Headscale not yet reachable) and are
// retried.
var ErrConfig = errors.New("overlay configuration error")

// JoinResult reports the overlay identity assigned to the agent after a
// successful join.
type JoinResult struct {
	// OverlayIP is the Headscale-assigned overlay IP (e.g. "100.64.0.5").
	OverlayIP string
	// DNSName is the MagicDNS FQDN for the node, when available.
	DNSName string
}

// Client joins the overlay network and exposes the resulting identity.
//
// Implementations must classify configuration/input errors by wrapping
// ErrConfig so that JoinWithRetry can distinguish permanent failures from
// transient ones.
type Client interface {
	// Join brings the overlay interface up, authenticating against the
	// configured Headscale control plane, and blocks until an overlay IP is
	// assigned or ctx is cancelled.  It returns the assigned identity on
	// success.
	Join(ctx context.Context) (JoinResult, error)

	// OverlayIP returns the overlay IP assigned by the most recent successful
	// Join, or the empty string if the agent has not joined.  Later tickets
	// (CARA-53) report this to the control plane.
	OverlayIP() string

	// Close leaves the overlay network, releasing any resources.  It is safe
	// to call even if Join never succeeded.
	Close() error
}
