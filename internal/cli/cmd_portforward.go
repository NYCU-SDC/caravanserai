package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

// NewPortForwardCmd returns the "port-forward" command.
//
// Usage:
//
//	caractrl port-forward <project>/<service> [LOCAL_PORT:]REMOTE_PORT [--node <agentAddr>]
func NewPortForwardCmd() *cobra.Command {
	var nodeAddr string

	cmd := &cobra.Command{
		Use:   "port-forward <project>/<service> [LOCAL_PORT:]REMOTE_PORT",
		Short: "Forward a local port to a container port on a remote Node",
		Long: `Forward a local port to a container port on a remote Node via the Agent's
WebSocket tunnel. This works similarly to kubectl port-forward.

Examples:
  # Forward local port 5432 to the "db" service's port 5432 in project "my-app"
  caractrl port-forward my-app/db 5432

  # Forward local port 15432 to remote port 5432
  caractrl port-forward my-app/db 15432:5432

  # Specify Agent address explicitly
  caractrl port-forward my-app/db 5432 --node 192.168.1.100:9090`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPortForward(cmd, args, nodeAddr)
		},
	}

	cmd.Flags().StringVar(&nodeAddr, "node", "", "Agent address (host:port); auto-resolved from server if omitted")
	return cmd
}

func runPortForward(cmd *cobra.Command, args []string, nodeAddr string) error {
	// Parse <project>/<service>
	parts := strings.SplitN(args[0], "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("invalid target %q: expected <project>/<service>", args[0])
	}
	project, service := parts[0], parts[1]

	// Parse [LOCAL_PORT:]REMOTE_PORT
	localPort, remotePort, err := parsePorts(args[1])
	if err != nil {
		return err
	}

	// Resolve agent address.
	if nodeAddr == "" {
		serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
		resolved, resolveErr := resolveAgentAddr(serverURL, project)
		if resolveErr != nil {
			return resolveErr
		}
		nodeAddr = resolved
	}

	// Set up signal handling.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Open local TCP listener.
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", localPort, err)
	}
	defer func() { _ = listener.Close() }()

	fmt.Fprintf(os.Stderr, "Forwarding 127.0.0.1:%d -> %s/%s:%d\n", localPort, project, service, remotePort)

	// Track active connections for graceful shutdown.
	var wg sync.WaitGroup

	// Accept loop in a goroutine so we can select on ctx.Done().
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				// Listener closed.
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				handleConnection(ctx, conn, nodeAddr, project, service, remotePort)
			}()
		}
	}()

	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "\nShutting down...")
	_ = listener.Close()
	wg.Wait()
	return nil
}

// parsePorts parses a port spec of the form "[LOCAL:]REMOTE" and returns
// (localPort, remotePort, error). If only one port is given, it is used for
// both local and remote.
func parsePorts(spec string) (int, int, error) {
	parts := strings.SplitN(spec, ":", 2)
	if len(parts) == 1 {
		p, err := strconv.Atoi(parts[0])
		if err != nil || p < 1 || p > 65535 {
			return 0, 0, fmt.Errorf("invalid port %q", spec)
		}
		return p, p, nil
	}

	local, err := strconv.Atoi(parts[0])
	if err != nil || local < 1 || local > 65535 {
		return 0, 0, fmt.Errorf("invalid local port %q", parts[0])
	}
	remote, err := strconv.Atoi(parts[1])
	if err != nil || remote < 1 || remote > 65535 {
		return 0, 0, fmt.Errorf("invalid remote port %q", parts[1])
	}
	return local, remote, nil
}

// resolveAgentAddr queries the cara-server to find the agent address for the
// project's node.
func resolveAgentAddr(serverURL, project string) (string, error) {
	client := NewClient(serverURL)
	ctx := context.Background()

	// 1. Get the project to find nodeRef.
	p, err := client.GetProject(ctx, project)
	if err != nil {
		return "", fmt.Errorf("get project %q: %w", project, err)
	}
	if p.Status.NodeRef == "" {
		return "", fmt.Errorf("project %q has no nodeRef (not scheduled); use --node to specify the agent address", project)
	}

	// 2. Get the node to find IP and agent port.
	node, err := client.GetNode(ctx, p.Status.NodeRef)
	if err != nil {
		return "", fmt.Errorf("get node %q: %w", p.Status.NodeRef, err)
	}

	ip := node.Status.Network.IP
	if ip == "" {
		return "", fmt.Errorf("node %q has no IP address; use --node to specify the agent address", p.Status.NodeRef)
	}

	agentPort := node.Status.Network.AgentPort
	if agentPort == 0 {
		agentPort = 9090 // default
		fmt.Fprintf(os.Stderr, "Warning: node %q has no agentPort set, defaulting to %d\n", p.Status.NodeRef, agentPort)
	}

	return net.JoinHostPort(ip, strconv.Itoa(agentPort)), nil
}

// handleConnection tunnels a single local TCP connection through a WebSocket
// to the Agent's forward endpoint.
func handleConnection(ctx context.Context, conn net.Conn, agentAddr, project, service string, remotePort int) {
	defer func() { _ = conn.Close() }()

	wsURL := url.URL{
		Scheme: "ws",
		Host:   agentAddr,
		Path:   fmt.Sprintf("/api/v1/forward/%s/%s/%d", project, service, remotePort),
	}

	dialer := websocket.Dialer{}
	wsConn, resp, err := dialer.DialContext(ctx, wsURL.String(), nil)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			fmt.Fprintf(os.Stderr, "Error: %s\n", extractProblemDetail(body, resp.Status))
		} else {
			fmt.Fprintf(os.Stderr, "WebSocket dial failed: %v\n", err)
		}
		return
	}

	fmt.Fprintf(os.Stderr, "Handling connection for %s\n", conn.RemoteAddr())

	done := make(chan struct{}, 2)

	// TCP -> WebSocket
	go func() {
		defer func() { done <- struct{}{} }()
		buf := make([]byte, 32*1024)
		for {
			n, readErr := conn.Read(buf)
			if n > 0 {
				if writeErr := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); writeErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	// WebSocket -> TCP
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			_, msg, readErr := wsConn.ReadMessage()
			if readErr != nil {
				return
			}
			if _, writeErr := conn.Write(msg); writeErr != nil {
				return
			}
		}
	}()

	// Wait for either direction to close.
	<-done

	_ = wsConn.Close()
	_ = conn.Close()

	// Wait for the other goroutine.
	<-done

	fmt.Fprintf(os.Stderr, "Connection from %s closed\n", conn.RemoteAddr())
}

// extractProblemDetail attempts to parse an RFC 7807 problem+json body and
// return the human-readable detail field. Falls back to the raw body if
// parsing fails.
func extractProblemDetail(body []byte, httpStatus string) string {
	var p struct {
		Detail string `json:"detail"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(body, &p); err == nil && p.Detail != "" {
		return p.Detail
	}
	// Fallback: return raw body with HTTP status.
	return fmt.Sprintf("%s: %s", httpStatus, strings.TrimSpace(string(body)))
}
