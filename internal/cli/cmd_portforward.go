package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

// NewPortForwardCmd returns the "port-forward" command.
//
// Usage:
//
//	caractl port-forward <project>/<service> [LOCAL_PORT:]REMOTE_PORT [--node <name>]
func NewPortForwardCmd() *cobra.Command {
	var node string

	cmd := &cobra.Command{
		Use:   "port-forward <project>/<service> [LOCAL_PORT:]REMOTE_PORT",
		Short: "Forward a local port to a container port on a remote Node",
		Long: `Forward a local port to a container port. Each connection to the local port
opens a WebSocket to cara-server, which relays it to the Agent on the Node
running the Project; the Agent accepts requests from cara-server alone. This
needs only a token for cara-server, and no route to the Node or its overlay.
It works similarly to kubectl port-forward.

The Node is looked up for each new connection, so connections opened after
the Project moves reach the new Node.

Examples:
  # Forward local port 5432 to the "db" service's port 5432 in project "my-app"
  caractl port-forward my-app/db 5432

  # Forward local port 15432 to remote port 5432
  caractl port-forward my-app/db 15432:5432

  # Reach a Node other than the one the Project is assigned to
  caractl port-forward my-app/db 5432 --node main`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, service, err := parseServiceTarget(args[0])
			if err != nil {
				return err
			}
			localPort, remotePort, err := parsePorts(args[1])
			if err != nil {
				return err
			}
			if err := checkNodeName(node); err != nil {
				return err
			}
			cmd.SilenceUsage = true
			serverURL, _ := cmd.Root().PersistentFlags().GetString("server")
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runPortForward(ctx, NewClient(serverURL), os.Getenv(TokenEnv), project, service, node, localPort, remotePort, cmd.ErrOrStderr())
		},
	}

	cmd.Flags().StringVar(&node, "node", "", "Node to reach; the Project's assigned Node if omitted")
	return cmd
}

func runPortForward(ctx context.Context, client *Client, token, project, service, node string, localPort, remotePort int, stderr io.Writer) error {
	// Fail now, not on the first connection, when the Project cannot be
	// reached: it does not exist, or nothing runs it.
	if node == "" {
		p, err := client.GetProject(ctx, project)
		if err != nil {
			return fmt.Errorf("get project %q: %w", project, err)
		}
		if p.Status.NodeRef == "" {
			return fmt.Errorf("project %q is not assigned to a node yet; use --node to reach one", project)
		}
	}

	target, err := forwardURL(client, project, service, node, remotePort)
	if err != nil {
		return err
	}
	header := http.Header{}
	if token != "" {
		header.Set("Authorization", "Bearer "+token)
	}

	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort)))
	if err != nil {
		return fmt.Errorf("listen on port %d: %w", localPort, err)
	}
	defer func() { _ = listener.Close() }()

	fmt.Fprintf(stderr, "Forwarding %s -> %s/%s:%d\n", listener.Addr(), project, service, remotePort)

	var wg sync.WaitGroup
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
				handleConnection(ctx, conn, target, header, stderr)
			}()
		}
	}()

	<-ctx.Done()
	fmt.Fprintln(stderr, "\nShutting down...")
	_ = listener.Close()
	wg.Wait()
	return nil
}

// forwardURL is the WebSocket URL of the server's forward route.
func forwardURL(client *Client, project, service, node string, remotePort int) (string, error) {
	query := url.Values{}
	if node != "" {
		query.Set("node", node)
	}
	u, err := url.Parse(client.serviceURL(project, service, "forward/"+strconv.Itoa(remotePort), query))
	if err != nil {
		return "", fmt.Errorf("server address: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	default:
		return "", fmt.Errorf("server address %q: scheme must be http or https", client.BaseURL)
	}
	return u.String(), nil
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

// dialTimeout bounds opening one tunnel. cara-server gives up on an Agent
// that does not answer sooner than this, and says why.
const dialTimeout = 30 * time.Second

// handleConnection tunnels a single local TCP connection through a WebSocket
// to cara-server's forward route.
func handleConnection(ctx context.Context, conn net.Conn, target string, header http.Header, stderr io.Writer) {
	defer func() { _ = conn.Close() }()

	dialer := websocket.Dialer{HandshakeTimeout: dialTimeout, Proxy: http.ProxyFromEnvironment}
	wsConn, resp, err := dialer.DialContext(ctx, target, header)
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			fmt.Fprintf(stderr, "Error: %s\n", ParseAPIError(bytes.TrimSpace(body), resp.Status, resp.StatusCode))
		} else {
			fmt.Fprintf(stderr, "Cannot open the tunnel through cara-server: %v\n", err)
		}
		return
	}

	fmt.Fprintf(stderr, "Handling connection for %s\n", conn.RemoteAddr())

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

	fmt.Fprintf(stderr, "Connection from %s closed\n", conn.RemoteAddr())
}
