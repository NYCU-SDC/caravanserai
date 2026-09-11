package main

import (
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubOverlay stands in for overlay.TsnetClient. Recording the address it was
// asked for is the point of the port test below: agentdialer builds the overlay
// address from the port the Node reported, so the two listeners must agree.
type stubOverlay struct {
	gotNetwork string
	gotAddr    string
	err        error
}

func (s *stubOverlay) Listen(network, addr string) (net.Listener, error) {
	s.gotNetwork, s.gotAddr = network, addr
	if s.err != nil {
		return nil, s.err
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// freePort returns a port nothing is listening on, so the host listener can
// bind without colliding with whatever else runs on the machine.
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	require.NoError(t, err)
	require.NoError(t, ln.Close())
	return port
}

func closeAll(t *testing.T, lns []net.Listener) {
	t.Helper()
	for _, ln := range lns {
		_ = ln.Close()
	}
}

// The defect this whole ticket exists for: with overlay networking on, the
// agent must listen on the overlay as well as the host stack. A plain socket
// on the host is invisible to tsnet's userspace stack, so serving only there
// leaves the overlay IP with nothing answering.
func TestAgentListenersServesOverlayWhenEnabled(t *testing.T) {
	ov := &stubOverlay{}

	lns, err := agentListeners(freePort(t), ov)
	require.NoError(t, err)
	defer closeAll(t, lns)

	require.Len(t, lns, 2, "overlay enabled must yield a host and an overlay listener")
	assert.Equal(t, "tcp", ov.gotNetwork)
}

// Overlay networking is opt-in. With it off the agent must behave exactly as
// before and create no tsnet listener.
func TestAgentListenersHostOnlyWhenOverlayDisabled(t *testing.T) {
	lns, err := agentListeners(freePort(t), nil)
	require.NoError(t, err)
	defer closeAll(t, lns)

	require.Len(t, lns, 1)
}

// Both listeners must use the port the Node reports, because that is the port
// cara-server's agentdialer puts in http://<overlayIP>:<port>. A mismatch here
// fails exactly like the missing listener did — reachable, nothing responding —
// so it is worth pinning rather than trusting.
func TestAgentListenersUseTheSamePort(t *testing.T) {
	port := freePort(t)
	ov := &stubOverlay{}

	lns, err := agentListeners(port, ov)
	require.NoError(t, err)
	defer closeAll(t, lns)

	_, hostPort, err := net.SplitHostPort(lns[0].Addr().String())
	require.NoError(t, err)
	assert.Equal(t, port, hostPort, "host listener must use the configured port")

	_, askedPort, err := net.SplitHostPort(ov.gotAddr)
	require.NoError(t, err)
	assert.Equal(t, port, askedPort, "overlay listener must use the same port as the host listener")
}

// Overlay was requested, so failing to listen on it is fatal rather than a
// silent fall back to the underlay — the same contract the join already has.
// The host listener must not stay bound after the failure.
func TestAgentListenersFailWhenOverlayListenFails(t *testing.T) {
	port := freePort(t)
	sentinel := errors.New("tsnet not up")

	lns, err := agentListeners(port, &stubOverlay{err: sentinel})

	require.Error(t, err)
	assert.ErrorIs(t, err, sentinel)
	assert.Nil(t, lns)

	// The port is free again, proving the host listener was closed on the way
	// out rather than leaked.
	probe, listenErr := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port))
	require.NoError(t, listenErr, "host listener should have been closed after the overlay failure")
	_ = probe.Close()
}
