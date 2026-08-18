package overlay

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"
	"tailscale.com/tsnet"
)

// TsnetConfig configures a tsnet-backed overlay Client.
type TsnetConfig struct {
	// ControlURL is the Headscale control-plane base URL.
	ControlURL string
	// PreauthKeyFile is the path to a file containing the pre-auth key.
	PreauthKeyFile string
	// Hostname is the hostname to register with Headscale.
	Hostname string
	// StateDir is where tsnet persists node keys.  When empty a per-user
	// default is used.
	StateDir string
}

// TsnetClient is a Client backed by an embedded userspace Tailscale node.
type TsnetClient struct {
	cfg    TsnetConfig
	logger *zap.Logger

	srv       *tsnet.Server
	overlayIP string
}

// NewTsnetClient builds a tsnet-backed overlay Client.  It validates the
// configuration but does not contact Headscale; the join happens in Join.
func NewTsnetClient(cfg TsnetConfig, logger *zap.Logger) (*TsnetClient, error) {
	if cfg.ControlURL == "" {
		return nil, fmt.Errorf("%w: headscale control URL is required", ErrConfig)
	}
	if cfg.PreauthKeyFile == "" {
		return nil, fmt.Errorf("%w: pre-auth key file is required", ErrConfig)
	}
	if cfg.Hostname == "" {
		return nil, fmt.Errorf("%w: overlay hostname is required", ErrConfig)
	}
	return &TsnetClient{cfg: cfg, logger: logger}, nil
}

// Join reads the pre-auth key, brings the tsnet interface up against the
// configured Headscale control plane, and blocks until an overlay IP is
// assigned or ctx is cancelled.
//
// Errors caused by the caller's input (unreadable or empty key file) wrap
// ErrConfig so JoinWithRetry does not retry them.  Failures reaching Headscale
// are returned unwrapped and are considered transient.
func (c *TsnetClient) Join(ctx context.Context) (JoinResult, error) {
	authKey, err := readPreauthKey(c.cfg.PreauthKeyFile)
	if err != nil {
		return JoinResult{}, err
	}

	stateDir, err := c.resolveStateDir()
	if err != nil {
		return JoinResult{}, err
	}

	// The pre-auth key is deliberately never logged.
	c.srv = &tsnet.Server{
		Dir:        stateDir,
		Hostname:   c.cfg.Hostname,
		AuthKey:    authKey,
		ControlURL: c.cfg.ControlURL,
		Logf:       c.tsnetLogf,
		UserLogf:   c.tsnetLogf,
	}

	status, err := c.srv.Up(ctx)
	if err != nil {
		// Ensure a half-started server does not leak if the join fails.
		_ = c.srv.Close()
		c.srv = nil
		return JoinResult{}, fmt.Errorf("bring up tsnet against %s: %w", c.cfg.ControlURL, err)
	}

	if len(status.TailscaleIPs) == 0 {
		_ = c.srv.Close()
		c.srv = nil
		return JoinResult{}, fmt.Errorf("headscale did not assign an overlay IP")
	}

	c.overlayIP = status.TailscaleIPs[0].String()

	var dnsName string
	if status.Self != nil {
		dnsName = strings.TrimSuffix(status.Self.DNSName, ".")
	}

	return JoinResult{OverlayIP: c.overlayIP, DNSName: dnsName}, nil
}

// OverlayIP returns the overlay IP from the last successful Join.
func (c *TsnetClient) OverlayIP() string {
	return c.overlayIP
}

// HTTPTransport returns an http.RoundTripper whose connections are dialed
// through the embedded tsnet node instead of the host network stack.  This is
// what lets cara-server reach agents on their Headscale overlay IPs (the
// 100.64.0.0/10 CGNAT range), which the host kernel has no route to.  It must
// be called after a successful Join; before then c.srv is nil and the returned
// transport would panic on first use.
func (c *TsnetClient) HTTPTransport() http.RoundTripper {
	return &http.Transport{DialContext: c.srv.Dial}
}

// Close leaves the overlay network.  It is safe to call when Join never ran.
func (c *TsnetClient) Close() error {
	if c.srv == nil {
		return nil
	}
	return c.srv.Close()
}

// tsnetLogf routes tsnet's verbose internal logging to the agent logger at
// debug level so it does not clutter normal operation.
func (c *TsnetClient) tsnetLogf(format string, args ...any) {
	c.logger.Debug("tsnet: " + fmt.Sprintf(format, args...))
}

// resolveStateDir returns the configured state directory, or a per-user
// default under the OS config dir.
func (c *TsnetClient) resolveStateDir() (string, error) {
	if c.cfg.StateDir != "" {
		return c.cfg.StateDir, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("%w: cannot determine state directory: %v", ErrConfig, err)
	}
	return filepath.Join(base, "cara-agent", "tsnet"), nil
}

// readPreauthKey reads and trims the pre-auth key from path.  A missing,
// unreadable, or empty file is a configuration error.
func readPreauthKey(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: read pre-auth key file %q: %v", ErrConfig, path, err)
	}
	key := strings.TrimSpace(string(raw))
	if key == "" {
		return "", fmt.Errorf("%w: pre-auth key file %q is empty", ErrConfig, path)
	}
	return key, nil
}
